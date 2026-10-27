// Package supportbundle creates bounded, redacted operational support archives.
package supportbundle

import (
	"archive/zip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/GerardSmit/multirunner/internal/configview"
	"github.com/GerardSmit/multirunner/internal/diagnostics"
	"github.com/GerardSmit/multirunner/internal/operations"
	"github.com/GerardSmit/multirunner/internal/secretmask"
	"github.com/GerardSmit/multirunner/internal/secureartifact"
)

const (
	retention       = 7 * 24 * time.Hour
	maxWindow       = 30 * 24 * time.Hour
	maxBundleEvents = 10000
)

var (
	ErrNotFound  = errors.New("support bundle not found")
	ErrIntegrity = errors.New("support bundle integrity verification failed")
	secretKey    = regexp.MustCompile(`(?i)(authorization|password|secret|token|(^|[_.-])pat($|[_.-])|access[_-]?token)`)
)

type Request struct {
	From time.Time `json:"from"`
	To   time.Time `json:"to"`
}

type Metadata struct {
	ID          string    `json:"id"`
	CommandID   string    `json:"command_id"`
	CreatedAt   time.Time `json:"created_at"`
	ExpiresAt   time.Time `json:"expires_at"`
	From        time.Time `json:"from"`
	To          time.Time `json:"to"`
	FileName    string    `json:"file_name"`
	Size        int64     `json:"size"`
	SHA256      string    `json:"sha256"`
	Path        string    `json:"-"`
	DownloadURL string    `json:"download_url"`
}

type Store interface {
	SaveSupportBundle(context.Context, Metadata) error
	SupportBundle(context.Context, string) (Metadata, error)
	DeleteExpiredSupportBundles(context.Context, time.Time) ([]Metadata, error)
}

type EventReader interface {
	ListOperationalEvents(context.Context, operations.EventQuery) ([]operations.Event, error)
	OperationalEventBounds(context.Context, string) (minimum, maximum int64, err error)
	OperationalSnapshot(context.Context, string) (operations.Snapshot, error)
}

type Options struct {
	Root           string
	HostID         string
	HostEpoch      string
	AppVersion     string
	Store          Store
	Events         EventReader
	Diagnostics    func(context.Context) diagnostics.Report
	Configuration  func() (configview.Snapshot, error)
	DatabaseHealth func(context.Context) (any, error)
	Now            func() time.Time
}

type Service struct {
	root           string
	hostID         string
	hostEpoch      string
	appVersion     string
	store          Store
	events         EventReader
	diagnostics    func(context.Context) diagnostics.Report
	configuration  func() (configview.Snapshot, error)
	databaseHealth func(context.Context) (any, error)
	now            func() time.Time
}

func New(options Options) (*Service, error) {
	if options.Root == "" || options.HostID == "" || options.HostEpoch == "" {
		return nil, errors.New("support bundle root, host ID, and host epoch are required")
	}
	if options.Store == nil || options.Events == nil || options.Diagnostics == nil ||
		options.Configuration == nil || options.DatabaseHealth == nil {
		return nil, errors.New("support bundle dependencies are required")
	}
	root, err := secureartifact.PrepareRoot(options.Root)
	if err != nil {
		return nil, fmt.Errorf("prepare support bundle root: %w", err)
	}
	now := options.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return &Service{
		root: root, hostID: options.HostID, hostEpoch: options.HostEpoch,
		appVersion: options.AppVersion, store: options.Store, events: options.Events,
		diagnostics: options.Diagnostics, configuration: options.Configuration,
		databaseHealth: options.DatabaseHealth, now: now,
	}, nil
}

