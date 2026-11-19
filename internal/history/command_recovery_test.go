package history

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/GerardSmit/multirunner/internal/control"
	"github.com/GerardSmit/multirunner/internal/operations"
)

func TestUnknownOutcomeRequiresAuditedFencedOperatorReconciliation(t *testing.T) {
	store := openTestStore(t)
	bindCommandEpoch(t, store)
	original, _, err := store.CreateCommand(t.Context(), testCommandRequest("uncertain", "pool:linux"))
	if err != nil {
		t.Fatal(err)
	}
	command, err := store.ClaimNextCommand(t.Context(), "worker", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	command = transitionForTest(t, store, command, control.StateUnknownOutcome, command.FencingToken)
	transition := control.Transition{
		ExpectedState: control.StateUnknownOutcome, To: control.StateReconciling,
		ActorKind: operations.ActorSystem, ActorID: "worker",
		FencingToken: command.FencingToken, LeaseOwner: "operator",
		LeaseDuration: time.Millisecond, Detail: json.RawMessage(`{"reason":"verify actual state"}`),
	}
	if _, err := store.TransitionCommand(t.Context(), command.ID, transition); err == nil {
		t.Fatal("system actor reopened unknown outcome")
	}
	transition.ActorKind = operations.ActorOperator
	transition.Detail = json.RawMessage(`{}`)
	if _, err := store.TransitionCommand(t.Context(), command.ID, transition); err == nil {
		t.Fatal("missing reason reopened unknown outcome")
	}
	transition.Detail = json.RawMessage(`{"reason":"verify actual state"}`)
	transition.FencingToken++
	if _, err := store.TransitionCommand(t.Context(), command.ID, transition); !errors.Is(err, control.ErrStaleFence) {
		t.Fatalf("stale operator reconciliation = %v", err)
	}
	transition.FencingToken = command.FencingToken
	resumed, err := store.TransitionCommand(t.Context(), command.ID, transition)
	if err != nil {
		t.Fatal(err)
	}
	if resumed.FencingToken <= command.FencingToken || resumed.CompletedAt != nil ||
		!resumed.State.BlocksConflictDomain() || resumed.ID != original.ID {
		t.Fatalf("unsafe reconciliation state: %+v", resumed)
	}
	replacement := testCommandRequest("replacement", "pool:linux")
	if _, _, err := store.CreateCommand(t.Context(), replacement); !errors.Is(err, control.ErrConflictDomainBusy) {
		t.Fatalf("reconciliation released uncertainty fence: %v", err)
	}
	var count int
	if err := store.db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM audit_events
		WHERE command_id=? AND actor_kind='operator' AND json_extract(payload_json, '$.detail.reason')='verify actual state'`,
		command.ID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("operator recovery audit count=%d err=%v", count, err)
	}
	resumed = transitionForTest(t, store, resumed, control.StateInterrupted, resumed.FencingToken)
	if _, _, err := store.CreateCommand(t.Context(), replacement); err != nil {
		t.Fatal(err)
	}
	next, err := store.ClaimNextCommand(t.Context(), "worker", time.Minute)
	if err != nil || next.FencingToken <= resumed.FencingToken {
		t.Fatalf("next conflict-domain fence=%d previous=%d err=%v", next.FencingToken, resumed.FencingToken, err)
	}
}

func TestConflictDomainFenceSurvivesCommandPruning(t *testing.T) {
	store := openTestStore(t)
	first, _, err := store.CreateCommand(t.Context(), testCommandRequest("first", "pool:linux"))
	if err != nil {
		t.Fatal(err)
	}
	first, err = store.ClaimNextCommand(t.Context(), "worker", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	first = transitionForTest(t, store, first, control.StateInterrupted, first.FencingToken)
	if _, err := store.db.ExecContext(t.Context(), `DELETE FROM command_transitions WHERE command_id=?`, first.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(t.Context(), `DELETE FROM audit_events WHERE command_id=?`, first.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(t.Context(), `DELETE FROM commands WHERE id=?`, first.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.CreateCommand(t.Context(), testCommandRequest("second", "pool:linux")); err != nil {
		t.Fatal(err)
	}
	next, err := store.ClaimNextCommand(t.Context(), "worker", time.Minute)
	if err != nil || next.FencingToken <= first.FencingToken {
		t.Fatalf("pruning reset domain fence: first=%d next=%d err=%v", first.FencingToken, next.FencingToken, err)
	}
}
