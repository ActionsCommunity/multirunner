package history

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

const minimumAuditRetention = 365 * 24 * time.Hour

type TieredRetentionPolicy struct {
	Operational time.Duration
	Health      time.Duration
	Lifecycle   time.Duration
	Audit       time.Duration
}

func DefaultTieredRetentionPolicy() TieredRetentionPolicy {
	return TieredRetentionPolicy{
		Operational: 90 * 24 * time.Hour,
		Health:      30 * 24 * time.Hour,
		Lifecycle:   90 * 24 * time.Hour,
		Audit:       minimumAuditRetention,
	}
}

type TieredPruneResult struct {
	OperationalEvents  int64
	HealthSnapshots    int64
	RunnerSessions     int64
	WebhookDeliveries  int64
	CommandTransitions int64
	AuditEvents        int64
	AccessAuditEvents  int64
	Commands           int64
	Restores           int64
	Updates            int64
}

func (s *Store) PruneTiered(
	ctx context.Context, at time.Time, policy TieredRetentionPolicy,
) (TieredPruneResult, error) {
	if at.IsZero() {
		return TieredPruneResult{}, errors.New("tiered retention time is required")
	}
	policy = normalizeTieredRetentionPolicy(policy)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return TieredPruneResult{}, fmt.Errorf("begin tiered retention: %w", err)
	}
	defer tx.Rollback()

	var result TieredPruneResult
	if result.OperationalEvents, err = compactOperationalEvents(
		ctx, tx, at.Add(-policy.Operational),
	); err != nil {
		return TieredPruneResult{}, err
	}
	lifecycleCutoff := timeMillis(at.Add(-policy.Lifecycle))
	auditCutoff := timeMillis(at.Add(-policy.Audit))
	deletions := []struct {
		name  string
		query string
		args  []any
		dest  *int64
	}{
		{"health snapshots", `DELETE FROM health_snapshots WHERE observed_at<?`,
			[]any{timeMillis(at.Add(-policy.Health))}, &result.HealthSnapshots},
		{"runner sessions", `DELETE FROM runner_sessions
			WHERE completed_at IS NOT NULL AND completed_at<?`,
			[]any{lifecycleCutoff}, &result.RunnerSessions},
		{"webhook deliveries", `DELETE FROM webhook_deliveries WHERE received_at<?`,
			[]any{lifecycleCutoff}, &result.WebhookDeliveries},
		{"command transitions", `DELETE FROM command_transitions WHERE occurred_at<?`,
			[]any{auditCutoff}, &result.CommandTransitions},
		{"audit events", `DELETE FROM audit_events WHERE occurred_at<?`,
			[]any{auditCutoff}, &result.AuditEvents},
		{"access audit events", `DELETE FROM access_audit_events WHERE occurred_at<?`,
			[]any{auditCutoff}, &result.AccessAuditEvents},
		{"restores", `DELETE FROM restores
			WHERE completed_at IS NOT NULL AND completed_at<?
			AND state IN ('succeeded', 'rolled_back', 'failed')`,
			[]any{auditCutoff}, &result.Restores},
		{"updates", `DELETE FROM updates
			WHERE completed_at IS NOT NULL AND completed_at<?
			AND state IN ('succeeded', 'rolled_back', 'failed')`,
			[]any{auditCutoff}, &result.Updates},
		{"commands", `DELETE FROM commands
			WHERE completed_at IS NOT NULL AND completed_at<?
			AND state IN ('succeeded', 'failed', 'cancelled', 'interrupted')
			AND NOT EXISTS (
				SELECT 1 FROM support_bundles b WHERE b.command_id=commands.id
			) AND NOT EXISTS (
				SELECT 1 FROM command_transitions t WHERE t.command_id=commands.id
			) AND NOT EXISTS (
				SELECT 1 FROM audit_events a WHERE a.command_id=commands.id
			)`, []any{auditCutoff}, &result.Commands},
	}
	for _, deletion := range deletions {
		execResult, execErr := tx.ExecContext(ctx, deletion.query, deletion.args...)
		if execErr != nil {
			return TieredPruneResult{}, fmt.Errorf(
				"prune tiered %s: %w", deletion.name, execErr)
		}
		if *deletion.dest, execErr = execResult.RowsAffected(); execErr != nil {
			return TieredPruneResult{}, fmt.Errorf(
				"count pruned tiered %s: %w", deletion.name, execErr)
		}
	}
	if err := tx.Commit(); err != nil {
		return TieredPruneResult{}, fmt.Errorf("commit tiered retention: %w", err)
	}
	return result, nil
}

