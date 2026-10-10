package history

import (
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/GerardSmit/multirunner/internal/restore"
)

func TestRestoreMetadataPersistsThroughRealSQLiteStore(t *testing.T) {
	store := openTestStore(t)
	host, err := store.EnsureOperationalHost(t.Context(), "installation-1", "host")
	if err != nil {
		t.Fatal(err)
	}
	created := time.Date(2026, time.October, 9, 12, 0, 0, 123000000, time.UTC)
	metadata := restore.Metadata{
		ID: "restore-1", CommandID: "command-1", BackupID: "backup-1",
		State: restore.StateStaging, CreatedAt: created, UpdatedAt: created,
		HostID: host.ID, SchemaVersion: CurrentSchemaVersion(), SizeBytes: 4096,
		SHA256: "abc123", DatabasePath: "history.db", StagedPath: "restore.staged.db",
		RollbackPath: "restore.rollback.db", HandoffPath: "restore-handoff.json",
		QuickCheck: "ok",
	}
	if err := store.CreateRestore(t.Context(), metadata); err != nil {
		t.Fatal(err)
	}
	persisted, err := store.Restore(t.Context(), metadata.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(persisted, metadata) {
		t.Fatalf("persisted metadata = %#v, want %#v", persisted, metadata)
	}

	metadata.State = restore.StateRolledBack
	metadata.UpdatedAt = created.Add(time.Minute)
	metadata.ActivatedAt = created.Add(10 * time.Second)
	metadata.CompletedAt = created.Add(50 * time.Second)
	metadata.RollbackReason = "startup health check failed"
	metadata.Error = "health timeout"
	if err := store.UpdateRestore(t.Context(), metadata); err != nil {
		t.Fatal(err)
	}
	persisted, err = store.Restore(t.Context(), metadata.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(persisted, metadata) {
		t.Fatalf("updated metadata = %#v, want %#v", persisted, metadata)
	}

	older := metadata
	older.ID = "restore-0"
	older.CommandID = "command-0"
	older.CreatedAt = created.Add(-time.Hour)
	older.UpdatedAt = older.CreatedAt
	older.ActivatedAt = time.Time{}
	older.CompletedAt = time.Time{}
	if err := store.CreateRestore(t.Context(), older); err != nil {
		t.Fatal(err)
	}
	items, err := store.ListRestores(t.Context(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].ID != metadata.ID {
		t.Fatalf("limited restores = %#v", items)
	}
}

func TestRestoreMetadataPersistenceReportsMissingAndDuplicateRows(t *testing.T) {
	store := openTestStore(t)
	host, err := store.EnsureOperationalHost(t.Context(), "installation-1", "host")
	if err != nil {
		t.Fatal(err)
	}
	missing := restore.Metadata{ID: "missing", UpdatedAt: time.Now().UTC()}
	if err := store.UpdateRestore(t.Context(), missing); !errors.Is(err, restore.ErrNotFound) {
		t.Fatalf("UpdateRestore missing error = %v", err)
	}
	if _, err := store.Restore(t.Context(), "missing"); !errors.Is(err, restore.ErrNotFound) {
		t.Fatalf("Restore missing error = %v", err)
	}

	now := time.Now().UTC().Truncate(time.Millisecond)
	item := restore.Metadata{
		ID: "restore-1", CommandID: "command-1", BackupID: "backup-1",
		State: restore.StateStaged, CreatedAt: now, UpdatedAt: now,
		HostID: host.ID,
	}
	if err := store.CreateRestore(t.Context(), item); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateRestore(t.Context(), item); err == nil {
		t.Fatal("duplicate restore metadata was accepted")
	}
}
