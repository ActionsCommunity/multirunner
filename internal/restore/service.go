package restore

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const handoffVersion = 1
const defaultRollbackRetention = 7 * 24 * time.Hour

type Options struct {
	Root                 string
	DatabasePath         string
	HostID               string
	CurrentSchemaVersion int
	Backups              BackupReader
	Store                Store
	Validate             Validator
	Now                  func() time.Time
	RollbackRetention    time.Duration
	HandoffKey           []byte
}

type Service struct {
	root                 string
	databasePath         string
	hostID               string
	currentSchemaVersion int
	backups              BackupReader
	store                Store
	validate             Validator
	now                  func() time.Time
	rollbackRetention    time.Duration
	handoffKey           []byte
}

type Handoff struct {
	Version               int              `json:"version"`
	ID                    string           `json:"id"`
	CommandID             string           `json:"command_id"`
	BackupID              string           `json:"backup_id"`
	State                 State            `json:"state"`
	ActivationPhase       ActivationPhase  `json:"activation_phase,omitempty"`
	CreatedAt             time.Time        `json:"created_at"`
	UpdatedAt             time.Time        `json:"updated_at"`
	ActivatedAt           time.Time        `json:"activated_at,omitempty"`
	CompletedAt           time.Time        `json:"completed_at,omitempty"`
	DatabasePath          string           `json:"database_path"`
	StagedPath            string           `json:"staged_path"`
	RollbackPath          string           `json:"rollback_path"`
	HostID                string           `json:"host_id"`
	SchemaVersion         int              `json:"schema_version"`
	SizeBytes             int64            `json:"size_bytes"`
	SHA256                string           `json:"sha256"`
	QuickCheck            string           `json:"quick_check,omitempty"`
	RollbackReason        string           `json:"rollback_reason,omitempty"`
	Error                 string           `json:"error,omitempty"`
	RollbackFiles         []ArtifactDigest `json:"rollback_files,omitempty"`
	RollbackHostID        string           `json:"rollback_host_id,omitempty"`
	RollbackSchemaVersion int              `json:"rollback_schema_version,omitempty"`
	MAC                   string           `json:"mac"`
}

type ActivationPhase string

const (
	ActivationPhasePrepared          ActivationPhase = "prepared"
	ActivationPhaseRollbackStaged    ActivationPhase = "rollback_staged"
	ActivationPhaseDatabaseActivated ActivationPhase = "database_activated"
	ActivationPhaseReady             ActivationPhase = "ready"
)

type ArtifactDigest struct {
	Suffix    string `json:"suffix"`
	SizeBytes int64  `json:"size_bytes"`
	SHA256    string `json:"sha256"`
}