func normalizeTieredRetentionPolicy(
	policy TieredRetentionPolicy,
) TieredRetentionPolicy {
	defaults := DefaultTieredRetentionPolicy()
	if policy.Operational <= 0 {
		policy.Operational = defaults.Operational
	}
	if policy.Health <= 0 {
		policy.Health = defaults.Health
	}
	if policy.Lifecycle <= 0 {
		policy.Lifecycle = defaults.Lifecycle
	}
	if policy.Audit < minimumAuditRetention {
		policy.Audit = minimumAuditRetention
	}
	return policy
}

func compactOperationalEvents(
	ctx context.Context, tx *sql.Tx, cutoff time.Time,
) (int64, error) {
	rows, err := tx.QueryContext(ctx, `SELECT h.id, h.last_sequence,
		COALESCE(a.through_sequence, 0)
		FROM host_epochs h
		LEFT JOIN operational_event_anchors a ON a.host_epoch=h.id
		ORDER BY h.id`)
	if err != nil {
		return 0, fmt.Errorf("list operational epochs for retention: %w", err)
	}
	type epoch struct {
		id            string
		last, through int64
	}
	var epochs []epoch
	for rows.Next() {
		var id string
		var last, through int64
		if err := rows.Scan(&id, &last, &through); err != nil {
			_ = rows.Close()
			return 0, fmt.Errorf("scan operational epoch for retention: %w", err)
		}
		epochs = append(epochs, epoch{id: id, last: last, through: through})
	}
	if err := rows.Close(); err != nil {
		return 0, fmt.Errorf("close operational retention epochs: %w", err)
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("read operational retention epochs: %w", err)
	}

	var deleted int64
	for _, epoch := range epochs {
		candidate := epoch.last
		var minimumRecent sql.NullInt64
		if err := tx.QueryRowContext(ctx, `SELECT MIN(sequence)
			FROM operational_events WHERE host_epoch=? AND timestamp>=?`,
			epoch.id, timeMillis(cutoff)).Scan(&minimumRecent); err != nil {
			return 0, fmt.Errorf("read operational retention cutoff: %w", err)
		}
		if minimumRecent.Valid && minimumRecent.Int64-1 < candidate {
			candidate = minimumRecent.Int64 - 1
		}
		var minimumReference sql.NullInt64
		if err := tx.QueryRowContext(ctx, `SELECT MIN(sequence) FROM (
			SELECT sequence FROM projection_watermarks WHERE host_epoch=?
			UNION ALL
			SELECT sequence FROM runner_state WHERE host_epoch=?
		)`, epoch.id, epoch.id).Scan(&minimumReference); err != nil {
			return 0, fmt.Errorf("read operational retention references: %w", err)
		}
		if minimumReference.Valid && minimumReference.Int64-1 < candidate {
			candidate = minimumReference.Int64 - 1
		}
		if candidate <= epoch.through {
			continue
		}
		var integrityHash string
		if err := tx.QueryRowContext(ctx, `SELECT integrity_hash
			FROM operational_events WHERE host_epoch=? AND sequence=?`,
			epoch.id, candidate).Scan(&integrityHash); err != nil {
			return 0, fmt.Errorf("read operational compaction anchor: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO operational_event_anchors (
			host_epoch, through_sequence, integrity_hash, compacted_at
		) VALUES (?, ?, ?, ?)
		ON CONFLICT(host_epoch) DO UPDATE SET
			through_sequence=excluded.through_sequence,
			integrity_hash=excluded.integrity_hash,
			compacted_at=excluded.compacted_at`,
			epoch.id, candidate, integrityHash, timeMillis(nowUTC())); err != nil {
			return 0, fmt.Errorf("write operational compaction anchor: %w", err)
		}
		execResult, err := tx.ExecContext(ctx, `DELETE FROM operational_events
			WHERE host_epoch=? AND sequence<=?`, epoch.id, candidate)
		if err != nil {
			return 0, fmt.Errorf("compact operational events: %w", err)
		}
		count, err := execResult.RowsAffected()
		if err != nil {
			return 0, fmt.Errorf("count compacted operational events: %w", err)
		}
		deleted += count
	}
	return deleted, nil
}
