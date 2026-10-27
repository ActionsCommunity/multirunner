package history

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/GerardSmit/multirunner/internal/operations"
)

func TestOpenMigratesEveryPriorVersionWithBackupAndChecksums(t *testing.T) {
	for version := 0; version < CurrentSchemaVersion(); version++ {
		t.Run(fmt.Sprintf("v%d", version), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "history.db")
			createDatabaseAtVersion(t, path, version)

			store, err := Open(t.Context(), path)
			if err != nil {
				t.Fatalf("Open version %d: %v", version, err)
			}
			defer store.Close()
			var latest, blankChecksums int
			if err := store.db.QueryRowContext(t.Context(),
				`SELECT COALESCE(MAX(version), 0),
					COUNT(*) FILTER (WHERE checksum='')
				FROM schema_migrations`,
			).Scan(&latest, &blankChecksums); err != nil {
				t.Fatal(err)
			}
			if latest != CurrentSchemaVersion() || blankChecksums != 0 {
				t.Fatalf("migration ledger latest=%d blank=%d", latest, blankChecksums)
			}
			manifests, err := filepath.Glob(path + ".pre-migration-*.db.json")
			if err != nil {
				t.Fatal(err)
			}
			if len(manifests) != 1 {
				t.Fatalf("pre-migration manifests = %v", manifests)
			}
			assertPreMigrationEvidence(t, manifests[0], version)
		})
	}
}

func TestOpenRejectsMigrationLedgerGapAndChecksumChange(t *testing.T) {
	t.Run("gap", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "gap.db")
		db, err := sql.Open("sqlite", path)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`CREATE TABLE schema_migrations (
			version INTEGER PRIMARY KEY, applied_at INTEGER NOT NULL
		); INSERT INTO schema_migrations VALUES (1, 0), (3, 0)`); err != nil {
			t.Fatal(err)
		}
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
		store, err := Open(t.Context(), path)
		if store != nil {
			_ = store.Close()
		}
		if !errors.Is(err, ErrMigrationLedger) {
			t.Fatalf("Open ledger gap error = %v", err)
		}
	})

	t.Run("checksum", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "checksum.db")
		store, err := Open(t.Context(), path)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.db.ExecContext(t.Context(),
			`UPDATE schema_migrations SET checksum='changed' WHERE version=1`); err != nil {
			t.Fatal(err)
		}
		if err := store.Close(); err != nil {
			t.Fatal(err)
		}
		store, err = Open(t.Context(), path)
		if store != nil {
			_ = store.Close()
		}
		if !errors.Is(err, ErrMigrationLedger) {
			t.Fatalf("Open changed checksum error = %v", err)
		}
	})
}

func TestVersion14RehashesLegacyMillisecondEventChains(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy-events.db")
	createDatabaseAtVersion(t, path, 13)
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	timestamp := time.Date(2026, 10, 9, 12, 0, 0, 987654321, time.UTC)
	event := operations.Event{
		ID: operations.EventID("epoch-1", 1), SchemaVersion: 1,
		HostID: "host-1", HostEpoch: "epoch-1", Sequence: 1,
		Type: "test.event", EntityType: "test", EntityID: "item",
		Timestamp: timestamp, ActorKind: operations.ActorSystem,
		Payload: json.RawMessage(`{"legacy":true}`),
	}
	legacyHash := operations.IntegrityHash("", event)
	if _, err := db.Exec(`INSERT INTO hosts (
			id, installation_id, display_name, created_at, updated_at
		) VALUES ('host-1', 'install-1', 'host', 0, 0);
		INSERT INTO host_epochs (
			id, host_id, started_at, last_sequence
		) VALUES ('epoch-1', 'host-1', 0, 1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO operational_events (
		id, schema_version, host_id, host_epoch, sequence, event_type,
		entity_type, entity_id, timestamp, correlation_id, causation_id,
		actor_kind, actor_id, payload_json, command_id, previous_hash,
		integrity_hash
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, '', '', ?, '', ?, '', '', ?)`,
		event.ID, event.SchemaVersion, event.HostID, event.HostEpoch, event.Sequence,
		event.Type, event.EntityType, event.EntityID, timeMillis(event.Timestamp),
		event.ActorKind, []byte(event.Payload), legacyHash); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	store, err := Open(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.DatabaseHealth(t.Context()); err != nil {
		t.Fatal(err)
	}
	var rehashed string
	if err := store.db.QueryRowContext(t.Context(), `SELECT integrity_hash
		FROM operational_events WHERE id=?`, event.ID).Scan(&rehashed); err != nil {
		t.Fatal(err)
	}
	if rehashed == legacyHash {
		t.Fatal("legacy nanosecond hash was not normalized to stored milliseconds")
	}
}

func TestSQLiteDSNAppliesPragmasToEveryConnection(t *testing.T) {
	dsn, err := sqliteDSN(filepath.Join(t.TempDir(), "connections.db"), false)
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(2)
	first, err := db.Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := db.Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	for index, connection := range []*sql.Conn{first, second} {
		var busyTimeout, foreignKeys int
		var journalMode string
		if err := connection.QueryRowContext(t.Context(),
			`PRAGMA busy_timeout`).Scan(&busyTimeout); err != nil {
			t.Fatal(err)
		}
		if err := connection.QueryRowContext(t.Context(),
			`PRAGMA foreign_keys`).Scan(&foreignKeys); err != nil {
			t.Fatal(err)
		}
		if err := connection.QueryRowContext(t.Context(),
			`PRAGMA journal_mode`).Scan(&journalMode); err != nil {
			t.Fatal(err)
		}
		if busyTimeout != 5000 || foreignKeys != 1 || journalMode != "wal" {
			t.Errorf("connection %d pragmas: busy=%d foreign=%d journal=%q",
				index, busyTimeout, foreignKeys, journalMode)
		}
	}
}

func createDatabaseAtVersion(t *testing.T, path string, version int) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if version == 0 {
		if _, err := db.Exec(`CREATE TABLE legacy_marker (id INTEGER PRIMARY KEY)`); err != nil {
			t.Fatal(err)
		}
		return
	}
	if _, err := db.Exec(`CREATE TABLE schema_migrations (
		version INTEGER PRIMARY KEY, applied_at INTEGER NOT NULL
	)`); err != nil {
		t.Fatal(err)
	}
	for _, migration := range migrations[:version] {
		if _, err := db.Exec(migration.sql); err != nil {
			t.Fatalf("apply fixture migration %d: %v", migration.version, err)
		}
		if _, err := db.Exec(
			`INSERT INTO schema_migrations(version, applied_at) VALUES(?, 0)`,
			migration.version,
		); err != nil {
			t.Fatal(err)
		}
	}
}

func assertPreMigrationEvidence(t *testing.T, manifestPath string, fromVersion int) {
	t.Helper()
	payload, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	var evidence preMigrationEvidence
	if err := json.Unmarshal(payload, &evidence); err != nil {
		t.Fatal(err)
	}
	if evidence.FromVersion != fromVersion ||
		evidence.ToVersion != CurrentSchemaVersion() ||
		evidence.QuickCheck != "ok" || evidence.ForeignKeyCount != 0 ||
		evidence.SizeBytes <= 0 || evidence.SHA256 == "" {
		t.Fatalf("pre-migration evidence = %+v", evidence)
	}
	file, err := os.Open(evidence.Backup)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		t.Fatal(err)
	}
	if got := hex.EncodeToString(hash.Sum(nil)); got != evidence.SHA256 {
		t.Fatalf("backup SHA-256 = %s, evidence = %s", got, evidence.SHA256)
	}
}
