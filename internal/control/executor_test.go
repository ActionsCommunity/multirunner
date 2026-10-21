package control_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"testing/synctest"
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
	release        <-chan struct{}
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
	release := a.release
	a.mu.Unlock()
	if release != nil {
		<-release
	}
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

func TestExecutorOperatorRecoveryOnlyReconcilesAndNeverRedispatches(t *testing.T) {
	store := openControlStore(t)
	command := queueCommand(t, store, "operator-recovery")
	adapter := &fakeAdapter{
		effect:    control.EffectResult{Disposition: control.EffectUnknown},
		reconcile: control.ReconcileResult{Disposition: control.ReconcileApplied},
	}
	executor := newTestExecutor(t, store, adapter, control.ExecutorHooks{})
	if _, err := executor.RunOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	command = assertCommandState(t, store, command.ID, control.StateUnknownOutcome)
	_, err := store.TransitionCommand(t.Context(), command.ID, control.Transition{
		ExpectedState: control.StateUnknownOutcome, To: control.StateReconciling,
		ActorKind: operations.ActorOperator, ActorID: "operator",
		FencingToken: command.FencingToken, LeaseOwner: "operator", LeaseDuration: time.Millisecond,
		Detail: json.RawMessage(`{"reason":"check authoritative completion evidence"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(3 * time.Millisecond)
	restarted := newTestExecutor(t, store, adapter, control.ExecutorHooks{})
	if _, err := restarted.RunOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	assertCommandState(t, store, command.ID, control.StateSucceeded)
	if adapter.executeCount != 1 || adapter.reconcileCount != 1 {
		t.Fatalf("operator recovery dispatched effect: execute=%d reconcile=%d", adapter.executeCount, adapter.reconcileCount)
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
	synctest.Test(t, func(t *testing.T) {
		deadline := time.Now().Add(100 * time.Millisecond)
		store := &deadlineStore{command: control.Command{
			ID: "deadline", Type: "pool.reconcile", State: control.StateQueued,
			TimeoutAt: &deadline,
		}}
		adapter := &fakeAdapter{blockUntilDone: true}
		executor, err := control.NewExecutor(
			store, control.AdapterMap{"pool.reconcile": adapter},
			control.ExecutorOptions{Owner: "executor", Lease: time.Second},
		)
		if err != nil {
			t.Fatal(err)
		}
		if worked, err := executor.RunOnce(t.Context()); !worked || err != nil {
			t.Fatalf("RunOnce deadline worked=%v err=%v", worked, err)
		}
		synctest.Wait()
		if store.command.State != control.StateUnknownOutcome || adapter.executeCount != 1 {
			t.Fatalf("deadline command state=%q execute count=%d", store.command.State, adapter.executeCount)
		}
		if store.command.Error != "command deadline expired while effect outcome was uncertain" {
			t.Fatalf("deadline command error = %q", store.command.Error)
		}
	})
}

func TestExecutorDoesNotReconcileWhileTimedOutEffectIsStillStopping(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		deadline := time.Now().Add(100 * time.Millisecond)
		store := &deadlineStore{command: control.Command{
			ID: "still-stopping", Type: "pool.reconcile", State: control.StateQueued, TimeoutAt: &deadline,
		}}
		release := make(chan struct{})
		adapter := &fakeAdapter{release: release,
			effect:    control.EffectResult{Disposition: control.EffectSucceeded},
			reconcile: control.ReconcileResult{Disposition: control.ReconcileApplied},
		}
		executor, err := control.NewExecutor(store, control.AdapterMap{"pool.reconcile": adapter},
			control.ExecutorOptions{Owner: "executor", Lease: time.Second})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := executor.RunOnce(t.Context()); err != nil {
			t.Fatal(err)
		}
		store.command.State = control.StateReconciling
		if _, err := executor.RunOnce(t.Context()); err != nil {
			t.Fatal(err)
		}
		if store.command.State != control.StateUnknownOutcome || adapter.reconcileCount != 0 {
			t.Fatalf("reconciled before effect stopped: state=%s calls=%d", store.command.State, adapter.reconcileCount)
		}
		close(release)
		synctest.Wait()
		store.command.State = control.StateReconciling
		if _, err := executor.RunOnce(t.Context()); err != nil {
			t.Fatal(err)
		}
		if store.command.State != control.StateSucceeded || adapter.reconcileCount != 1 || adapter.executeCount != 1 {
			t.Fatalf("recovery state=%s reconciliation=%d effects=%d", store.command.State, adapter.reconcileCount, adapter.executeCount)
		}
	})
}

type deadlineStore struct {
	command control.Command
}

func (*deadlineStore) ResumePendingCommands(context.Context, string) ([]control.Command, error) {
	return nil, nil
}

func (*deadlineStore) ExpireQueuedCommands(context.Context, string) ([]control.Command, error) {
	return nil, nil
}

func (s *deadlineStore) ReconcileExpiredCommands(context.Context, string, time.Duration) ([]control.Command, error) {
	if s.command.State == control.StateReconciling {
		return []control.Command{s.command}, nil
	}
	return nil, nil
}

func (s *deadlineStore) ClaimNextCommand(context.Context, string, time.Duration) (control.Command, error) {
	if s.command.State != control.StateQueued {
		return control.Command{}, control.ErrNotFound
	}
	s.command.State = control.StateClaimed
	return s.command, nil
}

func (s *deadlineStore) TransitionCommand(_ context.Context, _ string, transition control.Transition) (control.Command, error) {
	if s.command.State != transition.ExpectedState {
		return control.Command{}, fmt.Errorf("unexpected command state %q", s.command.State)
	}
	s.command.State = transition.To
	s.command.Error = transition.Error
	return s.command, nil
}

func (*deadlineStore) HeartbeatCommand(context.Context, string, string, int64, time.Duration) error {
	return errors.New("unexpected heartbeat before command deadline")
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
