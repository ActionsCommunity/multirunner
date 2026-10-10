package history

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/GerardSmit/multirunner/internal/restore"
)

const restoreColumns = `id, command_id, backup_id, state, created_at, updated_at,
	activated_at, completed_at, host_id, schema_version, size_bytes, sha256,
	database_path, staged_path, rollback_path, handoff_path, quick_check,
	rollback_reason, error`

func (s *Store) CreateRestore(ctx context.Context, metadata restore.Metadata) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO restores (
		id, command_id, backup_id, state, created_at, updated_at, activated_at,
		completed_at, host_id, schema_version, size_bytes, sha256, database_path,
		staged_path, rollback_path, handoff_path, quick_check, rollback_reason, error
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		metadata.ID, metadata.CommandID, metadata.BackupID, metadata.State,
		timeMillis(metadata.CreatedAt), timeMillis(metadata.UpdatedAt),
		nullableTime(optionalRestoreTime(metadata.ActivatedAt)),
		nullableTime(optionalRestoreTime(metadata.CompletedAt)),
		metadata.HostID, metadata.SchemaVersion, metadata.SizeBytes, metadata.SHA256,
		metadata.DatabasePath, metadata.StagedPath, metadata.RollbackPath,
		metadata.HandoffPath, metadata.QuickCheck, metadata.RollbackReason, metadata.Error)
	if err != nil {
		return fmt.Errorf("create restore metadata: %w", err)
	}
	return nil
}

func (s *Store) UpdateRestore(ctx context.Context, metadata restore.Metadata) error {
	result, err := s.db.ExecContext(ctx, `UPDATE restores SET
		backup_id=?, state=?, updated_at=?, activated_at=?, completed_at=?,
		host_id=?, schema_version=?, size_bytes=?, sha256=?, database_path=?,
		staged_path=?, rollback_path=?, handoff_path=?, quick_check=?,
		rollback_reason=?, error=? WHERE id=?`,
		metadata.BackupID, metadata.State, timeMillis(metadata.UpdatedAt),
		nullableTime(optionalRestoreTime(metadata.ActivatedAt)),
		nullableTime(optionalRestoreTime(metadata.CompletedAt)),
		metadata.HostID, metadata.SchemaVersion, metadata.SizeBytes, metadata.SHA256,
		metadata.DatabasePath, metadata.StagedPath, metadata.RollbackPath,
		metadata.HandoffPath, metadata.QuickCheck, metadata.RollbackReason,
		metadata.Error, metadata.ID)
	if err != nil {
		return fmt.Errorf("update restore metadata: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("update restore metadata: %w", err)
	}
	if affected != 1 {
		return restore.ErrNotFound
	}
	return nil
}

func (s *Store) Restore(ctx context.Context, id string) (restore.Metadata, error) {
	metadata, err := scanRestore(s.db.QueryRowContext(ctx,
		`SELECT `+restoreColumns+` FROM restores WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return restore.Metadata{}, restore.ErrNotFound
	}
	if err != nil {
		return restore.Metadata{}, fmt.Errorf("read restore metadata: %w", err)
	}
	return metadata, nil
}

func (s *Store) ListRestores(ctx context.Context, limit int) ([]restore.Metadata, error) {
	if limit <= 0 {
		limit = 100
	}
	if limit > 500 {
		limit = 500
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+restoreColumns+`
		FROM restores ORDER BY created_at DESC, id LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("list restores: %w", err)
	}
	defer rows.Close()
	var items []restore.Metadata
	for rows.Next() {
		metadata, err := scanRestore(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, metadata)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list restores: %w", err)
	}
	return items, nil
}

func scanRestore(row rowScanner) (restore.Metadata, error) {
	var metadata restore.Metadata
	var createdAt, updatedAt int64
	var activatedAt, completedAt sql.NullInt64
	err := row.Scan(
		&metadata.ID, &metadata.CommandID, &metadata.BackupID, &metadata.State,
		&createdAt, &updatedAt, &activatedAt, &completedAt, &metadata.HostID,
		&metadata.SchemaVersion, &metadata.SizeBytes, &metadata.SHA256,
		&metadata.DatabasePath, &metadata.StagedPath, &metadata.RollbackPath,
		&metadata.HandoffPath, &metadata.QuickCheck, &metadata.RollbackReason,
		&metadata.Error,
	)
	if err != nil {
		return restore.Metadata{}, err
	}
	metadata.CreatedAt = millisTime(createdAt)
	metadata.UpdatedAt = millisTime(updatedAt)
	metadata.ActivatedAt = nullableMillisTime(activatedAt)
	metadata.CompletedAt = nullableMillisTime(completedAt)
	return metadata, nil
}

func optionalRestoreTime(value time.Time) *time.Time {
	if value.IsZero() {
		return nil
	}
	return &value
}
