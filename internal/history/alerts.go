package history

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/GerardSmit/multirunner/internal/alerts"
	"github.com/GerardSmit/multirunner/internal/operations"
)

const alertColumns = `id, rule_id, dedup_key, state, severity, summary,
	details_json, source_event_id, first_observed_at, last_observed_at, due_at,
	opened_at, resolved_at, acknowledged_at, acknowledged_by, silenced_until,
	last_notified_at, occurrence_count, version`

func (s *Store) AlertRulesForEvent(ctx context.Context, eventType string) ([]alerts.Rule, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, version, name, family, severity,
		event_type, hold_seconds, cooldown_seconds, enabled
		FROM alert_rules WHERE enabled=1 AND event_type=? ORDER BY id`, eventType)
	if err != nil {
		return nil, fmt.Errorf("list alert rules: %w", err)
	}
	defer rows.Close()
	var rules []alerts.Rule
	for rows.Next() {
		var rule alerts.Rule
		var holdSeconds, cooldownSeconds int64
		var enabled int
		if err := rows.Scan(
			&rule.ID, &rule.Version, &rule.Name, &rule.Family, &rule.Severity,
			&rule.EventType, &holdSeconds, &cooldownSeconds, &enabled,
		); err != nil {
			return nil, fmt.Errorf("scan alert rule: %w", err)
		}
		rule.Hold = time.Duration(holdSeconds) * time.Second
		rule.Cooldown = time.Duration(cooldownSeconds) * time.Second
		rule.Enabled = enabled != 0
		rules = append(rules, rule)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list alert rules: %w", err)
	}
	return rules, nil
}

func (s *Store) ObserveAlert(ctx context.Context, observation alerts.Observation) (alerts.Instance, error) {
	if err := observation.Validate(); err != nil {
		return alerts.Instance{}, err
	}
	if observation.ObservedAt.IsZero() {
		observation.ObservedAt = nowUTC()
	}
	observation.ObservedAt = observation.ObservedAt.UTC()
	if len(observation.Details) == 0 {
		observation.Details = json.RawMessage(`{}`)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return alerts.Instance{}, fmt.Errorf("begin alert observation: %w", err)
	}
	defer tx.Rollback()
	var evaluatedAlertID string
	err = tx.QueryRowContext(ctx, `SELECT alert_id FROM alert_evaluated_events
		WHERE event_id=? AND rule_id=?`,
		observation.SourceEventID, observation.Rule.ID).Scan(&evaluatedAlertID)
	if err == nil {
		instance, readErr := readAlert(tx.QueryRowContext(ctx,
			`SELECT `+alertColumns+` FROM alert_instances WHERE id=?`, evaluatedAlertID))
		if readErr != nil {
			return alerts.Instance{}, fmt.Errorf("read evaluated alert: %w", readErr)
		}
		return instance, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return alerts.Instance{}, fmt.Errorf("read evaluated alert event: %w", err)
	}

	instance, err := readAlert(tx.QueryRowContext(ctx,
		`SELECT `+alertColumns+` FROM alert_instances WHERE rule_id=? AND dedup_key=?`,
		observation.Rule.ID, observation.DedupKey))
	if errors.Is(err, sql.ErrNoRows) {
		instance, err = newAlertInstance(observation)
		if err != nil {
			return alerts.Instance{}, err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO alert_instances (
			id, rule_id, dedup_key, state, severity, summary, details_json,
			source_event_id, first_observed_at, last_observed_at, due_at, opened_at,
			occurrence_count, version
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			instance.ID, instance.RuleID, instance.DedupKey, instance.State,
			instance.Severity, instance.Summary, []byte(instance.Details),
			instance.SourceEventID, timeMillis(instance.FirstObservedAt),
			timeMillis(instance.LastObservedAt), timeMillis(instance.DueAt),
			nullableTime(instance.OpenedAt), instance.OccurrenceCount, instance.Version,
		); err != nil {
			return alerts.Instance{}, fmt.Errorf("insert alert instance: %w", err)
		}
		event, err := s.recordAlertTransition(ctx, tx, alertTransitionInput{
			Instance: instance,
			To:       instance.State,
			Context: alertTransitionContext{
				ActorKind:     operations.ActorSystem,
				ActorID:       "alert-engine",
				Reason:        "rule matched",
				CorrelationID: observation.CorrelationID,
				OccurredAt:    observation.ObservedAt,
			},
			Notify: instance.State == alerts.StateOpen,
		})
		if err != nil {
			return alerts.Instance{}, err
		}
		if err := recordEvaluatedAlertEvent(
			ctx, tx, observation, instance.ID,
		); err != nil {
			return alerts.Instance{}, err
		}
		if err := tx.Commit(); err != nil {
			return alerts.Instance{}, fmt.Errorf("commit alert observation: %w", err)
		}
		s.notifyAlertEvents(event)
		return instance, nil
	}
	if err != nil {
		return alerts.Instance{}, fmt.Errorf("read alert instance: %w", err)
	}

	from := instance.State
	reopened := instance.State == alerts.StateResolved
	if reopened {
		instance.State = alerts.StatePending
		instance.FirstObservedAt = observation.ObservedAt
		instance.DueAt = observation.ObservedAt.Add(observation.Rule.Hold)
		instance.OpenedAt = nil
		instance.ResolvedAt = nil
		instance.AcknowledgedAt = nil
		instance.AcknowledgedBy = ""
		instance.LastNotifiedAt = nil
		instance.OccurrenceCount = 0
	}
	instance.LastObservedAt = observation.ObservedAt
	instance.SourceEventID = observation.SourceEventID
	instance.Summary = observation.Summary
	instance.Severity = observation.Rule.Severity
	instance.Details = append(json.RawMessage(nil), observation.Details...)
	instance.OccurrenceCount++
	instance.Version++

	transition := reopened
	reason := "rule matched again"
	if instance.State == alerts.StatePending && !observation.ObservedAt.Before(instance.DueAt) {
		instance.State = alerts.StateOpen
		openedAt := observation.ObservedAt
		instance.OpenedAt = &openedAt
		transition = true
		reason = "hold elapsed"
	}
	repeat := instance.State == alerts.StateOpen && !transition &&
		alertCooldownElapsed(instance.LastNotifiedAt, observation.ObservedAt, observation.Rule.Cooldown) &&
		!alertSilenced(instance, observation.ObservedAt)

	result, err := tx.ExecContext(ctx, `UPDATE alert_instances SET
		state=?, severity=?, summary=?, details_json=?, source_event_id=?,
		first_observed_at=?, last_observed_at=?, due_at=?, opened_at=?, resolved_at=?,
		acknowledged_at=?, acknowledged_by=?, last_notified_at=?, occurrence_count=?,
		version=? WHERE id=? AND version=?`,
		instance.State, instance.Severity, instance.Summary, []byte(instance.Details),
		instance.SourceEventID, timeMillis(instance.FirstObservedAt),
		timeMillis(instance.LastObservedAt), timeMillis(instance.DueAt),
		nullableTime(instance.OpenedAt), nullableTime(instance.ResolvedAt),
		nullableTime(instance.AcknowledgedAt), instance.AcknowledgedBy,
		nullableTime(instance.LastNotifiedAt), instance.OccurrenceCount,
		instance.Version, instance.ID, instance.Version-1,
	)
	if err != nil {
		return alerts.Instance{}, fmt.Errorf("update alert observation: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return alerts.Instance{}, fmt.Errorf("update alert observation: %w", err)
	}
	if affected != 1 {
		return alerts.Instance{}, alerts.ErrStateConflict
	}

	var event operations.Event
	if transition {
		event, err = s.recordAlertTransition(ctx, tx, alertTransitionInput{
			Instance: instance,
			From:     from,
			To:       instance.State,
			Context: alertTransitionContext{
				ActorKind:     operations.ActorSystem,
				ActorID:       "alert-engine",
				Reason:        reason,
				CorrelationID: observation.CorrelationID,
				OccurredAt:    observation.ObservedAt,
			},
			Notify: instance.State == alerts.StateOpen,
		})
	} else if repeat {
		event, err = s.recordAlertTransition(ctx, tx, alertTransitionInput{
			Instance: instance,
			From:     instance.State,
			To:       instance.State,
			Context: alertTransitionContext{
				ActorKind:     operations.ActorSystem,
				ActorID:       "alert-engine",
				Reason:        "cooldown elapsed",
				CorrelationID: observation.CorrelationID,
				OccurredAt:    observation.ObservedAt,
			},
			Notify:    true,
			EventType: "repeated",
		})
	}
	if err != nil {
		return alerts.Instance{}, err
	}
	if err := recordEvaluatedAlertEvent(
		ctx, tx, observation, instance.ID,
	); err != nil {
		return alerts.Instance{}, err
	}
	if err := tx.Commit(); err != nil {
		return alerts.Instance{}, fmt.Errorf("commit alert observation: %w", err)
	}

	s.notifyAlertEvents(event)
	return instance, nil
}

func recordEvaluatedAlertEvent(
	ctx context.Context, tx *sql.Tx, observation alerts.Observation, alertID string,
) error {
	if strings.TrimSpace(observation.SourceEventID) == "" {
		return nil
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO alert_evaluated_events (
		event_id, rule_id, alert_id, evaluated_at
	) VALUES (?, ?, ?, ?)`,
		observation.SourceEventID, observation.Rule.ID, alertID,
		timeMillis(observation.ObservedAt))
	if err != nil {
		return fmt.Errorf("record evaluated alert event: %w", err)
	}
	return nil
}

