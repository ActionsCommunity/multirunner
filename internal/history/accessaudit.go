package history

import (
	"context"
	"errors"
	"fmt"

	"github.com/GerardSmit/multirunner/internal/operations"
)

func (s *Store) RecordAccessAudit(ctx context.Context, audit AccessAudit) error {
	if audit.ID == "" {
		id, err := operations.NewOpaqueID()
		if err != nil {
			return err
		}
		audit.ID = id
	}
	if audit.OccurredAt.IsZero() || audit.ActorKind == "" || audit.ActorID == "" ||
		audit.Action == "" || audit.TargetType == "" || audit.TargetID == "" ||
		audit.Outcome == "" {
		return errors.New("access audit is incomplete")
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO access_audit_events (
		id, occurred_at, actor_kind, actor_id, action, target_type, target_id,
		correlation_id, outcome, byte_count, duration_ms, error_code
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		audit.ID, timeMillis(audit.OccurredAt), audit.ActorKind, audit.ActorID,
		audit.Action, audit.TargetType, audit.TargetID, audit.CorrelationID,
		audit.Outcome, audit.ByteCount, audit.Duration.Milliseconds(), audit.ErrorCode)
	if err != nil {
		return fmt.Errorf("record access audit: %w", err)
	}
	return nil
}