func (s *Service) Generate(ctx context.Context, commandID string, request Request) (Metadata, error) {
	if strings.TrimSpace(commandID) == "" {
		return Metadata{}, errors.New("support bundle command ID is required")
	}
	now := s.now().UTC()
	request, err := ValidateRequest(request, now)
	if err != nil {
		return Metadata{}, err
	}
	if err := s.removeExpired(ctx, now); err != nil {
		return Metadata{}, err
	}
	configuration, err := s.configuration()
	if err != nil {
		return Metadata{}, fmt.Errorf("inspect configuration: %w", err)
	}
	database, err := s.databaseHealth(ctx)
	if err != nil {
		return Metadata{}, fmt.Errorf("inspect database: %w", err)
	}
	snapshot, err := s.events.OperationalSnapshot(ctx, s.hostEpoch)
	if err != nil {
		return Metadata{}, fmt.Errorf("read operational snapshot: %w", err)
	}
	events, truncated, err := s.readEvents(ctx, request)
	if err != nil {
		return Metadata{}, err
	}
	report := s.diagnostics(ctx)
	id := commandID
	fileName := "multirunner-support-" + id + ".zip"
	temp, err := os.CreateTemp(s.root, ".support-*.tmp")
	if err != nil {
		return Metadata{}, fmt.Errorf("create support bundle: %w", err)
	}
	tempPath := temp.Name()
	defer func() {
		_ = temp.Close()
		_ = os.Remove(tempPath)
	}()
	createdAt := now
	expiresAt := now.Add(retention)
	manifest := map[string]any{
		"schema_version":   1,
		"bundle_id":        id,
		"command_id":       commandID,
		"created_at":       createdAt,
		"expires_at":       expiresAt,
		"from":             request.From,
		"to":               request.To,
		"host_id":          s.hostID,
		"host_epoch":       s.hostEpoch,
		"app_version":      s.appVersion,
		"event_count":      len(events),
		"events_truncated": truncated,
		"contents": []string{
			"manifest.json", "configuration.json", "diagnostics.json",
			"database.json", "operational-snapshot.json", "events.json",
		},
	}
	writer := zip.NewWriter(temp)
	for _, entry := range []struct {
		name  string
		value any
	}{
		{"manifest.json", manifest},
		{"configuration.json", configuration},
		{"diagnostics.json", report},
		{"database.json", database},
		{"operational-snapshot.json", snapshot},
		{"events.json", events},
	} {
		if err := writeJSONEntry(writer, entry.name, entry.value); err != nil {
			_ = writer.Close()
			return Metadata{}, err
		}
	}
	if err := writer.Close(); err != nil {
		return Metadata{}, fmt.Errorf("close support bundle archive: %w", err)
	}
	if err := temp.Sync(); err != nil {
		return Metadata{}, fmt.Errorf("flush support bundle: %w", err)
	}
	size, sum, err := secureartifact.DigestAndRewind(ctx, temp)
	if err != nil {
		return Metadata{}, fmt.Errorf("hash support bundle: %w", err)
	}
	if err := temp.Close(); err != nil {
		return Metadata{}, fmt.Errorf("close support bundle: %w", err)
	}
	target := filepath.Join(s.root, fileName)
	if err := os.Rename(tempPath, target); err != nil {
		return Metadata{}, fmt.Errorf("publish support bundle: %w", err)
	}
	metadata := Metadata{
		ID: id, CommandID: commandID, CreatedAt: createdAt, ExpiresAt: expiresAt,
		From: request.From, To: request.To, FileName: fileName, Size: size,
		SHA256: sum, Path: target, DownloadURL: "/api/v1/support-bundles/" + id,
	}
	if err := s.store.SaveSupportBundle(ctx, metadata); err != nil {
		_ = os.Remove(target)
		return Metadata{}, fmt.Errorf("save support bundle metadata: %w", err)
	}
	return metadata, nil
}

func (s *Service) Open(ctx context.Context, id string) (Metadata, *os.File, error) {
	metadata, err := s.store.SupportBundle(ctx, id)
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
		return Metadata{}, nil, fmt.Errorf("open support bundle: %w", err)
	}
	size, sum, err := secureartifact.DigestAndRewind(ctx, file)
	if err != nil {
		_ = file.Close()
		return Metadata{}, nil, fmt.Errorf("verify support bundle: %w", err)
	}
	if size != metadata.Size || sum != metadata.SHA256 {
		_ = file.Close()
		return Metadata{}, nil, ErrIntegrity
	}
	return metadata, file, nil
}

