package history

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/GerardSmit/multirunner/internal/operations"
)

func (s *Store) EnsureOperationalHost(ctx context.Context, installationID, displayName string) (operations.Host, error) {
	if installationID == "" {
		return operations.Host{}, errors.New("installation ID is required")
	}
	now := nowUTC()
	id, err := operations.NewOpaqueID()
	if err != nil {
		return operations.Host{}, err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO hosts (
		id, installation_id, display_name, created_at, updated_at
	) VALUES (?, ?, ?, ?, ?)
	ON CONFLICT(installation_id) DO UPDATE SET
		display_name=excluded.display_name, updated_at=excluded.updated_at`,
		id, installationID, displayName, timeMillis(now), timeMillis(now))
	if err != nil {
		return operations.Host{}, fmt.Errorf("ensure operational host: %w", err)
	}
	var host operations.Host
	var createdAt, updatedAt int64
	err = s.db.QueryRowContext(ctx, `SELECT id, installation_id, display_name, created_at, updated_at
		FROM hosts WHERE installation_id=?`, installationID).Scan(
		&host.ID, &host.InstallationID, &host.DisplayName, &createdAt, &updatedAt)
	if err != nil {
		return operations.Host{}, fmt.Errorf("read operational host: %w", err)
	}
	host.CreatedAt = millisTime(createdAt)
	host.UpdatedAt = millisTime(updatedAt)
	return host, nil
}

func (s *Store) StartHostEpoch(ctx context.Context, hostID string, startedAt time.Time) (operations.HostEpoch, error) {
	if hostID == "" {
		return operations.HostEpoch{}, errors.New("host ID is required")
	}
	if startedAt.IsZero() {
		startedAt = nowUTC()
	}
	id, err := operations.NewOpaqueID()
	if err != nil {
		return operations.HostEpoch{}, err
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO host_epochs (
		id, host_id, started_at, last_sequence
	) VALUES (?, ?, ?, 0)`, id, hostID, timeMillis(startedAt)); err != nil {
		return operations.HostEpoch{}, fmt.Errorf("start host epoch: %w", err)
	}
	return operations.HostEpoch{ID: id, HostID: hostID, StartedAt: startedAt.UTC()}, nil
}

func (s *Store) EndHostEpoch(ctx context.Context, epochID string, endedAt time.Time) error {
	if epochID == "" {
		return errors.New("host epoch ID is required")
	}
	if endedAt.IsZero() {
		endedAt = nowUTC()
	}
	result, err := s.db.ExecContext(ctx, `UPDATE host_epochs
		SET ended_at=? WHERE id=? AND ended_at IS NULL`, timeMillis(endedAt), epochID)
	if err != nil {
		return fmt.Errorf("end host epoch: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("end host epoch: %w", err)
	}
	if affected == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) UnfinishedHostEpochs(
	ctx context.Context, hostID, excludeEpochID string,
) ([]operations.HostEpoch, error) {
	if hostID == "" {
		return nil, errors.New("host ID is required")
	}
	statement := `SELECT id, host_id, started_at, last_sequence
		FROM host_epochs WHERE host_id=? AND ended_at IS NULL`
	args := []any{hostID}
	if excludeEpochID != "" {
		statement += ` AND id!=?`
		args = append(args, excludeEpochID)
	}
	statement += ` ORDER BY started_at, id`
	rows, err := s.db.QueryContext(ctx, statement, args...)
	if err != nil {
		return nil, fmt.Errorf("list unfinished host epochs: %w", err)
	}
	defer rows.Close()
	var epochs []operations.HostEpoch
	for rows.Next() {
		var epoch operations.HostEpoch
		var startedAt int64
		if err := rows.Scan(
			&epoch.ID, &epoch.HostID, &startedAt, &epoch.LastSequence,
		); err != nil {
			return nil, fmt.Errorf("scan unfinished host epoch: %w", err)
		}
		epoch.StartedAt = millisTime(startedAt)
		epochs = append(epochs, epoch)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list unfinished host epochs: %w", err)
	}
	return epochs, nil
}

func (s *Store) AppendOperationalEvent(ctx context.Context, epochID string, input operations.EventInput) (operations.Event, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return operations.Event{}, fmt.Errorf("begin operational event append: %w", err)
	}
	defer tx.Rollback()
	event, err := appendOperationalEventTx(ctx, tx, epochID, input)
	if err != nil {
		return operations.Event{}, err
	}
	if err := tx.Commit(); err != nil {
		return operations.Event{}, fmt.Errorf("commit operational event append: %w", err)
	}
	s.notifyOperationalEvent(event)
	return event, nil
}

