package history

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/GerardSmit/multirunner/internal/update"
)

const updateColumns = `id, command_id, state, created_at, updated_at,
	activated_at, completed_at, version, commit_hash, host_id, target_path,
	size_bytes, sha256, schema_min, schema_max, api_version, artifact_path,
	handoff_path, rollback_reason, error`

func (s *Store) CreateUpdate(ctx context.Context, metadata update.Metadata) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO updates (
		id, command_id, state, created_at, updated_at, activated_at, completed_at,
		version, commit_hash, host_id, target_path, size_bytes, sha256,
		schema_min, schema_max, api_version, artifact_path, handoff_path,
		rollback_reason, error
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		metadata.ID, metadata.CommandID, metadata.State,
		timeMillis(metadata.CreatedAt), timeMillis(metadata.UpdatedAt),
		nullableTime(optionalUpdateTime(metadata.ActivatedAt)),
		nullableTime(optionalUpdateTime(metadata.CompletedAt)),
		metadata.Version, metadata.Commit, metadata.HostID, metadata.TargetPath,
		metadata.SizeBytes, metadata.SHA256, metadata.SchemaMin, metadata.SchemaMax,
		metadata.APIVersion, metadata.ArtifactPath, metadata.HandoffPath,
		metadata.RollbackReason, metadata.Error)
	if err != nil {
		return fmt.Errorf("create update metadata: %w", err)
	}
	return nil
}

func (s *Store) UpdateUpdate(ctx context.Context, metadata update.Metadata) error {
	result, err := s.db.ExecContext(ctx, `UPDATE updates SET
		state=?, updated_at=?, activated_at=?, completed_at=?, version=?,
		commit_hash=?, host_id=?, target_path=?, size_bytes=?, sha256=?,
		schema_min=?, schema_max=?, api_version=?, artifact_path=?, handoff_path=?,
		rollback_reason=?, error=? WHERE id=?`,
		metadata.State, timeMillis(metadata.UpdatedAt),
		nullableTime(optionalUpdateTime(metadata.ActivatedAt)),
		nullableTime(optionalUpdateTime(metadata.CompletedAt)),
		metadata.Version, metadata.Commit, metadata.HostID, metadata.TargetPath,
		metadata.SizeBytes, metadata.SHA256, metadata.SchemaMin, metadata.SchemaMax,
		metadata.APIVersion, metadata.ArtifactPath, metadata.HandoffPath,
		metadata.RollbackReason, metadata.Error, metadata.ID)
	if err != nil {
		return fmt.Errorf("update update metadata: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("update update metadata: %w", err)
	}
	if affected != 1 {
		return update.ErrNotFound
	}
	return nil
}

func (s *Store) Update(ctx context.Context, id string) (update.Metadata, error) {
	metadata, err := scanUpdate(s.db.QueryRowContext(
		ctx, `SELECT `+updateColumns+` FROM updates WHERE id=?`, id,
	))
	if errors.Is(err, sql.ErrNoRows) {
		return update.Metadata{}, update.ErrNotFound
	}
	if err != nil {
		return update.Metadata{}, fmt.Errorf("read update metadata: %w", err)
	}
	return metadata, nil
}

func (s *Store) ListUpdates(ctx context.Context, limit int) ([]update.Metadata, error) {
	if limit <= 0 {
		limit = 100
	}
	if limit > 500 {
		limit = 500
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+updateColumns+`
		FROM updates ORDER BY created_at DESC, id LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("list updates: %w", err)
	}
	defer rows.Close()
	var items []update.Metadata
	for rows.Next() {
		metadata, err := scanUpdate(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, metadata)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list updates: %w", err)
	}
	return items, nil
}

func scanUpdate(row rowScanner) (update.Metadata, error) {
	var metadata update.Metadata
	var createdAt, updatedAt int64
	var activatedAt, completedAt sql.NullInt64
	err := row.Scan(
		&metadata.ID, &metadata.CommandID, &metadata.State,
		&createdAt, &updatedAt, &activatedAt, &completedAt,
		&metadata.Version, &metadata.Commit, &metadata.HostID, &metadata.TargetPath,
		&metadata.SizeBytes, &metadata.SHA256, &metadata.SchemaMin, &metadata.SchemaMax,
		&metadata.APIVersion, &metadata.ArtifactPath, &metadata.HandoffPath,
		&metadata.RollbackReason, &metadata.Error,
	)
	if err != nil {
		return update.Metadata{}, err
	}
	metadata.CreatedAt = millisTime(createdAt)
	metadata.UpdatedAt = millisTime(updatedAt)
	metadata.ActivatedAt = nullableMillisTime(activatedAt)
	metadata.CompletedAt = nullableMillisTime(completedAt)
	return metadata, nil
}

func optionalUpdateTime(value time.Time) *time.Time {
	if value.IsZero() {
		return nil
	}
	return &value
}
