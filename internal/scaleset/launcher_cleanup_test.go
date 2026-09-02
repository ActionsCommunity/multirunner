package scaleset

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GerardSmit/multirunner/internal/backend"
	upstream "github.com/actions/scaleset"
)

func TestShutdownKillsAndDeregistersRunners(t *testing.T) {
	jit := &fakeJIT{}
	be := &fakeBackend{}
	l := New(jit, be, Options{ScaleSetID: 1})

	if _, err := l.HandleDesiredRunnerCount(t.Context(), 2); err != nil {
		t.Fatalf("launch: %v", err)
	}
	if err := l.Shutdown(t.Context()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	waitFor(t, func() bool { return l.Running() == 0 })
	if len(jit.removed) != 2 {
		t.Fatalf("removed %d registrations, want 2", len(jit.removed))
	}
	for _, handle := range be.handles {
		if handle.kills() != 1 {
			t.Errorf("kill count = %d, want 1", handle.kills())
		}
	}
}

func TestShutdownGivesEveryRunnerIndependentCleanupContexts(t *testing.T) {
	jit := &fakeJIT{}
	be := &fakeBackend{}
	l := New(jit, be, Options{ScaleSetID: 1, cleanupTimeout: 20 * time.Millisecond})

	if _, err := l.HandleDesiredRunnerCount(t.Context(), 3); err != nil {
		t.Fatalf("launch: %v", err)
	}
	be.handles[0].setBlockKill(true)

	err := l.Shutdown(t.Context())
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("shutdown error = %v, want deadline exceeded from blocked kill", err)
	}
	if running := l.Running(); running != 1 {
		t.Fatalf("running after failed kill = %d, want 1", running)
	}

	if removed := jit.removedIDs(); len(removed) != 2 {
		t.Fatalf("removed %d registrations, want 2 because the unconfirmed runner must be preserved", len(removed))
	}
	for i, handle := range be.handles {
		if handle.kills() != 1 {
			t.Errorf("handle %d kill count = %d, want 1", i, handle.kills())
		}
	}

	be.handles[0].setBlockKill(false)
	if err := l.Shutdown(t.Context()); err != nil {
		t.Fatalf("retry shutdown: %v", err)
	}
	if running := l.Running(); running != 0 {
		t.Fatalf("running after successful retry = %d, want 0", running)
	}
	if be.handles[0].kills() != 2 {
		t.Fatalf("failed handle kill count = %d, want 2", be.handles[0].kills())
	}
	if removed := jit.removedIDs(); len(removed) != 3 {
		t.Fatalf("removed %d registrations after retry, want 3", len(removed))
	}
}

func TestShutdownStartsRemovalTimeoutAfterClientLockIsAvailable(t *testing.T) {
	jit := &fakeJIT{blockFirstRemove: true}
	be := &fakeBackend{}
	l := New(jit, be, Options{ScaleSetID: 1, cleanupTimeout: 20 * time.Millisecond})

	if _, err := l.HandleDesiredRunnerCount(t.Context(), 3); err != nil {
		t.Fatalf("launch: %v", err)
	}

	err := l.Shutdown(t.Context())
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("shutdown error = %v, want deadline exceeded from first removal", err)
	}
	waitFor(t, func() bool { return l.Running() == 0 })

	if attempts := jit.removalAttempts(); len(attempts) < 3 {
		t.Fatalf("attempted %d removals, want at least 3", len(attempts))
	}
	if len(jit.removed) < 2 {
		t.Fatalf("completed %d removals, want unaffected runners to converge", len(jit.removed))
	}
	if err := l.Shutdown(t.Context()); err != nil {
		t.Fatalf("retry shutdown: %v", err)
	}
	if len(jit.removed) != 3 {
		t.Fatalf("completed %d removals after retry, want 3", len(jit.removed))
	}
}

func TestAlreadyRemovedRegistrationIsSuccessfulCleanup(t *testing.T) {
	for _, removeErr := range []error{
		upstream.RunnerNotFoundError,
		errors.New(`request failed(status="404 Not Found")`),
	} {
		jit := &fakeJIT{removeErr: removeErr}
		be := &fakeBackend{}
		l := New(jit, be, Options{ScaleSetID: 1})

		if _, err := l.HandleDesiredRunnerCount(t.Context(), 1); err != nil {
			t.Fatalf("launch: %v", err)
		}
		be.finish(0)
		waitFor(t, func() bool { return l.Running() == 0 })
		if err := l.Shutdown(t.Context()); err != nil {
			t.Fatalf("duplicate cleanup after 404: %v", err)
		}
	}
}

