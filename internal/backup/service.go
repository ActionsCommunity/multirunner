package backup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/GerardSmit/multirunner/internal/secureartifact"
)

const (
	defaultRetention     = 30 * 24 * time.Hour
	minimumFreeBytes     = uint64(2 << 30)
	minimumFreePercent   = uint64(5)
	backupOverheadFactor = uint64(2)
)

type Options struct {
	Root       string
	HostID     string
	AppVersion string
	Source     Source
	Store      Store
	Validate   Validator
	FreeSpace  FreeSpace
	Now        func() time.Time
	Retention  time.Duration
}

type Service struct {
	root       string
	hostID     string
	appVersion string
	source     Source
	store      Store
	validate   Validator
	freeSpace  FreeSpace
	now        func() time.Time
	retention  time.Duration
}

func New(options Options) (*Service, error) {
	if strings.TrimSpace(options.Root) == "" || strings.TrimSpace(options.HostID) == "" {
		return nil, errors.New("backup root and host ID are required")
	}
	if options.Source == nil || options.Store == nil || options.Validate == nil {
		return nil, errors.New("backup source, store, and validator are required")
	}
	if options.FreeSpace == nil {
		options.FreeSpace = diskFreeSpace
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	if options.Retention <= 0 {
		options.Retention = defaultRetention
	}
	root, err := secureartifact.PrepareRoot(options.Root)
	if err != nil {
		return nil, fmt.Errorf("prepare backup root: %w", err)
	}
	return &Service{
		root: root, hostID: options.HostID,
		appVersion: options.AppVersion, source: options.Source, store: options.Store,
		validate: options.Validate, freeSpace: options.FreeSpace,
		now: options.Now, retention: options.Retention,
	}, nil
}

func ValidateRequest(request Request) error {
	switch request.Purpose {
	case "", PurposeManual, PurposePreRestore, PurposePreUpdate:
		return nil
	default:
		return fmt.Errorf("invalid backup purpose %q", request.Purpose)
	}
}

func (s *Service) Generate(
	ctx context.Context, id string, request Request,
) (Metadata, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return Metadata{}, errors.New("backup ID is required")
	}
	if err := ValidateRequest(request); err != nil {
		return Metadata{}, err
	}
	if request.Purpose == "" {
		request.Purpose = PurposeManual
	}
	var metadata Metadata
	reusingIntent := false
	if existing, err := s.store.Backup(ctx, id); err == nil {
		if existing.State == StateSucceeded {
			return withDownloadURL(existing), nil
		}
		if existing.State == StateCreating {
			if recovered, recoverErr := s.recoverCompleted(ctx, existing); recoverErr == nil {
				return recovered, nil
			}
			metadata = existing
			reusingIntent = true
		} else {
			return withDownloadURL(existing), nil
		}
	} else if !errors.Is(err, ErrNotFound) {
		return Metadata{}, err
	}

	snapshot, err := s.source.BackupSnapshot(ctx, s.hostID)
	if err != nil {
		return Metadata{}, fmt.Errorf("read backup source metadata: %w", err)
	}
	if !reusingIntent {
		now := s.now().UTC()
		metadata = Metadata{
			ID: id, CommandID: id, Purpose: request.Purpose, State: StateCreating,
			CreatedAt: now, ExpiresAt: now.Add(s.retention),
			FileName: id + ".db", FilePath: filepath.Join(s.root, id+".db"),
			ManifestPath:  filepath.Join(s.root, id+".json"),
			SchemaVersion: snapshot.SchemaVersion, AppVersion: s.appVersion,
			HostID: snapshot.HostID, AuditEventID: snapshot.AuditEventID,
			AuditIntegrityHash: snapshot.AuditIntegrityHash,
		}
		if err := s.store.CreateBackup(ctx, metadata); err != nil {
			return Metadata{}, fmt.Errorf("record backup intent: %w", err)
		}
	}

	tempPath := metadata.FilePath + ".tmp"
	_ = os.Remove(tempPath)
	if err := s.checkSpace(snapshot); err != nil {
		return Metadata{}, s.fail(ctx, metadata, err)
	}
	if err := s.source.OnlineBackup(ctx, tempPath); err != nil {
		return Metadata{}, s.fail(ctx, metadata, fmt.Errorf("create online backup: %w", err))
	}
	if err := os.Chmod(tempPath, 0o600); err != nil {
		return Metadata{}, s.fail(ctx, metadata, fmt.Errorf("protect backup file: %w", err))
	}
	validation, err := s.validate(ctx, tempPath, false)
	if err != nil {
		return Metadata{}, s.fail(ctx, metadata, fmt.Errorf("%w: %v", ErrIntegrity, err))
	}
	if err := validateSnapshot(snapshot, validation); err != nil {
		return Metadata{}, s.fail(ctx, metadata, err)
	}
	file, err := secureartifact.Open(s.root, tempPath)
	if err != nil {
		return Metadata{}, s.fail(ctx, metadata, fmt.Errorf("open backup for checksum: %w", err))
	}
	size, checksum, err := secureartifact.DigestAndRewind(ctx, file)
	closeErr := file.Close()
	if err != nil {
		return Metadata{}, s.fail(ctx, metadata, err)
	}
	if closeErr != nil {
		return Metadata{}, s.fail(ctx, metadata, fmt.Errorf("close backup after checksum: %w", closeErr))
	}
	metadata.State = StateSucceeded
	metadata.CompletedAt = s.now().UTC()
	metadata.SizeBytes = size
	metadata.SHA256 = checksum
	metadata.QuickCheck = validation.QuickCheck
	if err := writeManifest(metadata.ManifestPath, metadata); err != nil {
		return Metadata{}, s.fail(ctx, metadata, err)
	}
	if err := os.Rename(tempPath, metadata.FilePath); err != nil {
		return Metadata{}, s.fail(ctx, metadata, fmt.Errorf("activate backup file: %w", err))
	}
	if err := s.store.CompleteBackup(ctx, metadata); err != nil {
		return Metadata{}, fmt.Errorf("record completed backup: %w", err)
	}
	return withDownloadURL(metadata), nil
}