func (s *Store) AlertEventCursor(ctx context.Context, epochID string) (int64, error) {
	watermark, err := s.ProjectionWatermark(ctx, "alerts")
	if errors.Is(err, ErrNotFound) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	if watermark.HostEpoch != epochID {
		return 0, nil
	}
	return watermark.Sequence, nil
}

func (s *Store) AdvanceAlertEventCursor(
	ctx context.Context, event operations.Event,
) error {
	if event.HostEpoch == "" || event.Sequence < 1 || event.ID == "" {
		return errors.New("alert event cursor requires a durable event")
	}
	return s.SetProjectionWatermark(ctx, operations.ProjectionWatermark{
		Name: "alerts", HostEpoch: event.HostEpoch, Sequence: event.Sequence,
		EventID: event.ID, UpdatedAt: nowUTC(),
	})
}

func newAlertInstance(observation alerts.Observation) (alerts.Instance, error) {
	id, err := operations.NewOpaqueID()
	if err != nil {
		return alerts.Instance{}, err
	}
	state := alerts.StatePending
	dueAt := observation.ObservedAt.Add(observation.Rule.Hold)
	var openedAt *time.Time
	if observation.Rule.Hold <= 0 {
		state = alerts.StateOpen
		openedAt = timePointerUTC(observation.ObservedAt)
	}
	return alerts.Instance{
		ID: id, RuleID: observation.Rule.ID, DedupKey: observation.DedupKey,
		State: state, Severity: observation.Rule.Severity, Summary: observation.Summary,
		Details:         append(json.RawMessage(nil), observation.Details...),
		SourceEventID:   observation.SourceEventID,
		FirstObservedAt: observation.ObservedAt, LastObservedAt: observation.ObservedAt,
		DueAt: dueAt, OpenedAt: openedAt, OccurrenceCount: 1, Version: 1,
	}, nil
}

