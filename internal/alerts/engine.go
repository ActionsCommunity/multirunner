package alerts

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/GerardSmit/multirunner/internal/operations"
)

type Store interface {
	AlertRulesForEvent(context.Context, string) ([]Rule, error)
	ObserveAlert(context.Context, Observation) (Instance, error)
	ResolveAlertByRuleKey(context.Context, string, string, time.Time, string) error
	PromoteDueAlerts(context.Context, time.Time) ([]Instance, error)
	AlertEventCursor(context.Context, string) (int64, error)
	AdvanceAlertEventCursor(context.Context, operations.Event) error
	ListOperationalEvents(context.Context, operations.EventQuery) ([]operations.Event, error)
}

type EventSource interface {
	Subscribe(int) (*operations.Subscription, error)
}

type Engine struct {
	store   Store
	source  EventSource
	now     func() time.Time
	logger  *slog.Logger
	epochID string
}

type Options struct {
	Store   Store
	Source  EventSource
	Now     func() time.Time
	Logger  *slog.Logger
	EpochID string
}

func NewEngine(options Options) (*Engine, error) {
	if options.Store == nil || options.Source == nil {
		return nil, errors.New("alert store and event source are required")
	}
	if options.EpochID == "" {
		return nil, errors.New("alert host epoch is required")
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	if options.Logger == nil {
		options.Logger = slog.Default()
	}
	return &Engine{
		store: options.Store, source: options.Source, now: options.Now,
		logger: options.Logger, epochID: options.EpochID,
	}, nil
}

func (e *Engine) Run(ctx context.Context) error {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		subscription, err := e.source.Subscribe(4096)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			e.logger.Error("subscribe alert engine", "err", err)
			if err := waitForRetry(ctx, time.Second); err != nil {
				return err
			}
			continue
		}
		cursor, err := e.replay(ctx)
		if err != nil {
			subscription.Close()
			if ctx.Err() != nil {
				return ctx.Err()
			}
			e.logger.Error("replay alert events", "err", err)
			if err := waitForRetry(ctx, time.Second); err != nil {
				return err
			}
			continue
		}
		resubscribe := false
		for !resubscribe {
			select {
			case <-ctx.Done():
				subscription.Close()
				return ctx.Err()
			case event, ok := <-subscription.Events:
				if !ok {
					e.logger.Warn("alert event subscription closed; replaying durable journal")
					resubscribe = true
					continue
				}
				if event.HostEpoch != e.epochID || event.Sequence <= cursor {
					continue
				}
				if event.Sequence > cursor+1 {
					cursor, err = e.replay(ctx)
					if err != nil {
						e.logger.Error("replay alert event gap", "event_id", event.ID, "err", err)
						resubscribe = true
						continue
					}
					if event.Sequence <= cursor {
						continue
					}
				}
				if err := e.Evaluate(ctx, event); err != nil {
					e.logger.Error("evaluate alert event", "event_id", event.ID, "err", err)
					resubscribe = true
					continue
				}
				cursor = event.Sequence
			case <-ticker.C:
				if _, err := e.store.PromoteDueAlerts(ctx, e.now().UTC()); err != nil {
					e.logger.Error("promote due alerts", "err", err)
				}
			}
		}
		subscription.Close()
	}
}

func waitForRetry(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (e *Engine) Evaluate(ctx context.Context, event operations.Event) error {
	if err := e.resolveRecovery(ctx, event); err != nil {
		return err
	}
	rules, err := e.store.AlertRulesForEvent(ctx, event.Type)
	if err != nil {
		return err
	}
	for _, rule := range rules {
		details := append(json.RawMessage(nil), event.Payload...)
		if len(details) == 0 {
			details = json.RawMessage(`{}`)
		}
		_, err := e.store.ObserveAlert(ctx, Observation{
			Rule: rule, DedupKey: event.EntityID, Summary: rule.Name,
			Details: details, SourceEventID: event.ID, ObservedAt: event.Timestamp,
			CorrelationID: event.CorrelationID,
		})
		if err != nil {
			return fmt.Errorf("observe rule %s: %w", rule.ID, err)
		}
	}
	if event.HostEpoch != "" && event.Sequence > 0 {
		if err := e.store.AdvanceAlertEventCursor(ctx, event); err != nil {
			return fmt.Errorf("advance alert event cursor: %w", err)
		}
	}
	return nil
}

// Replay evaluates durable events for the engine's host epoch through the
// persisted cursor without subscribing to live delivery.
func (e *Engine) Replay(ctx context.Context) error {
	_, err := e.replay(ctx)
	return err
}

func (e *Engine) replay(ctx context.Context) (int64, error) {
	cursor, err := e.store.AlertEventCursor(ctx, e.epochID)
	if err != nil {
		return 0, fmt.Errorf("read alert event cursor: %w", err)
	}
	for {
		events, err := e.store.ListOperationalEvents(ctx, operations.EventQuery{
			HostEpoch: e.epochID, AfterSequence: cursor, Limit: 1000,
		})
		if err != nil {
			return cursor, fmt.Errorf("replay alert events: %w", err)
		}
		if len(events) == 0 {
			return cursor, nil
		}
		for _, event := range events {
			if err := e.Evaluate(ctx, event); err != nil {
				return cursor, err
			}
			cursor = event.Sequence
		}
	}
}

func (e *Engine) resolveRecovery(ctx context.Context, event operations.Event) error {
	switch event.Type {
	case "runner.registered", "runner.launched", "runner.stopped", "runner.failed":
		err := e.store.ResolveAlertByRuleKey(
			ctx, "runner-stuck", event.EntityID, event.Timestamp, event.CorrelationID,
		)
		if errors.Is(err, ErrNotFound) {
			return nil
		}
		return err
	default:
		return nil
	}
}
