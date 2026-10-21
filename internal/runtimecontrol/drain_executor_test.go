package runtimecontrol

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/GerardSmit/multirunner/internal/control"
	"github.com/GerardSmit/multirunner/internal/history"
	"github.com/GerardSmit/multirunner/internal/operations"
)

func TestDrainDoesNotBlockOtherCommandsAndCanBeCancelled(t *testing.T) {
	for _, action := range []string{CommandPoolCancelDrain, CommandPoolResume, CommandRunnerTerminate} {
		t.Run(action, func(t *testing.T) {
			store, err := history.Open(t.Context(), filepath.Join(t.TempDir(), "history.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			controller := &fakePool{name: "linux", sessions: []string{"warm-idle"}}
			registry := NewRegistry()
			registry.SetPools([]PoolController{controller})
			syncer := &fakeSyncer{}
			registry.SetHistorySyncer(syncer)
			executor, err := control.NewExecutor(store, registry, control.ExecutorOptions{
				Owner: "executor", PollInterval: time.Millisecond,
			})
			if err != nil {
				t.Fatal(err)
			}
			create := func(kind, targetType, targetID, domain string, parameters json.RawMessage) control.Command {
				t.Helper()
				command, _, err := store.CreateCommand(t.Context(), control.Request{
					IdempotencyKey: kind, Type: kind, HostID: "host",
					TargetType: targetType, TargetID: targetID, ConflictDomain: domain,
					Parameters: parameters, ActorKind: operations.ActorOperator, ActorID: "operator",
				})
				if err != nil {
					t.Fatal(err)
				}
				return command
			}
			run := func() {
				t.Helper()
				if _, err := executor.RunOnce(t.Context()); err != nil {
					t.Fatal(err)
				}
			}
			read := func(id string) control.Command {
				t.Helper()
				command, err := store.Command(t.Context(), id)
				if err != nil {
					t.Fatal(err)
				}
				return command
			}
			drain := create(CommandPoolDrain, "pool", "linux", "pool-drain:linux", nil)
			run()
			if current := read(drain.ID); current.State != control.StateReconciling || !controller.Paused() {
				t.Fatalf("drain is not pending: %+v", current)
			}
			unrelated := create(CommandHistorySync, "system", "history", "history", nil)
			run()
			if read(unrelated.ID).State != control.StateSucceeded || syncer.calls != 1 {
				t.Fatal("pending drain blocked unrelated command execution")
			}
			targetType, targetID := "pool", "linux"
			if action == CommandRunnerTerminate {
				targetType, targetID = "runner", "warm-idle"
			}
			cancel := create(action, targetType, targetID, "pool:linux", json.RawMessage(`{"pool":"linux"}`))
			run()
			if read(cancel.ID).State != control.StateSucceeded {
				t.Fatal("same-pool control could not execute during drain")
			}
			if action == CommandRunnerTerminate {
				// Recovery after a restart observes completion without executing
				// the drain effect again.
				executor, err = control.NewExecutor(store, registry, control.ExecutorOptions{Owner: "restarted"})
				if err != nil {
					t.Fatal(err)
				}
				time.Sleep(3 * time.Millisecond)
				run()
				if read(drain.ID).State != control.StateSucceeded {
					t.Fatal("drain did not complete after the last runner was terminated")
				}
			} else {
				if read(drain.ID).State != control.StateInterrupted || controller.Paused() {
					t.Fatal("resume did not durably cancel the drain")
				}
				controller.Pause()
				time.Sleep(3 * time.Millisecond)
				run()
				if read(drain.ID).State != control.StateInterrupted {
					t.Fatal("a later pause resurrected the cancelled drain")
				}
			}
		})
	}
}

func TestPendingDrainDeadlineInterruptsWithoutUnknownOutcome(t *testing.T) {
	store, err := history.Open(t.Context(), filepath.Join(t.TempDir(), "history.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	controller := &fakePool{name: "linux", sessions: []string{"warm-idle"}}
	registry := NewRegistry()
	registry.SetPools([]PoolController{controller})
	deadline := time.Now().Add(100 * time.Millisecond)
	command, _, err := store.CreateCommand(t.Context(), control.Request{
		IdempotencyKey: "drain", Type: CommandPoolDrain, HostID: "host",
		TargetType: "pool", TargetID: "linux", ConflictDomain: "pool-drain:linux",
		ActorKind: operations.ActorOperator, ActorID: "operator", TimeoutAt: &deadline,
	})
	if err != nil {
		t.Fatal(err)
	}
	executor, err := control.NewExecutor(store, registry, control.ExecutorOptions{
		Owner: "executor", PollInterval: time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := executor.RunOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Until(deadline) + 5*time.Millisecond)
	if _, err := executor.RunOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	command, err = store.Command(t.Context(), command.ID)
	if err != nil || command.State != control.StateInterrupted || command.State.BlocksConflictDomain() {
		t.Fatalf("expired drain = %+v, %v", command, err)
	}
	if !controller.Paused() || len(controller.ActiveSessions()) != 1 {
		t.Fatal("deadline unexpectedly resumed the pool or terminated a job")
	}
}