func appendOperationalEventTx(
	ctx context.Context, tx *sql.Tx, epochID string, input operations.EventInput,
) (operations.Event, error) {
	if epochID == "" {
		return operations.Event{}, errors.New("host epoch ID is required")
	}
	if err := input.Validate(); err != nil {
		return operations.Event{}, err
	}
	if input.Timestamp.IsZero() {
		input.Timestamp = nowUTC()
	}
	input.Timestamp = millisTime(timeMillis(input.Timestamp))
	payload := input.Payload
	if len(payload) == 0 {
		payload = json.RawMessage(`{}`)
	}
	payload = append(json.RawMessage(nil), payload...)

	var hostID string
	var lastSequence int64
	err := tx.QueryRowContext(ctx, `SELECT host_id, last_sequence
		FROM host_epochs WHERE id=? AND ended_at IS NULL`, epochID).Scan(&hostID, &lastSequence)
	if errors.Is(err, sql.ErrNoRows) {
		return operations.Event{}, ErrNotFound
	}
	if err != nil {
		return operations.Event{}, fmt.Errorf("read host epoch: %w", err)
	}
	sequence := lastSequence + 1
	event := operations.Event{
		ID: operations.EventID(epochID, sequence), SchemaVersion: operations.CurrentEventSchemaVersion,
		HostID: hostID, HostEpoch: epochID, Sequence: sequence,
		Type: input.Type, EntityType: input.EntityType, EntityID: input.EntityID,
		Timestamp: input.Timestamp.UTC(), CorrelationID: input.CorrelationID,
		CausationID: input.CausationID, ActorKind: input.ActorKind, ActorID: input.ActorID,
		Payload: payload, CommandID: input.CommandID,
	}
	if lastSequence > 0 {
		err = tx.QueryRowContext(ctx, `SELECT integrity_hash FROM operational_events
			WHERE host_epoch=? AND sequence=?`, epochID, lastSequence).Scan(&event.PreviousHash)
		if errors.Is(err, sql.ErrNoRows) {
			err = tx.QueryRowContext(ctx, `SELECT integrity_hash
				FROM operational_event_anchors
				WHERE host_epoch=? AND through_sequence=?`,
				epochID, lastSequence).Scan(&event.PreviousHash)
		}
		if err != nil {
			return operations.Event{}, fmt.Errorf("read previous operational event hash: %w", err)
		}
	}
	event.IntegrityHash = operations.IntegrityHash(event.PreviousHash, event)

	result, err := tx.ExecContext(ctx, `UPDATE host_epochs SET last_sequence=?
		WHERE id=? AND last_sequence=? AND ended_at IS NULL`, sequence, epochID, lastSequence)
	if err != nil {
		return operations.Event{}, fmt.Errorf("advance host sequence: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return operations.Event{}, fmt.Errorf("advance host sequence: %w", err)
	}
	if affected != 1 {
		return operations.Event{}, errors.New("host sequence changed during event append")
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO operational_events (
		id, schema_version, host_id, host_epoch, sequence, event_type, entity_type,
		entity_id, timestamp, correlation_id, causation_id, actor_kind, actor_id,
		payload_json, command_id, previous_hash, integrity_hash
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		event.ID, event.SchemaVersion, event.HostID, event.HostEpoch, event.Sequence,
		event.Type, event.EntityType, event.EntityID, timeMillis(event.Timestamp),
		event.CorrelationID, event.CausationID, event.ActorKind, event.ActorID,
		[]byte(event.Payload), event.CommandID, event.PreviousHash, event.IntegrityHash)
	if err != nil {
		return operations.Event{}, fmt.Errorf("insert operational event: %w", err)
	}
	if err := applyOperationalProjection(ctx, tx, event); err != nil {
		return operations.Event{}, err
	}
	return event, nil
}

func (s *Store) SetOperationalEventListener(listener func(operations.Event)) {
	s.eventMu.Lock()
	s.eventListener = listener
	s.eventMu.Unlock()
}

func (s *Store) SetCommandEpoch(epochID string) {
	s.eventMu.Lock()
	s.commandEpoch = epochID
	s.eventMu.Unlock()
}

func (s *Store) commandEpochID() string {
	s.eventMu.RLock()
	defer s.eventMu.RUnlock()
	return s.commandEpoch
}

func (s *Store) notifyOperationalEvent(event operations.Event) {
	s.eventMu.RLock()
	listener := s.eventListener
	s.eventMu.RUnlock()
	if listener != nil {
		listener(event)
	}
}

func (s *Store) ListOperationalEvents(ctx context.Context, query operations.EventQuery) ([]operations.Event, error) {
	if query.HostEpoch == "" {
		return nil, errors.New("host epoch is required")
	}
	limit := query.Limit
	if limit <= 0 {
		limit = 100
	}
	if limit > 1000 {
		limit = 1000
	}
	statement := `SELECT
		id, schema_version, host_id, host_epoch, sequence, event_type, entity_type,
		entity_id, timestamp, correlation_id, causation_id, actor_kind, actor_id,
		payload_json, command_id, previous_hash, integrity_hash
		FROM operational_events
		WHERE host_epoch=? AND sequence>?`
	args := []any{query.HostEpoch, query.AfterSequence}
	if query.MaxSequence > 0 {
		statement += ` AND sequence<=?`
		args = append(args, query.MaxSequence)
	}
	if !query.Since.IsZero() {
		statement += ` AND timestamp>=?`
		args = append(args, timeMillis(query.Since))
	}
	if !query.Until.IsZero() {
		statement += ` AND timestamp<=?`
		args = append(args, timeMillis(query.Until))
	}
	if query.Type != "" {
		statement += ` AND event_type=?`
		args = append(args, query.Type)
	}
	if query.EntityType != "" {
		statement += ` AND entity_type=?`
		args = append(args, query.EntityType)
	}
	if query.EntityID != "" {
		statement += ` AND entity_id=?`
		args = append(args, query.EntityID)
	}
	statement += ` ORDER BY sequence ASC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, statement, args...)
	if err != nil {
		return nil, fmt.Errorf("list operational events: %w", err)
	}
	defer rows.Close()
	var events []operations.Event
	for rows.Next() {
		var event operations.Event
		var timestamp int64
		var payload []byte
		if err := rows.Scan(
			&event.ID, &event.SchemaVersion, &event.HostID, &event.HostEpoch,
			&event.Sequence, &event.Type, &event.EntityType, &event.EntityID,
			&timestamp, &event.CorrelationID, &event.CausationID, &event.ActorKind,
			&event.ActorID, &payload, &event.CommandID, &event.PreviousHash,
			&event.IntegrityHash,
		); err != nil {
			return nil, fmt.Errorf("scan operational event: %w", err)
		}
		event.Timestamp = millisTime(timestamp)
		event.Payload = append(json.RawMessage(nil), payload...)
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list operational events: %w", err)
	}
	return events, nil
}

func applyOperationalProjection(ctx context.Context, tx *sql.Tx, event operations.Event) error {
	if event.EntityType != "runner_session" {
		return nil
	}
	var payload struct {
		Pool              string `json:"pool"`
		Target            string `json:"target"`
		Repository        string `json:"repository"`
		RunnerName        string `json:"runner_name"`
		BackendInstanceID string `json:"backend_instance_id"`
		Error             string `json:"error"`
	}
	if err := json.Unmarshal(event.Payload, &payload); err != nil {
		return fmt.Errorf("decode runner state projection payload: %w", err)
	}
	if len(event.Payload) > 0 && event.Payload[0] != '{' {
		return errors.New("runner state projection payload must be a JSON object")
	}
	status := event.Type
	if len(status) > len("runner.") && status[:len("runner.")] == "runner." {
		status = status[len("runner."):]
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO runner_state (
		id, host_id, host_epoch, pool_name, target, repository, runner_name,
		status, backend_id, error, last_event_id, sequence, updated_at
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	ON CONFLICT(id) DO UPDATE SET
		host_id=excluded.host_id, host_epoch=excluded.host_epoch,
		pool_name=excluded.pool_name, target=excluded.target,
		repository=excluded.repository, runner_name=excluded.runner_name,
		status=excluded.status, backend_id=excluded.backend_id,
		error=excluded.error, last_event_id=excluded.last_event_id,
		sequence=excluded.sequence, updated_at=excluded.updated_at
	WHERE excluded.sequence > runner_state.sequence`,
		event.EntityID, event.HostID, event.HostEpoch, payload.Pool, payload.Target,
		payload.Repository, payload.RunnerName, status, payload.BackendInstanceID,
		payload.Error, event.ID, event.Sequence, timeMillis(event.Timestamp))
	if err != nil {
		return fmt.Errorf("update runner state projection: %w", err)
	}
	return nil
}

func (s *Store) OperationalEventBounds(ctx context.Context, epochID string) (minimum, maximum int64, err error) {
	if epochID == "" {
		return 0, 0, errors.New("host epoch is required")
	}
	var min, max sql.NullInt64
	err = s.db.QueryRowContext(ctx, `SELECT MIN(sequence), MAX(sequence)
		FROM operational_events WHERE host_epoch=?`, epochID).Scan(&min, &max)
	if err != nil {
		return 0, 0, fmt.Errorf("read operational event bounds: %w", err)
	}
	if min.Valid {
		minimum = min.Int64
	}
	if max.Valid {
		maximum = max.Int64
	}
	return minimum, maximum, nil
}

func (s *Store) OperationalSnapshot(ctx context.Context, epochID string) (operations.Snapshot, error) {
	if epochID == "" {
		return operations.Snapshot{}, errors.New("host epoch is required")
	}
	var snapshot operations.Snapshot
	snapshot.HostEpoch = epochID
	snapshot.GeneratedAt = nowUTC()
	err := s.db.QueryRowContext(ctx, `SELECT last_sequence FROM host_epochs WHERE id=?`, epochID).
		Scan(&snapshot.LastSequence)
	if errors.Is(err, sql.ErrNoRows) {
		return operations.Snapshot{}, ErrNotFound
	}
	if err != nil {
		return operations.Snapshot{}, fmt.Errorf("read host epoch snapshot: %w", err)
	}
	rows, err := s.db.QueryContext(ctx, `SELECT
		id, host_id, host_epoch, pool_name, target, repository, runner_name,
		status, backend_id, error, last_event_id, sequence, updated_at
		FROM runner_state WHERE host_epoch=? ORDER BY updated_at DESC, id ASC`, epochID)
	if err != nil {
		return operations.Snapshot{}, fmt.Errorf("list runner state projection: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var runner operations.RunnerState
		var updatedAt int64
		if err := rows.Scan(
			&runner.ID, &runner.HostID, &runner.HostEpoch, &runner.Pool,
			&runner.Target, &runner.Repository, &runner.RunnerName, &runner.Status,
			&runner.BackendID, &runner.Error, &runner.LastEventID, &runner.Sequence,
			&updatedAt,
		); err != nil {
			return operations.Snapshot{}, fmt.Errorf("scan runner state projection: %w", err)
		}
		runner.UpdatedAt = millisTime(updatedAt)
		snapshot.Runners = append(snapshot.Runners, runner)
	}
	if err := rows.Err(); err != nil {
		return operations.Snapshot{}, fmt.Errorf("list runner state projection: %w", err)
	}
	return snapshot, nil
}

func (s *Store) OperationalSnapshotAt(ctx context.Context, epochID string, maxSequence int64) (operations.Snapshot, error) {
	if epochID == "" {
		return operations.Snapshot{}, errors.New("host epoch is required")
	}
	if maxSequence < 0 {
		return operations.Snapshot{}, errors.New("maximum sequence cannot be negative")
	}
	var currentSequence int64
	err := s.db.QueryRowContext(ctx, `SELECT last_sequence FROM host_epochs WHERE id=?`, epochID).
		Scan(&currentSequence)
	if errors.Is(err, sql.ErrNoRows) {
		return operations.Snapshot{}, ErrNotFound
	}
	if err != nil {
		return operations.Snapshot{}, fmt.Errorf("read host epoch snapshot: %w", err)
	}
	if maxSequence == 0 || maxSequence > currentSequence {
		maxSequence = currentSequence
	}
	var compactedThrough int64
	err = s.db.QueryRowContext(ctx, `SELECT COALESCE((
		SELECT through_sequence FROM operational_event_anchors WHERE host_epoch=?
	), 0)`, epochID).Scan(&compactedThrough)
	if err != nil {
		return operations.Snapshot{}, fmt.Errorf("read operational compaction bound: %w", err)
	}
	if maxSequence <= compactedThrough {
		return operations.Snapshot{}, fmt.Errorf(
			"%w: requested=%d compacted_through=%d",
			ErrHistoryCompacted, maxSequence, compactedThrough)
	}
	snapshot := operations.Snapshot{
		HostEpoch: epochID, LastSequence: maxSequence, GeneratedAt: nowUTC(),
	}
	rows, err := s.db.QueryContext(ctx, `WITH latest AS (
		SELECT entity_id, MAX(sequence) AS sequence
		FROM operational_events
		WHERE host_epoch=? AND entity_type='runner_session' AND sequence<=?
		GROUP BY entity_id
	)
	SELECT event.entity_id, event.host_id, event.host_epoch, event.event_type,
		event.payload_json, event.id, event.sequence, event.timestamp
	FROM operational_events event
	INNER JOIN latest ON latest.entity_id=event.entity_id AND latest.sequence=event.sequence
	WHERE event.host_epoch=?
	ORDER BY event.timestamp DESC, event.entity_id ASC`,
		epochID, maxSequence, epochID)
	if err != nil {
		return operations.Snapshot{}, fmt.Errorf("list historical runner state: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var runner operations.RunnerState
		var eventType string
		var payload []byte
		var updatedAt int64
		if err := rows.Scan(
			&runner.ID, &runner.HostID, &runner.HostEpoch, &eventType, &payload,
			&runner.LastEventID, &runner.Sequence, &updatedAt,
		); err != nil {
			return operations.Snapshot{}, fmt.Errorf("scan historical runner state: %w", err)
		}
		var state struct {
			Pool              string `json:"pool"`
			Target            string `json:"target"`
			Repository        string `json:"repository"`
			RunnerName        string `json:"runner_name"`
			BackendInstanceID string `json:"backend_instance_id"`
			Error             string `json:"error"`
		}
		if err := json.Unmarshal(payload, &state); err != nil {
			return operations.Snapshot{}, fmt.Errorf("decode historical runner state: %w", err)
		}
		runner.Pool = state.Pool
		runner.Target = state.Target
		runner.Repository = state.Repository
		runner.RunnerName = state.RunnerName
		runner.BackendID = state.BackendInstanceID
		runner.Error = state.Error
		runner.Status = eventType
		if strings.HasPrefix(runner.Status, "runner.") {
			runner.Status = strings.TrimPrefix(runner.Status, "runner.")
		}
		runner.UpdatedAt = millisTime(updatedAt)
		snapshot.Runners = append(snapshot.Runners, runner)
	}
	if err := rows.Err(); err != nil {
		return operations.Snapshot{}, fmt.Errorf("list historical runner state: %w", err)
	}
	return snapshot, nil
}

func (s *Store) SetProjectionWatermark(ctx context.Context, watermark operations.ProjectionWatermark) error {
	if watermark.Name == "" || watermark.HostEpoch == "" || watermark.EventID == "" || watermark.Sequence < 1 {
		return errors.New("projection watermark name, epoch, sequence, and event ID are required")
	}
	if watermark.UpdatedAt.IsZero() {
		watermark.UpdatedAt = nowUTC()
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO projection_watermarks (
		name, host_epoch, sequence, event_id, updated_at
	) VALUES (?, ?, ?, ?, ?)
	ON CONFLICT(name) DO UPDATE SET
		host_epoch=excluded.host_epoch, sequence=excluded.sequence,
		event_id=excluded.event_id, updated_at=excluded.updated_at
	WHERE excluded.host_epoch != projection_watermarks.host_epoch
	   OR excluded.sequence >= projection_watermarks.sequence`,
		watermark.Name, watermark.HostEpoch, watermark.Sequence, watermark.EventID,
		timeMillis(watermark.UpdatedAt))
	if err != nil {
		return fmt.Errorf("set projection watermark: %w", err)
	}
	return nil
}

func (s *Store) ProjectionWatermark(ctx context.Context, name string) (operations.ProjectionWatermark, error) {
	if name == "" {
		return operations.ProjectionWatermark{}, errors.New("projection watermark name is required")
	}
	var watermark operations.ProjectionWatermark
	var updatedAt int64
	err := s.db.QueryRowContext(ctx, `SELECT name, host_epoch, sequence, event_id, updated_at
		FROM projection_watermarks WHERE name=?`, name).Scan(
		&watermark.Name, &watermark.HostEpoch, &watermark.Sequence,
		&watermark.EventID, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return operations.ProjectionWatermark{}, ErrNotFound
	}
	if err != nil {
		return operations.ProjectionWatermark{}, fmt.Errorf("read projection watermark: %w", err)
	}
	watermark.UpdatedAt = millisTime(updatedAt)
	return watermark, nil
}
