package backup_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GerardSmit/multirunner/internal/backup"
	"github.com/GerardSmit/multirunner/internal/history"
	"github.com/GerardSmit/multirunner/internal/operations"
)

func TestServiceCreatesVerifiedOnlineBackupDuringWrites(t *testing.T) {
	store, hostID, epochID := backupStore(t)
	payload := make([]byte, 4096)
	for index := range payload {
		payload[index] = 'a'
	}
	encoded, err := json.Marshal(map[string]any{"padding": string(payload)})
	if err != nil {
		t.Fatal(err)
	}
	for index := 0; index < 1200; index++ {
		if _, err := store.AppendOperationalEvent(t.Context(), epochID, operations.EventInput{
			Type: "backup.fixture", EntityType: "fixture", EntityID: "seed",
			ActorKind: operations.ActorSystem, ActorID: "test", Payload: encoded,
		}); err != nil {
			t.Fatal(err)
		}
	}

	started := make(chan struct{})
	source := &signalingSource{Store: store, started: started}
	service := newBackupService(t, source, store, hostID, func(string) (uint64, uint64, error) {
		return 100 << 30, 200 << 30, nil
	})
	var writes atomic.Int64
	writerDone := make(chan struct{})
	go func() {
		<-started
		defer close(writerDone)
		for index := 0; index < 100; index++ {
			_, err := store.AppendOperationalEvent(context.Background(), epochID, operations.EventInput{
				Type: "backup.concurrent", EntityType: "fixture", EntityID: "writer",
				ActorKind: operations.ActorSystem, ActorID: "test",
				Payload: json.RawMessage(`{"write":"concurrent"}`),
			})
			if err != nil {
				return
			}
			writes.Add(1)
		}
	}()

	metadata, err := service.Generate(t.Context(), "backup-1", backup.Request{
		Purpose: backup.PurposeManual,
	})
	if err != nil {
		t.Fatal(err)
	}
	<-writerDone
	if writes.Load() == 0 {
		t.Fatal("no concurrent writes completed during online backup")
	}
	if metadata.State != backup.StateSucceeded || metadata.SHA256 == "" ||
		metadata.SizeBytes == 0 || metadata.QuickCheck != "ok" ||
		metadata.SchemaVersion != history.CurrentSchemaVersion() ||
		metadata.HostID != hostID {
		t.Fatalf("backup metadata = %+v", metadata)
	}
	if metadata.DownloadURL != "/api/v1/backups/backup-1/download" {
		t.Fatalf("download URL = %q", metadata.DownloadURL)
	}
	opened, file, err := service.Open(t.Context(), metadata.ID)
	if err != nil {
		t.Fatal(err)
	}
	if opened.SHA256 != metadata.SHA256 {
		t.Fatalf("opened backup = %+v", opened)
	}
	if _, err := io.Copy(io.Discard, file); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	validation, err := history.ValidateBackupDatabase(t.Context(), metadata.FilePath, false)
	if err != nil {
		t.Fatal(err)
	}
	if validation.QuickCheck != "ok" || validation.ForeignKeyCount != 0 ||
		validation.HostID != hostID {
		t.Fatalf("backup validation = %+v", validation)
	}
	again, err := service.Generate(t.Context(), "backup-1", backup.Request{
		Purpose: backup.PurposeManual,
	})
	if err != nil || again.SHA256 != metadata.SHA256 {
		t.Fatalf("idempotent backup = %+v err=%v", again, err)
	}
	_, openedFile, err := service.Open(t.Context(), metadata.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(metadata.FilePath, metadata.FilePath+".original"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(metadata.FilePath, []byte("replacement"), 0o600); err != nil {
		t.Fatal(err)
	}
	servedSize, err := io.Copy(io.Discard, openedFile)
	_ = openedFile.Close()
	if err != nil || servedSize != metadata.SizeBytes {
		t.Fatalf("opened backup changed after replacement: size=%d err=%v", servedSize, err)
	}
	if _, _, err := service.Open(t.Context(), metadata.ID); !errors.Is(err, backup.ErrIntegrity) {
		t.Fatalf("replacement backup error = %v", err)
	}
}

func TestServiceFailsClosedOnLowDiskAndCorruption(t *testing.T) {
	store, hostID, _ := backupStore(t)
	lowDisk := newBackupService(t, store, store, hostID, func(string) (uint64, uint64, error) {
		return 1, 100 << 30, nil
	})
	_, err := lowDisk.Generate(t.Context(), "backup-low-disk", backup.Request{})
	if !errors.Is(err, backup.ErrInsufficientSpace) {
		t.Fatalf("low disk error = %v", err)
	}
	failed, err := store.Backup(t.Context(), "backup-low-disk")
	if err != nil {
		t.Fatal(err)
	}
	if failed.State != backup.StateFailed || failed.Error == "" {
		t.Fatalf("failed backup metadata = %+v", failed)
	}

	service := newBackupService(t, store, store, hostID, func(string) (uint64, uint64, error) {
		return 100 << 30, 200 << 30, nil
	})
	metadata, err := service.Generate(t.Context(), "backup-corrupt", backup.Request{})
	if err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(metadata.FilePath, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteAt([]byte("corrupt"), 0); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := service.Open(t.Context(), metadata.ID); !errors.Is(err, backup.ErrIntegrity) {
		t.Fatalf("corrupt backup open error = %v", err)
	}
}

func TestServicePrunesExpiredBackupFiles(t *testing.T) {
	store, hostID, _ := backupStore(t)
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	root := filepath.Join(t.TempDir(), "backups")
	service, err := backup.New(backup.Options{
		Root: root, HostID: hostID, AppVersion: "v1.2.3",
		Source: store, Store: store, Validate: history.ValidateBackupDatabase,
		FreeSpace: func(string) (uint64, uint64, error) {
			return 100 << 30, 200 << 30, nil
		},
		Now: func() time.Time { return now }, Retention: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	metadata, err := service.Generate(t.Context(), "backup-expiring", backup.Request{})
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Hour)
	count, err := service.Prune(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("pruned backups = %d, want 1", count)
	}
	if _, err := os.Stat(metadata.FilePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("backup file still exists: %v", err)
	}
	if _, err := store.Backup(t.Context(), metadata.ID); !errors.Is(err, backup.ErrNotFound) {
		t.Fatalf("backup metadata still exists: %v", err)
	}
}

type signalingSource struct {
	*history.Store
	started chan struct{}
}

func (s *signalingSource) OnlineBackup(ctx context.Context, path string) error {
	close(s.started)
	return s.Store.OnlineBackup(ctx, path)
}

func backupStore(t *testing.T) (*history.Store, string, string) {
	t.Helper()
	store, err := history.Open(t.Context(), filepath.Join(t.TempDir(), "history.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	host, err := store.EnsureOperationalHost(t.Context(), "backup-test", "host")
	if err != nil {
		t.Fatal(err)
	}
	epoch, err := store.StartHostEpoch(t.Context(), host.ID, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	store.SetCommandEpoch(epoch.ID)
	if _, err := store.AppendOperationalEvent(t.Context(), epoch.ID, operations.EventInput{
		Type: "host.started", EntityType: "host", EntityID: host.ID,
		ActorKind: operations.ActorSystem, ActorID: "test",
		Payload: json.RawMessage(`{"status":"started"}`),
	}); err != nil {
		t.Fatal(err)
	}
	return store, host.ID, epoch.ID
}

func newBackupService(
	t *testing.T, source backup.Source, store backup.Store, hostID string,
	free backup.FreeSpace,
) *backup.Service {
	t.Helper()
	service, err := backup.New(backup.Options{
		Root: filepath.Join(t.TempDir(), "backups"), HostID: hostID,
		AppVersion: "v1.2.3", Source: source, Store: store,
		Validate: history.ValidateBackupDatabase, FreeSpace: free,
	})
	if err != nil {
		t.Fatal(err)
	}
	return service
}
