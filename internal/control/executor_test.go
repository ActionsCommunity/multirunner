package control_test

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/GerardSmit/multirunner/internal/control"
	"github.com/GerardSmit/multirunner/internal/history"
	"github.com/GerardSmit/multirunner/internal/operations"
)

var errSimulatedCrash = errors.New("simulated process crash")

type fakeAdapter struct {
	mu             sync.Mutex
	executeCount   int
	reconcileCount int
	lastInvocation control.Invocation
	effect         control.EffectResult
	reconcile      control.ReconcileResult
	blockUntilDone bool
}

func (a *fakeAdapter) Prepare(context.Context, control.Invocation) (json.RawMessage, error) {
	return json.RawMessage(`{"status":"before"}`), nil
}

func (a *fakeAdapter) Execute(ctx context.Context, invocation control.Invocation) (control.EffectResult, error) {
	a.mu.Lock()
	a.executeCount++
	a.lastInvocation = invocation
	effect := a.effect
	blockUntilDone := a.blockUntilDone
	a.mu.Unlock()
	if blockUntilDone {
		<-ctx.Done()
		return control.EffectResult{Disposition: control.EffectUnknown}, ctx.Err()
	}
	return effect, nil
}

func (a *fakeAdapter) Reconcile(_ context.Context, invocation control.Invocation) (control.ReconcileResult, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.reconcileCount++
	a.lastInvocation = invocation
	return a.reconcile, nil
}

func TestExecutorCrashBeforeEffectReconcilesAbsentWithoutDispatch(t *testing.T) {
	store := openControlStore(t)
	command := queueCommand(t, store, "before-effect")
	adapter := &fakeAdapter{
		effect:    control.EffectResult{Disposition: control.EffectSucceeded},
		reconcile: control.ReconcileResult{Disposition: control.ReconcileAbsent},
	}
	executor := newTestExecutor(t, store, adapter, control.ExecutorHooks{
		BeforeEffect: func(control.Command) error { return errSimulatedCrash },
	})
	if worked, err := executor.RunOnce(t.Context()); !worked || !errors.Is(err, errSimulatedCrash) {
		t.Fatalf("RunOnce before-effect crash worked=%v err=%v", worked, err)
	}
	assertCommandState(t, store, command.ID, control.StateRunning)
	time.Sleep(15 * time.Millisecond)

	recovery := newTestExecutor(t, store, adapter, control.ExecutorHooks{})
	if worked, err := recovery.RunOnce(t.Context()); !worked || err != nil {
		t.Fatalf("recovery RunOnce worked=%v err=%v", worked, err)
	}
	recovered := assertCommandState(t, store, command.ID, control.StateInterrupted)
	if adapter.executeCount != 0 || adapter.reconcileCount != 1 {
		t.Fatalf("adapter calls execute=%d reconcile=%d", adapter.executeCount, adapter.reconcileCount)
	}
	if recovered.FencingToken != 2 || adapter.lastInvocation.FencingToken != 2 {
		t.Fatalf("recovery fencing command=%d invocation=%d", recovered.FencingToken, adapter.lastInvocation.FencingToken)
	}
}

func TestExecutorCrashAfterEffectReconcilesAppliedWithoutRedispatch(t *testing.T) {
	store := openControlStore(t)
	command := queueCommand(t, store, "after-effect")
	adapter := &fakeAdapter{
		effect: control.EffectResult{
			Disposition: control.EffectSucceeded,
			Outcome:     json.RawMessage(`{"status":"applied"}`),
		},
		reconcile: control.ReconcileResult{
			Disposition: control.ReconcileApplied,
			Outcome:     json.RawMessage(`{"status":"observed"}`),
		},
	}
	executor := newTestExecutor(t, store, adapter, control.ExecutorHooks{
		AfterEffect: func(control.Command, control.EffectResult) error {
			return errSimulatedCrash
		},
	})
	if worked, err := executor.RunOnce(t.Context()); !worked || !errors.Is(err, errSimulatedCrash) {
		t.Fatalf("RunOnce after-effect crash worked=%v err=%v", worked, err)
	}
	time.Sleep(15 * time.Millisecond)

	recovery := newTestExecutor(t, store, adapter, control.ExecutorHooks{})
	if worked, err := recovery.RunOnce(t.Context()); !worked || err != nil {
		t.Fatalf("recovery RunOnce worked=%v err=%v", worked, err)
	}
	recovered := assertCommandState(t, store, command.ID, control.StateSucceeded)
	if adapter.executeCount != 1 || adapter.reconcileCount != 1 {
		t.Fatalf("adapter calls execute=%d reconcile=%d", adapter.executeCount, adapter.reconcileCount)
	}
	if string(recovered.Outcome) != `{"status":"observed"}` {
		t.Fatalf("reconciled outcome = %s", recovered.Outcome)
	}
}

