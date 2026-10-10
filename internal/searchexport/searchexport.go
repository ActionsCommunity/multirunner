// Package searchexport creates bounded, short-lived exports of durable search records.
package searchexport

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/GerardSmit/multirunner/internal/secureartifact"
)

const (
	retention  = 7 * 24 * time.Hour
	maxRecords = 10000
)

var (
	ErrNotFound            = errors.New("search export not found")
	ErrIdempotencyConflict = errors.New("search export idempotency conflict")
	ErrInvalidRequest      = errors.New("invalid search export request")
	ErrIntegrity           = errors.New("search export integrity verification failed")
)

type Request struct {
	Query      string `json:"query"`
	EntityType string `json:"entity_type,omitempty"`
	Repository string `json:"repository,omitempty"`
	State      string `json:"state,omitempty"`
}

type Record struct {
	EntityType string    `json:"entity_type"`
	EntityKey  string    `json:"entity_key"`
	Repository string    `json:"repository,omitempty"`
	Title      string    `json:"title"`
	Context    string    `json:"context"`
	Timestamp  time.Time `json:"timestamp"`
	State      string    `json:"state"`
	Route      string    `json:"route"`
}

type Metadata struct {
	ID             string    `json:"id"`
	IdempotencyKey string    `json:"-"`
	RequestHash    string    `json:"-"`
	CreatedAt      time.Time `json:"created_at"`
	ExpiresAt      time.Time `json:"expires_at"`
	FileName       string    `json:"file_name"`
	Size           int64     `json:"size"`
	SHA256         string    `json:"sha256"`
	RecordCount    int       `json:"record_count"`
	Request        Request   `json:"request"`
	Path           string    `json:"-"`
	DownloadURL    string    `json:"download_url"`
}

type Audit struct {
	ID            string
	OccurredAt    time.Time
	ActorKind     string
	ActorID       string
	Action        string
	TargetType    string
	TargetID      string
	CorrelationID string
	Outcome       string
	ByteCount     int64
	Duration      time.Duration
	ErrorCode     string
}

type Source interface {
	SearchExportRecords(context.Context, Request, int) ([]Record, error)
}

type Store interface {
	SaveSearchExport(context.Context, Metadata, Audit) error
	SearchExport(context.Context, string) (Metadata, error)
	DeleteExpiredSearchExports(context.Context, time.Time) ([]Metadata, error)
	RecordSearchExportAudit(context.Context, Audit) error
}

type Options struct {
	Root   string
	Source Source
	Store  Store
	Now    func() time.Time
}

type Service struct {
	root   string
	source Source
	store  Store
	now    func() time.Time
	mu     sync.Mutex
}

func New(options Options) (*Service, error) {
	if strings.TrimSpace(options.Root) == "" || options.Source == nil || options.Store == nil {
		return nil, errors.New("search export root, source, and store are required")
	}
	root, err := secureartifact.PrepareRoot(options.Root)
	if err != nil {
		return nil, fmt.Errorf("prepare search export root: %w", err)
	}
	now := options.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	service := &Service{root: root, source: options.Source, store: options.Store, now: now}
	if err := service.removeExpired(context.Background(), now().UTC()); err != nil {
		return nil, err
	}
	return service, nil
}

