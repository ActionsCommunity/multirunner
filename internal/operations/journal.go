package operations

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

var ErrJournalClosed = errors.New("operational journal is closed")

type RejectionReason string

const (
	RejectionQueueFull   RejectionReason = "queue_full"
	RejectionShutdown    RejectionReason = "shutdown"
	RejectionAppendError RejectionReason = "append_error"
)

type EventStore interface {
	AppendOperationalEvent(context.Context, string, EventInput) (Event, error)
}

type EventCommitNotifier interface {
	SetOperationalEventListener(func(Event))
}

type JournalOptions struct {
	QueueSize        int
	OnAppendError    func(error)
	OnQueueOverflow  func()
	OnSlowSubscriber func()
	OnRejected       func(RejectionReason)
}

type Journal struct {
	ctx            context.Context
	cancel         context.CancelFunc
	store          EventStore
	epochID        string
	queue          chan appendRequest
	done           chan struct{}
	options        JournalOptions
	closed         atomic.Bool
	storePublishes bool
	rejectionReady chan struct{}
	closeErr       error

	mu          sync.Mutex
	nextSubID   uint64
	subscribers map[uint64]chan Event

	rejectionMu sync.Mutex
	rejections  map[RejectionReason]uint64
}

type appendRequest struct {
	input  EventInput
	result chan appendResult
}

type appendResult struct {
	event Event
	err   error
}

type Subscription struct {
	Events <-chan Event
	cancel func()
	once   sync.Once
}

func (s *Subscription) Close() {
	if s == nil || s.cancel == nil {
		return
	}
	s.once.Do(s.cancel)
}

func NewJournal(parent context.Context, store EventStore, epochID string, options JournalOptions) (*Journal, error) {
	if parent == nil {
		return nil, errors.New("journal context is required")
	}
	if store == nil {
		return nil, errors.New("journal event store is required")
	}
	if epochID == "" {
		return nil, errors.New("journal host epoch is required")
	}
	if options.QueueSize <= 0 {
		options.QueueSize = 4096
	}
	ctx, cancel := context.WithCancel(parent)
	journal := &Journal{
		ctx: ctx, cancel: cancel, store: store, epochID: epochID,
		queue: make(chan appendRequest, options.QueueSize), done: make(chan struct{}),
		options: options, subscribers: make(map[uint64]chan Event),
		rejectionReady: make(chan struct{}, 1),
		rejections:     make(map[RejectionReason]uint64),
	}
	if notifier, ok := store.(EventCommitNotifier); ok {
		notifier.SetOperationalEventListener(journal.publish)
		journal.storePublishes = true
	}
	go journal.run()
	return journal, nil
}

func (j *Journal) Close() {
	_ = j.CloseWithError()
}

func (j *Journal) CloseWithError() error {
	if j == nil {
		return nil
	}
	if !j.closed.CompareAndSwap(false, true) {
		<-j.done
		return j.closeErr
	}
	j.cancel()
	<-j.done
	if notifier, ok := j.store.(EventCommitNotifier); ok {
		notifier.SetOperationalEventListener(nil)
	}
	return j.closeErr
}

func (j *Journal) Append(ctx context.Context, input EventInput) (Event, error) {
	if ctx == nil {
		return Event{}, errors.New("append context is required")
	}
	if j.closed.Load() {
		return Event{}, ErrJournalClosed
	}
	result := make(chan appendResult, 1)
	request := appendRequest{input: input, result: result}
	select {
	case <-ctx.Done():
		return Event{}, ctx.Err()
	case <-j.ctx.Done():
		return Event{}, ErrJournalClosed
	case j.queue <- request:
	}
	select {
	case <-ctx.Done():
		return Event{}, ctx.Err()
	case <-j.ctx.Done():
		select {
		case result := <-result:
			return result.event, result.err
		default:
			return Event{}, ErrJournalClosed
		}
	case result := <-result:
		return result.event, result.err
	}
}

func (j *Journal) TryAppend(input EventInput) bool {
	if j.closed.Load() {
		j.recordRejection(RejectionShutdown)
		return false
	}
	select {
	case <-j.ctx.Done():
		j.recordRejection(RejectionShutdown)
		return false
	case j.queue <- appendRequest{input: input}:
		return true
	default:
		j.recordRejection(RejectionQueueFull)
		if j.options.OnQueueOverflow != nil {
			j.options.OnQueueOverflow()
		}
		return false
	}
}

