package restore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/GerardSmit/multirunner/internal/backup"
)

type fakeBackupReader struct {
	path     string
	metadata backup.Metadata
}

func (r fakeBackupReader) Open(
	_ context.Context, id string,
) (backup.Metadata, *os.File, error) {
	if id != r.metadata.ID {
		return backup.Metadata{}, nil, backup.ErrNotFound
	}
	file, err := os.Open(r.path)
	return r.metadata, file, err
}

type fakeRestoreStore struct {
	items        map[string]Metadata
	reconciled   []string
	reconcileErr error
}

func (s *fakeRestoreStore) CreateRestore(_ context.Context, metadata Metadata) error {
	if s.items == nil {
		s.items = make(map[string]Metadata)
	}
	if _, exists := s.items[metadata.ID]; exists {
		return errors.New("duplicate restore")
	}
	s.items[metadata.ID] = metadata
	return nil
}

func (s *fakeRestoreStore) UpdateRestore(_ context.Context, metadata Metadata) error {
	if _, exists := s.items[metadata.ID]; !exists {
		return ErrNotFound
	}
	s.items[metadata.ID] = metadata
	return nil
}

func (s *fakeRestoreStore) Restore(_ context.Context, id string) (Metadata, error) {
	metadata, exists := s.items[id]
	if !exists {
		return Metadata{}, ErrNotFound
	}
	return metadata, nil
}

func (s *fakeRestoreStore) ListRestores(_ context.Context, _ int) ([]Metadata, error) {
	items := make([]Metadata, 0, len(s.items))
	for _, metadata := range s.items {
		items = append(items, metadata)
	}
	return items, nil
}

func (s *fakeRestoreStore) ReconcileRestoredSnapshot(
	_ context.Context, id string, _ time.Time,
) (SnapshotReconciliation, error) {
	s.reconciled = append(s.reconciled, id)
	if s.reconcileErr != nil {
		return SnapshotReconciliation{}, s.reconcileErr
	}
	return SnapshotReconciliation{CommandsInterrupted: 1, BackupsFailed: 1}, nil
}