func New(options Options) (*Service, error) {
	if strings.TrimSpace(options.Root) == "" ||
		strings.TrimSpace(options.DatabasePath) == "" ||
		strings.TrimSpace(options.HostID) == "" {
		return nil, errors.New("restore root, database path, and host ID are required")
	}
	if options.CurrentSchemaVersion < 1 {
		return nil, errors.New("current schema version is required")
	}
	if options.Backups == nil || options.Store == nil || options.Validate == nil {
		return nil, errors.New("restore backup reader, store, and validator are required")
	}
	if len(options.HandoffKey) < 32 {
		return nil, errors.New("restore handoff key must be at least 32 bytes")
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	if options.RollbackRetention <= 0 {
		options.RollbackRetention = defaultRollbackRetention
	}
	root, err := filepath.Abs(options.Root)
	if err != nil {
		return nil, fmt.Errorf("resolve restore root: %w", err)
	}
	databasePath, err := filepath.Abs(options.DatabasePath)
	if err != nil {
		return nil, fmt.Errorf("resolve restore database path: %w", err)
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, fmt.Errorf("create restore root: %w", err)
	}
	return &Service{
		root: filepath.Clean(root), databasePath: filepath.Clean(databasePath),
		hostID: options.HostID, currentSchemaVersion: options.CurrentSchemaVersion,
		backups: options.Backups, store: options.Store, validate: options.Validate,
		now: options.Now, rollbackRetention: options.RollbackRetention,
		handoffKey: append([]byte(nil), options.HandoffKey...),
	}, nil
}

func ValidateRequest(request Request) error {
	if strings.TrimSpace(request.BackupID) == "" {
		return errors.New("backup ID is required")
	}
	return nil
}

func (s *Service) Stage(
	ctx context.Context, id string, request Request,
) (Metadata, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return Metadata{}, errors.New("restore ID is required")
	}
	if err := ValidateRequest(request); err != nil {
		return Metadata{}, err
	}
	if existing, err := s.store.Restore(ctx, id); err == nil {
		return existing, nil
	} else if !errors.Is(err, ErrNotFound) {
		return Metadata{}, err
	}
	if existing, err := readHandoff(activePath(s.root), s.handoffKey); err == nil {
		switch existing.State {
		case StateSucceeded, StateRolledBack, StateFailed:
			archivePath := filepath.Join(s.root, existing.ID+"."+string(existing.State)+".json")
			if err := os.Rename(activePath(s.root), archivePath); err != nil {
				return Metadata{}, fmt.Errorf("archive completed restore handoff: %w", err)
			}
		default:
			return Metadata{}, ErrConflict
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return Metadata{}, err
	}

	backupMetadata, source, err := s.backups.Open(ctx, request.BackupID)
	if err != nil {
		return Metadata{}, fmt.Errorf("open restore backup: %w", err)
	}
	defer source.Close()
	if backupMetadata.HostID != s.hostID {
		return Metadata{}, fmt.Errorf("%w: backup host identity does not match", ErrIncompatible)
	}
	if backupMetadata.SchemaVersion > s.currentSchemaVersion {
		return Metadata{}, fmt.Errorf("%w: backup schema %d is newer than supported schema %d",
			ErrIncompatible, backupMetadata.SchemaVersion, s.currentSchemaVersion)
	}

	now := s.now().UTC()
	stagedPath := filepath.Join(s.root, id+".staged.db")
	rollbackPath := filepath.Join(s.root, id+".rollback.db")
	metadata := Metadata{
		ID: id, CommandID: id, BackupID: request.BackupID, State: StateStaging,
		CreatedAt: now, UpdatedAt: now, HostID: backupMetadata.HostID,
		SchemaVersion: backupMetadata.SchemaVersion, SizeBytes: backupMetadata.SizeBytes,
		SHA256: backupMetadata.SHA256, DatabasePath: s.databasePath,
		StagedPath: stagedPath, RollbackPath: rollbackPath,
		HandoffPath: activePath(s.root),
	}
	if err := s.store.CreateRestore(ctx, metadata); err != nil {
		return Metadata{}, fmt.Errorf("record restore intent: %w", err)
	}
	if err := copyVerified(ctx, source, stagedPath, backupMetadata.SizeBytes, backupMetadata.SHA256); err != nil {
		return Metadata{}, s.fail(ctx, metadata, err)
	}
	validation, err := s.validate(ctx, stagedPath, false)
	if err != nil {
		return Metadata{}, s.fail(ctx, metadata, fmt.Errorf("%w: %v", ErrIntegrity, err))
	}
	if validation.QuickCheck != "ok" || validation.ForeignKeyCount != 0 {
		return Metadata{}, s.fail(ctx, metadata, fmt.Errorf(
			"%w: quick_check=%q foreign_key_count=%d",
			ErrIntegrity, validation.QuickCheck, validation.ForeignKeyCount))
	}
	if validation.HostID != s.hostID || validation.SchemaVersion != backupMetadata.SchemaVersion {
		return Metadata{}, s.fail(ctx, metadata, ErrIncompatible)
	}
	metadata.State = StateStaged
	metadata.UpdatedAt = s.now().UTC()
	metadata.QuickCheck = validation.QuickCheck
	handoff := handoffFromMetadata(metadata)
	if err := writeHandoff(metadata.HandoffPath, handoff, s.handoffKey, true); err != nil {
		return Metadata{}, s.fail(ctx, metadata, err)
	}
	if err := s.store.UpdateRestore(ctx, metadata); err != nil {
		_ = os.Remove(metadata.HandoffPath)
		return Metadata{}, s.fail(ctx, metadata, err)
	}
	return metadata, nil
}

func (s *Service) Metadata(ctx context.Context, id string) (Metadata, error) {
	return s.store.Restore(ctx, id)
}

func (s *Service) Exists(ctx context.Context, id string) (bool, error) {
	_, err := s.store.Restore(ctx, id)
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	return err == nil, err
}

func (s *Service) List(ctx context.Context, limit int) ([]Metadata, error) {
	return s.store.ListRestores(ctx, limit)
}

func (s *Service) Prune(ctx context.Context) (int, error) {
	entries, err := os.ReadDir(s.root)
	if err != nil {
		return 0, fmt.Errorf("list restore artifacts: %w", err)
	}
	cutoff := s.now().UTC().Add(-s.rollbackRetention)
	count := 0
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return count, err
		}
		if entry.IsDir() || !isArchivedHandoff(entry.Name()) {
			continue
		}
		path := filepath.Join(s.root, entry.Name())
		handoff, err := readHandoff(path, s.handoffKey)
		if err != nil {
			return count, err
		}
		if handoff.CompletedAt.IsZero() || handoff.CompletedAt.After(cutoff) {
			continue
		}
		for _, candidate := range []string{
			handoff.StagedPath,
			handoff.RollbackPath,
			handoff.RollbackPath + "-wal",
			handoff.RollbackPath + "-shm",
		} {
			if candidate != "" && pathWithin(s.root, candidate) {
				if err := os.Remove(candidate); err != nil && !errors.Is(err, os.ErrNotExist) {
					return count, fmt.Errorf("remove expired restore artifact: %w", err)
				}
			}
		}
		if err := os.Remove(path); err != nil {
			return count, fmt.Errorf("remove expired restore handoff: %w", err)
		}
		count++
	}
	return count, nil
}