func TestIdleExitAndShutdownCleanupAreNotDuplicated(t *testing.T) {
	jit := &fakeJIT{}
	be := &fakeBackend{}
	l := New(jit, be, Options{ScaleSetID: 1})

	if _, err := l.HandleDesiredRunnerCount(t.Context(), 1); err != nil {
		t.Fatalf("launch: %v", err)
	}
	be.finish(0)
	waitFor(t, func() bool { return len(jit.removedIDs()) == 1 })
	if err := l.Shutdown(t.Context()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	if removed := jit.removedIDs(); len(removed) != 1 {
		t.Fatalf("cleanup attempts completed for %v, want exactly one", removed)
	}
}

func TestIdleExitReportsStopWhileCleanupRemainsRetryable(t *testing.T) {
	jit := &fakeJIT{blockFirstRemove: true}
	be := &fakeBackend{}
	stopped := make(chan error, 1)
	l := New(jit, be, Options{
		ScaleSetID:     1,
		cleanupTimeout: 20 * time.Millisecond,
		OnStop: func(_ int, err error) {
			stopped <- err
		},
	})

	if _, err := l.HandleDesiredRunnerCount(t.Context(), 1); err != nil {
		t.Fatalf("launch: %v", err)
	}
	be.finish(0)
	waitFor(t, func() bool {
		return len(jit.removalAttempts()) == 1
	})
	select {
	case err := <-stopped:
		if err != nil {
			t.Fatalf("runner stop callback error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("runner stop callback did not report process exit")
	}
	if attempts := be.ownedRemovalAttempts(); len(attempts) != 0 {
		t.Fatalf("backend removed before registration converged: %v", attempts)
	}
	if _, err := l.HandleDesiredRunnerCount(t.Context(), 0); err != nil {
		t.Fatalf("retry cleanup: %v", err)
	}
	if attempts := be.ownedRemovalAttempts(); len(attempts) != 1 {
		t.Fatalf("backend cleanup attempts = %v, want one", attempts)
	}
	select {
	case err := <-stopped:
		t.Fatalf("runner stop callback was duplicated: %v", err)
	default:
	}
}

func TestReconcileRemovesOwnedBackendResourcesAndRegistrations(t *testing.T) {
	ownership := backend.RunnerOwnership{
		Instance:   "host-a",
		Target:     "https://github.com/o/r",
		Pool:       "linux",
		ScaleSetID: 7,
	}
	be := &fakeBackend{owned: []backend.OwnedRunner{
		{ResourceID: "container-1", Name: "runner-1", RunnerID: 11},
		{ResourceID: "container-2", Name: "runner-2", RunnerID: 12},
	}}
	jit := &fakeJIT{}
	l := New(jit, be, Options{ScaleSetID: 7, Ownership: ownership})

	count, err := l.Reconcile(t.Context())
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if count != 2 {
		t.Fatalf("reconciled = %d, want 2", count)
	}
	if be.listOwnership != ownership {
		t.Fatalf("listed ownership = %+v, want %+v", be.listOwnership, ownership)
	}
	if attempts := be.ownedRemovalAttempts(); len(attempts) != 2 {
		t.Fatalf("backend removals = %v, want two", attempts)
	}
	if removed := jit.removedIDs(); len(removed) != 2 {
		t.Fatalf("registration removals = %v, want two", removed)
	}
}

func TestReconcileRejectsBackendWithoutOwnershipCapability(t *testing.T) {
	l := New(&fakeJIT{}, unsupportedBackend{}, Options{ScaleSetID: 7})
	if _, err := l.Reconcile(t.Context()); err == nil ||
		!strings.Contains(err.Error(), "does not support scale-set ownership reconciliation") {
		t.Fatalf("Reconcile error = %v, want unsupported ownership capability", err)
	}
}

func TestReconcilePreservesBackendRecordWhenRemovalTimesOut(t *testing.T) {
	be := &fakeBackend{
		owned:             []backend.OwnedRunner{{ResourceID: "container-1", Name: "runner-1", RunnerID: 11}},
		blockOwnedRemoval: true,
	}
	jit := &fakeJIT{}
	l := New(jit, be, Options{
		ScaleSetID: 7,
		Ownership: backend.RunnerOwnership{
			Instance: "host-a", Target: "https://github.com/o/r", Pool: "linux", ScaleSetID: 7,
		},
		cleanupTimeout: 20 * time.Millisecond,
	})

	count, err := l.Reconcile(t.Context())
	if count != 0 || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Reconcile = (%d, %v), want zero and deadline exceeded", count, err)
	}
	if removed := jit.removedIDs(); len(removed) != 1 {
		t.Fatalf("removed registrations %v, want registration cleanup before backend removal", removed)
	}
	if attempts := be.ownedRemovalAttempts(); len(attempts) != 1 {
		t.Fatalf("backend removal attempts = %v, want retained retry record", attempts)
	}
}

func TestReconcilePreservesBackendRecordWhenDeregistrationFails(t *testing.T) {
	be := &fakeBackend{
		owned: []backend.OwnedRunner{{ResourceID: "container-1", Name: "runner-1", RunnerID: 11}},
	}
	jit := &fakeJIT{removeErr: errors.New("service unavailable")}
	l := New(jit, be, Options{
		ScaleSetID: 7,
		Ownership: backend.RunnerOwnership{
			Instance: "host-a", Target: "https://github.com/o/r", Pool: "linux", ScaleSetID: 7,
		},
	})

	count, err := l.Reconcile(t.Context())
	if count != 0 || err == nil {
		t.Fatalf("Reconcile = (%d, %v), want zero and deregistration error", count, err)
	}
	if attempts := be.ownedRemovalAttempts(); len(attempts) != 0 {
		t.Fatalf("backend removal attempts = %v, want none before deregistration", attempts)
	}
}

func TestIdleExitPreservesBackendRecordUntilDeregistrationConverges(t *testing.T) {
	jit := &fakeJIT{removeErr: errors.New("service unavailable")}
	be := &fakeBackend{}
	l := New(jit, be, Options{ScaleSetID: 1})

	if _, err := l.HandleDesiredRunnerCount(t.Context(), 1); err != nil {
		t.Fatalf("launch: %v", err)
	}
	be.finish(0)
	waitFor(t, func() bool {
		return len(jit.removalAttempts()) == 1
	})
	if attempts := be.ownedRemovalAttempts(); len(attempts) != 0 {
		t.Fatalf("backend removed before deregistration: %v", attempts)
	}

	jit.setRemovalError(nil)
	if _, err := l.HandleDesiredRunnerCount(t.Context(), 0); err != nil {
		t.Fatalf("retry cleanup: %v", err)
	}
	if attempts := be.ownedRemovalAttempts(); len(attempts) != 1 {
		t.Fatalf("backend removal attempts = %v, want one after deregistration", attempts)
	}
}

func TestBusyExitCleanupRetryDoesNotDeregisterCompletedRunner(t *testing.T) {
	jit := &fakeJIT{}
	be := &fakeBackend{removeOwnedErr: errors.New("daemon unavailable")}
	l := New(jit, be, Options{ScaleSetID: 1})

	if _, err := l.HandleDesiredRunnerCount(t.Context(), 1); err != nil {
		t.Fatalf("launch: %v", err)
	}
	req := be.requests()[0]
	if err := l.HandleJobStarted(t.Context(), &upstream.JobStarted{
		RunnerID: 1, RunnerName: req.Name,
	}); err != nil {
		t.Fatalf("HandleJobStarted: %v", err)
	}
	be.finish(0)
	waitFor(t, func() bool { return len(be.ownedRemovalAttempts()) == 1 })
	if removed := jit.removedIDs(); len(removed) != 0 {
		t.Fatalf("busy runner deregistered after normal exit: %v", removed)
	}

	be.setOwnedRemovalError(nil)
	if _, err := l.HandleDesiredRunnerCount(t.Context(), 0); err != nil {
		t.Fatalf("retry cleanup: %v", err)
	}
	if removed := jit.removedIDs(); len(removed) != 0 {
		t.Fatalf("busy runner deregistered during cleanup retry: %v", removed)
	}
}

func TestReconcileDeregistersBeforeRemovingBackendResource(t *testing.T) {
	var mu sync.Mutex
	var events []string
	record := func(event string) {
		mu.Lock()
		defer mu.Unlock()
		events = append(events, event)
	}
	jit := &fakeJIT{removeHook: func(int64) { record("registration") }}
	be := &fakeBackend{
		owned:           []backend.OwnedRunner{{ResourceID: "container-1", Name: "runner-1", RunnerID: 11}},
		removeOwnedHook: func(string) { record("backend") },
	}
	l := New(jit, be, Options{
		ScaleSetID: 7,
		Ownership: backend.RunnerOwnership{
			Instance: "host-a", Target: "https://github.com/o/r", Pool: "linux", ScaleSetID: 7,
		},
	})

	if _, err := l.Reconcile(t.Context()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if got := strings.Join(events, ","); got != "registration,backend" {
		t.Fatalf("cleanup order = %q, want registration,backend", got)
	}
}

func TestLifecycleCallbacksTrackRunner(t *testing.T) {
	be := &fakeBackend{}
	var starts, stops atomic.Int32
	l := New(&fakeJIT{}, be, Options{
		ScaleSetID: 1,
		OnStart:    func() { starts.Add(1) },
		OnStop:     func(int, error) { stops.Add(1) },
	})

	if _, err := l.HandleDesiredRunnerCount(t.Context(), 1); err != nil {
		t.Fatalf("launch: %v", err)
	}
	if got := starts.Load(); got != 1 {
		t.Fatalf("start callbacks = %d, want 1", got)
	}
	be.finish(0)
	waitFor(t, func() bool { return stops.Load() == 1 })
}

func TestJobCallbacksDoNotProvision(t *testing.T) {
	be := &fakeBackend{}
	l := New(&fakeJIT{}, be, Options{ScaleSetID: 1})

	if err := l.HandleJobStarted(t.Context(), &upstream.JobStarted{}); err != nil {
		t.Fatalf("HandleJobStarted: %v", err)
	}
	if err := l.HandleJobCompleted(t.Context(), &upstream.JobCompleted{}); err != nil {
		t.Fatalf("HandleJobCompleted: %v", err)
	}
	if n := len(be.requests()); n != 0 {
		t.Fatalf("job callbacks launched %d runners, want 0", n)
	}
}