func (s *Store) PromoteDueAlerts(ctx context.Context, at time.Time) ([]alerts.Instance, error) {
	if at.IsZero() {
		at = nowUTC()
	}
	at = at.UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin due alert promotion: %w", err)
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT `+alertColumns+` FROM alert_instances
		WHERE state='pending' AND due_at<=? ORDER BY due_at, id`, timeMillis(at))
	if err != nil {
		return nil, fmt.Errorf("list due alerts: %w", err)
	}
	var instances []alerts.Instance
	for rows.Next() {
		instance, err := scanAlert(rows)
		if err != nil {
			_ = rows.Close()
			return nil, err
		}
		instances = append(instances, instance)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("close due alert rows: %w", err)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read due alert rows: %w", err)
	}
	var events []operations.Event
	for index := range instances {
		instance := &instances[index]
		from := instance.State
		instance.State = alerts.StateOpen
		instance.OpenedAt = timePointerUTC(at)
		instance.Version++
		result, err := tx.ExecContext(ctx, `UPDATE alert_instances
			SET state=?, opened_at=?, version=? WHERE id=? AND state=? AND version=?`,
			instance.State, timeMillis(at), instance.Version, instance.ID, from, instance.Version-1)
		if err != nil {
			return nil, fmt.Errorf("promote due alert: %w", err)
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return nil, fmt.Errorf("promote due alert: %w", err)
		}
		if affected != 1 {
			return nil, alerts.ErrStateConflict
		}
		event, err := s.recordAlertTransition(ctx, tx, alertTransitionInput{
			Instance: *instance,
			From:     from,
			To:       instance.State,
			Context: alertTransitionContext{
				ActorKind:  operations.ActorSystem,
				ActorID:    "alert-engine",
				Reason:     "hold elapsed",
				OccurredAt: at,
			},
			Notify: true,
		})
		if err != nil {
			return nil, err
		}
		if event.ID != "" {
			events = append(events, event)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit due alert promotion: %w", err)
	}
	for _, event := range events {
		s.notifyOperationalEvent(event)
	}
	return instances, nil
}

func (s *Store) ResolveAlertByRuleKey(
	ctx context.Context, ruleID, dedupKey string, at time.Time, correlationID string,
) error {
	if ruleID == "" || dedupKey == "" {
		return errors.New("alert rule and dedup key are required")
	}
	if at.IsZero() {
		at = nowUTC()
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin alert resolution: %w", err)
	}
	defer tx.Rollback()
	instance, err := readAlert(tx.QueryRowContext(ctx,
		`SELECT `+alertColumns+` FROM alert_instances WHERE rule_id=? AND dedup_key=?`,
		ruleID, dedupKey))
	if errors.Is(err, sql.ErrNoRows) {
		return alerts.ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("read alert for resolution: %w", err)
	}
	if instance.State == alerts.StateResolved {
		return nil
	}
	from := instance.State
	instance.State = alerts.StateResolved
	instance.ResolvedAt = timePointerUTC(at)
	instance.Version++
	result, err := tx.ExecContext(ctx, `UPDATE alert_instances
		SET state=?, resolved_at=?, version=? WHERE id=? AND state=? AND version=?`,
		instance.State, timeMillis(at), instance.Version, instance.ID, from, instance.Version-1)
	if err != nil {
		return fmt.Errorf("resolve alert: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("resolve alert: %w", err)
	}
	if affected != 1 {
		return alerts.ErrStateConflict
	}
	event, err := s.recordAlertTransition(ctx, tx, alertTransitionInput{
		Instance: instance,
		From:     from,
		To:       instance.State,
		Context: alertTransitionContext{
			ActorKind:     operations.ActorSystem,
			ActorID:       "alert-engine",
			Reason:        "recovery event observed",
			CorrelationID: correlationID,
			OccurredAt:    at,
		},
	})
	if err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit alert resolution: %w", err)
	}
	s.notifyAlertEvents(event)
	return nil
}

func (s *Store) Alert(ctx context.Context, id string) (alerts.Instance, error) {
	if strings.TrimSpace(id) == "" {
		return alerts.Instance{}, errors.New("alert ID is required")
	}
	instance, err := readAlert(s.db.QueryRowContext(ctx,
		`SELECT `+alertColumns+` FROM alert_instances WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return alerts.Instance{}, alerts.ErrNotFound
	}
	if err != nil {
		return alerts.Instance{}, fmt.Errorf("read alert: %w", err)
	}
	return instance, nil
}

func (s *Store) ListAlerts(ctx context.Context, options alerts.ListOptions) ([]alerts.Instance, error) {
	limit := options.Limit
	if limit <= 0 {
		limit = 100
	}
	if limit > 500 {
		limit = 500
	}
	if options.Offset < 0 {
		return nil, errors.New("alert offset cannot be negative")
	}
	statement := `SELECT ` + alertColumns + ` FROM alert_instances WHERE 1=1`
	var args []any
	if options.State != "" {
		if err := validateAlertState(options.State); err != nil {
			return nil, err
		}
		statement += ` AND state=?`
		args = append(args, options.State)
	}
	if options.Severity != "" {
		statement += ` AND severity=?`
		args = append(args, options.Severity)
	}
	statement += ` ORDER BY last_observed_at DESC, id LIMIT ? OFFSET ?`
	args = append(args, limit, options.Offset)
	rows, err := s.db.QueryContext(ctx, statement, args...)
	if err != nil {
		return nil, fmt.Errorf("list alerts: %w", err)
	}
	defer rows.Close()
	var instances []alerts.Instance
	for rows.Next() {
		instance, err := scanAlert(rows)
		if err != nil {
			return nil, fmt.Errorf("scan alert: %w", err)
		}
		instances = append(instances, instance)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list alerts: %w", err)
	}
	return instances, nil
}

