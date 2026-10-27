package history

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/GerardSmit/multirunner/internal/control"
	"github.com/GerardSmit/multirunner/internal/operations"
)

func TestCommandStoreIdempotencyConflictDomainsAndAudit(t *testing.T) {
	store := openTestStore(t)
	hostID := bindCommandEpoch(t, store)
	var committedEvents []operations.Event
	store.SetOperationalEventListener(func(event operations.Event) {
		committedEvents = append(committedEvents, event)
	})
	request := testCommandRequest("key-1", "pool:linux")
	request.HostID = hostID
	request.Parameters = json.RawMessage(`{"desired":2,"labels":["linux","x64"]}`)
	command, created, err := store.CreateCommand(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	if !created || command.State != control.StateQueued {
		t.Fatalf("CreateCommand = %+v created=%v", command, created)
	}

	duplicate := request
	duplicate.Parameters = json.RawMessage(`{"labels":["linux","x64"],"desired":2}`)
	duplicate.CorrelationID = "retry-correlation"
	duplicate.ClientMetadata = json.RawMessage(`{"remote_addr":"127.0.0.1:2"}`)
	replayed, created, err := store.CreateCommand(t.Context(), duplicate)
	if err != nil {
		t.Fatal(err)
	}
	if created || replayed.ID != command.ID {
		t.Fatalf("duplicate command = %+v created=%v", replayed, created)
	}

	mismatch := request
	mismatch.Parameters = json.RawMessage(`{"desired":3,"labels":["linux","x64"]}`)
	if _, _, err := store.CreateCommand(t.Context(), mismatch); !errors.Is(err, control.ErrIdempotencyConflict) {
		t.Fatalf("mismatched idempotency error = %v", err)
	}

	blocked := testCommandRequest("key-2", "pool:linux")
	blocked.HostID = hostID
	if _, _, err := store.CreateCommand(t.Context(), blocked); !errors.Is(err, control.ErrConflictDomainBusy) {
		t.Fatalf("conflict domain error = %v", err)
	}

	claimed, err := store.ClaimNextCommand(t.Context(), "worker-1", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if claimed.ID != command.ID || claimed.Attempt != 1 || claimed.FencingToken != 1 {
		t.Fatalf("ClaimNextCommand = %+v", claimed)
	}
	command = transitionForTest(t, store, claimed, control.StateRunning, claimed.FencingToken)
	command = transitionForTest(t, store, command, control.StateSucceeded, command.FencingToken)
	if command.CompletedAt == nil {
		t.Fatal("terminal command has no completion timestamp")
	}

	released, created, err := store.CreateCommand(t.Context(), blocked)
	if err != nil || !created {
		t.Fatalf("released conflict domain: command=%+v created=%v err=%v", released, created, err)
	}
	var transitions, audits int
	if err := store.db.QueryRowContext(t.Context(),
		`SELECT COUNT(*) FROM command_transitions WHERE command_id=?`, command.ID).Scan(&transitions); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRowContext(t.Context(),
		`SELECT COUNT(*) FROM audit_events WHERE command_id=?`, command.ID).Scan(&audits); err != nil {
		t.Fatal(err)
	}
	if transitions != 6 || audits != 6 {
		t.Fatalf("command evidence transitions=%d audits=%d, want 6 each", transitions, audits)
	}
	if len(committedEvents) != 9 {
		t.Fatalf("committed command events = %d, want 9", len(committedEvents))
	}
	for index, event := range committedEvents[:6] {
		if event.CommandID != command.ID || event.EntityType != "command" ||
			event.Sequence != int64(index+1) {
			t.Fatalf("command event %d = %+v", index, event)
		}
	}
}

func TestCommandStoreFencingRecoveryAndUnknownOutcomeBlock(t *testing.T) {
	store := openTestStore(t)
	hostID := bindCommandEpoch(t, store)
	request := testCommandRequest("key-1", "runner:one")
	request.HostID = hostID
	command, _, err := store.CreateCommand(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	command, err = store.ClaimNextCommand(t.Context(), "worker-1", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.HeartbeatCommand(t.Context(), command.ID, "worker-1", command.FencingToken+1, time.Minute); !errors.Is(err, control.ErrStaleFence) {
		t.Fatalf("stale heartbeat error = %v", err)
	}
	if _, err := store.db.ExecContext(t.Context(),
		`UPDATE commands SET lease_expires_at=? WHERE id=?`,
		time.Now().Add(-time.Minute).UnixMilli(), command.ID); err != nil {
		t.Fatal(err)
	}
	reconciled, err := store.ReconcileExpiredCommands(t.Context(), "reconciler", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(reconciled) != 1 || reconciled[0].State != control.StateReconciling ||
		reconciled[0].FencingToken != command.FencingToken+1 {
		t.Fatalf("ReconcileExpiredCommands = %+v", reconciled)
	}
	if _, err := store.TransitionCommand(t.Context(), command.ID, control.Transition{
		ExpectedState: control.StateReconciling, To: control.StateSucceeded,
		ActorKind: operations.ActorSystem, ActorID: "worker-1",
		FencingToken: command.FencingToken,
	}); !errors.Is(err, control.ErrStaleFence) {
		t.Fatalf("stale transition error = %v", err)
	}
	unknown := transitionForTest(t, store, reconciled[0], control.StateUnknownOutcome,
		reconciled[0].FencingToken)
	if !unknown.State.Terminal() || !unknown.State.BlocksConflictDomain() {
		t.Fatalf("unknown outcome state flags are incorrect: %q", unknown.State)
	}
	replacement := testCommandRequest("key-2", "runner:one")
	replacement.HostID = hostID
	if _, _, err := store.CreateCommand(t.Context(),
		replacement); !errors.Is(err, control.ErrConflictDomainBusy) {
		t.Fatalf("unknown outcome conflict error = %v", err)
	}
}

func TestReconcileRestoredSnapshotInvalidatesCommandsAndCreatingBackups(t *testing.T) {
	store := openTestStore(t)
	hostID := bindCommandEpoch(t, store)

	uncertainRequest := testCommandRequest("uncertain", "pool:uncertain")
	uncertainRequest.HostID = hostID
	uncertain, _, err := store.CreateCommand(t.Context(), uncertainRequest)
	if err != nil {
		t.Fatal(err)
	}
	uncertain, err = store.ClaimNextCommand(t.Context(), "worker", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	uncertain = transitionForTest(t, store, uncertain, control.StateRunning, uncertain.FencingToken)
	uncertain = transitionForTest(t, store, uncertain, control.StateUnknownOutcome, uncertain.FencingToken)

	queuedRequest := testCommandRequest("queued", "pool:queued")
	queuedRequest.HostID = hostID
	queued, _, err := store.CreateCommand(t.Context(), queuedRequest)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if _, err := store.db.ExecContext(t.Context(), `INSERT INTO backups (
		id, command_id, purpose, state, created_at, expires_at, file_name,
		file_path, manifest_path, schema_version, app_version, host_id
	) VALUES (?, ?, 'manual', 'creating', ?, ?, ?, ?, ?, ?, ?, ?)`,
		"backup-stale", queued.ID, now.UnixMilli(), now.Add(time.Hour).UnixMilli(),
		"backup-stale.db", "backup-stale.db", "backup-stale.json", CurrentSchemaVersion(),
		"test", hostID,
	); err != nil {
		t.Fatal(err)
	}

	result, err := store.ReconcileRestoredSnapshot(t.Context(), "restore-1", now)
	if err != nil {
		t.Fatal(err)
	}
	if result.CommandsInterrupted != 2 || result.BackupsFailed != 1 {
		t.Fatalf("reconciliation result = %+v", result)
	}
	assertStoredCommandState(t, store, uncertain.ID, control.StateInterrupted)
	assertStoredCommandState(t, store, queued.ID, control.StateInterrupted)
	var backupState, backupError string
	if err := store.db.QueryRowContext(t.Context(),
		`SELECT state, error FROM backups WHERE id='backup-stale'`,
	).Scan(&backupState, &backupError); err != nil {
		t.Fatal(err)
	}
	if backupState != "failed" || backupError == "" {
		t.Fatalf("backup state=%q error=%q", backupState, backupError)
	}

	replacement := testCommandRequest("replacement", uncertain.ConflictDomain)
	replacement.HostID = hostID
	if _, created, err := store.CreateCommand(t.Context(), replacement); err != nil || !created {
		t.Fatalf("replacement command created=%v err=%v", created, err)
	}
}

func TestCommandCreationReplayAndStartupResumeSafeStates(t *testing.T) {
	store := openTestStore(t)
	hostID := bindCommandEpoch(t, store)
	request := testCommandRequest("lost-response", "pool:lost-response")
	request.HostID = hostID
	command, created, err := store.CreateCommand(t.Context(), request)
	if err != nil || !created || command.State != control.StateQueued {
		t.Fatalf("atomic create = %+v created=%v err=%v", command, created, err)
	}

	replayed, created, err := store.CreateCommand(t.Context(), request)
	if err != nil || created || replayed.ID != command.ID ||
		replayed.State != control.StateQueued {
		t.Fatalf("lost-response replay = %+v created=%v err=%v", replayed, created, err)
	}

	if _, err := store.db.ExecContext(t.Context(), `UPDATE commands SET
		state='received', validated_at=NULL, queued_at=NULL WHERE id=?`, command.ID); err != nil {
		t.Fatal(err)
	}
	replayed, created, err = store.CreateCommand(t.Context(), request)
	if err != nil || created || replayed.State != control.StateQueued {
		t.Fatalf("stranded received replay = %+v created=%v err=%v", replayed, created, err)
	}

	receivedRequest := testCommandRequest("startup-received", "pool:startup-received")
	receivedRequest.HostID = hostID
	received, _, err := store.CreateCommand(t.Context(), receivedRequest)
	if err != nil {
		t.Fatal(err)
	}
	validatedRequest := testCommandRequest("startup-validated", "pool:startup-validated")
	validatedRequest.HostID = hostID
	validated, _, err := store.CreateCommand(t.Context(), validatedRequest)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(t.Context(), `UPDATE commands SET
		state='received', validated_at=NULL, queued_at=NULL WHERE id=?`, received.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(t.Context(), `UPDATE commands SET
		state='validated', queued_at=NULL WHERE id=?`, validated.ID); err != nil {
		t.Fatal(err)
	}
	resumed, err := store.ResumePendingCommands(t.Context(), "startup")
	if err != nil {
		t.Fatal(err)
	}
	if len(resumed) != 2 {
		t.Fatalf("resumed commands = %d, want 2", len(resumed))
	}
	assertStoredCommandState(t, store, received.ID, control.StateQueued)
	assertStoredCommandState(t, store, validated.ID, control.StateQueued)

	pendingRequest := testCommandRequest("pending-confirmation", "pool:pending-confirmation")
	pendingRequest.HostID = hostID
	pendingRequest.Confirmation = control.ConfirmationPending
	pending, created, err := store.CreateCommand(t.Context(), pendingRequest)
	if err != nil || !created || pending.State != control.StateAwaitingConfirmation {
		t.Fatalf("pending confirmation create = %+v created=%v err=%v", pending, created, err)
	}
}

func TestReconcileExpiredCommandsReclaimsAfterSecondCrash(t *testing.T) {
	store := openTestStore(t)
	hostID := bindCommandEpoch(t, store)
	request := testCommandRequest("second-reconcile", "runner:second-reconcile")
	request.HostID = hostID
	command, _, err := store.CreateCommand(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	command, err = store.ClaimNextCommand(t.Context(), "worker-1", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	command = transitionForTest(t, store, command, control.StateRunning, command.FencingToken)
	expireCommandLease(t, store, command.ID)

	first, err := store.ReconcileExpiredCommands(t.Context(), "reconciler-1", time.Minute)
	if err != nil || len(first) != 1 {
		t.Fatalf("first reconciliation = %+v err=%v", first, err)
	}
	expireCommandLease(t, store, command.ID)
	second, err := store.ReconcileExpiredCommands(t.Context(), "reconciler-2", time.Minute)
	if err != nil || len(second) != 1 {
		t.Fatalf("second reconciliation = %+v err=%v", second, err)
	}
	reclaimed := second[0]
	if reclaimed.State != control.StateReconciling ||
		reclaimed.FencingToken != first[0].FencingToken+1 ||
		reclaimed.Attempt != first[0].Attempt+1 ||
		reclaimed.LeaseOwner != "reconciler-2" {
		t.Fatalf("reclaimed reconciliation = %+v, first=%+v", reclaimed, first[0])
	}
	if _, err := store.TransitionCommand(t.Context(), command.ID, control.Transition{
		ExpectedState: control.StateReconciling, To: control.StateSucceeded,
		ActorKind: operations.ActorSystem, ActorID: "reconciler-1",
		FencingToken: first[0].FencingToken,
	}); !errors.Is(err, control.ErrStaleFence) {
		t.Fatalf("first reconciler stale transition error = %v", err)
	}
	transitionForTest(t, store, reclaimed, control.StateUnknownOutcome, reclaimed.FencingToken)
}

func expireCommandLease(t *testing.T, store *Store, id string) {
	t.Helper()
	if _, err := store.db.ExecContext(t.Context(),
		`UPDATE commands SET lease_expires_at=? WHERE id=?`,
		time.Now().Add(-time.Minute).UnixMilli(), id,
	); err != nil {
		t.Fatal(err)
	}
}

func bindCommandEpoch(t *testing.T, store *Store) string {
	t.Helper()
	host, err := store.EnsureOperationalHost(t.Context(), "command-tests", "test")
	if err != nil {
		t.Fatal(err)
	}
	epoch, err := store.StartHostEpoch(t.Context(), host.ID, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	store.SetCommandEpoch(epoch.ID)
	return host.ID
}

func testCommandRequest(key, domain string) control.Request {
	return control.Request{
		IdempotencyKey: key, Type: "runner.recycle", HostID: "host",
		TargetType: "runner", TargetID: "runner-1", ConflictDomain: domain,
		Parameters: json.RawMessage(`{}`), ActorKind: operations.ActorOperator,
		ActorID: "local-operator", Reason: "test", CorrelationID: "correlation",
		ClientMetadata: json.RawMessage(`{"remote_addr":"127.0.0.1:1"}`),
	}
}

func transitionForTest(
	t *testing.T, store *Store, command control.Command, to control.State,
	fencingToken int64,
) control.Command {
	t.Helper()
	transitioned, err := store.TransitionCommand(t.Context(), command.ID, control.Transition{
		ExpectedState: command.State, To: to, ActorKind: operations.ActorSystem,
		ActorID: "test", FencingToken: fencingToken,
	})
	if err != nil {
		t.Fatalf("transition %s to %s: %v", command.State, to, err)
	}
	return transitioned
}

func assertStoredCommandState(
	t *testing.T, store *Store, id string, expected control.State,
) {
	t.Helper()
	command, err := store.Command(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if command.State != expected {
		t.Fatalf("command %s state = %q, want %q", id, command.State, expected)
	}
}
