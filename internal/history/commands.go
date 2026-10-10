package history

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/GerardSmit/multirunner/internal/control"
	"github.com/GerardSmit/multirunner/internal/operations"
	"github.com/GerardSmit/multirunner/internal/restore"
)

const commandColumns = `id, idempotency_scope, idempotency_key, request_hash,
	command_type, command_version, host_id, target_type, target_id, conflict_domain,
	parameters_json, actor_kind, actor_id, reason, confirmation, state,
	correlation_id, client_metadata_json, created_at, validated_at, queued_at,
	started_at, heartbeat_at, completed_at, timeout_at, before_state_json,
	outcome_json, error, lease_owner, lease_expires_at, attempt, fencing_token`

type rowScanner interface {
	Scan(...any) error
}

func (s *Store) CreateCommand(ctx context.Context, request control.Request) (control.Command, bool, error) {
	if request.Version == 0 {
		request.Version = control.CurrentCommandVersion
	}
	if request.Confirmation == "" {
		request.Confirmation = control.ConfirmationNotRequired
	}
	if err := request.Validate(); err != nil {
		return control.Command{}, false, err
	}
	parameters, err := canonicalCommandJSON(request.Parameters)
	if err != nil {
		return control.Command{}, false, fmt.Errorf("canonicalize command parameters: %w", err)
	}
	metadata, err := canonicalCommandJSON(request.ClientMetadata)
	if err != nil {
		return control.Command{}, false, fmt.Errorf("canonicalize command client metadata: %w", err)
	}
	request.Parameters = parameters
	request.ClientMetadata = metadata
	requestHash, err := request.CanonicalHash()
	if err != nil {
		return control.Command{}, false, err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return control.Command{}, false, fmt.Errorf("begin command creation: %w", err)
	}
	defer tx.Rollback()

	existing, err := readCommand(tx.QueryRowContext(ctx,
		`SELECT `+commandColumns+` FROM commands
		 WHERE idempotency_scope=? AND idempotency_key=?`,
		request.IdempotencyScope(), request.IdempotencyKey))
	if err == nil {
		if existing.RequestHash != requestHash {
			return control.Command{}, false, control.ErrIdempotencyConflict
		}
		existing, events, err := advanceSafePreExecutionCommand(
			ctx, tx, s.commandEpochID(), existing,
			request.ActorKind, request.ActorID, nowUTC(),
		)
		if err != nil {
			return control.Command{}, false, err
		}
		if err := tx.Commit(); err != nil {
			return control.Command{}, false, fmt.Errorf("commit idempotent command replay: %w", err)
		}
		for _, event := range events {
			if event.ID != "" {
				s.notifyOperationalEvent(event)
			}
		}
		return existing, false, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return control.Command{}, false, fmt.Errorf("read idempotent command: %w", err)
	}

	var blockingID string
	err = tx.QueryRowContext(ctx, `SELECT id FROM commands
		WHERE conflict_domain=?
		  AND state NOT IN ('succeeded', 'failed', 'cancelled', 'interrupted')
		LIMIT 1`, request.ConflictDomain).Scan(&blockingID)
	if err == nil {
		return control.Command{}, false, fmt.Errorf("%w: %s", control.ErrConflictDomainBusy, blockingID)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return control.Command{}, false, fmt.Errorf("read command conflict domain: %w", err)
	}

	id, err := operations.NewOpaqueID()
	if err != nil {
		return control.Command{}, false, err
	}
	now := nowUTC()
	command := control.Command{
		ID: id, IdempotencyScope: request.IdempotencyScope(),
		IdempotencyKey: request.IdempotencyKey, RequestHash: requestHash,
		Type: request.Type, Version: request.Version, HostID: request.HostID,
		TargetType: request.TargetType, TargetID: request.TargetID,
		ConflictDomain: request.ConflictDomain, Parameters: parameters,
		ActorKind: request.ActorKind, ActorID: request.ActorID, Reason: request.Reason,
		Confirmation: request.Confirmation, State: control.StateReceived,
		CorrelationID: request.CorrelationID, ClientMetadata: metadata,
		CreatedAt: now, TimeoutAt: request.TimeoutAt,
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO commands (
		id, idempotency_scope, idempotency_key, request_hash, command_type,
		command_version, host_id, target_type, target_id, conflict_domain,
		parameters_json, actor_kind, actor_id, reason, confirmation, state,
		correlation_id, client_metadata_json, created_at, timeout_at
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		command.ID, command.IdempotencyScope, command.IdempotencyKey,
		command.RequestHash, command.Type, command.Version, command.HostID,
		command.TargetType, command.TargetID, command.ConflictDomain,
		[]byte(command.Parameters), command.ActorKind, command.ActorID,
		command.Reason, command.Confirmation, command.State, command.CorrelationID,
		[]byte(command.ClientMetadata), timeMillis(command.CreatedAt),
		nullableTime(command.TimeoutAt))
	if err != nil {
		if isSQLiteConstraint(err) {
			var concurrentID string
			lookupErr := tx.QueryRowContext(ctx, `SELECT id FROM commands
				WHERE conflict_domain=?
				  AND state NOT IN ('succeeded', 'failed', 'cancelled', 'interrupted')
				LIMIT 1`, request.ConflictDomain).Scan(&concurrentID)
			if lookupErr == nil {
				return control.Command{}, false, fmt.Errorf(
					"%w: %s", control.ErrConflictDomainBusy, concurrentID,
				)
			}
			if !errors.Is(lookupErr, sql.ErrNoRows) {
				return control.Command{}, false, fmt.Errorf(
					"read concurrent command conflict: %w", lookupErr,
				)
			}
		}
		return control.Command{}, false, fmt.Errorf("insert command: %w", err)
	}
	if err := recordCommandTransition(ctx, tx, command, "", command.State,
		command.ActorKind, command.ActorID, json.RawMessage(`{"action":"created"}`)); err != nil {
		return control.Command{}, false, err
	}
	if err := recordCommandAudit(ctx, tx, command, command.ActorKind, command.ActorID, "command.created",
		json.RawMessage(`{"state":"received"}`)); err != nil {
		return control.Command{}, false, err
	}
	event, err := appendCommandEvent(ctx, tx, s.commandEpochID(), command, "",
		command.State, command.ActorKind, command.ActorID)
	if err != nil {
		return control.Command{}, false, err
	}
	events := []operations.Event{event}
	command, advancedEvents, err := advanceSafePreExecutionCommand(
		ctx, tx, s.commandEpochID(), command,
		request.ActorKind, request.ActorID, now,
	)
	if err != nil {
		return control.Command{}, false, err
	}
	events = append(events, advancedEvents...)
	if err := tx.Commit(); err != nil {
		return control.Command{}, false, fmt.Errorf("commit command creation: %w", err)
	}
	for _, event := range events {
		if event.ID != "" {
			s.notifyOperationalEvent(event)
		}
	}
	return command, true, nil
}

func (s *Store) ResumePendingCommands(
	ctx context.Context, actorID string,
) ([]control.Command, error) {
	if strings.TrimSpace(actorID) == "" {
		return nil, errors.New("resume actor ID is required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin pending command resume: %w", err)
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT `+commandColumns+` FROM commands
		WHERE state IN ('received', 'validated') ORDER BY created_at, id`)
	if err != nil {
		return nil, fmt.Errorf("list pending commands: %w", err)
	}
	var pending []control.Command
	for rows.Next() {
		command, scanErr := readCommand(rows)
		if scanErr != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("scan pending command: %w", scanErr)
		}
		pending = append(pending, command)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("close pending command rows: %w", err)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list pending commands: %w", err)
	}
	now := nowUTC()
	resumed := make([]control.Command, 0, len(pending))
	var events []operations.Event
	for _, command := range pending {
		command, commandEvents, err := advanceSafePreExecutionCommand(
			ctx, tx, s.commandEpochID(), command,
			operations.ActorSystem, actorID, now,
		)
		if err != nil {
			return nil, err
		}
		resumed = append(resumed, command)
		events = append(events, commandEvents...)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit pending command resume: %w", err)
	}
	for _, event := range events {
		if event.ID != "" {
			s.notifyOperationalEvent(event)
		}
	}
	return resumed, nil
}

func (s *Store) Command(ctx context.Context, id string) (control.Command, error) {
	if id == "" {
		return control.Command{}, errors.New("command ID is required")
	}
	command, err := readCommand(s.db.QueryRowContext(ctx,
		`SELECT `+commandColumns+` FROM commands WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return control.Command{}, control.ErrNotFound
	}
	if err != nil {
		return control.Command{}, fmt.Errorf("read command: %w", err)
	}
	return command, nil
}

func (s *Store) TransitionCommand(ctx context.Context, id string, transition control.Transition) (control.Command, error) {
	if id == "" {
		return control.Command{}, errors.New("command ID is required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return control.Command{}, fmt.Errorf("begin command transition: %w", err)
	}
	defer tx.Rollback()
	command, event, err := transitionCommand(ctx, tx, s.commandEpochID(), id, transition, nowUTC())
	if err != nil {
		return control.Command{}, err
	}
	if err := tx.Commit(); err != nil {
		return control.Command{}, fmt.Errorf("commit command transition: %w", err)
	}
	if event.ID != "" {
		s.notifyOperationalEvent(event)
	}
	return command, nil
}

func (s *Store) ClaimNextCommand(ctx context.Context, owner string, lease time.Duration) (control.Command, error) {
	if strings.TrimSpace(owner) == "" || lease <= 0 {
		return control.Command{}, errors.New("claim owner and positive lease are required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return control.Command{}, fmt.Errorf("begin command claim: %w", err)
	}
	defer tx.Rollback()
	var id string
	err = tx.QueryRowContext(ctx, `SELECT id FROM commands
		WHERE state='queued' ORDER BY created_at, id LIMIT 1`).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return control.Command{}, control.ErrNotFound
	}
	if err != nil {
		return control.Command{}, fmt.Errorf("select queued command: %w", err)
	}
	command, event, err := transitionCommand(ctx, tx, s.commandEpochID(), id, control.Transition{
		ExpectedState: control.StateQueued, To: control.StateClaimed,
		ActorKind: operations.ActorSystem, ActorID: owner,
		LeaseOwner: owner, LeaseDuration: lease,
	}, nowUTC())
	if err != nil {
		return control.Command{}, err
	}
	if err := tx.Commit(); err != nil {
		return control.Command{}, fmt.Errorf("commit command claim: %w", err)
	}
	if event.ID != "" {
		s.notifyOperationalEvent(event)
	}
	return command, nil
}

func (s *Store) ExpireQueuedCommands(
	ctx context.Context, actorID string,
) ([]control.Command, error) {
	if strings.TrimSpace(actorID) == "" {
		return nil, errors.New("expiration actor ID is required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin queued command expiration: %w", err)
	}
	defer tx.Rollback()
	now := nowUTC()
	rows, err := tx.QueryContext(ctx, `SELECT id FROM commands
		WHERE state='queued' AND timeout_at IS NOT NULL AND timeout_at<=?
		ORDER BY timeout_at, id`, timeMillis(now))
	if err != nil {
		return nil, fmt.Errorf("list expired queued commands: %w", err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("scan expired queued command: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("close expired queued command rows: %w", err)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list expired queued commands: %w", err)
	}
	expired := make([]control.Command, 0, len(ids))
	events := make([]operations.Event, 0, len(ids))
	for _, id := range ids {
		command, event, err := transitionCommand(
			ctx, tx, s.commandEpochID(), id, control.Transition{
				ExpectedState: control.StateQueued, To: control.StateInterrupted,
				ActorKind: operations.ActorSystem, ActorID: actorID,
				Error:  "command deadline expired before execution",
				Detail: json.RawMessage(`{"code":"command_deadline_expired","effect_started":false}`),
			}, now,
		)
		if err != nil {
			return nil, err
		}
		expired = append(expired, command)
		if event.ID != "" {
			events = append(events, event)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit queued command expiration: %w", err)
	}
	for _, event := range events {
		s.notifyOperationalEvent(event)
	}
	return expired, nil
}

func (s *Store) HeartbeatCommand(ctx context.Context, id, owner string, fencingToken int64, lease time.Duration) error {
	if id == "" || owner == "" || fencingToken < 1 || lease <= 0 {
		return errors.New("command ID, owner, fencing token, and positive lease are required")
	}
	now := nowUTC()
	result, err := s.db.ExecContext(ctx, `UPDATE commands
		SET heartbeat_at=?, lease_expires_at=?
		WHERE id=? AND lease_owner=? AND fencing_token=?
		  AND state IN ('claimed', 'running', 'reconciling')`,
		timeMillis(now), timeMillis(now.Add(lease)), id, owner, fencingToken)
	if err != nil {
		return fmt.Errorf("heartbeat command: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("heartbeat command: %w", err)
	}
	if affected != 1 {
		return control.ErrStaleFence
	}
	return nil
}

func (s *Store) ReconcileExpiredCommands(ctx context.Context, owner string, lease time.Duration) ([]control.Command, error) {
	if strings.TrimSpace(owner) == "" || lease <= 0 {
		return nil, errors.New("reconciliation owner and positive lease are required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin expired command reconciliation: %w", err)
	}
	defer tx.Rollback()
	now := nowUTC()
	rows, err := tx.QueryContext(ctx, `SELECT id, state, fencing_token FROM commands
		WHERE state IN ('claimed', 'running', 'reconciling') AND lease_expires_at IS NOT NULL
		  AND lease_expires_at<=?
		ORDER BY lease_expires_at, id`, timeMillis(now))
	if err != nil {
		return nil, fmt.Errorf("list expired commands: %w", err)
	}
	type expired struct {
		id    string
		state control.State
		token int64
	}
	var expiredCommands []expired
	for rows.Next() {
		var command expired
		if err := rows.Scan(&command.id, &command.state, &command.token); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("scan expired command: %w", err)
		}
		expiredCommands = append(expiredCommands, command)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("close expired command rows: %w", err)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list expired commands: %w", err)
	}
	reconciled := make([]control.Command, 0, len(expiredCommands))
	events := make([]operations.Event, 0, len(expiredCommands))
	for _, expiredCommand := range expiredCommands {
		command, event, err := transitionCommand(ctx, tx, s.commandEpochID(), expiredCommand.id, control.Transition{
			ExpectedState: expiredCommand.state, To: control.StateReconciling,
			ActorKind: operations.ActorSystem, ActorID: owner,
			LeaseOwner: owner, LeaseDuration: lease,
			FencingToken: expiredCommand.token,
			Detail:       json.RawMessage(`{"reason":"lease_expired"}`),
		}, now)
		if err != nil {
			return nil, err
		}
		reconciled = append(reconciled, command)
		if event.ID != "" {
			events = append(events, event)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit expired command reconciliation: %w", err)
	}
	for _, event := range events {
		s.notifyOperationalEvent(event)
	}
	return reconciled, nil
}

func advanceSafePreExecutionCommand(
	ctx context.Context, tx *sql.Tx, epochID string, command control.Command,
	actorKind operations.ActorKind, actorID string, now time.Time,
) (control.Command, []operations.Event, error) {
	var events []operations.Event
	if command.State == control.StateReceived {
		advanced, event, err := transitionCommand(ctx, tx, epochID, command.ID, control.Transition{
			ExpectedState: control.StateReceived, To: control.StateValidated,
			ActorKind: actorKind, ActorID: actorID,
		}, now)
		if err != nil {
			return control.Command{}, nil, err
		}
		command = advanced
		events = append(events, event)
	}
	if command.State == control.StateValidated {
		target := control.StateQueued
		if command.Confirmation == control.ConfirmationPending {
			target = control.StateAwaitingConfirmation
		}
		advanced, event, err := transitionCommand(ctx, tx, epochID, command.ID, control.Transition{
			ExpectedState: control.StateValidated, To: target,
			ActorKind: actorKind, ActorID: actorID,
		}, now)
		if err != nil {
			return control.Command{}, nil, err
		}
		command = advanced
		events = append(events, event)
	}
	return command, events, nil
}

func (s *Store) ReconcileRestoredSnapshot(
	ctx context.Context, restoreID string, at time.Time,
) (restore.SnapshotReconciliation, error) {
	if strings.TrimSpace(restoreID) == "" {
		return restore.SnapshotReconciliation{}, errors.New("restore ID is required")
	}
	if at.IsZero() {
		at = nowUTC()
	} else {
		at = at.UTC()
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return restore.SnapshotReconciliation{}, fmt.Errorf("begin restored snapshot reconciliation: %w", err)
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT `+commandColumns+` FROM commands
		WHERE state NOT IN ('succeeded', 'failed', 'cancelled', 'interrupted')
		ORDER BY created_at, id`)
	if err != nil {
		return restore.SnapshotReconciliation{}, fmt.Errorf("list restored snapshot commands: %w", err)
	}
	var commands []control.Command
	for rows.Next() {
		command, scanErr := readCommand(rows)
		if scanErr != nil {
			_ = rows.Close()
			return restore.SnapshotReconciliation{}, fmt.Errorf("scan restored snapshot command: %w", scanErr)
		}
		commands = append(commands, command)
	}
	if err := rows.Close(); err != nil {
		return restore.SnapshotReconciliation{}, fmt.Errorf("close restored snapshot command rows: %w", err)
	}
	if err := rows.Err(); err != nil {
		return restore.SnapshotReconciliation{}, fmt.Errorf("list restored snapshot commands: %w", err)
	}
	events := make([]operations.Event, 0, len(commands))
	for _, command := range commands {
		from := command.State
		command.State = control.StateInterrupted
		command.CompletedAt = timePointerUTC(at)
		command.Error = "command invalidated by restored database activation"
		command.LeaseOwner = ""
		command.LeaseExpiresAt = nil
		result, updateErr := tx.ExecContext(ctx, `UPDATE commands SET
			state=?, completed_at=?, error=?, lease_owner='', lease_expires_at=NULL
			WHERE id=? AND state=? AND fencing_token=?`,
			command.State, timeMillis(at), command.Error, command.ID, from,
			command.FencingToken)
		if updateErr != nil {
			return restore.SnapshotReconciliation{}, fmt.Errorf("invalidate restored snapshot command: %w", updateErr)
		}
		affected, updateErr := result.RowsAffected()
		if updateErr != nil {
			return restore.SnapshotReconciliation{}, fmt.Errorf("invalidate restored snapshot command: %w", updateErr)
		}
		if affected != 1 {
			return restore.SnapshotReconciliation{}, control.ErrStateConflict
		}
		detail, _ := json.Marshal(map[string]any{
			"code": "restored_snapshot_invalidated", "restore_id": restoreID,
		})
		if err := recordCommandTransition(
			ctx, tx, command, from, command.State,
			operations.ActorSystem, "service-supervisor", detail,
		); err != nil {
			return restore.SnapshotReconciliation{}, err
		}
		if err := recordCommandAudit(
			ctx, tx, command, operations.ActorSystem, "service-supervisor",
			"command.invalidated_by_restore", detail,
		); err != nil {
			return restore.SnapshotReconciliation{}, err
		}
		event, eventErr := appendCommandEvent(
			ctx, tx, s.commandEpochID(), command, from, command.State,
			operations.ActorSystem, "service-supervisor",
		)
		if eventErr != nil {
			return restore.SnapshotReconciliation{}, eventErr
		}
		if event.ID != "" {
			events = append(events, event)
		}
	}
	artifactError := "backup interrupted by restored database activation"
	result, err := tx.ExecContext(ctx, `UPDATE backups SET
		state='failed', completed_at=?, error=? WHERE state='creating'`,
		timeMillis(at), artifactError)
	if err != nil {
		return restore.SnapshotReconciliation{}, fmt.Errorf("finalize restored snapshot backups: %w", err)
	}
	backupsFailed, err := result.RowsAffected()
	if err != nil {
		return restore.SnapshotReconciliation{}, fmt.Errorf("count finalized restored snapshot backups: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return restore.SnapshotReconciliation{}, fmt.Errorf("commit restored snapshot reconciliation: %w", err)
	}
	for _, event := range events {
		s.notifyOperationalEvent(event)
	}
	return restore.SnapshotReconciliation{
		CommandsInterrupted: len(commands),
		BackupsFailed:       int(backupsFailed),
	}, nil
}

func transitionCommand(
	ctx context.Context, tx *sql.Tx, epochID, id string,
	transition control.Transition, now time.Time,
) (control.Command, operations.Event, error) {
	command, err := readCommand(tx.QueryRowContext(ctx,
		`SELECT `+commandColumns+` FROM commands WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return control.Command{}, operations.Event{}, control.ErrNotFound
	}
	if err != nil {
		return control.Command{}, operations.Event{}, fmt.Errorf("read command for transition: %w", err)
	}
	if transition.ExpectedState != "" && command.State != transition.ExpectedState {
		return control.Command{}, operations.Event{}, control.ErrStateConflict
	}
	if !control.CanTransition(command.State, transition.To) {
		return control.Command{}, operations.Event{}, fmt.Errorf("%w: %s to %s", control.ErrStateConflict, command.State, transition.To)
	}
	if !transition.ActorKind.Valid() || strings.TrimSpace(transition.ActorID) == "" {
		return control.Command{}, operations.Event{}, errors.New("transition actor kind and ID are required")
	}
	if command.State == control.StateClaimed || command.State == control.StateRunning ||
		command.State == control.StateReconciling {
		if transition.FencingToken != command.FencingToken {
			return control.Command{}, operations.Event{}, control.ErrStaleFence
		}
	}
	detail, err := canonicalCommandJSON(transition.Detail)
	if err != nil {
		return control.Command{}, operations.Event{}, fmt.Errorf("canonicalize transition detail: %w", err)
	}
	if len(transition.BeforeState) > 0 {
		command.BeforeState, err = canonicalCommandJSON(transition.BeforeState)
		if err != nil {
			return control.Command{}, operations.Event{}, fmt.Errorf("canonicalize command before state: %w", err)
		}
	}
	if len(transition.Outcome) > 0 {
		command.Outcome, err = canonicalCommandJSON(transition.Outcome)
		if err != nil {
			return control.Command{}, operations.Event{}, fmt.Errorf("canonicalize command outcome: %w", err)
		}
	}
	from := command.State
	command.State = transition.To
	command.Error = transition.Error
	switch transition.To {
	case control.StateValidated:
		command.ValidatedAt = timePointerUTC(now)
	case control.StateQueued:
		command.QueuedAt = timePointerUTC(now)
	case control.StateClaimed:
		if transition.LeaseOwner == "" || transition.LeaseDuration <= 0 {
			return control.Command{}, operations.Event{}, errors.New("claimed command requires lease owner and duration")
		}
		command.LeaseOwner = transition.LeaseOwner
		command.LeaseExpiresAt = timePointerUTC(now.Add(transition.LeaseDuration))
		command.HeartbeatAt = timePointerUTC(now)
		command.Attempt++
		command.FencingToken++
	case control.StateRunning:
		if command.StartedAt == nil {
			command.StartedAt = timePointerUTC(now)
		}
		command.HeartbeatAt = timePointerUTC(now)
	case control.StateReconciling:
		if transition.LeaseOwner == "" || transition.LeaseDuration <= 0 {
			return control.Command{}, operations.Event{}, errors.New("reconciling command requires lease owner and duration")
		}
		command.LeaseOwner = transition.LeaseOwner
		command.LeaseExpiresAt = timePointerUTC(now.Add(transition.LeaseDuration))
		command.HeartbeatAt = timePointerUTC(now)
		command.Attempt++
		command.FencingToken++
	}
	if transition.To.Terminal() {
		command.CompletedAt = timePointerUTC(now)
		command.LeaseOwner = ""
		command.LeaseExpiresAt = nil
	}
	result, err := tx.ExecContext(ctx, `UPDATE commands SET
		state=?, validated_at=?, queued_at=?, started_at=?, heartbeat_at=?,
		completed_at=?, before_state_json=?, outcome_json=?, error=?,
		lease_owner=?, lease_expires_at=?, attempt=?, fencing_token=?
		WHERE id=? AND state=? AND fencing_token=?`,
		command.State, nullableTime(command.ValidatedAt), nullableTime(command.QueuedAt),
		nullableTime(command.StartedAt), nullableTime(command.HeartbeatAt),
		nullableTime(command.CompletedAt), nullableJSON(command.BeforeState),
		nullableJSON(command.Outcome), command.Error, command.LeaseOwner,
		nullableTime(command.LeaseExpiresAt), command.Attempt, command.FencingToken,
		command.ID, from, command.FencingToken-fenceIncrement(transition.To))
	if err != nil {
		return control.Command{}, operations.Event{}, fmt.Errorf("update command transition: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return control.Command{}, operations.Event{}, fmt.Errorf("update command transition: %w", err)
	}
	if affected != 1 {
		return control.Command{}, operations.Event{}, control.ErrStateConflict
	}
	if err := recordCommandTransition(ctx, tx, command, from, transition.To,
		transition.ActorKind, transition.ActorID, detail); err != nil {
		return control.Command{}, operations.Event{}, err
	}
	auditPayload, _ := json.Marshal(map[string]any{
		"from": from, "to": transition.To, "fencing_token": command.FencingToken,
	})
	if err := recordCommandAudit(ctx, tx, command, transition.ActorKind, transition.ActorID,
		"command.transitioned", auditPayload); err != nil {
		return control.Command{}, operations.Event{}, err
	}
	event, err := appendCommandEvent(ctx, tx, epochID, command, from, transition.To,
		transition.ActorKind, transition.ActorID)
	if err != nil {
		return control.Command{}, operations.Event{}, err
	}
	return command, event, nil
}

func appendCommandEvent(
	ctx context.Context, tx *sql.Tx, epochID string, command control.Command,
	from, to control.State, actorKind operations.ActorKind, actorID string,
) (operations.Event, error) {
	if epochID == "" {
		return operations.Event{}, nil
	}
	payload, err := json.Marshal(map[string]any{
		"command_type": command.Type, "state": to, "previous_state": from,
		"target_type": command.TargetType, "target_id": command.TargetID,
		"conflict_domain": command.ConflictDomain, "attempt": command.Attempt,
		"fencing_token": command.FencingToken, "has_error": command.Error != "",
	})
	if err != nil {
		return operations.Event{}, err
	}
	event, err := appendOperationalEventTx(ctx, tx, epochID, operations.EventInput{
		Type: "command." + string(to), EntityType: "command", EntityID: command.ID,
		CorrelationID: command.CorrelationID, ActorKind: actorKind, ActorID: actorID,
		Payload: payload, CommandID: command.ID,
	})
	if err != nil {
		return operations.Event{}, fmt.Errorf("append command operational event: %w", err)
	}
	return event, nil
}

func readCommand(row rowScanner) (control.Command, error) {
	var command control.Command
	var parameters, metadata, beforeState, outcome []byte
	var createdAt int64
	var validatedAt, queuedAt, startedAt, heartbeatAt, completedAt sql.NullInt64
	var timeoutAt, leaseExpiresAt sql.NullInt64
	err := row.Scan(
		&command.ID, &command.IdempotencyScope, &command.IdempotencyKey,
		&command.RequestHash, &command.Type, &command.Version, &command.HostID,
		&command.TargetType, &command.TargetID, &command.ConflictDomain,
		&parameters, &command.ActorKind, &command.ActorID, &command.Reason,
		&command.Confirmation, &command.State, &command.CorrelationID, &metadata,
		&createdAt, &validatedAt, &queuedAt, &startedAt, &heartbeatAt, &completedAt,
		&timeoutAt, &beforeState, &outcome, &command.Error, &command.LeaseOwner,
		&leaseExpiresAt, &command.Attempt, &command.FencingToken,
	)
	if err != nil {
		return control.Command{}, err
	}
	command.Parameters = append(json.RawMessage(nil), parameters...)
	command.ClientMetadata = append(json.RawMessage(nil), metadata...)
	command.BeforeState = append(json.RawMessage(nil), beforeState...)
	command.Outcome = append(json.RawMessage(nil), outcome...)
	command.CreatedAt = millisTime(createdAt)
	command.ValidatedAt = nullMillisTime(validatedAt)
	command.QueuedAt = nullMillisTime(queuedAt)
	command.StartedAt = nullMillisTime(startedAt)
	command.HeartbeatAt = nullMillisTime(heartbeatAt)
	command.CompletedAt = nullMillisTime(completedAt)
	command.TimeoutAt = nullMillisTime(timeoutAt)
	command.LeaseExpiresAt = nullMillisTime(leaseExpiresAt)
	return command, nil
}

func recordCommandTransition(
	ctx context.Context, tx *sql.Tx, command control.Command, from, to control.State,
	actorKind operations.ActorKind, actorID string, detail json.RawMessage,
) error {
	id, err := operations.NewOpaqueID()
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO command_transitions (
		id, command_id, from_state, to_state, occurred_at, actor_kind, actor_id,
		detail_json, fencing_token
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, command.ID, from, to, timeMillis(nowUTC()), actorKind, actorID,
		[]byte(detail), command.FencingToken)
	if err != nil {
		return fmt.Errorf("record command transition: %w", err)
	}
	return nil
}

func recordCommandAudit(
	ctx context.Context, tx *sql.Tx, command control.Command,
	actorKind operations.ActorKind, actorID, action string, payload json.RawMessage,
) error {
	id, err := operations.NewOpaqueID()
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO audit_events (
		id, command_id, occurred_at, actor_kind, actor_id, action, target_type,
		target_id, correlation_id, payload_json
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, command.ID, timeMillis(nowUTC()), actorKind, actorID,
		action, command.TargetType, command.TargetID, command.CorrelationID,
		[]byte(payload))
	if err != nil {
		return fmt.Errorf("record command audit: %w", err)
	}
	return nil
}

func canonicalCommandJSON(value json.RawMessage) (json.RawMessage, error) {
	if len(value) == 0 {
		return json.RawMessage(`{}`), nil
	}
	var decoded any
	if err := json.Unmarshal(value, &decoded); err != nil {
		return nil, err
	}
	return json.Marshal(decoded)
}

func nullableJSON(value json.RawMessage) any {
	if len(value) == 0 {
		return nil
	}
	return []byte(value)
}

func nullMillisTime(value sql.NullInt64) *time.Time {
	if !value.Valid {
		return nil
	}
	result := millisTime(value.Int64)
	return &result
}

func timePointerUTC(value time.Time) *time.Time {
	result := value.UTC()
	return &result
}

func fenceIncrement(state control.State) int64 {
	if state == control.StateClaimed || state == control.StateReconciling {
		return 1
	}
	return 0
}