func (j *Journal) Subscribe(buffer int) (*Subscription, error) {
	if buffer <= 0 {
		return nil, errors.New("subscription buffer must be positive")
	}
	if j.closed.Load() {
		return nil, ErrJournalClosed
	}
	select {
	case <-j.ctx.Done():
		return nil, ErrJournalClosed
	default:
	}
	channel := make(chan Event, buffer)
	j.mu.Lock()
	id := j.nextSubID
	j.nextSubID++
	j.subscribers[id] = channel
	j.mu.Unlock()
	return &Subscription{
		Events: channel,
		cancel: func() {
			j.mu.Lock()
			if existing, ok := j.subscribers[id]; ok {
				delete(j.subscribers, id)
				close(existing)
			}
			j.mu.Unlock()
		},
	}, nil
}

func (j *Journal) run() {
	defer close(j.done)
	defer j.closeSubscribers()
	for {
		select {
		case <-j.ctx.Done():
			j.rejectPending()
			flushContext, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			j.closeErr = j.flushRejectionEvidence(flushContext)
			cancel()
			return
		default:
		}
		select {
		case <-j.ctx.Done():
			j.rejectPending()
			flushContext, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			j.closeErr = j.flushRejectionEvidence(flushContext)
			cancel()
			return
		case <-j.rejectionReady:
			if err := j.flushRejectionEvidence(j.ctx); err != nil &&
				j.options.OnAppendError != nil {
				j.options.OnAppendError(fmt.Errorf("append journal rejection evidence: %w", err))
			}
		case request := <-j.queue:
			event, err := j.store.AppendOperationalEvent(j.ctx, j.epochID, request.input)
			if err != nil {
				if request.result == nil {
					reason := RejectionAppendError
					if j.ctx.Err() != nil && errors.Is(err, context.Canceled) {
						reason = RejectionShutdown
					}
					j.recordRejection(reason)
				}
				if j.options.OnAppendError != nil {
					j.options.OnAppendError(fmt.Errorf("append operational event: %w", err))
				}
			} else if !j.storePublishes {
				j.publish(event)
			}
			if request.result != nil {
				request.result <- appendResult{event: event, err: err}
			}
		}
	}
}

func (j *Journal) publish(event Event) {
	j.mu.Lock()
	defer j.mu.Unlock()
	for id, subscriber := range j.subscribers {
		select {
		case subscriber <- event:
		default:
			delete(j.subscribers, id)
			close(subscriber)
			if j.options.OnSlowSubscriber != nil {
				j.options.OnSlowSubscriber()
			}
		}
	}
}

func (j *Journal) rejectPending() {
	for {
		select {
		case request := <-j.queue:
			if request.result != nil {
				request.result <- appendResult{err: ErrJournalClosed}
			} else {
				j.recordRejection(RejectionShutdown)
			}
		default:
			return
		}
	}
}

func (j *Journal) closeSubscribers() {
	j.mu.Lock()
	defer j.mu.Unlock()
	for id, subscriber := range j.subscribers {
		delete(j.subscribers, id)
		close(subscriber)
	}
}

func (j *Journal) recordRejection(reason RejectionReason) {
	j.rejectionMu.Lock()
	j.rejections[reason]++
	j.rejectionMu.Unlock()
	if j.options.OnRejected != nil {
		j.options.OnRejected(reason)
	}
	select {
	case j.rejectionReady <- struct{}{}:
	default:
	}
}

func (j *Journal) flushRejectionEvidence(ctx context.Context) error {
	counts := j.takeRejections()
	if len(counts) == 0 {
		return nil
	}
	payload, err := json.Marshal(map[string]any{
		"rejected": counts,
		"message":  "Best-effort operational events were rejected before durable append.",
	})
	if err != nil {
		j.restoreRejections(counts)
		return fmt.Errorf("encode journal rejection evidence: %w", err)
	}
	event, err := j.store.AppendOperationalEvent(ctx, j.epochID, EventInput{
		Type:       "console.journal_events_rejected",
		EntityType: "health",
		EntityID:   "operational-journal",
		Timestamp:  time.Now().UTC(),
		ActorKind:  ActorSystem,
		ActorID:    "multirunner",
		Payload:    payload,
	})
	if err != nil {
		j.restoreRejections(counts)
		return err
	}
	if !j.storePublishes {
		j.publish(event)
	}
	return nil
}

func (j *Journal) takeRejections() map[RejectionReason]uint64 {
	j.rejectionMu.Lock()
	defer j.rejectionMu.Unlock()
	if len(j.rejections) == 0 {
		return nil
	}
	counts := j.rejections
	j.rejections = make(map[RejectionReason]uint64)
	return counts
}

func (j *Journal) restoreRejections(counts map[RejectionReason]uint64) {
	j.rejectionMu.Lock()
	defer j.rejectionMu.Unlock()
	for reason, count := range counts {
		j.rejections[reason] += count
	}
}