func (s *Service) Metadata(ctx context.Context, id string) (Metadata, error) {
	metadata, err := s.store.Backup(ctx, id)
	if err != nil {
		return Metadata{}, err
	}
	return withDownloadURL(metadata), nil
}

func (s *Service) Exists(ctx context.Context, id string) (bool, error) {
	metadata, err := s.store.Backup(ctx, id)
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	return metadata.State == StateSucceeded, err
}

func (s *Service) List(ctx context.Context, limit int) ([]Metadata, error) {
	items, err := s.store.ListBackups(ctx, limit)
	if err != nil {
		return nil, err
	}
	for index := range items {
		items[index] = withDownloadURL(items[index])
	}
	return items, nil
}

func (s *Service) Open(
	ctx context.Context, id string,
) (Metadata, *os.File, error) {
	metadata, err := s.store.Backup(ctx, id)
	if err != nil {
		return Metadata{}, nil, err
	}
	if metadata.State != StateSucceeded {
		return Metadata{}, nil, ErrNotFound
	}
	if !pathWithin(s.root, metadata.FilePath) {
		return Metadata{}, nil, ErrIntegrity
	}
	file, err := secureartifact.Open(s.root, metadata.FilePath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Metadata{}, nil, ErrNotFound
		}
		return Metadata{}, nil, fmt.Errorf("open backup: %w", err)
	}
	size, checksum, err := secureartifact.DigestAndRewind(ctx, file)
	if err != nil {
		_ = file.Close()
		return Metadata{}, nil, err
	}
	if size != metadata.SizeBytes || checksum != metadata.SHA256 {
		_ = file.Close()
		return Metadata{}, nil, ErrIntegrity
	}
	return withDownloadURL(metadata), file, nil
}

func (s *Service) Prune(ctx context.Context) (int, error) {
	expired, err := s.store.PruneBackups(ctx, s.now().UTC())
	if err != nil {
		return 0, err
	}
	for _, metadata := range expired {
		if pathWithin(s.root, metadata.FilePath) {
			_ = os.Remove(metadata.FilePath)
		}
		if pathWithin(s.root, metadata.ManifestPath) {
			_ = os.Remove(metadata.ManifestPath)
		}
	}
	return len(expired), nil
}

