package history

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/GerardSmit/multirunner/internal/backup"
	sqlite3 "modernc.org/sqlite"
)

const backupColumns = `id, command_id, purpose, state, created_at, completed_at,
	expires_at, file_name, file_path, manifest_path, size_bytes, sha256,
	schema_version, app_version, host_id, audit_event_id, audit_integrity_hash,
	quick_check, error`

func CurrentSchemaVersion() int {
	if len(migrations) == 0 {
		return 0
	}
	return migrations[len(migrations)-1].version
}

func (s *Store) OnlineBackup(ctx context.Context, destination string) error {
	return OnlineBackupDatabase(ctx, s.path, destination)
}

// OnlineBackupDatabase creates a consistent SQLite backup without opening the
// source through Store or applying migrations.
func OnlineBackupDatabase(ctx context.Context, source, destination string) error {
	if source == "" || destination == "" {
		return errors.New("backup source and destination are required")
	}
	dsn, err := sqliteDSN(source, true)
	if err != nil {
		return err
	}
	sourceDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		return fmt.Errorf("open online backup source: %w", err)
	}
	defer sourceDB.Close()
	sourceDB.SetMaxOpenConns(1)
	sourceDB.SetMaxIdleConns(1)
	connection, err := sourceDB.Conn(ctx)
	if err != nil {
		return fmt.Errorf("acquire backup connection: %w", err)
	}
	defer connection.Close()
	type backuper interface {
		NewBackup(string) (*sqlite3.Backup, error)
	}
	err = connection.Raw(func(driverConnection any) error {
		source, ok := driverConnection.(backuper)
		if !ok {
			return errors.New("SQLite driver does not support online backup")
		}
		operation, err := source.NewBackup(destination)
		if err != nil {
			return err
		}
		finished := false
		defer func() {
			if !finished {
				_ = operation.Finish()
			}
		}()
		for more := true; more; {
			if err := ctx.Err(); err != nil {
				return err
			}
			more, err = operation.Step(128)
			if err != nil {
				return err
			}
		}
		if err := operation.Finish(); err != nil {
			return err
		}
		finished = true
		return nil
	})
	if err != nil {
		return fmt.Errorf("online SQLite backup: %w", err)
	}
	return nil
}