func (s *Service) Exists(ctx context.Context, id string) (bool, error) {
	metadata, err := s.Metadata(ctx, id)
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !metadata.ExpiresAt.After(s.now()) {
		return false, nil
	}
	_, err = os.Stat(metadata.Path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}

func (s *Service) Metadata(ctx context.Context, id string) (Metadata, error) {
	metadata, err := s.store.SupportBundle(ctx, id)
	if err != nil {
		return Metadata{}, err
	}
	if !metadata.ExpiresAt.After(s.now()) {
		return Metadata{}, ErrNotFound
	}
	if _, err := os.Stat(metadata.Path); errors.Is(err, os.ErrNotExist) {
		return Metadata{}, ErrNotFound
	} else if err != nil {
		return Metadata{}, fmt.Errorf("inspect support bundle: %w", err)
	}
	return metadata, nil
}

func (s *Service) readEvents(ctx context.Context, request Request) ([]operations.Event, bool, error) {
	_, maximum, err := s.events.OperationalEventBounds(ctx, s.hostEpoch)
	if err != nil {
		return nil, false, fmt.Errorf("read operational event bounds: %w", err)
	}
	var result []operations.Event
	after := int64(0)
	for after < maximum && len(result) < maxBundleEvents {
		events, err := s.events.ListOperationalEvents(ctx, operations.EventQuery{
			HostEpoch: s.hostEpoch, AfterSequence: after, MaxSequence: maximum,
			Since: request.From, Until: request.To, Limit: 1000,
		})
		if err != nil {
			return nil, false, fmt.Errorf("read operational events: %w", err)
		}
		if len(events) == 0 {
			break
		}
		result = append(result, events...)
		after = events[len(events)-1].Sequence
	}
	truncated := len(result) >= maxBundleEvents
	if len(result) > maxBundleEvents {
		result = result[:maxBundleEvents]
	}
	return result, truncated, nil
}

func (s *Service) removeExpired(ctx context.Context, now time.Time) error {
	expired, err := s.store.DeleteExpiredSupportBundles(ctx, now)
	if err != nil {
		return fmt.Errorf("prune support bundle metadata: %w", err)
	}
	for _, metadata := range expired {
		if !secureartifact.Contains(s.root, metadata.Path) {
			return fmt.Errorf("remove expired support bundle: %w", secureartifact.ErrInsecure)
		}
		if err := os.Remove(metadata.Path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove expired support bundle: %w", err)
		}
	}
	return nil
}

func ValidateRequest(request Request, now time.Time) (Request, error) {
	if request.To.IsZero() {
		request.To = now
	}
	if request.From.IsZero() {
		request.From = request.To.Add(-24 * time.Hour)
	}
	request.From = request.From.UTC()
	request.To = request.To.UTC()
	if request.From.After(request.To) {
		return Request{}, errors.New("support bundle start must not be after end")
	}
	if request.To.Sub(request.From) > maxWindow {
		return Request{}, errors.New("support bundle time window cannot exceed 30 days")
	}
	if request.To.After(now.Add(5 * time.Minute)) {
		return Request{}, errors.New("support bundle end cannot be in the future")
	}
	return request, nil
}

func writeJSONEntry(writer *zip.Writer, name string, value any) error {
	sanitized, err := sanitize(value)
	if err != nil {
		return fmt.Errorf("sanitize %s: %w", name, err)
	}
	data, err := json.MarshalIndent(sanitized, "", "  ")
	if err != nil {
		return fmt.Errorf("encode %s: %w", name, err)
	}
	entry, err := writer.CreateHeader(&zip.FileHeader{
		Name: name, Method: zip.Deflate,
	})
	if err != nil {
		return fmt.Errorf("create %s: %w", name, err)
	}
	if _, err := entry.Write(append(data, '\n')); err != nil {
		return fmt.Errorf("write %s: %w", name, err)
	}
	return nil
}

func sanitize(value any) (any, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	var decoded any
	if err := json.Unmarshal(data, &decoded); err != nil {
		return nil, err
	}
	return redact(decoded, ""), nil
}

func redact(value any, key string) any {
	if secretKey.MatchString(key) {
		return "<redacted>"
	}
	switch typed := value.(type) {
	case map[string]any:
		for childKey, child := range typed {
			typed[childKey] = redact(child, childKey)
		}
		return typed
	case []any:
		for index, child := range typed {
			typed[index] = redact(child, key)
		}
		return typed
	case string:
		masked := secretmask.Text(typed, nil)
		return strings.ReplaceAll(masked, "***", "<redacted>")
	default:
		return value
	}
}