func (s *Service) checkSpace(snapshot Snapshot) error {
	available, total, err := s.freeSpace(s.root)
	if err != nil {
		return fmt.Errorf("read backup disk capacity: %w", err)
	}
	required := uint64(max(snapshot.DatabaseBytes+snapshot.WALBytes, 1)) * backupOverheadFactor
	floor := minimumFreeBytes
	if percent := total * minimumFreePercent / 100; percent > floor {
		floor = percent
	}
	if required > ^uint64(0)-floor || available < floor+required {
		return fmt.Errorf("%w: available=%d required=%d protected_floor=%d",
			ErrInsufficientSpace, available, required, floor)
	}
	return nil
}

func (s *Service) recoverCompleted(
	ctx context.Context, metadata Metadata,
) (Metadata, error) {
	if !pathWithin(s.root, metadata.FilePath) {
		return Metadata{}, ErrIntegrity
	}
	validation, err := s.validate(ctx, metadata.FilePath, false)
	if err != nil {
		return Metadata{}, err
	}
	if err := validateSnapshot(Snapshot{
		SchemaVersion: metadata.SchemaVersion,
		HostID:        metadata.HostID,
	}, validation); err != nil {
		return Metadata{}, err
	}
	file, err := secureartifact.Open(s.root, metadata.FilePath)
	if err != nil {
		return Metadata{}, err
	}
	size, checksum, err := secureartifact.DigestAndRewind(ctx, file)
	closeErr := file.Close()
	if err != nil {
		return Metadata{}, err
	}
	if closeErr != nil {
		return Metadata{}, closeErr
	}
	metadata.State = StateSucceeded
	metadata.CompletedAt = s.now().UTC()
	metadata.SizeBytes = size
	metadata.SHA256 = checksum
	metadata.QuickCheck = validation.QuickCheck
	if err := writeManifest(metadata.ManifestPath, metadata); err != nil {
		return Metadata{}, err
	}
	if err := s.store.CompleteBackup(ctx, metadata); err != nil {
		return Metadata{}, err
	}
	return withDownloadURL(metadata), nil
}

func (s *Service) fail(
	ctx context.Context, metadata Metadata, cause error,
) error {
	_ = os.Remove(metadata.FilePath + ".tmp")
	_ = os.Remove(metadata.ManifestPath)
	errorText := cause.Error()
	if len(errorText) > 1000 {
		errorText = errorText[:1000]
	}
	if err := s.store.FailBackup(ctx, metadata.ID, errorText, s.now().UTC()); err != nil {
		return errors.Join(cause, err)
	}
	return cause
}

func validateSnapshot(snapshot Snapshot, validation Validation) error {
	if validation.QuickCheck != "ok" || validation.ForeignKeyCount != 0 {
		return fmt.Errorf("%w: quick_check=%q foreign_key_count=%d",
			ErrIntegrity, validation.QuickCheck, validation.ForeignKeyCount)
	}
	if validation.SchemaVersion != snapshot.SchemaVersion {
		return fmt.Errorf("%w: schema changed from %d to %d",
			ErrIncompatible, snapshot.SchemaVersion, validation.SchemaVersion)
	}
	if validation.HostID != snapshot.HostID {
		return fmt.Errorf("%w: host identity changed", ErrIncompatible)
	}
	return nil
}

func writeManifest(path string, metadata Metadata) error {
	data, err := json.MarshalIndent(metadata, "", "  ")
	if err != nil {
		return fmt.Errorf("encode backup manifest: %w", err)
	}
	temp := path + ".tmp"
	if err := os.WriteFile(temp, append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("write backup manifest: %w", err)
	}
	if err := os.Rename(temp, path); err != nil {
		_ = os.Remove(temp)
		return fmt.Errorf("activate backup manifest: %w", err)
	}
	return nil
}

func pathWithin(root, path string) bool {
	relative, err := filepath.Rel(root, path)
	return err == nil && relative != "." && relative != "" &&
		relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) &&
		!filepath.IsAbs(relative)
}

func withDownloadURL(metadata Metadata) Metadata {
	if metadata.State == StateSucceeded {
		metadata.DownloadURL = "/api/v1/backups/" + metadata.ID + "/download"
	}
	return metadata
}
