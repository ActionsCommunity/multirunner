package history

import (
	"errors"
	"testing"
	"time"

	"github.com/GerardSmit/multirunner/internal/update"
)

func TestUpdateMetadataLifecycle(t *testing.T) {
	store := openTestStore(t)
	host, err := store.EnsureOperationalHost(t.Context(), "installation-1", "")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	metadata := update.Metadata{
		ID: "update-1", CommandID: "command-1", State: update.StateStaged,
		CreatedAt: now, UpdatedAt: now, Version: "v1.2.0",
		Commit: "0123456789abcdef0123456789abcdef01234567",
		HostID: host.ID, TargetPath: "multirunner_v1.2.0_windows_amd64.exe",
		SizeBytes: 123, SHA256: "sha", SchemaMin: 12, SchemaMax: 13,
		APIVersion: "v1", ArtifactPath: "update-1.exe", HandoffPath: "active.json",
	}
	if err := store.CreateUpdate(t.Context(), metadata); err != nil {
		t.Fatal(err)
	}
	metadata.State = update.StateSucceeded
	metadata.ActivatedAt = now.Add(time.Minute)
	metadata.CompletedAt = now.Add(2 * time.Minute)
	metadata.UpdatedAt = metadata.CompletedAt
	if err := store.UpdateUpdate(t.Context(), metadata); err != nil {
		t.Fatal(err)
	}
	got, err := store.Update(t.Context(), metadata.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != update.StateSucceeded || got.CompletedAt != metadata.CompletedAt ||
		got.HostID != host.ID {
		t.Fatalf("update = %+v", got)
	}
	items, err := store.ListUpdates(t.Context(), 10)
	if err != nil || len(items) != 1 || items[0].ID != metadata.ID {
		t.Fatalf("updates = %+v, %v", items, err)
	}
	if _, err := store.Update(t.Context(), "missing"); !errors.Is(err, update.ErrNotFound) {
		t.Fatalf("missing update error = %v", err)
	}
}
