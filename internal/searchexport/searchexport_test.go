package searchexport

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type memorySource struct {
	records []Record
	limit   int
}

func (s *memorySource) SearchExportRecords(
	_ context.Context, _ Request, limit int,
) ([]Record, error) {
	s.limit = limit
	return append([]Record(nil), s.records...), nil
}

type memoryStore struct {
	exports map[string]Metadata
	audits  []Audit
}

func (s *memoryStore) SaveSearchExport(
	_ context.Context, metadata Metadata, audit Audit,
) error {
	s.exports[metadata.ID] = metadata
	s.audits = append(s.audits, audit)
	return nil
}

func (s *memoryStore) SearchExport(_ context.Context, id string) (Metadata, error) {
	metadata, ok := s.exports[id]
	if !ok {
		return Metadata{}, ErrNotFound
	}
	return metadata, nil
}

func (s *memoryStore) DeleteExpiredSearchExports(
	_ context.Context, before time.Time,
) ([]Metadata, error) {
	var result []Metadata
	for id, metadata := range s.exports {
		if !metadata.ExpiresAt.After(before) {
			result = append(result, metadata)
			delete(s.exports, id)
		}
	}
	return result, nil
}

func (s *memoryStore) RecordSearchExportAudit(_ context.Context, audit Audit) error {
	s.audits = append(s.audits, audit)
	return nil
}

func TestGenerateIsBoundedIdempotentAndExpires(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	source := &memorySource{records: []Record{{
		EntityType: "run", EntityKey: "o/r:42", Repository: "o/r",
		Title: "Release verification", Timestamp: now, State: "success",
		Route: "/runs?repository=o%2Fr&run_id=42",
	}}}
	store := &memoryStore{exports: make(map[string]Metadata)}
	service, err := New(Options{
		Root:   filepath.Join(t.TempDir(), "exports"),
		Source: source, Store: store, Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	audit := Audit{
		ID: "audit-1", OccurredAt: now, ActorKind: "operator", ActorID: "local",
		Action: "history.export.generate", TargetType: "search_export", Outcome: "succeeded",
	}
	request := Request{Query: "release", EntityType: "run"}
	metadata, created, err := service.Generate(t.Context(), "key-1", request, audit)
	if err != nil {
		t.Fatal(err)
	}
	if !created || source.limit != maxRecords || metadata.RecordCount != 1 ||
		metadata.ExpiresAt.Sub(metadata.CreatedAt) != retention {
		t.Fatalf("metadata = %+v, created=%v, limit=%d", metadata, created, source.limit)
	}
	content, err := os.ReadFile(metadata.Path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(content), `"title":"Release verification"`) ||
		strings.Contains(string(content), "job log") {
		t.Fatalf("export content = %s", content)
	}
	_, opened, err := service.Open(t.Context(), metadata.ID, audit)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(metadata.Path, metadata.Path+".original"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(metadata.Path, []byte("replacement"), 0o600); err != nil {
		t.Fatal(err)
	}
	served, err := os.ReadFile(opened.Name())
	if err != nil {
		t.Fatal(err)
	}
	openedContent, err := io.ReadAll(opened)
	_ = opened.Close()
	if err != nil || !strings.Contains(string(openedContent), `"title":"Release verification"`) {
		t.Fatalf("opened export changed after replacement: %q, %v", openedContent, err)
	}
	if string(served) != "replacement" {
		t.Fatalf("replacement path content = %q", served)
	}
	if _, _, err := service.Open(t.Context(), metadata.ID, audit); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("replacement export error = %v", err)
	}
	replayed, created, err := service.Generate(t.Context(), "key-1", request, audit)
	if err != nil || created || replayed.ID != metadata.ID {
		t.Fatalf("replay = %+v, created=%v, err=%v", replayed, created, err)
	}
	_, _, err = service.Generate(t.Context(), "key-1", Request{Query: "different"}, audit)
	if !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("idempotency conflict = %v", err)
	}
	now = now.Add(8 * 24 * time.Hour)
	_, _, err = service.Open(t.Context(), metadata.ID, audit)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expired open = %v", err)
	}
	if _, err := os.Stat(filepath.Clean(metadata.Path)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expired file still exists: %v", err)
	}
}
