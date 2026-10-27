package history

import (
	"context"
	"fmt"
	"time"
)

type AlertPruneResult struct {
	Alerts     int64
	Deliveries int64
}

func (s *Store) PruneAlertHistory(
	ctx context.Context, before time.Time,
) (AlertPruneResult, error) {
	if before.IsZero() {
		return AlertPruneResult{}, fmt.Errorf("alert retention cutoff is required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return AlertPruneResult{}, fmt.Errorf("begin alert retention: %w", err)
	}
	defer tx.Rollback()
	result := AlertPruneResult{}
	deliveryResult, err := tx.ExecContext(ctx, `DELETE FROM notification_deliveries
		WHERE state IN ('succeeded', 'dead_letter') AND updated_at<?`,
		timeMillis(before))
	if err != nil {
		return AlertPruneResult{}, fmt.Errorf("prune terminal notification deliveries: %w", err)
	}
	result.Deliveries, err = deliveryResult.RowsAffected()
	if err != nil {
		return AlertPruneResult{}, fmt.Errorf("count pruned notification deliveries: %w", err)
	}

	rows, err := tx.QueryContext(ctx, `SELECT id FROM alert_instances
		WHERE state='resolved' AND resolved_at IS NOT NULL AND resolved_at<?
		ORDER BY resolved_at, id`, timeMillis(before))
	if err != nil {
		return AlertPruneResult{}, fmt.Errorf("list expired alerts: %w", err)
	}
	var alertIDs []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return AlertPruneResult{}, fmt.Errorf("scan expired alert: %w", err)
		}
		alertIDs = append(alertIDs, id)
	}
	if err := rows.Close(); err != nil {
		return AlertPruneResult{}, fmt.Errorf("close expired alert rows: %w", err)
	}
	if err := rows.Err(); err != nil {
		return AlertPruneResult{}, fmt.Errorf("read expired alert rows: %w", err)
	}
	for _, alertID := range alertIDs {
		for _, statement := range []string{
			`DELETE FROM notification_deliveries WHERE alert_id=?`,
			`DELETE FROM alert_annotations WHERE alert_id=?`,
			`DELETE FROM alert_silences WHERE alert_id=?`,
			`DELETE FROM alert_audit_events WHERE alert_id=?`,
			`DELETE FROM alert_mutations WHERE alert_id=?`,
			`DELETE FROM alert_evaluated_events WHERE alert_id=?`,
			`DELETE FROM alert_transitions WHERE alert_id=?`,
			`DELETE FROM alert_instances WHERE id=?`,
		} {
			deleted, err := tx.ExecContext(ctx, statement, alertID)
			if err != nil {
				return AlertPruneResult{}, fmt.Errorf("prune alert %s: %w", alertID, err)
			}
			if statement == `DELETE FROM notification_deliveries WHERE alert_id=?` {
				count, err := deleted.RowsAffected()
				if err != nil {
					return AlertPruneResult{}, fmt.Errorf("count pruned alert deliveries: %w", err)
				}
				result.Deliveries += count
			}
		}
		result.Alerts++
	}
	if err := tx.Commit(); err != nil {
		return AlertPruneResult{}, fmt.Errorf("commit alert retention: %w", err)
	}
	return result, nil
}
