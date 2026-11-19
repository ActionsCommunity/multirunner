package supportbundle

import (
	"archive/zip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/GerardSmit/multirunner/internal/configview"
	"github.com/GerardSmit/multirunner/internal/diagnostics"
	"github.com/GerardSmit/multirunner/internal/operations"
)

type memoryStore struct {
	bundles map[string]Metadata
	expired []Metadata
}

func (s *memoryStore) SaveSupportBundle(_ context.Context, bundle Metadata) error {
	if s.bundles == nil {
		s.bundles = make(map[string]Metadata)
	}
	s.bundles[bundle.ID] = bundle
	return nil
}

func (s *memoryStore) SupportBundle(_ context.Context, id string) (Metadata, error) {
	bundle, ok := s.bundles[id]
	if !ok {
		return Metadata{}, ErrNotFound
	}
	return bundle, nil
}

func (s *memoryStore) DeleteExpiredSupportBundles(
	_ context.Context, _ time.Time,
) ([]Metadata, error) {
	expired := s.expired
	s.expired = nil
	return expired, nil
}

type memoryEvents struct {
	query operations.EventQuery
	event operations.Event
}

func (e *memoryEvents) ListOperationalEvents(
	_ context.Context, query operations.EventQuery,
) ([]operations.Event, error) {
	e.query = query
	if e.event.Sequence > query.AfterSequence &&
		(e.event.Timestamp.Equal(query.Since) || e.event.Timestamp.After(query.Since)) &&
		(e.event.Timestamp.Equal(query.Until) || e.event.Timestamp.Before(query.Until)) {
		return []operations.Event{e.event}, nil
	}
	return nil, nil
}

func (e *memoryEvents) OperationalEventBounds(
	context.Context, string,
) (int64, int64, error) {
	return e.event.Sequence, e.event.Sequence, nil
}

func (e *memoryEvents) OperationalSnapshot(
	_ context.Context, epoch string,
) (operations.Snapshot, error) {
	return operations.Snapshot{HostEpoch: epoch, LastSequence: e.event.Sequence}, nil
}

func TestGenerateCreatesRedactedBoundedArchiveAndPrunesExpired(t *testing.T) {
	now := time.Date(2026, time.October, 8, 20, 0, 0, 0, time.UTC)
	root := filepath.Join(t.TempDir(), "support-bundles")
	expiredPath := filepath.Join(root, "expired.zip")
	store := &memoryStore{
		bundles: make(map[string]Metadata),
	}
	events := &memoryEvents{event: operations.Event{
		ID: "epoch:4", HostEpoch: "epoch", Sequence: 4,
		Timestamp: now.Add(-30 * time.Minute), Type: "service.warning",
		EntityType: "host", EntityID: "host",
		ActorKind: operations.ActorSystem,
		Payload:   json.RawMessage(`{"authorization":"Bearer abcdefghijklmnopqrstuvwxyz","token":"ghp_123456789012345678901234567890"}`),
	}}
	service, err := New(Options{
		Root: root, HostID: "host", HostEpoch: "epoch", AppVersion: "v1",
		Store: store, Events: events, Now: func() time.Time { return now },
		Diagnostics: func(context.Context) diagnostics.Report {
			return diagnostics.Report{Status: diagnostics.StatusWarn, Results: []diagnostics.Result{{
				ID: "check", Evidence: map[string]any{
					"password": "do-not-include",
				},
			}}}
		},
		Configuration: func() (configview.Snapshot, error) {
			return configview.Snapshot{
				SourceFile:      `C:\multirunner\config.yaml`,
				ValidationError: "token ghp_123456789012345678901234567890",
				RawRedacted:     "auth:\n  pat: <redacted>\n",
			}, nil
		},
		DatabaseHealth: func(context.Context) (any, error) {
			return map[string]any{"quick_check": "ok"}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(expiredPath, []byte("expired"), 0o600); err != nil {
		t.Fatal(err)
	}
	store.expired = []Metadata{{ID: "expired", Path: expiredPath}}
	from := now.Add(-time.Hour)
	metadata, err := service.Generate(t.Context(), "command-1", Request{From: from, To: now})
	if err != nil {
		t.Fatal(err)
	}
	if metadata.ID != "command-1" || metadata.Size == 0 || metadata.SHA256 == "" ||
		!metadata.ExpiresAt.Equal(now.Add(7*24*time.Hour)) {
		t.Fatalf("metadata = %+v", metadata)
	}
	if _, err := os.Stat(expiredPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expired file still exists: %v", err)
	}
	if !events.query.Since.Equal(from) || !events.query.Until.Equal(now) {
		t.Fatalf("event query = %+v", events.query)
	}
	reader, err := zip.OpenReader(metadata.Path)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	var contents strings.Builder
	for _, file := range reader.File {
		names = append(names, file.Name)
		entry, err := file.Open()
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(entry)
		_ = entry.Close()
		if err != nil {
			t.Fatal(err)
		}
		contents.Write(data)
	}
	if len(names) != 6 {
		t.Fatalf("archive entries = %v", names)
	}
	text := contents.String()
	for _, secret := range []string{
		"do-not-include",
		"ghp_123456789012345678901234567890",
		"Bearer abcdefghijklmnopqrstuvwxyz",
	} {
		if strings.Contains(text, secret) {
			t.Fatalf("archive contains secret %q: %s", secret, text)
		}
	}
	if !strings.Contains(text, "redacted") {
		t.Fatalf("archive does not contain redaction marker: %s", text)
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	_, opened, err := service.Open(t.Context(), metadata.ID)
	if err != nil {
		t.Fatal(err)
	}
	originalPath := metadata.Path + ".original"
	if err := os.Rename(metadata.Path, originalPath); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(metadata.Path, []byte("replacement"), 0o600); err != nil {
		t.Fatal(err)
	}
	served, err := io.ReadAll(opened)
	_ = opened.Close()
	if err != nil || len(served) < 2 || string(served[:2]) != "PK" {
		t.Fatalf("opened support bundle changed after replacement: %q, %v", served, err)
	}
	if _, _, err := service.Open(t.Context(), metadata.ID); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("replacement support bundle error = %v", err)
	}
}

func TestGenerateRejectsUnsafeTimeWindows(t *testing.T) {
	now := time.Date(2026, time.October, 8, 20, 0, 0, 0, time.UTC)
	testCases := []Request{
		{From: now, To: now.Add(-time.Minute)},
		{From: now.Add(-31 * 24 * time.Hour), To: now},
		{From: now, To: now.Add(6 * time.Minute)},
	}
	for _, request := range testCases {
		if _, err := ValidateRequest(request, now); err == nil {
			t.Fatalf("ValidateRequest(%+v) succeeded", request)
		}
	}
}