func TestExecutorUnknownReconciliationBlocksConflictDomain(t *testing.T) {
	store := openControlStore(t)
	command := queueCommand(t, store, "unknown")
	adapter := &fakeAdapter{
		effect: control.EffectResult{Disposition: control.EffectSucceeded},
		reconcile: control.ReconcileResult{
			Disposition: control.ReconcileUnknown,
			Error:       "backend state is ambiguous",
		},
	}
	executor := newTestExecutor(t, store, adapter, control.ExecutorHooks{
		AfterEffect: func(control.Command, control.EffectResult) error {
			return errSimulatedCrash
		},
	})
	_, _ = executor.RunOnce(t.Context())
	time.Sleep(15 * time.Millisecond)
	recovery := newTestExecutor(t, store, adapter, control.ExecutorHooks{})
	if _, err := recovery.RunOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	assertCommandState(t, store, command.ID, control.StateUnknownOutcome)
	request := testControlRequest("replacement", "pool:unknown")
	if _, _, err := store.CreateCommand(t.Context(), request); !errors.Is(err, control.ErrConflictDomainBusy) {
		t.Fatalf("replacement command error = %v", err)
	}
}

func TestExecutorRecordsUnsupportedCapabilityWithoutEffect(t *testing.T) {
	store := openControlStore(t)
	command := queueCommand(t, store, "unsupported")
	executor, err := control.NewExecutor(store, control.AdapterMap{}, control.ExecutorOptions{
		Owner: "executor", Lease: 10 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	if worked, err := executor.RunOnce(t.Context()); !worked || err != nil {
		t.Fatalf("RunOnce unsupported worked=%v err=%v", worked, err)
	}
	failed := assertCommandState(t, store, command.ID, control.StateFailed)
	if failed.Error != "command capability is unsupported" {
		t.Fatalf("unsupported error = %q", failed.Error)
	}
}

func TestExecutorRejectsExpiredQueuedCommandWithoutEffect(t *testing.T) {
	store := openControlStore(t)
	expired := time.Now().Add(-time.Second)
	command := queueCommandWithTimeout(t, store, "expired", &expired)
	adapter := &fakeAdapter{
		effect: control.EffectResult{Disposition: control.EffectSucceeded},
	}
	executor := newTestExecutor(t, store, adapter, control.ExecutorHooks{})
	if worked, err := executor.RunOnce(t.Context()); !worked || err != nil {
		t.Fatalf("RunOnce expired worked=%v err=%v", worked, err)
	}
	interrupted := assertCommandState(t, store, command.ID, control.StateInterrupted)
	if adapter.executeCount != 0 {
		t.Fatalf("expired command execute count = %d", adapter.executeCount)
	}
	if interrupted.Error != "command deadline expired before execution" {
		t.Fatalf("expired command error = %q", interrupted.Error)
	}
}

func TestExecutorDeadlineDuringEffectRecordsUnknownOutcome(t *testing.T) {
	store := openControlStore(t)
	deadline := time.Now().Add(100 * time.Millisecond)
	command := queueCommandWithTimeout(t, store, "deadline", &deadline)
	adapter := &fakeAdapter{blockUntilDone: true}
	executor := newTestExecutor(t, store, adapter, control.ExecutorHooks{})
	if worked, err := executor.RunOnce(t.Context()); !worked || err != nil {
		t.Fatalf("RunOnce deadline worked=%v err=%v", worked, err)
	}
	unknown := assertCommandState(t, store, command.ID, control.StateUnknownOutcome)
	if adapter.executeCount != 1 {
		t.Fatalf("deadline command execute count = %d", adapter.executeCount)
	}
	if unknown.Error != "command deadline expired while effect outcome was uncertain" {
		t.Fatalf("deadline command error = %q", unknown.Error)
	}
}

func openControlStore(t *testing.T) *history.Store {
	t.Helper()
	store, err := history.Open(t.Context(), filepath.Join(t.TempDir(), "history.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func queueCommand(t *testing.T, store *history.Store, suffix string) control.Command {
	return queueCommandWithTimeout(t, store, suffix, nil)
}

func queueCommandWithTimeout(
	t *testing.T, store *history.Store, suffix string, timeoutAt *time.Time,
) control.Command {
	t.Helper()
	request := testControlRequest("key-"+suffix, "pool:"+suffix)
	request.TimeoutAt = timeoutAt
	command, _, err := store.CreateCommand(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	if command.State != control.StateQueued {
		t.Fatalf("created command state = %q", command.State)
	}
	return command
}

func testControlRequest(key, domain string) control.Request {
	return control.Request{
		IdempotencyKey: key, Type: "pool.reconcile", HostID: "host",
		TargetType: "pool", TargetID: "linux", ConflictDomain: domain,
		Parameters: json.RawMessage(`{"pool":"linux"}`),
		ActorKind:  operations.ActorOperator, ActorID: "local-operator",
		Reason: "test executor",
	}
}

func newTestExecutor(
	t *testing.T, store *history.Store, adapter *fakeAdapter, hooks control.ExecutorHooks,
) *control.Executor {
	t.Helper()
	executor, err := control.NewExecutor(store,
		control.AdapterMap{"pool.reconcile": adapter},
		control.ExecutorOptions{
			Owner: "executor", Lease: 5 * time.Millisecond, Hooks: hooks,
		})
	if err != nil {
		t.Fatal(err)
	}
	return executor
}

func assertCommandState(
	t *testing.T, store *history.Store, id string, state control.State,
) control.Command {
	t.Helper()
	command, err := store.Command(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if command.State != state {
		t.Fatalf("command state = %q, want %q", command.State, state)
	}
	return command
}
