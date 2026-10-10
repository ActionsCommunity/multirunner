package operations

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type journalStore struct {
	mu       sync.Mutex
	sequence int64
	inputs   []EventInput
	block    chan struct{}
	started  chan struct{}
	once     sync.Once
	err      error
}

type notifyingJournalStore struct {
	journalStore
	listener func(Event)
}

type shutdownJournalStore struct {
	mu       sync.Mutex
	sequence int64
	calls    int
	started  chan struct{}
	inputs   []EventInput
}

func (s *shutdownJournalStore) AppendOperationalEvent(
	ctx context.Context, epoch string, input EventInput,
) (Event, error) {
	s.mu.Lock()
	s.calls++
	call := s.calls
	s.mu.Unlock()
	if call == 1 {
		close(s.started)
		<-ctx.Done()
		return Event{}, ctx.Err()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sequence++
	s.inputs = append(s.inputs, input)
	return Event{
		ID: EventID(epoch, s.sequence), HostEpoch: epoch, Sequence: s.sequence,
		Type: input.Type, EntityType: input.EntityType, EntityID: input.EntityID,
		ActorKind: input.ActorKind,
	}, nil
}

func (s *notifyingJournalStore) SetOperationalEventListener(listener func(Event)) {
	s.listener = listener
}

func (s *notifyingJournalStore) AppendOperationalEvent(
	ctx context.Context, epoch string, input EventInput,
) (Event, error) {
	event, err := s.journalStore.AppendOperationalEvent(ctx, epoch, input)
	if err == nil && s.listener != nil {
		s.listener(event)
	}
	return event, err
}

func (s *journalStore) AppendOperationalEvent(ctx context.Context, epoch string, input EventInput) (Event, error) {
	if s.started != nil {
		s.once.Do(func() { close(s.started) })
	}
	if s.block != nil {
		select {
		case <-ctx.Done():
			return Event{}, ctx.Err()
		case <-s.block:
		}
	}
	if s.err != nil {
		return Event{}, s.err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sequence++
	s.inputs = append(s.inputs, input)
	return Event{
		ID: EventID(epoch, s.sequence), HostEpoch: epoch, Sequence: s.sequence,
		Type: input.Type, EntityType: input.EntityType, EntityID: input.EntityID,
		ActorKind: input.ActorKind,
	}, nil
}

func validEventInput(entityID string) EventInput {
	return EventInput{
		Type: "runner.planned", EntityType: "runner_session", EntityID: entityID,
		ActorKind: ActorSystem,
	}
}

func TestJournalAppendPublishesDurableEvent(t *testing.T) {
	store := &journalStore{}
	journal, err := NewJournal(t.Context(), store, "epoch", JournalOptions{QueueSize: 2})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(journal.Close)
	subscription, err := journal.Subscribe(1)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(subscription.Close)

	event, err := journal.Append(t.Context(), validEventInput("session-1"))
	if err != nil {
		t.Fatal(err)
	}
	select {
	case published := <-subscription.Events:
		if published.ID != event.ID || published.Sequence != 1 {
			t.Fatalf("published event = %+v, appended = %+v", published, event)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for published event")
	}
}

func TestJournalPublishesCommittedStoreEventsExactlyOnce(t *testing.T) {
	store := &notifyingJournalStore{}
	journal, err := NewJournal(t.Context(), store, "epoch", JournalOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(journal.Close)
	subscription, err := journal.Subscribe(4)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(subscription.Close)

	event, err := journal.Append(t.Context(), validEventInput("session"))
	if err != nil {
		t.Fatal(err)
	}
	if published := <-subscription.Events; published.ID != event.ID {
		t.Fatalf("published event = %+v, want %+v", published, event)
	}
	select {
	case duplicate := <-subscription.Events:
		t.Fatalf("event published twice: %+v", duplicate)
	case <-time.After(20 * time.Millisecond):
	}

	external := Event{
		ID: "epoch:99", HostEpoch: "epoch", Sequence: 99,
		Type: "command.queued", EntityType: "command", EntityID: "command",
	}
	store.listener(external)
	if published := <-subscription.Events; published.ID != external.ID {
		t.Fatalf("external event = %+v, want %+v", published, external)
	}
}

func TestJournalTryAppendReportsQueueOverflow(t *testing.T) {
	store := &journalStore{block: make(chan struct{}), started: make(chan struct{})}
	var overflows atomic.Int32
	journal, err := NewJournal(t.Context(), store, "epoch", JournalOptions{
		QueueSize: 1, OnQueueOverflow: func() { overflows.Add(1) },
	})
	if err != nil {
		t.Fatal(err)
	}
	var unblock sync.Once
	release := func() { unblock.Do(func() { close(store.block) }) }
	t.Cleanup(func() {
		release()
		journal.Close()
	})

	if !journal.TryAppend(validEventInput("first")) {
		t.Fatal("first TryAppend rejected")
	}
	select {
	case <-store.started:
	case <-time.After(time.Second):
		t.Fatal("journal did not start first append")
	}
	if !journal.TryAppend(validEventInput("queued")) {
		t.Fatal("queued TryAppend rejected")
	}
	if journal.TryAppend(validEventInput("overflow")) {
		t.Fatal("overflow TryAppend accepted")
	}
	if overflows.Load() != 1 {
		t.Fatalf("overflow callbacks = %d, want 1", overflows.Load())
	}
	release()
	deadline := time.Now().Add(time.Second)
	for {
		store.mu.Lock()
		var evidence EventInput
		for _, input := range store.inputs {
			if input.Type == "console.journal_events_rejected" {
				evidence = input
				break
			}
		}
		store.mu.Unlock()
		if evidence.Type != "" {
			if !strings.Contains(string(evidence.Payload), `"queue_full":1`) {
				t.Fatalf("rejection evidence = %s", evidence.Payload)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("journal did not persist queue overflow evidence")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestJournalDropsSlowSubscriberWithoutBlocking(t *testing.T) {
	store := &journalStore{}
	var slow atomic.Int32
	journal, err := NewJournal(t.Context(), store, "epoch", JournalOptions{
		QueueSize: 2, OnSlowSubscriber: func() { slow.Add(1) },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(journal.Close)
	subscription, err := journal.Subscribe(1)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := journal.Append(t.Context(), validEventInput("first")); err != nil {
		t.Fatal(err)
	}
	if _, err := journal.Append(t.Context(), validEventInput("second")); err != nil {
		t.Fatal(err)
	}
	if slow.Load() != 1 {
		t.Fatalf("slow callbacks = %d, want 1", slow.Load())
	}
	first, ok := <-subscription.Events
	if !ok || first.Sequence != 1 {
		t.Fatalf("first buffered event = %+v, open=%v", first, ok)
	}
	if _, ok := <-subscription.Events; ok {
		t.Fatal("slow subscription remained open")
	}
}

func TestCertifiedSixteenClientFanoutDropsSlowClientWithinSLO(t *testing.T) {
	store := &journalStore{}
	var slow atomic.Int32
	journal, err := NewJournal(t.Context(), store, "epoch", JournalOptions{
		QueueSize: 4, OnSlowSubscriber: func() { slow.Add(1) },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(journal.Close)

	subscriptions := make([]*Subscription, 0, 16)
	for index := 0; index < 16; index++ {
		buffer := 2
		if index == 0 {
			buffer = 1
		}
		subscription, err := journal.Subscribe(buffer)
		if err != nil {
			t.Fatal(err)
		}
		subscriptions = append(subscriptions, subscription)
		t.Cleanup(subscription.Close)
	}

	started := time.Now()
	if _, err := journal.Append(t.Context(), validEventInput("first")); err != nil {
		t.Fatal(err)
	}
	if _, err := journal.Append(t.Context(), validEventInput("second")); err != nil {
		t.Fatal(err)
	}
	duration := time.Since(started)
	if duration > time.Second {
		t.Fatalf("16-client fanout took %s, SLO is 1s", duration)
	}
	if slow.Load() != 1 {
		t.Fatalf("slow client callbacks = %d, want 1", slow.Load())
	}
	if _, ok := <-subscriptions[0].Events; !ok {
		t.Fatal("slow client lost its first buffered event")
	}
	if _, ok := <-subscriptions[0].Events; ok {
		t.Fatal("slow client remained connected")
	}
	for index, subscription := range subscriptions[1:] {
		first := <-subscription.Events
		second := <-subscription.Events
		if first.Sequence != 1 || second.Sequence != 2 {
			t.Fatalf("client %d events = %d,%d", index+2, first.Sequence, second.Sequence)
		}
	}
}

func TestJournalAppendErrorAndClose(t *testing.T) {
	sentinel := errors.New("disk unavailable")
	var observed atomic.Int32
	store := &journalStore{err: sentinel}
	journal, err := NewJournal(t.Context(), store, "epoch", JournalOptions{
		OnAppendError: func(err error) {
			if errors.Is(err, sentinel) {
				observed.Add(1)
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := journal.Append(t.Context(), validEventInput("failed")); !errors.Is(err, sentinel) {
		t.Fatalf("Append error = %v, want sentinel", err)
	}
	if observed.Load() != 1 {
		t.Fatalf("append error callbacks = %d, want 1", observed.Load())
	}
	journal.Close()
	if _, err := journal.Append(t.Context(), validEventInput("closed")); !errors.Is(err, ErrJournalClosed) {
		t.Fatalf("Append after close = %v, want ErrJournalClosed", err)
	}

	if journal.TryAppend(validEventInput("closed")) {
		t.Fatal("TryAppend after close succeeded")
	}
	if _, err := journal.Subscribe(1); !errors.Is(err, ErrJournalClosed) {
		t.Fatalf("Subscribe after close = %v, want ErrJournalClosed", err)
	}
}

func TestJournalCloseReportsRejectedPendingBestEffortEvents(t *testing.T) {
	store := &shutdownJournalStore{started: make(chan struct{})}
	var shutdowns atomic.Int32
	journal, err := NewJournal(t.Context(), store, "epoch", JournalOptions{
		QueueSize: 2,
		OnRejected: func(reason RejectionReason) {
			if reason == RejectionShutdown {
				shutdowns.Add(1)
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !journal.TryAppend(validEventInput("in-flight")) {
		t.Fatal("in-flight event rejected")
	}
	select {
	case <-store.started:
	case <-time.After(time.Second):
		t.Fatal("journal did not start first append")
	}
	if !journal.TryAppend(validEventInput("pending")) {
		t.Fatal("pending event rejected")
	}
	closed := make(chan error, 1)
	go func() { closed <- journal.CloseWithError() }()
	if err := <-closed; err != nil {
		t.Fatalf("Close: %v", err)
	}
	if shutdowns.Load() == 0 {
		t.Fatal("shutdown rejection was not reported")
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	var evidence EventInput
	for _, input := range store.inputs {
		if input.Type == "console.journal_events_rejected" {
			evidence = input
		}
	}
	if evidence.Type == "" || !strings.Contains(string(evidence.Payload), `"shutdown":`) {
		t.Fatalf("shutdown evidence = %s", evidence.Payload)
	}
}
