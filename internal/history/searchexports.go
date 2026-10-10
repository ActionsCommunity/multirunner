package history

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/GerardSmit/multirunner/internal/searchexport"
)

func (s *Store) SearchExportRecords(
	ctx context.Context, request searchexport.Request, limit int,
) ([]searchexport.Record, error) {
	results, err := s.search(ctx, SearchOptions{
		Query: request.Query, EntityType: request.EntityType,
		Repository: request.Repository, State: request.State, Limit: limit,
	}, limit)
	if err != nil {
		return nil, err
	}
	records := make([]searchexport.Record, len(results))
	for index, result := range results {
		records[index] = searchexport.Record{
			EntityType: result.EntityType, EntityKey: result.EntityKey,
			Repository: result.Repository, Title: result.Title, Context: result.Context,
			Timestamp: result.Timestamp, State: result.State, Route: result.Route,
		}
	}
	return records, nil
}

func (s *Store) SaveSearchExport(
	ctx context.Context, metadata searchexport.Metadata, audit searchexport.Audit,
) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin search export save: %w", err)
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, `INSERT INTO generated_exports (
		id, idempotency_key, request_hash, created_at, expires_at, file_name,
		file_path, size_bytes, sha256, record_count, query, entity_type,
		repository, state
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		metadata.ID, metadata.IdempotencyKey, metadata.RequestHash,
		timeMillis(metadata.CreatedAt), timeMillis(metadata.ExpiresAt),
		metadata.FileName, metadata.Path, metadata.Size, metadata.SHA256,
		metadata.RecordCount, metadata.Request.Query, metadata.Request.EntityType,
		metadata.Request.Repository, metadata.Request.State)
	if err != nil {
		return fmt.Errorf("save search export: %w", err)
	}
	if err := recordSearchExportAudit(ctx, tx, audit); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit search export save: %w", err)
	}
	return nil
}

func (s *Store) SearchExport(
	ctx context.Context, id string,
) (searchexport.Metadata, error) {
	var metadata searchexport.Metadata
	var createdAt, expiresAt int64
	err := s.db.QueryRowContext(ctx, `SELECT id, idempotency_key, request_hash,
		created_at, expires_at, file_name, file_path, size_bytes, sha256,
		record_count, query, entity_type, repository, state
		FROM generated_exports WHERE id=?`, id).Scan(
		&metadata.ID, &metadata.IdempotencyKey, &metadata.RequestHash,
		&createdAt, &expiresAt, &metadata.FileName, &metadata.Path, &metadata.Size,
		&metadata.SHA256, &metadata.RecordCount, &metadata.Request.Query,
		&metadata.Request.EntityType, &metadata.Request.Repository, &metadata.Request.State,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return searchexport.Metadata{}, searchexport.ErrNotFound
	}
	if err != nil {
		return searchexport.Metadata{}, fmt.Errorf("read search export: %w", err)
	}
	metadata.CreatedAt = millisTime(createdAt)
	metadata.ExpiresAt = millisTime(expiresAt)
	metadata.DownloadURL = "/api/v1/exports/" + metadata.ID
	return metadata, nil
}

func (s *Store) DeleteExpiredSearchExports(
	ctx context.Context, before time.Time,
) ([]searchexport.Metadata, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin search export pruning: %w", err)
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT id, file_path
		FROM generated_exports WHERE expires_at<=?`, timeMillis(before))
	if err != nil {
		return nil, fmt.Errorf("list expired search exports: %w", err)
	}
	var exports []searchexport.Metadata
	for rows.Next() {
		var metadata searchexport.Metadata
		if err := rows.Scan(&metadata.ID, &metadata.Path); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan expired search export: %w", err)
		}
		exports = append(exports, metadata)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("close expired search exports: %w", err)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read expired search exports: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM generated_exports WHERE expires_at<=?`,
		timeMillis(before)); err != nil {
		return nil, fmt.Errorf("delete expired search exports: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit search export pruning: %w", err)
	}
	return exports, nil
}

func (s *Store) RecordSearchExportAudit(
	ctx context.Context, audit searchexport.Audit,
) error {
	return recordSearchExportAudit(ctx, s.db, audit)
}

type auditExecutor interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

func recordSearchExportAudit(
	ctx context.Context, executor auditExecutor, audit searchexport.Audit,
) error {
	if audit.ID == "" || audit.OccurredAt.IsZero() || audit.ActorKind == "" ||
		audit.ActorID == "" || audit.Action == "" || audit.TargetType == "" ||
		audit.TargetID == "" || audit.Outcome == "" {
		return errors.New("search export audit is incomplete")
	}
	_, err := executor.ExecContext(ctx, `INSERT INTO access_audit_events (
		id, occurred_at, actor_kind, actor_id, action, target_type, target_id,
		correlation_id, outcome, byte_count, duration_ms, error_code
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		audit.ID, timeMillis(audit.OccurredAt), audit.ActorKind, audit.ActorID,
		audit.Action, audit.TargetType, audit.TargetID, audit.CorrelationID,
		audit.Outcome, audit.ByteCount, audit.Duration.Milliseconds(), audit.ErrorCode)
	if err != nil {
		return fmt.Errorf("record search export audit: %w", err)
	}
	return nil
}