func (s *Store) BackupSnapshot(
	ctx context.Context, hostID string,
) (backup.Snapshot, error) {
	if hostID == "" {
		return backup.Snapshot{}, errors.New("backup host ID is required")
	}
	var snapshot backup.Snapshot
	snapshot.HostID = hostID
	if err := s.db.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(version), 0) FROM schema_migrations`,
	).Scan(&snapshot.SchemaVersion); err != nil {
		return backup.Snapshot{}, fmt.Errorf("read backup schema version: %w", err)
	}
	var exists int
	if err := s.db.QueryRowContext(ctx,
		`SELECT 1 FROM hosts WHERE id=?`, hostID,
	).Scan(&exists); errors.Is(err, sql.ErrNoRows) {
		return backup.Snapshot{}, backup.ErrIncompatible
	} else if err != nil {
		return backup.Snapshot{}, fmt.Errorf("read backup host: %w", err)
	}
	err := s.db.QueryRowContext(ctx, `SELECT e.id, e.integrity_hash
		FROM operational_events e
		JOIN host_epochs h ON h.id=e.host_epoch
		WHERE e.host_id=?
		ORDER BY h.started_at DESC, e.sequence DESC LIMIT 1`,
		hostID).Scan(&snapshot.AuditEventID, &snapshot.AuditIntegrityHash)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return backup.Snapshot{}, fmt.Errorf("read backup audit head: %w", err)
	}
	if info, err := os.Stat(s.path); err == nil {
		snapshot.DatabaseBytes = info.Size()
	} else if !errors.Is(err, os.ErrNotExist) {
		return backup.Snapshot{}, fmt.Errorf("stat history database: %w", err)
	}
	if info, err := os.Stat(s.path + "-wal"); err == nil {
		snapshot.WALBytes = info.Size()
	} else if !errors.Is(err, os.ErrNotExist) {
		return backup.Snapshot{}, fmt.Errorf("stat history WAL: %w", err)
	}
	return snapshot, nil
}

func ValidateBackupDatabase(
	ctx context.Context, path string, applyMigrations bool,
) (backup.Validation, error) {
	if applyMigrations {
		store, err := Open(ctx, path)
		if err != nil {
			return backup.Validation{}, err
		}
		if err := store.Close(); err != nil {
			return backup.Validation{}, err
		}
	}
	dsn, err := sqliteDSN(path, true)
	if err != nil {
		return backup.Validation{}, err
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return backup.Validation{}, fmt.Errorf("open backup for validation: %w", err)
	}
	defer db.Close()
	var validation backup.Validation
	if err := db.QueryRowContext(ctx, `PRAGMA quick_check`).Scan(&validation.QuickCheck); err != nil {
		return backup.Validation{}, fmt.Errorf("run backup quick_check: %w", err)
	}
	rows, err := db.QueryContext(ctx, `PRAGMA foreign_key_check`)
	if err != nil {
		return backup.Validation{}, fmt.Errorf("run backup foreign_key_check: %w", err)
	}
	for rows.Next() {
		validation.ForeignKeyCount++
	}
	if err := rows.Close(); err != nil {
		return backup.Validation{}, fmt.Errorf("close backup foreign_key_check: %w", err)
	}
	if err := rows.Err(); err != nil {
		return backup.Validation{}, fmt.Errorf("read backup foreign_key_check: %w", err)
	}
	if err := db.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(version), 0) FROM schema_migrations`,
	).Scan(&validation.SchemaVersion); err != nil {
		return backup.Validation{}, fmt.Errorf("read backup schema version: %w", err)
	}
	if err := db.QueryRowContext(ctx,
		`SELECT host_id FROM host_epochs ORDER BY started_at DESC, id DESC LIMIT 1`,
	).Scan(&validation.HostID); err != nil {
		return backup.Validation{}, fmt.Errorf("read backup host identity: %w", err)
	}
	verification, err := verifyOperationalChains(ctx, db)
	if err != nil {
		return backup.Validation{}, err
	}
	validation.VerifiedEpochs = verification.Epochs
	validation.VerifiedEvents = verification.Events
	validation.CompactedEvents = verification.CompactedEvents
	return validation, nil
}

func (s *Store) CreateBackup(ctx context.Context, metadata backup.Metadata) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO backups (
		id, command_id, purpose, state, created_at, expires_at, file_name,
		file_path, manifest_path, schema_version, app_version, host_id,
		audit_event_id, audit_integrity_hash
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		metadata.ID, metadata.CommandID, metadata.Purpose, metadata.State,
		timeMillis(metadata.CreatedAt), timeMillis(metadata.ExpiresAt),
		metadata.FileName, metadata.FilePath, metadata.ManifestPath,
		metadata.SchemaVersion, metadata.AppVersion, metadata.HostID,
		metadata.AuditEventID, metadata.AuditIntegrityHash)
	if err != nil {
		return fmt.Errorf("create backup metadata: %w", err)
	}
	return nil
}