func TestStageActivateAndCommitHealthy(t *testing.T) {
	fixture := newRestoreFixture(t, 12, "host-1")
	metadata, err := fixture.service.Stage(t.Context(), "restore-1", Request{BackupID: "backup-1"})
	if err != nil {
		t.Fatal(err)
	}
	if metadata.State != StateStaged {
		t.Fatalf("state = %q, want staged", metadata.State)
	}
	if active, err := HasActiveHandoff(fixture.databasePath, fixture.handoffKey); err != nil || !active {
		t.Fatalf("active staged handoff = %v, %v", active, err)
	}
	assertFileContent(t, metadata.StagedPath, "restored database")

	handoff, activated, err := ActivatePending(t.Context(), ActivationOptions{
		DatabasePath: fixture.databasePath, CurrentSchemaVersion: 12,
		Validate: fixture.validator, HandoffKey: fixture.handoffKey,
		Now: func() time.Time { return fixture.now.Add(time.Minute) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if !activated || handoff.State != StateActivating {
		t.Fatalf("activation = (%v, %q), want true activating", activated, handoff.State)
	}
	assertFileContent(t, fixture.databasePath, "restored database")
	assertFileContent(t, handoff.RollbackPath, "current database")
	assertFileContent(t, handoff.RollbackPath+"-wal", "current wal")
	assertFileContent(t, handoff.RollbackPath+"-shm", "current shm")

	restoredStore := &fakeRestoreStore{}
	committed, ok, err := CommitHealthy(
		t.Context(), fixture.databasePath, fixture.handoffKey,
		restoredStore, fixture.now.Add(2*time.Minute),
	)
	if err != nil {
		t.Fatal(err)
	}
	if !ok || committed.State != StateSucceeded {
		t.Fatalf("commit = (%v, %q), want true succeeded", ok, committed.State)
	}
	if active, err := HasActiveHandoff(fixture.databasePath, fixture.handoffKey); err != nil || active {
		t.Fatalf("active committed handoff = %v, %v", active, err)
	}
	if len(restoredStore.reconciled) != 1 || restoredStore.reconciled[0] != "restore-1" {
		t.Fatalf("restored snapshot reconciliations = %v", restoredStore.reconciled)
	}
	persisted, err := restoredStore.Restore(t.Context(), "restore-1")
	if err != nil || persisted.State != StateSucceeded {
		t.Fatalf("persisted restore = %+v, err=%v", persisted, err)
	}
}

func TestRollbackRestoresDatabaseAndSidecars(t *testing.T) {
	fixture := newRestoreFixture(t, 12, "host-1")
	if _, err := fixture.service.Stage(t.Context(), "restore-1", Request{BackupID: "backup-1"}); err != nil {
		t.Fatal(err)
	}
	if _, activated, err := ActivatePending(t.Context(), ActivationOptions{
		DatabasePath: fixture.databasePath, CurrentSchemaVersion: 12,
		Validate: fixture.validator, HandoffKey: fixture.handoffKey,
	}); err != nil || !activated {
		t.Fatalf("activate = %v, %v", activated, err)
	}
	handoff, rolledBack, err := RollbackPending(
		t.Context(), ActivationOptions{
			DatabasePath: fixture.databasePath, CurrentSchemaVersion: 12,
			Validate: fixture.validator, HandoffKey: fixture.handoffKey,
		},
		"startup failed", fixture.now.Add(time.Minute),
	)
	if err != nil {
		t.Fatal(err)
	}
	if !rolledBack || handoff.State != StateRolledBack ||
		handoff.RollbackReason != "startup failed" {
		t.Fatalf("rollback = (%v, %+v)", rolledBack, handoff)
	}
	assertFileContent(t, fixture.databasePath, "current database")
	assertFileContent(t, fixture.databasePath+"-wal", "current wal")
	assertFileContent(t, fixture.databasePath+"-shm", "current shm")

	if _, recorded, err := CommitHealthy(
		t.Context(), fixture.databasePath, fixture.handoffKey,
		fixture.store, fixture.now.Add(2*time.Minute),
	); err != nil || !recorded {
		t.Fatalf("record rollback = %v, %v", recorded, err)
	}
	persisted, err := fixture.store.Restore(t.Context(), "restore-1")
	if err != nil || persisted.State != StateRolledBack {
		t.Fatalf("persisted rollback = %+v, err=%v", persisted, err)
	}
}

func TestActivationPhaseRecoveryAfterRestart(t *testing.T) {
	for _, phase := range []ActivationPhase{
		ActivationPhasePrepared,
		ActivationPhaseRollbackStaged,
		ActivationPhaseDatabaseActivated,
		ActivationPhaseReady,
	} {
		t.Run(string(phase), func(t *testing.T) {
			fixture := newRestoreFixture(t, 12, "host-1")
			if _, err := fixture.service.Stage(
				t.Context(), "restore-1", Request{BackupID: "backup-1"},
			); err != nil {
				t.Fatal(err)
			}
			crashErr := errors.New("simulated process termination")
			_, activated, err := activatePendingWithHooks(
				t.Context(),
				ActivationOptions{
					DatabasePath: fixture.databasePath, CurrentSchemaVersion: 12,
					Validate: fixture.validator, HandoffKey: fixture.handoffKey,
					Now: func() time.Time { return fixture.now },
				},
				activationHooks{
					afterPhase: func(persisted ActivationPhase) error {
						if persisted == phase {
							return crashErr
						}
						return nil
					},
				},
			)
			if !errors.Is(err, crashErr) || activated {
				t.Fatalf("interrupted activation = %v, %v", activated, err)
			}
			handoff, err := readHandoff(
				filepath.Join(fixture.root, "active.json"), fixture.handoffKey,
			)
			if err != nil {
				t.Fatal(err)
			}
			if handoff.ActivationPhase != phase {
				t.Fatalf("persisted phase = %q, want %q",
					handoff.ActivationPhase, phase)
			}

			if _, rolledBack, err := RollbackPending(
				t.Context(),
				ActivationOptions{
					DatabasePath: fixture.databasePath, CurrentSchemaVersion: 12,
					Validate: fixture.validator, HandoffKey: fixture.handoffKey,
				},
				"restart recovery", fixture.now.Add(time.Minute),
			); err != nil || !rolledBack {
				t.Fatalf("restart rollback = %v, %v", rolledBack, err)
			}
			assertFileContent(t, fixture.databasePath, "current database")
			assertFileContent(t, fixture.databasePath+"-wal", "current wal")
			assertFileContent(t, fixture.databasePath+"-shm", "current shm")
		})
	}
}

func TestActivationRecoveryAcrossMutationPhaseGaps(t *testing.T) {
	crashErr := errors.New("simulated process termination")
	tests := map[string]struct {
		hooks activationHooks
		phase ActivationPhase
	}{
		"after first rollback move": {
			phase: ActivationPhasePrepared,
			hooks: activationHooks{
				afterRollbackMove: func(suffix string) error {
					if suffix == "" {
						return crashErr
					}
					return nil
				},
			},
		},
		"after database move": {
			phase: ActivationPhaseRollbackStaged,
			hooks: activationHooks{
				afterDatabaseMove: func() error {
					return crashErr
				},
			},
		},
	}
	for name, testCase := range tests {
		t.Run(name, func(t *testing.T) {
			fixture := newRestoreFixture(t, 12, "host-1")
			if _, err := fixture.service.Stage(
				t.Context(), "restore-1", Request{BackupID: "backup-1"},
			); err != nil {
				t.Fatal(err)
			}
			if _, activated, err := activatePendingWithHooks(
				t.Context(),
				ActivationOptions{
					DatabasePath: fixture.databasePath, CurrentSchemaVersion: 12,
					Validate: fixture.validator, HandoffKey: fixture.handoffKey,
				},
				testCase.hooks,
			); !errors.Is(err, crashErr) || activated {
				t.Fatalf("interrupted activation = %v, %v", activated, err)
			}
			handoff, err := readHandoff(
				filepath.Join(fixture.root, "active.json"), fixture.handoffKey,
			)
			if err != nil {
				t.Fatal(err)
			}
			if handoff.ActivationPhase != testCase.phase {
				t.Fatalf("persisted phase = %q, want %q",
					handoff.ActivationPhase, testCase.phase)
			}
			if _, rolledBack, err := RollbackPending(
				t.Context(),
				ActivationOptions{
					DatabasePath: fixture.databasePath, CurrentSchemaVersion: 12,
					Validate: fixture.validator, HandoffKey: fixture.handoffKey,
				},
				"restart recovery", fixture.now,
			); err != nil || !rolledBack {
				t.Fatalf("restart rollback = %v, %v", rolledBack, err)
			}
			assertFileContent(t, fixture.databasePath, "current database")
			assertFileContent(t, fixture.databasePath+"-wal", "current wal")
			assertFileContent(t, fixture.databasePath+"-shm", "current shm")
		})
	}
}

func TestActivationReturnsPrimaryAndRollbackFailures(t *testing.T) {
	fixture := newRestoreFixture(t, 12, "host-1")
	if _, err := fixture.service.Stage(
		t.Context(), "restore-1", Request{BackupID: "backup-1"},
	); err != nil {
		t.Fatal(err)
	}
	activationErr := errors.New("activate rename failed")
	rollbackErr := errors.New("rollback failed")
	_, activated, err := activatePendingWithHooks(
		t.Context(),
		ActivationOptions{
			DatabasePath: fixture.databasePath, CurrentSchemaVersion: 12,
			Validate: fixture.validator, HandoffKey: fixture.handoffKey,
		},
		activationHooks{
			rename: func(string, string) error {
				return activationErr
			},
			rollback: func(
				context.Context, string, Handoff, ActivationOptions,
			) error {
				return rollbackErr
			},
		},
	)
	if activated || !errors.Is(err, activationErr) || !errors.Is(err, rollbackErr) {
		t.Fatalf("activation errors = activated:%v err:%v", activated, err)
	}
}

func TestCommitHealthyRejectsIncompleteActivationPhase(t *testing.T) {
	fixture := newRestoreFixture(t, 12, "host-1")
	if _, err := fixture.service.Stage(
		t.Context(), "restore-1", Request{BackupID: "backup-1"},
	); err != nil {
		t.Fatal(err)
	}
	crashErr := errors.New("simulated process termination")
	if _, _, err := activatePendingWithHooks(
		t.Context(),
		ActivationOptions{
			DatabasePath: fixture.databasePath, CurrentSchemaVersion: 12,
			Validate: fixture.validator, HandoffKey: fixture.handoffKey,
		},
		activationHooks{
			afterPhase: func(phase ActivationPhase) error {
				if phase == ActivationPhaseDatabaseActivated {
					return crashErr
				}
				return nil
			},
		},
	); !errors.Is(err, crashErr) {
		t.Fatalf("interrupted activation error = %v", err)
	}
	if _, committed, err := CommitHealthy(
		t.Context(), fixture.databasePath, fixture.handoffKey,
		&fakeRestoreStore{}, fixture.now,
	); !errors.Is(err, ErrIntegrity) || committed {
		t.Fatalf("incomplete phase commit = %v, %v", committed, err)
	}
}

func TestCommitHealthyKeepsHandoffWhenSnapshotReconciliationFails(t *testing.T) {
	fixture := newRestoreFixture(t, 12, "host-1")
	if _, err := fixture.service.Stage(
		t.Context(), "restore-1", Request{BackupID: "backup-1"},
	); err != nil {
		t.Fatal(err)
	}
	if _, activated, err := ActivatePending(t.Context(), ActivationOptions{
		DatabasePath: fixture.databasePath, CurrentSchemaVersion: 12,
		Validate: fixture.validator, HandoffKey: fixture.handoffKey,
	}); err != nil || !activated {
		t.Fatalf("activate = %v, %v", activated, err)
	}
	store := &fakeRestoreStore{reconcileErr: errors.New("reconciliation failed")}
	if _, committed, err := CommitHealthy(
		t.Context(), fixture.databasePath, fixture.handoffKey, store, fixture.now,
	); err == nil || committed {
		t.Fatalf("commit after reconciliation failure = %v, %v", committed, err)
	}
	if _, err := os.Stat(filepath.Join(fixture.root, "active.json")); err != nil {
		t.Fatalf("active handoff was not retained: %v", err)
	}

	store.reconcileErr = nil
	if _, committed, err := CommitHealthy(
		t.Context(), fixture.databasePath, fixture.handoffKey, store, fixture.now,
	); err != nil || !committed {
		t.Fatalf("retry commit = %v, %v", committed, err)
	}
}

func TestActivationRejectsTamperedStageWithoutMovingCurrentDatabase(t *testing.T) {
	fixture := newRestoreFixture(t, 12, "host-1")
	metadata, err := fixture.service.Stage(t.Context(), "restore-1", Request{BackupID: "backup-1"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(metadata.StagedPath, []byte("tampered"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, activated, err := ActivatePending(t.Context(), ActivationOptions{
		DatabasePath: fixture.databasePath, CurrentSchemaVersion: 12,
		Validate: fixture.validator, HandoffKey: fixture.handoffKey,
	})
	if !errors.Is(err, ErrIntegrity) || activated {
		t.Fatalf("activation = (%v, %v), want integrity failure", activated, err)
	}
	assertFileContent(t, fixture.databasePath, "current database")
}

func TestActivationRejectsStageReplacementAfterValidation(t *testing.T) {
	fixture := newRestoreFixture(t, 12, "host-1")
	metadata, err := fixture.service.Stage(t.Context(), "restore-1", Request{BackupID: "backup-1"})
	if err != nil {
		t.Fatal(err)
	}
	validator := func(
		_ context.Context, _ string, _ bool,
	) (backup.Validation, error) {
		if err := os.Rename(metadata.StagedPath, metadata.StagedPath+".validated"); err != nil {
			return backup.Validation{}, err
		}
		if err := os.WriteFile(metadata.StagedPath, []byte("replacement"), 0o600); err != nil {
			return backup.Validation{}, err
		}
		return backup.Validation{
			QuickCheck: "ok", SchemaVersion: 12, HostID: "host-1",
		}, nil
	}
	_, activated, err := ActivatePending(t.Context(), ActivationOptions{
		DatabasePath: fixture.databasePath, CurrentSchemaVersion: 12,
		Validate: validator, HandoffKey: fixture.handoffKey,
	})
	if !errors.Is(err, ErrIntegrity) || activated {
		t.Fatalf("activation = (%v, %v), want identity failure", activated, err)
	}
	assertFileContent(t, fixture.databasePath, "current database")
}

func TestRollbackRejectsForgedHandoffPaths(t *testing.T) {
	fixture := newRestoreFixture(t, 12, "host-1")
	if _, err := fixture.service.Stage(t.Context(), "restore-1", Request{BackupID: "backup-1"}); err != nil {
		t.Fatal(err)
	}
	if _, activated, err := ActivatePending(t.Context(), ActivationOptions{
		DatabasePath: fixture.databasePath, CurrentSchemaVersion: 12,
		Validate: fixture.validator, HandoffKey: fixture.handoffKey,
	}); err != nil || !activated {
		t.Fatalf("activate = %v, %v", activated, err)
	}
	handoffPath := filepath.Join(fixture.root, "active.json")
	data, err := os.ReadFile(handoffPath)
	if err != nil {
		t.Fatal(err)
	}
	var forged map[string]any
	if err := json.Unmarshal(data, &forged); err != nil {
		t.Fatal(err)
	}
	forged["rollback_path"] = filepath.Join(t.TempDir(), "attacker.db")
	data, err = json.Marshal(forged)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(handoffPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	_, rolledBack, err := RollbackPending(
		t.Context(), ActivationOptions{
			DatabasePath: fixture.databasePath, CurrentSchemaVersion: 12,
			Validate: fixture.validator, HandoffKey: fixture.handoffKey,
		}, "test", fixture.now,
	)
	if !errors.Is(err, ErrIntegrity) || rolledBack {
		t.Fatalf("rollback = (%v, %v), want authenticated handoff failure", rolledBack, err)
	}
	assertFileContent(t, fixture.databasePath, "restored database")
}

func TestRollbackRejectsUnsignedSidecar(t *testing.T) {
	fixture := newRestoreFixture(t, 12, "host-1")
	if err := os.Remove(fixture.databasePath + "-wal"); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.service.Stage(t.Context(), "restore-1", Request{BackupID: "backup-1"}); err != nil {
		t.Fatal(err)
	}
	handoff, activated, err := ActivatePending(t.Context(), ActivationOptions{
		DatabasePath: fixture.databasePath, CurrentSchemaVersion: 12,
		Validate: fixture.validator, HandoffKey: fixture.handoffKey,
	})
	if err != nil || !activated {
		t.Fatalf("activate = %v, %v", activated, err)
	}
	if err := os.WriteFile(handoff.RollbackPath+"-wal", []byte("unsigned wal"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, rolledBack, err := RollbackPending(
		t.Context(), ActivationOptions{
			DatabasePath: fixture.databasePath, CurrentSchemaVersion: 12,
			Validate: fixture.validator, HandoffKey: fixture.handoffKey,
		}, "test", fixture.now,
	)
	if !errors.Is(err, ErrIntegrity) || rolledBack {
		t.Fatalf("rollback = (%v, %v), want unsigned sidecar rejection", rolledBack, err)
	}
	assertFileContent(t, fixture.databasePath, "restored database")
}

func TestRollbackRemovesUnsignedTargetSidecar(t *testing.T) {
	fixture := newRestoreFixture(t, 12, "host-1")
	if err := os.Remove(fixture.databasePath + "-wal"); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.service.Stage(t.Context(), "restore-1", Request{BackupID: "backup-1"}); err != nil {
		t.Fatal(err)
	}
	if _, activated, err := ActivatePending(t.Context(), ActivationOptions{
		DatabasePath: fixture.databasePath, CurrentSchemaVersion: 12,
		Validate: fixture.validator, HandoffKey: fixture.handoffKey,
	}); err != nil || !activated {
		t.Fatalf("activate = %v, %v", activated, err)
	}
	writeTestFile(t, fixture.databasePath+"-wal", "failed restore wal")
	_, rolledBack, err := RollbackPending(
		t.Context(), ActivationOptions{
			DatabasePath: fixture.databasePath, CurrentSchemaVersion: 12,
			Validate: fixture.validator, HandoffKey: fixture.handoffKey,
		}, "startup failed", fixture.now,
	)
	if err != nil || !rolledBack {
		t.Fatalf("rollback = (%v, %v)", rolledBack, err)
	}
	assertFileContent(t, fixture.databasePath, "current database")
	if _, err := os.Stat(fixture.databasePath + "-wal"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unsigned target WAL still exists: %v", err)
	}
}

func TestStageRejectsForeignHostAndFutureSchema(t *testing.T) {
	for name, testCase := range map[string]struct {
		schema int
		host   string
	}{
		"foreign host":  {schema: 12, host: "host-2"},
		"future schema": {schema: 13, host: "host-1"},
	} {
		t.Run(name, func(t *testing.T) {
			fixture := newRestoreFixture(t, testCase.schema, testCase.host)
			_, err := fixture.service.Stage(
				t.Context(), "restore-1", Request{BackupID: "backup-1"},
			)
			if !errors.Is(err, ErrIncompatible) {
				t.Fatalf("Stage error = %v, want incompatible", err)
			}
		})
	}
}

func TestTerminalHandoffIsArchivedForNextRestore(t *testing.T) {
	fixture := newRestoreFixture(t, 12, "host-1")
	if _, err := fixture.service.Stage(t.Context(), "restore-1", Request{BackupID: "backup-1"}); err != nil {
		t.Fatal(err)
	}
	if _, activated, err := ActivatePending(t.Context(), ActivationOptions{
		DatabasePath: fixture.databasePath, CurrentSchemaVersion: 12,
		Validate: fixture.validator, HandoffKey: fixture.handoffKey,
	}); err != nil || !activated {
		t.Fatalf("activate = %v, %v", activated, err)
	}
	if _, _, err := RollbackPending(
		t.Context(), ActivationOptions{
			DatabasePath: fixture.databasePath, CurrentSchemaVersion: 12,
			Validate: fixture.validator, HandoffKey: fixture.handoffKey,
		}, "test", fixture.now,
	); err != nil {
		t.Fatal(err)
	}
	fixture.reader.metadata.ID = "backup-2"
	fixture.service.backups = fixture.reader
	if _, err := fixture.service.Stage(t.Context(), "restore-2", Request{BackupID: "backup-2"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(fixture.root, "restore-1.rolled_back.json")); err != nil {
		t.Fatalf("archived handoff: %v", err)
	}
}

func TestPruneRemovesExpiredRollbackArtifacts(t *testing.T) {
	fixture := newRestoreFixture(t, 12, "host-1")
	if _, err := fixture.service.Stage(t.Context(), "restore-1", Request{BackupID: "backup-1"}); err != nil {
		t.Fatal(err)
	}
	handoff, activated, err := ActivatePending(t.Context(), ActivationOptions{
		DatabasePath: fixture.databasePath, CurrentSchemaVersion: 12,
		Validate: fixture.validator, HandoffKey: fixture.handoffKey,
		Now: func() time.Time { return fixture.now },
	})
	if err != nil || !activated {
		t.Fatalf("activate = %v, %v", activated, err)
	}
	if _, committed, err := CommitHealthy(
		t.Context(), fixture.databasePath, fixture.handoffKey, &fakeRestoreStore{},
		fixture.now,
	); err != nil || !committed {
		t.Fatalf("commit = %v, %v", committed, err)
	}
	fixture.service.now = func() time.Time {
		return fixture.now.Add(defaultRollbackRetention + time.Hour)
	}
	count, err := fixture.service.Prune(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("pruned handoffs = %d, want 1", count)
	}
	for _, path := range []string{
		handoff.RollbackPath,
		filepath.Join(fixture.root, "restore-1.succeeded.json"),
	} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s still exists: %v", path, err)
		}
	}
}

type restoreFixture struct {
	root         string
	databasePath string
	now          time.Time
	reader       fakeBackupReader
	store        *fakeRestoreStore
	validator    Validator
	service      *Service
	handoffKey   []byte
}

func newRestoreFixture(t *testing.T, backupSchema int, backupHost string) *restoreFixture {
	t.Helper()
	dir := t.TempDir()
	databasePath := filepath.Join(dir, "history.db")
	writeTestFile(t, databasePath, "current database")
	writeTestFile(t, databasePath+"-wal", "current wal")
	writeTestFile(t, databasePath+"-shm", "current shm")
	backupPath := filepath.Join(dir, "backup.db")
	writeTestFile(t, backupPath, "restored database")
	data := []byte("restored database")
	sum := sha256.Sum256(data)
	reader := fakeBackupReader{
		path: backupPath,
		metadata: backup.Metadata{
			ID: "backup-1", HostID: backupHost, SchemaVersion: backupSchema,
			SizeBytes: int64(len(data)), SHA256: hex.EncodeToString(sum[:]),
		},
	}
	store := &fakeRestoreStore{}
	validator := func(
		_ context.Context, _ string, _ bool,
	) (backup.Validation, error) {
		return backup.Validation{
			QuickCheck: "ok", SchemaVersion: backupSchema, HostID: backupHost,
		}, nil
	}
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	handoffKey := bytes.Repeat([]byte{0x5a}, 32)
	root := databasePath + ".restore"
	service, err := New(Options{
		Root: root, DatabasePath: databasePath, HostID: "host-1",
		CurrentSchemaVersion: 12, Backups: reader, Store: store,
		Validate: validator, HandoffKey: handoffKey,
		Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	return &restoreFixture{
		root: root, databasePath: databasePath, now: now, reader: reader,
		store: store, validator: validator, service: service,
		handoffKey: handoffKey,
	}
}

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func assertFileContent(t *testing.T, path, expected string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != expected {
		t.Fatalf("%s = %q, want %q", path, data, expected)
	}
}
