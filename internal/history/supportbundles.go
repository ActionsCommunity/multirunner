package history

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/GerardSmit/multirunner/internal/supportbundle"
)

func (s *Store) SaveSupportBundle(ctx context.Context, bundle supportbundle.Metadata) error {
	if bundle.ID == "" || bundle.CommandID == "" || bundle.Path == "" ||
		bundle.FileName == "" || bundle.CreatedAt.IsZero() || bundle.ExpiresAt.IsZero() {
		return errors.New("support bundle metadata is incomplete")
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO support_bundles (
		id, command_id, created_at, expires_at, from_at, to_at, file_name,
		file_path, size_bytes, sha256
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		bundle.ID, bundle.CommandID, timeMillis(bundle.CreatedAt),
		timeMillis(bundle.ExpiresAt), timeMillis(bundle.From), timeMillis(bundle.To),
		bundle.FileName, bundle.Path, bundle.Size, bundle.SHA256)
	if err != nil {
		return fmt.Errorf("save support bundle: %w", err)
	}
	return nil
}

func (s *Store) SupportBundle(ctx context.Context, id string) (supportbundle.Metadata, error) {
	var bundle supportbundle.Metadata
	var createdAt, expiresAt, fromAt, toAt int64
	err := s.db.QueryRowContext(ctx, `SELECT
		id, command_id, created_at, expires_at, from_at, to_at, file_name,
		file_path, size_bytes, sha256
		FROM support_bundles WHERE id=?`, id).Scan(
		&bundle.ID, &bundle.CommandID, &createdAt, &expiresAt, &fromAt, &toAt,
		&bundle.FileName, &bundle.Path, &bundle.Size, &bundle.SHA256)
	if errors.Is(err, sql.ErrNoRows) {
		return supportbundle.Metadata{}, supportbundle.ErrNotFound
	}
	if err != nil {
		return supportbundle.Metadata{}, fmt.Errorf("read support bundle: %w", err)
	}
	bundle.CreatedAt = millisTime(createdAt)
	bundle.ExpiresAt = millisTime(expiresAt)
	bundle.From = millisTime(fromAt)
	bundle.To = millisTime(toAt)
	bundle.DownloadURL = "/api/v1/support-bundles/" + bundle.ID
	return bundle, nil
}

func (s *Store) DeleteExpiredSupportBundles(
	ctx context.Context, before time.Time,
) ([]supportbundle.Metadata, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin support bundle pruning: %w", err)
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT
		id, command_id, created_at, expires_at, from_at, to_at, file_name,
		file_path, size_bytes, sha256
		FROM support_bundles WHERE expires_at<=?`, timeMillis(before))
	if err != nil {
		return nil, fmt.Errorf("list expired support bundles: %w", err)
	}
	var bundles []supportbundle.Metadata
	for rows.Next() {
		var bundle supportbundle.Metadata
		var createdAt, expiresAt, fromAt, toAt int64
		if err := rows.Scan(
			&bundle.ID, &bundle.CommandID, &createdAt, &expiresAt, &fromAt, &toAt,
			&bundle.FileName, &bundle.Path, &bundle.Size, &bundle.SHA256,
		); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan expired support bundle: %w", err)
		}
		bundle.CreatedAt = millisTime(createdAt)
		bundle.ExpiresAt = millisTime(expiresAt)
		bundle.From = millisTime(fromAt)
		bundle.To = millisTime(toAt)
		bundles = append(bundles, bundle)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("close expired support bundles: %w", err)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read expired support bundles: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM support_bundles WHERE expires_at<=?`,
		timeMillis(before)); err != nil {
		return nil, fmt.Errorf("delete expired support bundles: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit support bundle pruning: %w", err)
	}
	return bundles, nil
}
