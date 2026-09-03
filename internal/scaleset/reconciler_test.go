package scaleset

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/GerardSmit/multirunner/internal/backend"
)

func TestPeriodicReconciliationRunsAndStopsWithContext(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	be := &fakeBackend{owned: []backend.OwnedRunner{{
		ResourceID: "stale", Name: "stale", RunnerID: 11,
	}}}
	l := New(ctx, &fakeJIT{}, be, Options{
		ScaleSetID:        7,
		Ownership:         backend.RunnerOwnership{Instance: "host-a", Target: "target", Pool: "linux", ScaleSetID: 7},
		reconcileInterval: 5 * time.Millisecond,
	})

	done := make(chan struct{})
	go func() {
		defer close(done)
		l.reconcilePeriodically(ctx)
	}()
	waitFor(t, func() bool { return be.ownedListCalls() > 0 })
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("periodic reconciliation did not stop after cancellation")
	}
	calls := be.ownedListCalls()
	time.Sleep(15 * time.Millisecond)
	if got := be.ownedListCalls(); got != calls {
		t.Fatalf("reconciliation continued after cancellation: %d calls became %d", calls, got)
	}
}

func TestReconciliationPassIsBounded(t *testing.T) {
	be := &fakeBackend{blockList: true, listStarted: make(chan struct{})}
	l := New(t.Context(), &fakeJIT{}, be, Options{
		ScaleSetID:     7,
		Ownership:      backend.RunnerOwnership{Instance: "host-a", Target: "target", Pool: "linux", ScaleSetID: 7},
		cleanupTimeout: 20 * time.Millisecond,
	})

	_, err := l.Reconcile(t.Context())
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Reconcile error = %v, want deadline exceeded", err)
	}
}