func (s *Store) TransitionAlert(
	ctx context.Context, request alerts.TransitionRequest,
) (alerts.Instance, error) {
	if strings.TrimSpace(request.AlertID) == "" || request.ExpectedVersion < 1 ||
		!validAlertIdempotencyKey(request.IdempotencyKey) {
		return alerts.Instance{}, errors.New("alert ID, expected version, and idempotency key are required")
	}
	if request.To != alerts.StateResolved {
		return alerts.Instance{}, errors.New("operator alert transition must resolve the alert")
	}
	if !request.ActorKind.Valid() || strings.TrimSpace(request.ActorID) == "" {
		return alerts.Instance{}, errors.New("alert transition actor is required")
	}
	if request.OccurredAt.IsZero() {
		request.OccurredAt = nowUTC()
	}
	requestHash, err := alertMutationHash(struct {
		ExpectedState   alerts.State `json:"expected_state"`
		ExpectedVersion int          `json:"expected_version"`
		To              alerts.State `json:"to"`
		Reason          string       `json:"reason"`
	}{
		ExpectedState: request.ExpectedState, ExpectedVersion: request.ExpectedVersion,
		To: request.To, Reason: request.Reason,
	})
	if err != nil {
		return alerts.Instance{}, err
	}
	scope := alertMutationScope("resolve", request.AlertID, request.ActorID)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return alerts.Instance{}, fmt.Errorf("begin alert transition: %w", err)
	}
	defer tx.Rollback()
	if _, found, err := findAlertMutation(
		ctx, tx, scope, request.IdempotencyKey, requestHash,
	); err != nil {
		return alerts.Instance{}, err
	} else if found {
		instance, err := readAlert(tx.QueryRowContext(ctx,
			`SELECT `+alertColumns+` FROM alert_instances WHERE id=?`, request.AlertID))
		if err != nil {
			return alerts.Instance{}, fmt.Errorf("read idempotent alert transition: %w", err)
		}
		return instance, nil
	}
	instance, err := readAlert(tx.QueryRowContext(ctx,
		`SELECT `+alertColumns+` FROM alert_instances WHERE id=?`, request.AlertID))
	if errors.Is(err, sql.ErrNoRows) {
		return alerts.Instance{}, alerts.ErrNotFound
	}
	if err != nil {
		return alerts.Instance{}, fmt.Errorf("read alert for transition: %w", err)
	}
	if instance.Version != request.ExpectedVersion ||
		(request.ExpectedState != "" && instance.State != request.ExpectedState) {
		return alerts.Instance{}, alerts.ErrStateConflict
	}
	if instance.State == alerts.StateResolved {
		return alerts.Instance{}, alerts.ErrStateConflict
	}
	from := instance.State
	instance.State = request.To
	instance.ResolvedAt = timePointerUTC(request.OccurredAt)
	instance.Version++
	result, err := tx.ExecContext(ctx, `UPDATE alert_instances
		SET state=?, resolved_at=?, version=?
		WHERE id=? AND state=? AND version=?`,
		instance.State, timeMillis(request.OccurredAt), instance.Version,
		instance.ID, from, request.ExpectedVersion)
	if err != nil {
		return alerts.Instance{}, fmt.Errorf("update alert transition: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return alerts.Instance{}, fmt.Errorf("update alert transition: %w", err)
	}
	if affected != 1 {
		return alerts.Instance{}, alerts.ErrStateConflict
	}
	event, err := s.recordAlertTransition(ctx, tx, alertTransitionInput{
		Instance: instance,
		From:     from,
		To:       instance.State,
		Context: alertTransitionContext{
			ActorKind:     request.ActorKind,
			ActorID:       request.ActorID,
			Reason:        request.Reason,
			CorrelationID: request.CorrelationID,
			OccurredAt:    request.OccurredAt,
		},
	})
	if err != nil {
		return alerts.Instance{}, err
	}
	if err := recordAlertMutation(ctx, tx, alertMutationInput{
		Scope:       scope,
		Key:         request.IdempotencyKey,
		RequestHash: requestHash,
		Action:      "resolve",
		AlertID:     instance.ID,
		OccurredAt:  request.OccurredAt,
	}); err != nil {
		return alerts.Instance{}, err
	}
	if err := tx.Commit(); err != nil {
		return alerts.Instance{}, fmt.Errorf("commit alert transition: %w", err)
	}
	s.notifyAlertEvents(event)
	return instance, nil
}

