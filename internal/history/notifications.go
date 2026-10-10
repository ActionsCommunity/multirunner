package history

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/GerardSmit/multirunner/internal/alerts"
)

const deliveryColumns = `id, alert_id, transition_id, endpoint_id, dedup_key,
	payload_json, state, attempt, next_attempt_at, lease_owner, lease_expires_at,
	last_error, created_at, updated_at, delivered_at, dead_lettered_at`

func (s *Store) SyncNotificationEndpoints(
	ctx context.Context, endpoints []alerts.Endpoint, at time.Time,
) error {
	if at.IsZero() {
		at = nowUTC()
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin notification endpoint sync: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `UPDATE notification_endpoints
		SET enabled=0, updated_at=?`, timeMillis(at)); err != nil {
		return fmt.Errorf("disable notification endpoints: %w", err)
	}
	seen := make(map[string]struct{}, len(endpoints))
	for _, endpoint := range endpoints {
		if strings.TrimSpace(endpoint.ID) == "" || strings.TrimSpace(endpoint.Name) == "" ||
			strings.TrimSpace(endpoint.Kind) == "" || strings.TrimSpace(endpoint.ConfigRef) == "" {
			return errors.New("notification endpoint ID, name, kind, and config reference are required")
		}
		if _, exists := seen[endpoint.ID]; exists {
			return fmt.Errorf("duplicate notification endpoint ID %q", endpoint.ID)
		}
		seen[endpoint.ID] = struct{}{}
		_, err := tx.ExecContext(ctx, `INSERT INTO notification_endpoints (
			id, name, kind, config_ref, enabled, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET name=excluded.name, kind=excluded.kind,
			config_ref=excluded.config_ref, enabled=excluded.enabled,
			updated_at=excluded.updated_at`,
			endpoint.ID, endpoint.Name, endpoint.Kind, endpoint.ConfigRef,
			boolInt(endpoint.Enabled), timeMillis(at), timeMillis(at))
		if err != nil {
			return fmt.Errorf("sync notification endpoint %q: %w", endpoint.Name, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit notification endpoint sync: %w", err)
	}
	return nil
}

func (s *Store) ClaimNotificationDelivery(
	ctx context.Context, owner string, at time.Time, lease time.Duration,
) (alerts.Delivery, error) {
	if strings.TrimSpace(owner) == "" || lease <= 0 {
		return alerts.Delivery{}, errors.New("delivery owner and positive lease are required")
	}
	if at.IsZero() {
		at = nowUTC()
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return alerts.Delivery{}, fmt.Errorf("begin notification delivery claim: %w", err)
	}
	defer tx.Rollback()
	var id string
	err = tx.QueryRowContext(ctx, `SELECT id FROM notification_deliveries
		WHERE next_attempt_at<=? AND (
			state IN ('pending', 'retry') OR
			(state='claimed' AND lease_expires_at IS NOT NULL AND lease_expires_at<=?)
		)
		ORDER BY next_attempt_at, created_at, id LIMIT 1`,
		timeMillis(at), timeMillis(at)).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return alerts.Delivery{}, alerts.ErrNotFound
	}
	if err != nil {
		return alerts.Delivery{}, fmt.Errorf("select notification delivery: %w", err)
	}
	result, err := tx.ExecContext(ctx, `UPDATE notification_deliveries SET
		state='claimed', attempt=attempt+1, lease_owner=?, lease_expires_at=?,
		updated_at=? WHERE id=? AND (
			state IN ('pending', 'retry') OR
			(state='claimed' AND lease_expires_at IS NOT NULL AND lease_expires_at<=?)
		)`,
		owner, timeMillis(at.Add(lease)), timeMillis(at), id, timeMillis(at))
	if err != nil {
		return alerts.Delivery{}, fmt.Errorf("claim notification delivery: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return alerts.Delivery{}, fmt.Errorf("claim notification delivery: %w", err)
	}
	if affected != 1 {
		return alerts.Delivery{}, alerts.ErrStateConflict
	}
	delivery, err := readDelivery(tx.QueryRowContext(ctx,
		`SELECT `+deliveryColumns+` FROM notification_deliveries WHERE id=?`, id))
	if err != nil {
		return alerts.Delivery{}, fmt.Errorf("read claimed notification delivery: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return alerts.Delivery{}, fmt.Errorf("commit notification delivery claim: %w", err)
	}
	return delivery, nil
}

func (s *Store) CompleteNotificationDelivery(
	ctx context.Context, id, owner string, at time.Time,
) error {
	if at.IsZero() {
		at = nowUTC()
	}
	result, err := s.db.ExecContext(ctx, `UPDATE notification_deliveries SET
		state='succeeded', delivered_at=?, updated_at=?, lease_owner='',
		lease_expires_at=NULL, last_error=''
		WHERE id=? AND state='claimed' AND lease_owner=?`,
		timeMillis(at), timeMillis(at), id, owner)
	if err != nil {
		return fmt.Errorf("complete notification delivery: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("complete notification delivery: %w", err)
	}
	if affected != 1 {
		return alerts.ErrStateConflict
	}
	return nil
}

func (s *Store) FailNotificationDelivery(
	ctx context.Context, id, owner, errorText string, at, next time.Time, dead bool,
) error {
	if at.IsZero() {
		at = nowUTC()
	}
	errorText = strings.TrimSpace(errorText)
	if len(errorText) > 1000 {
		errorText = errorText[:1000]
	}
	state := alerts.DeliveryRetry
	var deadAt any
	if dead {
		state = alerts.DeliveryDeadLetter
		deadAt = timeMillis(at)
		next = at
	} else if !next.After(at) {
		return errors.New("retry time must be after failure time")
	}
	result, err := s.db.ExecContext(ctx, `UPDATE notification_deliveries SET
		state=?, next_attempt_at=?, updated_at=?, lease_owner='',
		lease_expires_at=NULL, last_error=?, dead_lettered_at=?
		WHERE id=? AND state='claimed' AND lease_owner=?`,
		state, timeMillis(next), timeMillis(at), errorText, deadAt, id, owner)
	if err != nil {
		return fmt.Errorf("fail notification delivery: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("fail notification delivery: %w", err)
	}
	if affected != 1 {
		return alerts.ErrStateConflict
	}
	return nil
}

func readDelivery(row rowScanner) (alerts.Delivery, error) {
	var delivery alerts.Delivery
	var payload []byte
	var nextAttempt, createdAt, updatedAt int64
	var leaseExpires, deliveredAt, deadAt sql.NullInt64
	err := row.Scan(
		&delivery.ID, &delivery.AlertID, &delivery.TransitionID,
		&delivery.EndpointID, &delivery.DedupKey, &payload, &delivery.State,
		&delivery.Attempt, &nextAttempt, &delivery.LeaseOwner, &leaseExpires,
		&delivery.LastError, &createdAt, &updatedAt, &deliveredAt, &deadAt,
	)
	if err != nil {
		return alerts.Delivery{}, err
	}
	delivery.Payload = append(delivery.Payload[:0], payload...)
	delivery.NextAttemptAt = millisTime(nextAttempt)
	delivery.CreatedAt = millisTime(createdAt)
	delivery.UpdatedAt = millisTime(updatedAt)
	delivery.LeaseExpiresAt = pointerTime(leaseExpires)
	delivery.DeliveredAt = pointerTime(deliveredAt)
	delivery.DeadLetteredAt = pointerTime(deadAt)
	return delivery, nil
}
