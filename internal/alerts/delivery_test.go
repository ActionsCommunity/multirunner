package alerts

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"
)

type workerStore struct {
	delivery      Delivery
	failures      int
	claimFailures int
	dead          bool
	next          time.Time
	done          bool
	cancel        context.CancelFunc
}

func (s *workerStore) ClaimNotificationDelivery(
	context.Context, string, time.Time, time.Duration,
) (Delivery, error) {
	if s.claimFailures > 0 {
		s.claimFailures--
		return Delivery{}, errors.New("temporary store failure")
	}
	if s.done {
		return Delivery{}, ErrNotFound
	}
	s.delivery.Attempt++
	return s.delivery, nil
}

func (s *workerStore) CompleteNotificationDelivery(
	context.Context, string, string, time.Time,
) error {
	s.done = true
	if s.cancel != nil {
		s.cancel()
	}
	return nil
}

func (s *workerStore) FailNotificationDelivery(
	_ context.Context, _, _, _ string, _, next time.Time, dead bool,
) error {
	s.failures++
	s.next = next
	s.dead = dead
	s.done = true
	return nil
}

type deliveryAdapterFunc func(context.Context, Delivery) error

func (f deliveryAdapterFunc) Deliver(ctx context.Context, delivery Delivery) error {
	return f(ctx, delivery)
}

func TestDeliveryWorkerRetriesThenDeadLetters(t *testing.T) {
	started := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	store := &workerStore{delivery: Delivery{
		ID: "delivery-1", EndpointID: "endpoint-1", Attempt: 6,
	}}
	worker, err := NewDeliveryWorker(DeliveryWorkerOptions{
		Store: store, Adapters: AdapterMap{
			"endpoint-1": deliveryAdapterFunc(func(context.Context, Delivery) error {
				return errors.New("temporary failure")
			}),
		},
		Owner: "worker-1", Now: func() time.Time { return started },
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	worked, err := worker.RunOnce(t.Context())
	if err != nil || !worked {
		t.Fatalf("RunOnce worked=%v err=%v", worked, err)
	}
	if store.failures != 1 || store.dead || store.next != started.Add(5*time.Minute+20*time.Second) {
		t.Fatalf("retry state failures=%d dead=%v next=%v", store.failures, store.dead, store.next)
	}

	store = &workerStore{delivery: Delivery{
		ID: "delivery-2", EndpointID: "endpoint-1", Attempt: 7,
	}}
	worker.store = store
	worked, err = worker.RunOnce(t.Context())
	if err != nil || !worked || !store.dead {
		t.Fatalf("dead letter worked=%v err=%v store=%+v", worked, err, store)
	}
}

func TestDeliveryWorkerPermanentAndMissingAdaptersDeadLetter(t *testing.T) {
	started := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name     string
		adapters AdapterMap
	}{
		{
			name: "permanent",
			adapters: AdapterMap{
				"endpoint-1": deliveryAdapterFunc(func(context.Context, Delivery) error {
					return &PermanentError{Err: errors.New("invalid endpoint")}
				}),
			},
		},
		{name: "missing", adapters: AdapterMap{}},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := &workerStore{delivery: Delivery{
				ID: "delivery-1", EndpointID: "endpoint-1",
			}}
			worker, err := NewDeliveryWorker(DeliveryWorkerOptions{
				Store: store, Adapters: test.adapters, Owner: "worker-1",
				Now:    func() time.Time { return started },
				Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
			})
			if err != nil {
				t.Fatal(err)
			}
			if worked, err := worker.RunOnce(t.Context()); err != nil || !worked {
				t.Fatalf("RunOnce worked=%v err=%v", worked, err)
			}
			if !store.dead {
				t.Fatal("delivery was not dead-lettered")
			}
		})
	}
}

func TestDeliveryWorkerContinuesAfterTransientStoreFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	store := &workerStore{
		delivery:      Delivery{ID: "delivery-1", EndpointID: "endpoint-1"},
		claimFailures: 1,
		cancel:        cancel,
	}
	worker, err := NewDeliveryWorker(DeliveryWorkerOptions{
		Store: store, Adapters: AdapterMap{
			"endpoint-1": deliveryAdapterFunc(func(context.Context, Delivery) error {
				return nil
			}),
		},
		Owner: "worker-1", PollInterval: time.Millisecond,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := worker.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run error = %v, want context cancellation", err)
	}
	if !store.done || store.delivery.Attempt != 1 {
		t.Fatalf("delivery after transient failure = %+v", store)
	}
}