func (s *Store) AcknowledgeAlert(
	ctx context.Context, request alerts.AcknowledgeRequest,
) (alerts.Instance, error) {
	if strings.TrimSpace(request.AlertID) == "" || request.ExpectedVersion < 1 ||
		strings.TrimSpace(request.ActorID) == "" ||
		!validAlertIdempotencyKey(request.IdempotencyKey) {
		return alerts.Instance{}, errors.New("alert ID, expected version, and actor are required")
	}
	if request.OccurredAt.IsZero() {
		request.OccurredAt = nowUTC()
	}
	requestHash, err := alertMutationHash(struct {
		ExpectedVersion int    `json:"expected_version"`
		Reason          string `json:"reason"`
	}{ExpectedVersion: request.ExpectedVersion, Reason: request.Reason})
	if err != nil {
		return alerts.Instance{}, err
	}
	scope := alertMutationScope("acknowledge", request.AlertID, request.ActorID)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return alerts.Instance{}, fmt.Errorf("begin alert acknowledgement: %w", err)
	}
	defer tx.Rollback()
	if _, found, err := findAlertMutation(
		ctx, tx, scope, request.IdempotencyKey, requestHash,
	); err != nil {
		return alerts.Instance{}, err
	} else if found {
		instance, err := readAlert(tx.QueryRowContext(ctx,
			`SELECT `+alertColumns+` FROM alert_instances WHERE id=?`, request.AlertID))
		if err != nil {
			return alerts.Instance{}, fmt.Errorf("read idempotent alert acknowledgement: %w", err)
		}
		return instance, nil
	}
	instance, err := readAlert(tx.QueryRowContext(ctx,
		`SELECT `+alertColumns+` FROM alert_instances WHERE id=?`, request.AlertID))
	if errors.Is(err, sql.ErrNoRows) {
		return alerts.Instance{}, alerts.ErrNotFound
	}
	if err != nil {
		return alerts.Instance{}, fmt.Errorf("read alert for acknowledgement: %w", err)
	}
	if instance.Version != request.ExpectedVersion || instance.State == alerts.StateResolved {
		return alerts.Instance{}, alerts.ErrStateConflict
	}
	instance.AcknowledgedAt = timePointerUTC(request.OccurredAt)
	instance.AcknowledgedBy = request.ActorID
	instance.Version++
	result, err := tx.ExecContext(ctx, `UPDATE alert_instances SET
		acknowledged_at=?, acknowledged_by=?, version=?
		WHERE id=? AND version=? AND state!='resolved'`,
		timeMillis(request.OccurredAt), request.ActorID, instance.Version,
		request.AlertID, request.ExpectedVersion)
	if err != nil {
		return alerts.Instance{}, fmt.Errorf("acknowledge alert: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return alerts.Instance{}, fmt.Errorf("acknowledge alert: %w", err)
	}
	if affected != 1 {
		return alerts.Instance{}, alerts.ErrStateConflict
	}
	event, err := s.recordAlertTransition(ctx, tx, alertTransitionInput{
		Instance: instance,
		From:     instance.State,
		To:       instance.State,
		Context: alertTransitionContext{
			ActorKind:     operations.ActorOperator,
			ActorID:       request.ActorID,
			Reason:        request.Reason,
			CorrelationID: request.CorrelationID,
			OccurredAt:    request.OccurredAt,
		},
		EventType: "acknowledged",
	})
	if err != nil {
		return alerts.Instance{}, err
	}
	if err := recordAlertMutation(ctx, tx, alertMutationInput{
		Scope:       scope,
		Key:         request.IdempotencyKey,
		RequestHash: requestHash,
		Action:      "acknowledge",
		AlertID:     instance.ID,
		OccurredAt:  request.OccurredAt,
	}); err != nil {
		return alerts.Instance{}, err
	}
	if err := tx.Commit(); err != nil {
		return alerts.Instance{}, fmt.Errorf("commit alert acknowledgement: %w", err)
	}
	s.notifyAlertEvents(event)
	return instance, nil
}

