package history

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"time"
)

type migrationLedgerState struct {
	exists         bool
	hasChecksum    bool
	latest         int
	blankChecksums int
}

func (s migrationLedgerState) needsChanges() bool {
	return !s.exists || !s.hasChecksum || s.blankChecksums > 0 ||
		s.latest < CurrentSchemaVersion()
}

func sqliteDSN(path string, readOnly bool) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve SQLite path: %w", err)
	}
	query := url.Values{}
	if readOnly {
		query.Set("mode", "ro")
	} else {
		query.Add("_pragma", "journal_mode(WAL)")
	}
	query.Add("_pragma", "busy_timeout(5000)")
	query.Add("_pragma", "foreign_keys(1)")
	return "file:" + filepath.ToSlash(absolute) + "?" + query.Encode(), nil
}

func inspectMigrationLedger(
	ctx context.Context, db *sql.DB,
) (migrationLedgerState, error) {
	var state migrationLedgerState
	var exists int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master
		WHERE type='table' AND name='schema_migrations'`).Scan(&exists); err != nil {
		return state, fmt.Errorf("inspect migration ledger: %w", err)
	}
	if exists == 0 {
		return state, nil
	}
	state.exists = true
	columns, err := db.QueryContext(ctx, `PRAGMA table_info(schema_migrations)`)
	if err != nil {
		return state, fmt.Errorf("inspect migration ledger columns: %w", err)
	}
	for columns.Next() {
		var cid, notNull, primaryKey int
		var name, columnType string
		var defaultValue any
		if err := columns.Scan(
			&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey,
		); err != nil {
			_ = columns.Close()
			return state, fmt.Errorf("scan migration ledger column: %w", err)
		}
		if name == "checksum" {
			state.hasChecksum = true
		}
	}
	if err := columns.Close(); err != nil {
		return state, fmt.Errorf("close migration ledger columns: %w", err)
	}
	if err := columns.Err(); err != nil {
		return state, fmt.Errorf("read migration ledger columns: %w", err)
	}

	query := `SELECT version, '' FROM schema_migrations ORDER BY version`
	if state.hasChecksum {
		query = `SELECT version, checksum FROM schema_migrations ORDER BY version`
	}
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return state, fmt.Errorf("read migration ledger: %w", err)
	}
	expected := 1
	for rows.Next() {
		var version int
		var checksum string
		if err := rows.Scan(&version, &checksum); err != nil {
			_ = rows.Close()
			return state, fmt.Errorf("scan migration ledger: %w", err)
		}
		if version > CurrentSchemaVersion() {
			_ = rows.Close()
			return state, fmt.Errorf("%w: database=%d supported=%d",
				ErrSchemaTooNew, version, CurrentSchemaVersion())
		}
		if version != expected {
			_ = rows.Close()
			return state, fmt.Errorf("%w: expected version %d, found %d",
				ErrMigrationLedger, expected, version)
		}
		want := migrations[version-1].checksum()
		if checksum == "" {
			state.blankChecksums++
		} else if checksum != want {
			_ = rows.Close()
			return state, fmt.Errorf("%w: migration %d checksum mismatch",
				ErrMigrationLedger, version)
		}
		state.latest = version
		expected++
	}
	if err := rows.Close(); err != nil {
		return state, fmt.Errorf("close migration ledger: %w", err)
	}
	if err := rows.Err(); err != nil {
		return state, fmt.Errorf("read migration ledger: %w", err)
	}
	return state, nil
}

type preMigrationEvidence struct {
	Source          string    `json:"source"`
	Backup          string    `json:"backup"`
	CreatedAt       time.Time `json:"created_at"`
	FromVersion     int       `json:"from_version"`
	ToVersion       int       `json:"to_version"`
	SizeBytes       int64     `json:"size_bytes"`
	SHA256          string    `json:"sha256"`
	QuickCheck      string    `json:"quick_check"`
	ForeignKeyCount int       `json:"foreign_key_count"`
}

func createPreMigrationBackup(
	ctx context.Context, source string, fromVersion int,
) (string, error) {
	absolute, err := filepath.Abs(source)
	if err != nil {
		return "", fmt.Errorf("resolve pre-migration backup source: %w", err)
	}
	stamp := nowUTC().Format("20060102T150405.000000000Z")
	backupPath := fmt.Sprintf("%s.pre-migration-v%d-to-v%d-%s.db",
		absolute, fromVersion, CurrentSchemaVersion(), stamp)
	if err := OnlineBackupDatabase(ctx, absolute, backupPath); err != nil {
		return "", fmt.Errorf("create pre-migration backup: %w", err)
	}
	evidence, err := verifyPreMigrationBackup(ctx, absolute, backupPath, fromVersion)
	if err != nil {
		_ = os.Remove(backupPath)
		return "", err
	}
	manifestPath := backupPath + ".json"
	payload, err := json.MarshalIndent(evidence, "", "  ")
	if err != nil {
		_ = os.Remove(backupPath)
		return "", fmt.Errorf("encode pre-migration backup evidence: %w", err)
	}
	payload = append(payload, '\n')
	temporary := manifestPath + ".new"
	if err := os.WriteFile(temporary, payload, 0o600); err != nil {
		_ = os.Remove(backupPath)
		return "", fmt.Errorf("write pre-migration backup evidence: %w", err)
	}
	if err := os.Rename(temporary, manifestPath); err != nil {
		_ = os.Remove(temporary)
		_ = os.Remove(backupPath)
		return "", fmt.Errorf("publish pre-migration backup evidence: %w", err)
	}
	return manifestPath, nil
}

func verifyPreMigrationBackup(
	ctx context.Context, source, backupPath string, fromVersion int,
) (preMigrationEvidence, error) {
	evidence := preMigrationEvidence{
		Source: source, Backup: backupPath, CreatedAt: nowUTC(),
		FromVersion: fromVersion, ToVersion: CurrentSchemaVersion(),
	}
	file, err := os.Open(backupPath)
	if err != nil {
		return evidence, fmt.Errorf("open pre-migration backup: %w", err)
	}
	hash := sha256.New()
	evidence.SizeBytes, err = io.Copy(hash, file)
	closeErr := file.Close()
	if err != nil {
		return evidence, fmt.Errorf("hash pre-migration backup: %w", err)
	}
	if closeErr != nil {
		return evidence, fmt.Errorf("close pre-migration backup: %w", closeErr)
	}
	evidence.SHA256 = hex.EncodeToString(hash.Sum(nil))

	dsn, err := sqliteDSN(backupPath, true)
	if err != nil {
		return evidence, err
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return evidence, fmt.Errorf("open pre-migration backup for verification: %w", err)
	}
	defer db.Close()
	if err := db.QueryRowContext(ctx, `PRAGMA quick_check`).Scan(&evidence.QuickCheck); err != nil {
		return evidence, fmt.Errorf("verify pre-migration backup: %w", err)
	}
	if evidence.QuickCheck != "ok" {
		return evidence, fmt.Errorf("verify pre-migration backup: quick_check=%q",
			evidence.QuickCheck)
	}
	rows, err := db.QueryContext(ctx, `PRAGMA foreign_key_check`)
	if err != nil {
		return evidence, fmt.Errorf("verify pre-migration backup foreign keys: %w", err)
	}
	for rows.Next() {
		evidence.ForeignKeyCount++
	}
	if err := rows.Close(); err != nil {
		return evidence, fmt.Errorf("close pre-migration foreign key check: %w", err)
	}
	if err := rows.Err(); err != nil {
		return evidence, fmt.Errorf("read pre-migration foreign key check: %w", err)
	}
	if evidence.ForeignKeyCount != 0 {
		return evidence, errors.New("verify pre-migration backup: foreign key violations")
	}
	return evidence, nil
}
