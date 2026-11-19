package history

import (
	"context"
	"fmt"
)

type DatabaseHealth struct {
	QuickCheck      string `json:"quick_check"`
	MigrationCount  int    `json:"migration_count"`
	LatestMigration int    `json:"latest_migration"`
	VerifiedEpochs  int    `json:"verified_epochs"`
	VerifiedEvents  int64  `json:"verified_events"`
	CompactedEvents int64  `json:"compacted_events"`
}

func (s *Store) DatabaseHealth(ctx context.Context) (DatabaseHealth, error) {
	var health DatabaseHealth
	if err := s.db.QueryRowContext(ctx, `PRAGMA quick_check`).Scan(&health.QuickCheck); err != nil {
		return DatabaseHealth{}, fmt.Errorf("run SQLite quick check: %w", err)
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(MAX(version), 0)
		FROM schema_migrations`).Scan(&health.MigrationCount, &health.LatestMigration); err != nil {
		return DatabaseHealth{}, fmt.Errorf("read migration health: %w", err)
	}
	verification, err := verifyOperationalChains(ctx, s.db)
	if err != nil {
		return DatabaseHealth{}, err
	}
	health.VerifiedEpochs = verification.Epochs
	health.VerifiedEvents = verification.Events
	health.CompactedEvents = verification.CompactedEvents
	return health, nil
}