func (s *Store) SilenceAlert(
	ctx context.Context, request alerts.SilenceRequest,
) (alerts.Instance, alerts.Silence, error) {
	if strings.TrimSpace(request.AlertID) == "" || request.ExpectedVersion < 1 ||
		strings.TrimSpace(request.ActorID) == "" || strings.TrimSpace(request.Reason) == "" ||
		!validAlertIdempotencyKey(request.IdempotencyKey) {
		return alerts.Instance{}, alerts.Silence{}, errors.New("alert, expected version, actor, and reason are required")
	}
	if request.OccurredAt.IsZero() {
		request.OccurredAt = nowUTC()
	}
	if !request.Until.After(request.OccurredAt) {
		return alerts.Instance{}, alerts.Silence{}, errors.New("silence expiry must be in the future")
	}
	if request.Until.After(request.OccurredAt.Add(30 * 24 * time.Hour)) {
		return alerts.Instance{}, alerts.Silence{}, errors.New("silence cannot exceed 30 days")
	}
	requestHash, err := alertMutationHash(struct {
		ExpectedVersion int       `json:"expected_version"`
		Until           time.Time `json:"until"`
		Reason          string    `json:"reason"`
	}{
		ExpectedVersion: request.ExpectedVersion, Until: request.Until.UTC(),
		Reason: request.Reason,
	})
	if err != nil {
		return alerts.Instance{}, alerts.Silence{}, err
	}
	scope := alertMutationScope("silence", request.AlertID, request.ActorID)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return alerts.Instance{}, alerts.Silence{}, fmt.Errorf("begin alert silence: %w", err)
	}
	defer tx.Rollback()
	if resultID, found, err := findAlertMutation(
		ctx, tx, scope, request.IdempotencyKey, requestHash,
	); err != nil {
		return alerts.Instance{}, alerts.Silence{}, err
	} else if found {
		instance, err := readAlert(tx.QueryRowContext(ctx,
			`SELECT `+alertColumns+` FROM alert_instances WHERE id=?`, request.AlertID))
		if err != nil {
			return alerts.Instance{}, alerts.Silence{}, fmt.Errorf("read idempotent alert silence: %w", err)
		}
		silence, err := readAlertSilence(tx.QueryRowContext(ctx, `SELECT id, alert_id,
			starts_at, expires_at, created_at, created_by, reason, correlation_id
			FROM alert_silences WHERE id=?`, resultID))
		if err != nil {
			return alerts.Instance{}, alerts.Silence{}, fmt.Errorf("read idempotent silence: %w", err)
		}
		return instance, silence, nil
	}
	instance, err := readAlert(tx.QueryRowContext(ctx,
		`SELECT `+alertColumns+` FROM alert_instances WHERE id=?`, request.AlertID))
	if errors.Is(err, sql.ErrNoRows) {
		return alerts.Instance{}, alerts.Silence{}, alerts.ErrNotFound
	}
	if err != nil {
		return alerts.Instance{}, alerts.Silence{}, fmt.Errorf("read alert for silence: %w", err)
	}
	if instance.Version != request.ExpectedVersion || instance.State == alerts.StateResolved {
		return alerts.Instance{}, alerts.Silence{}, alerts.ErrStateConflict
	}
	id, err := operations.NewOpaqueID()
	if err != nil {
		return alerts.Instance{}, alerts.Silence{}, err
	}
	silence := alerts.Silence{
		ID: id, AlertID: instance.ID, StartsAt: request.OccurredAt.UTC(),
		ExpiresAt: request.Until.UTC(), CreatedAt: request.OccurredAt.UTC(),
		CreatedBy: request.ActorID, Reason: request.Reason,
		CorrelationID: request.CorrelationID,
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO alert_silences (
		id, alert_id, starts_at, expires_at, created_at, created_by, reason,
		correlation_id
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		silence.ID, silence.AlertID, timeMillis(silence.StartsAt),
		timeMillis(silence.ExpiresAt), timeMillis(silence.CreatedAt),
		silence.CreatedBy, silence.Reason, silence.CorrelationID); err != nil {
		return alerts.Instance{}, alerts.Silence{}, fmt.Errorf("insert alert silence: %w", err)
	}
	instance.SilencedUntil = timePointerUTC(silence.ExpiresAt)
	instance.Version++
	result, err := tx.ExecContext(ctx, `UPDATE alert_instances
		SET silenced_until=?, version=? WHERE id=? AND version=? AND state!='resolved'`,
		timeMillis(silence.ExpiresAt), instance.Version, instance.ID, request.ExpectedVersion)
	if err != nil {
		return alerts.Instance{}, alerts.Silence{}, fmt.Errorf("silence alert: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return alerts.Instance{}, alerts.Silence{}, fmt.Errorf("silence alert: %w", err)
	}
	if affected != 1 {
		return alerts.Instance{}, alerts.Silence{}, alerts.ErrStateConflict
	}
	event, err := s.recordAlertTransition(ctx, tx, alertTransitionInput{
		Instance: instance,
		From:     instance.State,
		To:       instance.State,
		Context: alertTransitionContext{
			ActorKind:     operations.ActorOperator,
			ActorID:       request.ActorID,
			Reason:        request.Reason,
			CorrelationID: request.CorrelationID,
			OccurredAt:    request.OccurredAt,
		},
		EventType: "silenced",
	})
	if err != nil {
		return alerts.Instance{}, alerts.Silence{}, err
	}
	if err := recordAlertMutation(ctx, tx, alertMutationInput{
		Scope:       scope,
		Key:         request.IdempotencyKey,
		RequestHash: requestHash,
		Action:      "silence",
		AlertID:     instance.ID,
		ResultID:    silence.ID,
		OccurredAt:  request.OccurredAt,
	}); err != nil {
		return alerts.Instance{}, alerts.Silence{}, err
	}
	if err := tx.Commit(); err != nil {
		return alerts.Instance{}, alerts.Silence{}, fmt.Errorf("commit alert silence: %w", err)
	}
	s.notifyAlertEvents(event)
	return instance, silence, nil
}

func (s *Store) AddAlertAnnotation(
	ctx context.Context, request alerts.AnnotationRequest,
) (alerts.Annotation, error) {
	request.Body = strings.TrimSpace(request.Body)
	if strings.TrimSpace(request.AlertID) == "" || request.Body == "" ||
		strings.TrimSpace(request.ActorID) == "" ||
		!validAlertIdempotencyKey(request.IdempotencyKey) {
		return alerts.Annotation{}, errors.New("alert, annotation, and actor are required")
	}
	if len(request.Body) > 4000 {
		return alerts.Annotation{}, errors.New("alert annotation cannot exceed 4000 characters")
	}
	if request.OccurredAt.IsZero() {
		request.OccurredAt = nowUTC()
	}
	requestHash, err := alertMutationHash(struct {
		Body string `json:"body"`
	}{Body: request.Body})
	if err != nil {
		return alerts.Annotation{}, err
	}
	scope := alertMutationScope("annotate", request.AlertID, request.ActorID)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return alerts.Annotation{}, fmt.Errorf("begin alert annotation: %w", err)
	}
	defer tx.Rollback()
	if resultID, found, err := findAlertMutation(
		ctx, tx, scope, request.IdempotencyKey, requestHash,
	); err != nil {
		return alerts.Annotation{}, err
	} else if found {
		annotation, err := readAlertAnnotation(tx.QueryRowContext(ctx, `SELECT id,
			alert_id, body, created_at, created_by, correlation_id
			FROM alert_annotations WHERE id=?`, resultID))
		if err != nil {
			return alerts.Annotation{}, fmt.Errorf("read idempotent annotation: %w", err)
		}
		return annotation, nil
	}
	instance, err := readAlert(tx.QueryRowContext(ctx,
		`SELECT `+alertColumns+` FROM alert_instances WHERE id=?`, request.AlertID))
	if errors.Is(err, sql.ErrNoRows) {
		return alerts.Annotation{}, alerts.ErrNotFound
	}
	if err != nil {
		return alerts.Annotation{}, fmt.Errorf("read alert for annotation: %w", err)
	}
	id, err := operations.NewOpaqueID()
	if err != nil {
		return alerts.Annotation{}, err
	}
	annotation := alerts.Annotation{
		ID: id, AlertID: instance.ID, Body: request.Body,
		CreatedAt: request.OccurredAt.UTC(), CreatedBy: request.ActorID,
		CorrelationID: request.CorrelationID,
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO alert_annotations (
		id, alert_id, body, created_at, created_by, correlation_id
	) VALUES (?, ?, ?, ?, ?, ?)`,
		annotation.ID, annotation.AlertID, annotation.Body,
		timeMillis(annotation.CreatedAt), annotation.CreatedBy,
		annotation.CorrelationID); err != nil {
		return alerts.Annotation{}, fmt.Errorf("insert alert annotation: %w", err)
	}
	event, err := s.recordAlertTransition(ctx, tx, alertTransitionInput{
		Instance: instance,
		From:     instance.State,
		To:       instance.State,
		Context: alertTransitionContext{
			ActorKind:     operations.ActorOperator,
			ActorID:       request.ActorID,
			Reason:        "annotation added",
			CorrelationID: request.CorrelationID,
			OccurredAt:    request.OccurredAt,
		},
		EventType: "annotated",
	})
	if err != nil {
		return alerts.Annotation{}, err
	}
	if err := recordAlertMutation(ctx, tx, alertMutationInput{
		Scope:       scope,
		Key:         request.IdempotencyKey,
		RequestHash: requestHash,
		Action:      "annotate",
		AlertID:     instance.ID,
		ResultID:    annotation.ID,
		OccurredAt:  request.OccurredAt,
	}); err != nil {
		return alerts.Annotation{}, err
	}
	if err := tx.Commit(); err != nil {
		return alerts.Annotation{}, fmt.Errorf("commit alert annotation: %w", err)
	}
	s.notifyAlertEvents(event)
	return annotation, nil
}

type alertTransitionContext struct {
	ActorKind     operations.ActorKind
	ActorID       string
	Reason        string
	CorrelationID string
	OccurredAt    time.Time
}

type alertTransitionInput struct {
	Instance  alerts.Instance
	From      alerts.State
	To        alerts.State
	Context   alertTransitionContext
	Notify    bool
	EventType string
}

type alertMutationInput struct {
	Scope       string
	Key         string
	RequestHash string
	Action      string
	AlertID     string
	ResultID    string
	OccurredAt  time.Time
}

func (s *Store) recordAlertTransition(
	ctx context.Context, tx *sql.Tx, input alertTransitionInput,
) (operations.Event, error) {
	instance := input.Instance
	from := input.From
	to := input.To
	action := input.Context
	transitionID, err := operations.NewOpaqueID()
	if err != nil {
		return operations.Event{}, err
	}
	detail, err := json.Marshal(map[string]any{
		"rule_id": instance.RuleID, "severity": instance.Severity,
		"occurrence_count": instance.OccurrenceCount,
	})
	if err != nil {
		return operations.Event{}, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO alert_transitions (
		id, alert_id, from_state, to_state, occurred_at, actor_kind, actor_id,
		reason, detail_json, correlation_id
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		transitionID, instance.ID, from, to, timeMillis(action.OccurredAt),
		action.ActorKind, action.ActorID, action.Reason, detail, action.CorrelationID)
	if err != nil {
		return operations.Event{}, fmt.Errorf("insert alert transition: %w", err)
	}
	auditID, err := operations.NewOpaqueID()
	if err != nil {
		return operations.Event{}, err
	}
	auditPayload, err := json.Marshal(map[string]any{
		"from": from, "to": to, "reason": action.Reason, "version": instance.Version,
	})
	if err != nil {
		return operations.Event{}, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO alert_audit_events (
		id, alert_id, occurred_at, actor_kind, actor_id, action, correlation_id,
		payload_json
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		auditID, instance.ID, timeMillis(action.OccurredAt), action.ActorKind,
		action.ActorID, "alert."+alertEventType(input.EventType, to),
		action.CorrelationID, auditPayload)
	if err != nil {
		return operations.Event{}, fmt.Errorf("insert alert audit event: %w", err)
	}
	if input.Notify && to == alerts.StateOpen && !alertSilenced(instance, action.OccurredAt) {
		enqueued, err := enqueueAlertDeliveries(
			ctx, tx, instance, transitionID, action.OccurredAt,
		)
		if err != nil {
			return operations.Event{}, err
		}
		if enqueued {
			if _, err := tx.ExecContext(ctx, `UPDATE alert_instances
				SET last_notified_at=? WHERE id=?`,
				timeMillis(action.OccurredAt), instance.ID); err != nil {
				return operations.Event{}, fmt.Errorf("mark alert notified: %w", err)
			}
		}
	}
	epochID := s.commandEpochID()
	if epochID == "" {
		return operations.Event{}, nil
	}
	eventPayload, err := json.Marshal(map[string]any{
		"rule_id": instance.RuleID, "state": to, "previous_state": from,
		"severity": instance.Severity, "summary": instance.Summary,
		"occurrence_count": instance.OccurrenceCount,
	})
	if err != nil {
		return operations.Event{}, err
	}
	event, err := appendOperationalEventTx(ctx, tx, epochID, operations.EventInput{
		Type: "alert." + alertEventType(input.EventType, to), EntityType: "alert",
		EntityID: instance.ID, Timestamp: action.OccurredAt,
		CorrelationID: action.CorrelationID, ActorKind: action.ActorKind,
		ActorID: action.ActorID, Payload: eventPayload,
	})
	if err != nil {
		return operations.Event{}, fmt.Errorf("append alert event: %w", err)
	}
	return event, nil
}

func enqueueAlertDeliveries(
	ctx context.Context, tx *sql.Tx, instance alerts.Instance,
	transitionID string, at time.Time,
) (bool, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT id FROM notification_endpoints WHERE enabled=1 ORDER BY id`)
	if err != nil {
		return false, fmt.Errorf("list notification endpoints: %w", err)
	}
	var endpointIDs []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return false, fmt.Errorf("scan notification endpoint: %w", err)
		}
		endpointIDs = append(endpointIDs, id)
	}
	if err := rows.Close(); err != nil {
		return false, fmt.Errorf("close notification endpoint rows: %w", err)
	}
	if err := rows.Err(); err != nil {
		return false, fmt.Errorf("read notification endpoint rows: %w", err)
	}
	payload, err := json.Marshal(map[string]any{
		"alert_id": instance.ID, "rule_id": instance.RuleID,
		"state": instance.State, "severity": instance.Severity,
		"summary": instance.Summary, "occurrence_count": instance.OccurrenceCount,
		"observed_at": instance.LastObservedAt,
	})
	if err != nil {
		return false, err
	}
	for _, endpointID := range endpointIDs {
		id, err := operations.NewOpaqueID()
		if err != nil {
			return false, err
		}
		dedupKey := transitionID + ":" + endpointID
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO notification_deliveries (
			id, alert_id, transition_id, endpoint_id, dedup_key, payload_json,
			state, next_attempt_at, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, 'pending', ?, ?, ?)`,
			id, instance.ID, transitionID, endpointID, dedupKey, payload,
			timeMillis(at), timeMillis(at), timeMillis(at)); err != nil {
			return false, fmt.Errorf("enqueue notification delivery: %w", err)
		}
	}
	return len(endpointIDs) > 0, nil
}

func alertEventType(eventType string, state alerts.State) string {
	if eventType != "" {
		return eventType
	}
	return string(state)
}

func alertCooldownElapsed(last *time.Time, at time.Time, cooldown time.Duration) bool {
	return last == nil || cooldown <= 0 || !at.Before(last.Add(cooldown))
}

func alertSilenced(instance alerts.Instance, at time.Time) bool {
	return instance.SilencedUntil != nil && at.Before(*instance.SilencedUntil)
}

func (s *Store) notifyAlertEvents(events ...operations.Event) {
	for _, event := range events {
		if event.ID != "" {
			s.notifyOperationalEvent(event)
		}
	}
}

func readAlert(row rowScanner) (alerts.Instance, error) {
	return scanAlert(row)
}

func scanAlert(row rowScanner) (alerts.Instance, error) {
	var instance alerts.Instance
	var details []byte
	var firstObserved, lastObserved, dueAt int64
	var openedAt, resolvedAt, acknowledgedAt, silencedUntil, lastNotified sql.NullInt64
	err := row.Scan(
		&instance.ID, &instance.RuleID, &instance.DedupKey, &instance.State,
		&instance.Severity, &instance.Summary, &details, &instance.SourceEventID,
		&firstObserved, &lastObserved, &dueAt, &openedAt, &resolvedAt,
		&acknowledgedAt, &instance.AcknowledgedBy, &silencedUntil, &lastNotified,
		&instance.OccurrenceCount, &instance.Version,
	)
	if err != nil {
		return alerts.Instance{}, err
	}
	instance.Details = append(json.RawMessage(nil), details...)
	instance.FirstObservedAt = millisTime(firstObserved)
	instance.LastObservedAt = millisTime(lastObserved)
	instance.DueAt = millisTime(dueAt)
	instance.OpenedAt = pointerTime(openedAt)
	instance.ResolvedAt = pointerTime(resolvedAt)
	instance.AcknowledgedAt = pointerTime(acknowledgedAt)
	instance.SilencedUntil = pointerTime(silencedUntil)
	instance.LastNotifiedAt = pointerTime(lastNotified)
	return instance, nil
}

func validateAlertState(state alerts.State) error {
	switch state {
	case alerts.StatePending, alerts.StateOpen, alerts.StateResolved:
		return nil
	default:
		return fmt.Errorf("invalid alert state %q", strings.TrimSpace(string(state)))
	}
}

func validAlertIdempotencyKey(key string) bool {
	key = strings.TrimSpace(key)
	return key != "" && len(key) <= 128
}

func alertMutationScope(action, alertID, actorID string) string {
	return strings.Join([]string{action, alertID, actorID}, "\x00")
}

func alertMutationHash(value any) (string, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return "", fmt.Errorf("encode alert mutation: %w", err)
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:]), nil
}

func findAlertMutation(
	ctx context.Context, tx *sql.Tx, scope, key, requestHash string,
) (string, bool, error) {
	var existingHash, resultID string
	err := tx.QueryRowContext(ctx, `SELECT request_hash, result_id
		FROM alert_mutations WHERE idempotency_scope=? AND idempotency_key=?`,
		scope, strings.TrimSpace(key)).Scan(&existingHash, &resultID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("read alert idempotency record: %w", err)
	}
	if existingHash != requestHash {
		return "", false, alerts.ErrIdempotencyConflict
	}
	return resultID, true, nil
}

func recordAlertMutation(
	ctx context.Context, tx *sql.Tx, input alertMutationInput,
) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO alert_mutations (
		idempotency_scope, idempotency_key, request_hash, action, alert_id,
		result_id, created_at
	) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		input.Scope, strings.TrimSpace(input.Key), input.RequestHash, input.Action,
		input.AlertID, input.ResultID, timeMillis(input.OccurredAt))
	if err != nil {
		return fmt.Errorf("record alert idempotency: %w", err)
	}
	return nil
}

func readAlertSilence(row rowScanner) (alerts.Silence, error) {
	var silence alerts.Silence
	var startsAt, expiresAt, createdAt int64
	err := row.Scan(
		&silence.ID, &silence.AlertID, &startsAt, &expiresAt, &createdAt,
		&silence.CreatedBy, &silence.Reason, &silence.CorrelationID,
	)
	if err != nil {
		return alerts.Silence{}, err
	}
	silence.StartsAt = millisTime(startsAt)
	silence.ExpiresAt = millisTime(expiresAt)
	silence.CreatedAt = millisTime(createdAt)
	return silence, nil
}

func readAlertAnnotation(row rowScanner) (alerts.Annotation, error) {
	var annotation alerts.Annotation
	var createdAt int64
	err := row.Scan(
		&annotation.ID, &annotation.AlertID, &annotation.Body, &createdAt,
		&annotation.CreatedBy, &annotation.CorrelationID,
	)
	if err != nil {
		return alerts.Annotation{}, err
	}
	annotation.CreatedAt = millisTime(createdAt)
	return annotation, nil
}
