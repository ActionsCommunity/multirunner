package history

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/GerardSmit/multirunner/internal/supportbundle"
)

func TestSupportBundleStoreSaveReadAndPrune(t *testing.T) {
	ctx := t.Context()
	store := openTestStore(t)
	hostID := bindCommandEpoch(t, store)

	req1 := testCommandRequest("bundle-key-1", "system:bundle-1")
	req1.HostID = hostID
	cmd1, _, err := store.CreateCommand(ctx, req1)
	if err != nil {
		t.Fatalf("CreateCommand(1): %v", err)
	}
	req2 := testCommandRequest("bundle-key-2", "system:bundle-2")
	req2.HostID = hostID
	cmd2, _, err := store.CreateCommand(ctx, req2)
	if err != nil {
		t.Fatalf("CreateCommand(2): %v", err)
	}

	if err := store.SaveSupportBundle(ctx, supportbundle.Metadata{ID: "incomplete"}); err == nil {
		t.Fatal("expected incomplete metadata error, got nil")
	}

	if _, err := store.SupportBundle(ctx, "missing"); !errors.Is(err, supportbundle.ErrNotFound) {
		t.Fatalf("SupportBundle(missing) error = %v, want ErrNotFound", err)
	}

	base := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	expiredBundle := supportbundle.Metadata{
		ID:        "bundle-expired",
		CommandID: cmd1.ID,
		CreatedAt: base.Add(-2 * time.Hour),
		ExpiresAt: base.Add(-1 * time.Hour),
		From:      base.Add(-3 * time.Hour),
		To:        base.Add(-2 * time.Hour),
		FileName:  "expired.zip",
		Path:      filepath.Join(t.TempDir(), "expired.zip"),
		Size:      1024,
		SHA256:    "deadbeef",
	}
	activeBundle := supportbundle.Metadata{
		ID:        "bundle-active",
		CommandID: cmd2.ID,
		CreatedAt: base.Add(-30 * time.Minute),
		ExpiresAt: base.Add(30 * time.Minute),
		From:      base.Add(-1 * time.Hour),
		To:        base,
		FileName:  "active.zip",
		Path:      filepath.Join(t.TempDir(), "active.zip"),
		Size:      2048,
		SHA256:    "cafebabe",
	}

	if err := store.SaveSupportBundle(ctx, expiredBundle); err != nil {
		t.Fatalf("SaveSupportBundle(expired): %v", err)
	}
	if err := store.SaveSupportBundle(ctx, activeBundle); err != nil {
		t.Fatalf("SaveSupportBundle(active): %v", err)
	}

	got, err := store.SupportBundle(ctx, activeBundle.ID)
	if err != nil {
		t.Fatalf("SupportBundle(active): %v", err)
	}
	if got.ID != activeBundle.ID || got.CommandID != activeBundle.CommandID ||
		got.FileName != activeBundle.FileName || got.Size != activeBundle.Size ||
		got.SHA256 != activeBundle.SHA256 || got.DownloadURL != "/api/v1/support-bundles/bundle-active" {
		t.Fatalf("unexpected bundle metadata: %+v", got)
	}

	pruned, err := store.DeleteExpiredSupportBundles(ctx, base)
	if err != nil {
		t.Fatalf("DeleteExpiredSupportBundles: %v", err)
	}
	if len(pruned) != 1 || pruned[0].ID != expiredBundle.ID {
		t.Fatalf("DeleteExpiredSupportBundles returned %+v, want [%s]", pruned, expiredBundle.ID)
	}

	if _, err := store.SupportBundle(ctx, expiredBundle.ID); !errors.Is(err, supportbundle.ErrNotFound) {
		t.Fatalf("SupportBundle(expired after prune) error = %v, want ErrNotFound", err)
	}
	if _, err := store.SupportBundle(ctx, activeBundle.ID); err != nil {
		t.Fatalf("SupportBundle(active after prune) error = %v, want nil", err)
	}
}