func (s *Service) fail(ctx context.Context, metadata Metadata, cause error) error {
	_ = os.Remove(metadata.StagedPath)
	metadata.State = StateFailed
	metadata.UpdatedAt = s.now().UTC()
	metadata.CompletedAt = metadata.UpdatedAt
	metadata.Error = boundedError(cause)
	if err := s.store.UpdateRestore(ctx, metadata); err != nil {
		return errors.Join(cause, err)
	}
	return cause
}

func handoffFromMetadata(metadata Metadata) Handoff {
	return Handoff{
		Version: handoffVersion, ID: metadata.ID, CommandID: metadata.CommandID,
		BackupID: metadata.BackupID, State: metadata.State,
		CreatedAt: metadata.CreatedAt, UpdatedAt: metadata.UpdatedAt,
		ActivatedAt: metadata.ActivatedAt, CompletedAt: metadata.CompletedAt,
		DatabasePath: metadata.DatabasePath, StagedPath: metadata.StagedPath,
		RollbackPath: metadata.RollbackPath, HostID: metadata.HostID,
		SchemaVersion: metadata.SchemaVersion, SizeBytes: metadata.SizeBytes,
		SHA256: metadata.SHA256, QuickCheck: metadata.QuickCheck,
		RollbackReason: metadata.RollbackReason, Error: metadata.Error,
	}
}

func (h Handoff) Metadata() Metadata {
	return Metadata{
		ID: h.ID, CommandID: h.CommandID, BackupID: h.BackupID, State: h.State,
		CreatedAt: h.CreatedAt, UpdatedAt: h.UpdatedAt, ActivatedAt: h.ActivatedAt,
		CompletedAt: h.CompletedAt, HostID: h.HostID,
		SchemaVersion: h.SchemaVersion, SizeBytes: h.SizeBytes, SHA256: h.SHA256,
		DatabasePath: h.DatabasePath, StagedPath: h.StagedPath,
		RollbackPath: h.RollbackPath, QuickCheck: h.QuickCheck,
		RollbackReason: h.RollbackReason, Error: h.Error,
	}
}

func activePath(root string) string {
	return filepath.Join(root, "active.json")
}

// HasActiveHandoff reports whether a restore is staged or activating and still
// depends on the current console secret for integrity verification.
func HasActiveHandoff(databasePath string, handoffKey []byte) (bool, error) {
	_, _, path, err := activationPaths(databasePath)
	if err != nil {
		return false, err
	}
	handoff, err := readHandoff(path, handoffKey)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return handoff.State == StateStaged || handoff.State == StateActivating, nil
}

func isArchivedHandoff(name string) bool {
	for _, suffix := range []string{
		"." + string(StateSucceeded) + ".json",
		"." + string(StateRolledBack) + ".json",
		"." + string(StateFailed) + ".json",
	} {
		if strings.HasSuffix(name, suffix) {
			return true
		}
	}
	return false
}