func (s *Service) Generate(
	ctx context.Context, idempotencyKey string, request Request, audit Audit,
) (Metadata, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	idempotencyKey = strings.TrimSpace(idempotencyKey)
	if idempotencyKey == "" || len(idempotencyKey) > 128 {
		return Metadata{}, false, fmt.Errorf(
			"%w: idempotency key must contain 1 to 128 characters", ErrInvalidRequest,
		)
	}
	request.Query = strings.TrimSpace(request.Query)
	request.EntityType = strings.TrimSpace(request.EntityType)
	request.Repository = strings.TrimSpace(request.Repository)
	request.State = strings.TrimSpace(request.State)
	requestJSON, err := json.Marshal(request)
	if err != nil {
		return Metadata{}, false, fmt.Errorf("encode search export request: %w", err)
	}
	requestSum := sha256.Sum256(requestJSON)
	requestHash := hex.EncodeToString(requestSum[:])
	idSum := sha256.Sum256([]byte(idempotencyKey))
	id := hex.EncodeToString(idSum[:16])
	existing, err := s.store.SearchExport(ctx, id)
	if err == nil {
		if existing.RequestHash != requestHash {
			return Metadata{}, false, ErrIdempotencyConflict
		}
		if existing.ExpiresAt.After(s.now()) {
			return existing, false, nil
		}
		if err := s.removeExpired(ctx, s.now().UTC()); err != nil {
			return Metadata{}, false, err
		}
	}
	if !errors.Is(err, ErrNotFound) {
		return Metadata{}, false, err
	}
	now := s.now().UTC()
	if err := s.removeExpired(ctx, now); err != nil {
		return Metadata{}, false, err
	}
	records, err := s.source.SearchExportRecords(ctx, request, maxRecords)
	if err != nil {
		return Metadata{}, false, err
	}
	temp, err := os.CreateTemp(s.root, ".search-export-*.tmp")
	if err != nil {
		return Metadata{}, false, fmt.Errorf("create search export: %w", err)
	}
	tempPath := temp.Name()
	defer func() {
		_ = temp.Close()
		_ = os.Remove(tempPath)
	}()
	buffer := bufio.NewWriter(temp)
	encoder := json.NewEncoder(buffer)
	encoder.SetEscapeHTML(true)
	for _, record := range records {
		if err := encoder.Encode(record); err != nil {
			return Metadata{}, false, fmt.Errorf("write search export: %w", err)
		}
	}
	if err := buffer.Flush(); err != nil {
		return Metadata{}, false, fmt.Errorf("flush search export: %w", err)
	}
	if err := temp.Sync(); err != nil {
		return Metadata{}, false, fmt.Errorf("sync search export: %w", err)
	}
	size, sum, err := secureartifact.DigestAndRewind(ctx, temp)
	if err != nil {
		return Metadata{}, false, fmt.Errorf("hash search export: %w", err)
	}
	if err := temp.Close(); err != nil {
		return Metadata{}, false, fmt.Errorf("close search export: %w", err)
	}
	fileName := "multirunner-search-" + id + ".jsonl"
	target := filepath.Join(s.root, fileName)
	if err := os.Rename(tempPath, target); err != nil {
		return Metadata{}, false, fmt.Errorf("publish search export: %w", err)
	}
	metadata := Metadata{
		ID: id, IdempotencyKey: idempotencyKey, RequestHash: requestHash,
		CreatedAt: now, ExpiresAt: now.Add(retention), FileName: fileName,
		Size: size, SHA256: sum, RecordCount: len(records), Request: request,
		Path: target, DownloadURL: "/api/v1/exports/" + id,
	}
	audit.TargetID = id
	audit.ByteCount = size
	if err := s.store.SaveSearchExport(ctx, metadata, audit); err != nil {
		_ = os.Remove(target)
		return Metadata{}, false, fmt.Errorf("save search export metadata: %w", err)
	}
	return metadata, true, nil
}

func (s *Service) Open(
	ctx context.Context, id string, audit Audit,
) (Metadata, *os.File, error) {
	metadata, err := s.store.SearchExport(ctx, id)
	if err != nil {
		return Metadata{}, nil, err
	}
	if !metadata.ExpiresAt.After(s.now()) {
		_ = os.Remove(metadata.Path)
		return Metadata{}, nil, ErrNotFound
	}
	file, err := secureartifact.Open(s.root, metadata.Path)
	if errors.Is(err, os.ErrNotExist) {
		return Metadata{}, nil, ErrNotFound
	}
	if err != nil {
		return Metadata{}, nil, fmt.Errorf("open search export: %w", err)
	}
	size, sum, err := secureartifact.DigestAndRewind(ctx, file)
	if err != nil {
		_ = file.Close()
		return Metadata{}, nil, fmt.Errorf("verify search export: %w", err)
	}
	if size != metadata.Size || sum != metadata.SHA256 {
		_ = file.Close()
		return Metadata{}, nil, ErrIntegrity
	}
	audit.TargetID = id
	audit.ByteCount = metadata.Size
	if err := s.store.RecordSearchExportAudit(ctx, audit); err != nil {
		_ = file.Close()
		return Metadata{}, nil, fmt.Errorf("audit search export download: %w", err)
	}
	return metadata, file, nil
}

func (s *Service) removeExpired(ctx context.Context, now time.Time) error {
	expired, err := s.store.DeleteExpiredSearchExports(ctx, now)
	if err != nil {
		return fmt.Errorf("prune search export metadata: %w", err)
	}
	for _, metadata := range expired {
		if !secureartifact.Contains(s.root, metadata.Path) {
			return fmt.Errorf("remove expired search export: %w", secureartifact.ErrInsecure)
		}
		if err := os.Remove(metadata.Path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove expired search export: %w", err)
		}
	}
	return nil
}
