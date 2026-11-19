package alerts

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

type DeliveryStore interface {
	ClaimNotificationDelivery(context.Context, string, time.Time, time.Duration) (Delivery, error)
	CompleteNotificationDelivery(context.Context, string, string, time.Time) error
	FailNotificationDelivery(context.Context, string, string, string, time.Time, time.Time, bool) error
}

type DeliveryAdapter interface {
	Deliver(context.Context, Delivery) error
}

type AdapterResolver interface {
	AdapterFor(string) (DeliveryAdapter, bool)
}

type AdapterMap map[string]DeliveryAdapter

func (m AdapterMap) AdapterFor(endpointID string) (DeliveryAdapter, bool) {
	adapter, ok := m[endpointID]
	return adapter, ok
}

type PermanentError struct {
	Err error
}

func (e *PermanentError) Error() string { return e.Err.Error() }
func (e *PermanentError) Unwrap() error { return e.Err }

type DeliveryWorker struct {
	store        DeliveryStore
	adapters     AdapterResolver
	owner        string
	now          func() time.Time
	lease        time.Duration
	pollInterval time.Duration
	timeout      time.Duration
	maxAttempts  int
	logger       *slog.Logger
}

type DeliveryWorkerOptions struct {
	Store        DeliveryStore
	Adapters     AdapterResolver
	Owner        string
	Now          func() time.Time
	Lease        time.Duration
	PollInterval time.Duration
	Timeout      time.Duration
	MaxAttempts  int
	Logger       *slog.Logger
}

func NewDeliveryWorker(options DeliveryWorkerOptions) (*DeliveryWorker, error) {
	if options.Store == nil || options.Adapters == nil {
		return nil, errors.New("delivery store and adapter resolver are required")
	}
	if options.Owner == "" {
		return nil, errors.New("delivery worker owner is required")
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	if options.Lease <= 0 {
		options.Lease = 30 * time.Second
	}
	if options.PollInterval <= 0 {
		options.PollInterval = time.Second
	}
	if options.Timeout <= 0 {
		options.Timeout = 10 * time.Second
	}
	if options.MaxAttempts <= 0 {
		options.MaxAttempts = 8
	}
	if options.Logger == nil {
		options.Logger = slog.Default()
	}
	return &DeliveryWorker{
		store: options.Store, adapters: options.Adapters, owner: options.Owner,
		now: options.Now, lease: options.Lease, pollInterval: options.PollInterval,
		timeout: options.Timeout, maxAttempts: options.MaxAttempts,
		logger: options.Logger,
	}, nil
}

func (w *DeliveryWorker) Run(ctx context.Context) error {
	ticker := time.NewTicker(w.pollInterval)
	defer ticker.Stop()
	for {
		worked, err := w.RunOnce(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			w.logger.Error("notification delivery worker iteration failed", "err", err)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-ticker.C:
			}
			continue
		}
		if worked {
			continue
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (w *DeliveryWorker) RunOnce(ctx context.Context) (bool, error) {
	now := w.now().UTC()
	delivery, err := w.store.ClaimNotificationDelivery(ctx, w.owner, now, w.lease)
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("claim notification delivery: %w", err)
	}
	adapter, ok := w.adapters.AdapterFor(delivery.EndpointID)
	if !ok {
		err := w.store.FailNotificationDelivery(
			ctx, delivery.ID, w.owner, "notification endpoint is unavailable",
			now, now, true,
		)
		return true, err
	}
	deliverContext, cancel := context.WithTimeout(ctx, w.timeout)
	deliverErr := adapter.Deliver(deliverContext, delivery)
	cancel()
	completedAt := w.now().UTC()
	if deliverErr == nil {
		if err := w.store.CompleteNotificationDelivery(
			ctx, delivery.ID, w.owner, completedAt,
		); err != nil {
			return true, err
		}
		return true, nil
	}
	var permanent *PermanentError
	dead := errors.As(deliverErr, &permanent) || delivery.Attempt >= w.maxAttempts
	next := completedAt.Add(deliveryBackoff(delivery.Attempt))
	if err := w.store.FailNotificationDelivery(
		ctx, delivery.ID, w.owner, deliverErr.Error(), completedAt, next, dead,
	); err != nil {
		return true, err
	}
	if dead {
		w.logger.Error("notification delivery dead-lettered",
			"delivery_id", delivery.ID, "endpoint_id", delivery.EndpointID,
			"attempt", delivery.Attempt, "err", deliverErr)
	}
	return true, nil
}

func deliveryBackoff(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	delay := 5 * time.Second
	for index := 1; index < attempt && delay < 15*time.Minute; index++ {
		delay *= 2
	}
	if delay > 15*time.Minute {
		return 15 * time.Minute
	}
	return delay
}