func (s *Store) CompleteBackup(ctx context.Context, metadata backup.Metadata) error {
	result, err := s.db.ExecContext(ctx, `UPDATE backups SET
		state=?, completed_at=?, size_bytes=?, sha256=?, quick_check=?, error=''
		WHERE id=? AND state='creating'`,
		metadata.State, timeMillis(metadata.CompletedAt), metadata.SizeBytes,
		metadata.SHA256, metadata.QuickCheck, metadata.ID)
	if err != nil {
		return fmt.Errorf("complete backup metadata: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("complete backup metadata: %w", err)
	}
	if affected != 1 {
		return backup.ErrNotFound
	}
	return nil
}

func (s *Store) FailBackup(
	ctx context.Context, id, errorText string, at time.Time,
) error {
	result, err := s.db.ExecContext(ctx, `UPDATE backups SET
		state='failed', completed_at=?, error=? WHERE id=? AND state='creating'`,
		timeMillis(at), errorText, id)
	if err != nil {
		return fmt.Errorf("fail backup metadata: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("fail backup metadata: %w", err)
	}
	if affected != 1 {
		return backup.ErrNotFound
	}
	return nil
}

func (s *Store) Backup(ctx context.Context, id string) (backup.Metadata, error) {
	metadata, err := readBackup(s.db.QueryRowContext(ctx,
		`SELECT `+backupColumns+` FROM backups WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return backup.Metadata{}, backup.ErrNotFound
	}
	if err != nil {
		return backup.Metadata{}, fmt.Errorf("read backup metadata: %w", err)
	}
	return metadata, nil
}

func (s *Store) ListBackups(
	ctx context.Context, limit int,
) ([]backup.Metadata, error) {
	if limit <= 0 {
		limit = 100
	}
	if limit > 500 {
		limit = 500
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+backupColumns+`
		FROM backups ORDER BY created_at DESC, id LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("list backups: %w", err)
	}
	defer rows.Close()
	var items []backup.Metadata
	for rows.Next() {
		metadata, err := scanBackup(rows)
		if err != nil {
			return nil, fmt.Errorf("scan backup metadata: %w", err)
		}
		items = append(items, metadata)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list backups: %w", err)
	}
	return items, nil
}

func (s *Store) PruneBackups(
	ctx context.Context, at time.Time,
) ([]backup.Metadata, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin backup pruning: %w", err)
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT `+backupColumns+`
		FROM backups WHERE state IN ('succeeded', 'failed') AND expires_at<=?
		ORDER BY expires_at, id`, timeMillis(at))
	if err != nil {
		return nil, fmt.Errorf("list expired backups: %w", err)
	}
	var items []backup.Metadata
	for rows.Next() {
		metadata, err := scanBackup(rows)
		if err != nil {
			_ = rows.Close()
			return nil, err
		}
		items = append(items, metadata)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("close expired backup rows: %w", err)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read expired backup rows: %w", err)
	}
	for _, metadata := range items {
		if _, err := tx.ExecContext(ctx, `DELETE FROM backups WHERE id=?`, metadata.ID); err != nil {
			return nil, fmt.Errorf("delete expired backup metadata: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit backup pruning: %w", err)
	}
	return items, nil
}

func readBackup(row rowScanner) (backup.Metadata, error) {
	return scanBackup(row)
}

func scanBackup(row rowScanner) (backup.Metadata, error) {
	var metadata backup.Metadata
	var createdAt, expiresAt int64
	var completedAt sql.NullInt64
	err := row.Scan(
		&metadata.ID, &metadata.CommandID, &metadata.Purpose, &metadata.State,
		&createdAt, &completedAt, &expiresAt, &metadata.FileName,
		&metadata.FilePath, &metadata.ManifestPath, &metadata.SizeBytes,
		&metadata.SHA256, &metadata.SchemaVersion, &metadata.AppVersion,
		&metadata.HostID, &metadata.AuditEventID, &metadata.AuditIntegrityHash,
		&metadata.QuickCheck, &metadata.Error,
	)
	if err != nil {
		return backup.Metadata{}, err
	}
	metadata.CreatedAt = millisTime(createdAt)
	metadata.CompletedAt = nullableMillisTime(completedAt)
	metadata.ExpiresAt = millisTime(expiresAt)
	return metadata, nil
}

func nullableMillisTime(value sql.NullInt64) time.Time {
	if !value.Valid {
		return time.Time{}
	}
	return millisTime(value.Int64)
}