func copyVerified(
	ctx context.Context, source io.Reader, destination string, expectedSize int64,
	expectedSHA256 string,
) error {
	tempPath := destination + ".tmp"
	_ = os.Remove(tempPath)
	file, err := os.OpenFile(tempPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("create staged restore: %w", err)
	}
	hash := sha256.New()
	written, copyErr := copyContext(ctx, io.MultiWriter(file, hash), source)
	syncErr := file.Sync()
	closeErr := file.Close()
	if copyErr != nil || syncErr != nil || closeErr != nil {
		_ = os.Remove(tempPath)
		return errors.Join(copyErr, syncErr, closeErr)
	}
	checksum := hex.EncodeToString(hash.Sum(nil))
	if written != expectedSize || !strings.EqualFold(checksum, expectedSHA256) {
		_ = os.Remove(tempPath)
		return fmt.Errorf("%w: staged backup checksum or size changed", ErrIntegrity)
	}
	if err := os.Rename(tempPath, destination); err != nil {
		_ = os.Remove(tempPath)
		return fmt.Errorf("activate staged restore: %w", err)
	}
	return nil
}

func copyContext(ctx context.Context, destination io.Writer, source io.Reader) (int64, error) {
	buffer := make([]byte, 128<<10)
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		count, readErr := source.Read(buffer)
		if count > 0 {
			written, writeErr := destination.Write(buffer[:count])
			total += int64(written)
			if writeErr != nil {
				return total, writeErr
			}
			if written != count {
				return total, io.ErrShortWrite
			}
		}
		if errors.Is(readErr, io.EOF) {
			return total, nil
		}
		if readErr != nil {
			return total, readErr
		}
	}
}

func writeHandoff(path string, handoff Handoff, key []byte, exclusive bool) error {
	if len(key) < 32 {
		return ErrIntegrity
	}
	handoff.MAC = ""
	unsigned, err := json.Marshal(handoff)
	if err != nil {
		return fmt.Errorf("encode restore handoff for authentication: %w", err)
	}
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write(unsigned)
	handoff.MAC = hex.EncodeToString(mac.Sum(nil))
	data, err := json.MarshalIndent(handoff, "", "  ")
	if err != nil {
		return fmt.Errorf("encode restore handoff: %w", err)
	}
	tempPath := path + ".tmp"
	_ = os.Remove(tempPath)
	flags := os.O_CREATE | os.O_WRONLY | os.O_TRUNC
	if exclusive {
		flags = os.O_CREATE | os.O_EXCL | os.O_WRONLY
		tempPath = path
	}
	file, err := os.OpenFile(tempPath, flags, 0o600)
	if err != nil {
		if exclusive && errors.Is(err, os.ErrExist) {
			return ErrConflict
		}
		return fmt.Errorf("create restore handoff: %w", err)
	}
	if _, err := file.Write(append(data, '\n')); err != nil {
		_ = file.Close()
		_ = os.Remove(tempPath)
		return fmt.Errorf("write restore handoff: %w", err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		_ = os.Remove(tempPath)
		return fmt.Errorf("sync restore handoff: %w", err)
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(tempPath)
		return fmt.Errorf("close restore handoff: %w", err)
	}
	if !exclusive {
		if err := os.Rename(tempPath, path); err != nil {
			_ = os.Remove(tempPath)
			return fmt.Errorf("replace restore handoff: %w", err)
		}
	}
	return nil
}

func readHandoff(path string, key []byte) (Handoff, error) {
	if len(key) < 32 {
		return Handoff{}, ErrIntegrity
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return Handoff{}, err
	}
	var handoff Handoff
	if err := json.Unmarshal(data, &handoff); err != nil {
		return Handoff{}, fmt.Errorf("decode restore handoff: %w", err)
	}
	if handoff.Version != handoffVersion || handoff.ID == "" {
		return Handoff{}, ErrIntegrity
	}
	provided, err := hex.DecodeString(handoff.MAC)
	if err != nil {
		return Handoff{}, ErrIntegrity
	}
	handoff.MAC = ""
	unsigned, err := json.Marshal(handoff)
	if err != nil {
		return Handoff{}, ErrIntegrity
	}
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write(unsigned)
	if !hmac.Equal(provided, mac.Sum(nil)) {
		return Handoff{}, ErrIntegrity
	}
	handoff.MAC = hex.EncodeToString(provided)
	return handoff, nil
}

func boundedError(err error) string {
	if err == nil {
		return ""
	}
	text := err.Error()
	if len(text) > 1000 {
		text = text[:1000]
	}
	return text
}
