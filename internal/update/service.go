package update

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
	"regexp"
	"strings"
	"time"
)

const (
	handoffVersion           = 1
	defaultArtifactRetention = 7 * 24 * time.Hour
)

var updateIDPattern = regexp.MustCompile(`\A[A-Za-z0-9][A-Za-z0-9_-]{0,127}\z`)

type Options struct {
	Root              string
	TrustedRoot       []byte
	Installed         Installed
	HostID            string
	Policy            Policy
	Source            Source
	Store             Store
	HandoffKey        []byte
	Now               func() time.Time
	ArtifactRetention time.Duration
}

type Service struct {
	root              string
	trustedRoot       []byte
	installed         Installed
	hostID            string
	policy            Policy
	source            Source
	store             Store
	handoffKey        []byte
	now               func() time.Time
	artifactRetention time.Duration
}

type Handoff struct {
	Version            int             `json:"version"`
	ID                 string          `json:"id"`
	CommandID          string          `json:"command_id"`
	State              State           `json:"state"`
	CreatedAt          time.Time       `json:"created_at"`
	UpdatedAt          time.Time       `json:"updated_at"`
	ActivatedAt        time.Time       `json:"activated_at,omitempty"`
	CompletedAt        time.Time       `json:"completed_at,omitempty"`
	TargetPath         string          `json:"target_path"`
	ArtifactPath       string          `json:"artifact_path"`
	RollbackPath       string          `json:"rollback_path,omitempty"`
	RollbackUpdateID   string          `json:"rollback_update_id,omitempty"`
	RollbackSizeBytes  int64           `json:"rollback_size_bytes,omitempty"`
	RollbackSHA256     string          `json:"rollback_sha256,omitempty"`
	VersionName        string          `json:"version_name"`
	Commit             string          `json:"commit"`
	HostID             string          `json:"host_id"`
	SizeBytes          int64           `json:"size_bytes"`
	SHA256             string          `json:"sha256"`
	SchemaMin          int             `json:"schema_min"`
	SchemaMax          int             `json:"schema_max"`
	APIVersion         string          `json:"api_version"`
	PreUpdateInstalled Installed       `json:"pre_update_installed"`
	DatabasePath       string          `json:"database_path,omitempty"`
	DatabaseBackupPath string          `json:"database_backup_path,omitempty"`
	DatabaseSizeBytes  int64           `json:"database_size_bytes,omitempty"`
	DatabaseSHA256     string          `json:"database_sha256,omitempty"`
	DatabaseSchema     int             `json:"database_schema,omitempty"`
	DatabaseHostID     string          `json:"database_host_id,omitempty"`
	Trust              TrustedState    `json:"trust"`
	Root               json.RawMessage `json:"root"`
	Bundle             MetadataBundle  `json:"bundle"`
	RollbackCurrent    *Current        `json:"rollback_current,omitempty"`
	RollbackReason     string          `json:"rollback_reason,omitempty"`
	Error              string          `json:"error,omitempty"`
	MAC                string          `json:"mac"`
}

type Current struct {
	Version      int            `json:"version"`
	UpdateID     string         `json:"update_id"`
	ArtifactPath string         `json:"artifact_path"`
	TargetPath   string         `json:"target_path"`
	VersionName  string         `json:"version_name"`
	Commit       string         `json:"commit"`
	SizeBytes    int64          `json:"size_bytes"`
	SHA256       string         `json:"sha256"`
	SchemaMin    int            `json:"schema_min"`
	SchemaMax    int            `json:"schema_max"`
	APIVersion   string         `json:"api_version"`
	Bundle       MetadataBundle `json:"bundle"`
	VerifiedAt   time.Time      `json:"verified_at"`
	ActivatedAt  time.Time      `json:"activated_at"`
	MAC          string         `json:"mac"`
}

func New(options Options) (*Service, error) {
	if strings.TrimSpace(options.Root) == "" {
		return nil, errors.New("update root is required")
	}
	if options.Source == nil || options.Store == nil {
		return nil, errors.New("update source and store are required")
	}
	if len(options.HandoffKey) < 32 {
		return nil, errors.New("update handoff key must be at least 32 bytes")
	}
	if len(bytesTrimSpace(options.TrustedRoot)) == 0 {
		return nil, ErrTrustMissing
	}
	if _, err := VerifyEmbeddedRoot(options.TrustedRoot); err != nil {
		return nil, err
	}
	if err := validatePolicy(options.Policy); err != nil {
		return nil, err
	}
	if strings.TrimSpace(options.Installed.Version) == "" ||
		strings.TrimSpace(options.Installed.OS) == "" ||
		strings.TrimSpace(options.Installed.Arch) == "" ||
		options.Installed.SchemaVersion < 1 ||
		strings.TrimSpace(options.HostID) == "" {
		return nil, errors.New("installed update identity is incomplete")
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	if options.ArtifactRetention <= 0 {
		options.ArtifactRetention = defaultArtifactRetention
	}
	root, err := filepath.Abs(options.Root)
	if err != nil {
		return nil, fmt.Errorf("resolve update root: %w", err)
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, fmt.Errorf("create update root: %w", err)
	}
	return &Service{
		root: filepath.Clean(root), trustedRoot: append([]byte(nil), options.TrustedRoot...),
		installed: options.Installed, hostID: options.HostID, policy: options.Policy,
		source: options.Source, store: options.Store,
		handoffKey: append([]byte(nil), options.HandoffKey...),
		now:        options.Now, artifactRetention: options.ArtifactRetention,
	}, nil
}

func ValidateRequest(request Request) error {
	if request.Version != "" && !validReleaseVersion(request.Version) {
		return errors.New("update version must be a stable semantic version")
	}
	return nil
}

func (s *Service) Stage(ctx context.Context, id string, request Request) (Metadata, error) {
	id = strings.TrimSpace(id)
	if !updateIDPattern.MatchString(id) {
		return Metadata{}, errors.New("update ID is invalid")
	}
	if err := ValidateRequest(request); err != nil {
		return Metadata{}, err
	}
	if len(bytesTrimSpace(s.trustedRoot)) == 0 {
		return Metadata{}, ErrTrustMissing
	}
	createMetadata := true
	if existing, err := s.store.Update(ctx, id); err == nil {
		switch existing.State {
		case StateStaged, StateActivating, StateSucceeded:
			return existing, nil
		case StateStaging:
			createMetadata = false
		case StateRolledBack, StateFailed:
			return existing, nil
		default:
			return Metadata{}, ErrIntegrity
		}
	} else if !errors.Is(err, ErrNotFound) {
		return Metadata{}, err
	}
	if existing, err := readHandoff(activePath(s.root), s.handoffKey); err == nil {
		switch existing.State {
		case StateSucceeded, StateRolledBack, StateFailed:
			if err := archiveHandoff(s.root, existing); err != nil {
				return Metadata{}, err
			}
		case StateStaged:
			if existing.ID != id {
				return Metadata{}, ErrConflict
			}
			metadata := existing.Metadata()
			if err := s.store.UpdateUpdate(ctx, metadata); errors.Is(err, ErrNotFound) {
				if err := s.store.CreateUpdate(ctx, metadata); err != nil {
					return Metadata{}, err
				}
			} else if err != nil {
				return Metadata{}, err
			}
			return metadata, nil
		default:
			return Metadata{}, ErrConflict
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return Metadata{}, err
	}

	trustedRoot, trustedState, trustedChain, err := s.loadTrust()
	if err != nil {
		return Metadata{}, err
	}
	bundle, err := s.source.Bundle(ctx, trustedState.RootVersion)
	if err != nil {
		return Metadata{}, fmt.Errorf("fetch update metadata: %w", err)
	}
	verified, err := Verify(
		trustedRoot, bundle, trustedState, s.installed, s.policy, s.now().UTC(),
	)
	if err != nil {
		return Metadata{}, err
	}
	if request.Version != "" && verified.Target.Custom.Version != request.Version {
		return Metadata{}, fmt.Errorf("%w: requested %s, trusted target is %s",
			ErrNotFound, request.Version, verified.Target.Custom.Version)
	}
	metadata := metadataFromVerified(id, s.hostID, verified, s.now().UTC())
	if createMetadata {
		if err := s.store.CreateUpdate(ctx, metadata); err != nil {
			return Metadata{}, err
		}
	} else {
		if err := s.store.UpdateUpdate(ctx, metadata); err != nil {
			return Metadata{}, err
		}
	}
	fullChain := append(append([][]byte(nil), trustedChain...), bundle.RootUpdates...)
	if err := s.persistTrust(verified, fullChain); err != nil {
		return Metadata{}, s.fail(ctx, metadata, err)
	}

	artifactPath, err := derivedArtifactPath(s.root, id, s.installed.OS)
	if err != nil {
		return Metadata{}, s.fail(ctx, metadata, err)
	}
	metadata.ArtifactPath = artifactPath
	reader, err := s.source.OpenTarget(ctx, verified.TargetPath, verified.Target)
	if err != nil {
		return Metadata{}, s.fail(ctx, metadata, fmt.Errorf("open update target: %w", err))
	}
	defer reader.Close()
	if err := stageArtifact(ctx, artifactPath, reader, verified.Target); err != nil {
		return Metadata{}, s.fail(ctx, metadata, err)
	}
	activationBundle := bundle
	activationBundle.RootUpdates = fullChain
	handoff := handoffFromMetadata(metadata, verified, activationBundle)
	handoff.ArtifactPath = artifactPath
	handoff.State = StateStaged
	handoff.UpdatedAt = s.now().UTC()
	metadata.HandoffPath = activePath(s.root)
	if err := writeSignedJSON(metadata.HandoffPath, &handoff, s.handoffKey, true); err != nil {
		_ = os.Remove(artifactPath)
		return Metadata{}, s.fail(ctx, metadata, err)
	}
	metadata.State = StateStaged
	metadata.UpdatedAt = s.now().UTC()
	if err := s.store.UpdateUpdate(ctx, metadata); err != nil {
		_ = os.Remove(metadata.HandoffPath)
		_ = os.Remove(artifactPath)
		return Metadata{}, s.fail(ctx, metadata, err)
	}
	return metadata, nil
}

func (s *Service) Metadata(ctx context.Context, id string) (Metadata, error) {
	if metadata, err := s.store.Update(ctx, id); err == nil {
		return metadata, nil
	} else if !errors.Is(err, ErrNotFound) {
		return Metadata{}, err
	}
	handoff, err := readHandoff(activePath(s.root), s.handoffKey)
	if err != nil {
		return Metadata{}, err
	}
	if handoff.ID != id {
		return Metadata{}, ErrNotFound
	}
	return handoff.Metadata(), nil
}

func (s *Service) Exists(ctx context.Context, id string) (bool, error) {
	_, err := s.Metadata(ctx, id)
	if errors.Is(err, ErrNotFound) || errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}

func (s *Service) List(ctx context.Context, limit int) ([]Metadata, error) {
	return s.store.ListUpdates(ctx, limit)
}

func (s *Service) Prune(ctx context.Context) (int, error) {
	current, _, err := readCurrent(currentPath(s.root), s.handoffKey)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return 0, err
	}
	entries, err := os.ReadDir(s.root)
	if err != nil {
		return 0, err
	}
	cutoff := s.now().UTC().Add(-s.artifactRetention)
	count := 0
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") ||
			entry.Name() == filepath.Base(activePath(s.root)) ||
			entry.Name() == filepath.Base(currentPath(s.root)) ||
			entry.Name() == filepath.Base(trustPath(s.root)) {
			continue
		}
		handoff, readErr := readHandoff(filepath.Join(s.root, entry.Name()), s.handoffKey)
		if readErr != nil || handoff.CompletedAt.IsZero() || handoff.CompletedAt.After(cutoff) ||
			handoff.ID == current.UpdateID {
			continue
		}
		if handoff.ArtifactPath != "" {
			_ = os.Remove(handoff.ArtifactPath)
		}
		if err := os.Remove(filepath.Join(s.root, entry.Name())); err != nil {
			return count, err
		}
		count++
	}
	return count, nil
}

func (s *Service) fail(ctx context.Context, metadata Metadata, cause error) error {
	metadata.State = StateFailed
	metadata.Error = boundedError(cause)
	metadata.CompletedAt = s.now().UTC()
	metadata.UpdatedAt = metadata.CompletedAt
	if err := s.store.UpdateUpdate(ctx, metadata); err != nil {
		return errors.Join(cause, err)
	}
	return cause
}

func (s *Service) loadTrust() ([]byte, TrustedState, [][]byte, error) {
	var persisted struct {
		Root  json.RawMessage `json:"root"`
		State TrustedState    `json:"state"`
		Chain [][]byte        `json:"chain,omitempty"`
		MAC   string          `json:"mac"`
	}
	if err := readSignedJSON(trustPath(s.root), &persisted, s.handoffKey); err == nil {
		return append([]byte(nil), persisted.Root...), persisted.State,
			cloneByteSlices(persisted.Chain), nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, TrustedState{}, nil, err
	}
	root, err := decodeEnvelope[Root](s.trustedRoot)
	if err != nil {
		return nil, TrustedState{}, nil, err
	}
	return append([]byte(nil), s.trustedRoot...), TrustedState{
		RootVersion: root.Signed.Version,
	}, nil, nil
}

func (s *Service) persistTrust(verified Verified, chain [][]byte) error {
	root, err := json.Marshal(verified.Root)
	if err != nil {
		return err
	}
	state := struct {
		Root  json.RawMessage `json:"root"`
		State TrustedState    `json:"state"`
		Chain [][]byte        `json:"chain,omitempty"`
		MAC   string          `json:"mac"`
	}{
		Root: root, State: verified.NextState, Chain: cloneByteSlices(chain),
	}
	return writeSignedJSON(trustPath(s.root), &state, s.handoffKey, false)
}

func metadataFromVerified(id, hostID string, verified Verified, now time.Time) Metadata {
	return Metadata{
		ID: id, CommandID: id, State: StateStaging,
		CreatedAt: now, UpdatedAt: now,
		Version: verified.Target.Custom.Version, Commit: verified.Target.Custom.Commit,
		HostID:     hostID,
		TargetPath: verified.TargetPath, SizeBytes: verified.Target.Length,
		SHA256:    verified.Target.Hashes["sha256"],
		SchemaMin: verified.Target.Custom.SchemaMin, SchemaMax: verified.Target.Custom.SchemaMax,
		APIVersion: verified.Target.Custom.APIVersion,
	}
}

func handoffFromMetadata(
	metadata Metadata,
	verified Verified,
	bundle MetadataBundle,
) Handoff {
	root, _ := json.Marshal(verified.Root)
	return Handoff{
		Version: handoffVersion, ID: metadata.ID, CommandID: metadata.CommandID,
		State: metadata.State, CreatedAt: metadata.CreatedAt, UpdatedAt: metadata.UpdatedAt,
		TargetPath: metadata.TargetPath, VersionName: metadata.Version, Commit: metadata.Commit,
		HostID:    metadata.HostID,
		SizeBytes: metadata.SizeBytes, SHA256: metadata.SHA256,
		SchemaMin: metadata.SchemaMin, SchemaMax: metadata.SchemaMax,
		APIVersion: metadata.APIVersion, Trust: verified.NextState, Root: root,
		Bundle: bundle,
	}
}

func (h Handoff) Metadata() Metadata {
	return Metadata{
		ID: h.ID, CommandID: h.CommandID, State: h.State,
		CreatedAt: h.CreatedAt, UpdatedAt: h.UpdatedAt,
		ActivatedAt: h.ActivatedAt, CompletedAt: h.CompletedAt,
		Version: h.VersionName, Commit: h.Commit, HostID: h.HostID, TargetPath: h.TargetPath,
		SizeBytes: h.SizeBytes, SHA256: h.SHA256,
		SchemaMin: h.SchemaMin, SchemaMax: h.SchemaMax, APIVersion: h.APIVersion,
		ArtifactPath: h.ArtifactPath, HandoffPath: activePath(filepath.Dir(h.ArtifactPath)),
		RollbackReason: h.RollbackReason, Error: h.Error,
	}
}

func stageArtifact(ctx context.Context, destination string, source io.Reader, target TargetFile) error {
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		return err
	}
	part := destination + ".part"
	file, err := os.OpenFile(part, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o700)
	if err != nil {
		return err
	}
	removePart := true
	defer func() {
		_ = file.Close()
		if removePart {
			_ = os.Remove(part)
		}
	}()
	hash := sha256.New()
	written, err := io.Copy(io.MultiWriter(file, hash), io.LimitReader(&contextReader{ctx: ctx, reader: source}, target.Length+1))
	if err != nil {
		return fmt.Errorf("copy update artifact: %w", err)
	}
	if written != target.Length || !strings.EqualFold(hex.EncodeToString(hash.Sum(nil)), target.Hashes["sha256"]) {
		return ErrIntegrity
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Rename(part, destination); err != nil {
		return err
	}
	removePart = false
	return syncDirectory(filepath.Dir(destination))
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r *contextReader) Read(buffer []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(buffer)
}

func activePath(root string) string {
	return filepath.Join(root, "active.json")
}

// HasActiveHandoff reports whether an update is staged or activating and still
// depends on the current console secret for integrity verification.
func HasActiveHandoff(root string, handoffKey []byte) (bool, error) {
	handoff, err := readHandoff(activePath(root), handoffKey)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return handoff.State == StateStaged || handoff.State == StateActivating, nil
}

func currentPath(root string) string {
	return filepath.Join(root, "current.json")
}

func trustPath(root string) string {
	return filepath.Join(root, "trust.json")
}

func derivedArtifactPath(root, id, goos string) (string, error) {
	if !updateIDPattern.MatchString(id) {
		return "", ErrIntegrity
	}
	extension := ""
	if goos == "windows" {
		extension = ".exe"
	}
	return filepath.Join(root, "update-"+id+extension), nil
}

func archiveHandoff(root string, handoff Handoff) error {
	return os.Rename(
		activePath(root),
		filepath.Join(root, handoff.ID+"."+string(handoff.State)+".json"),
	)
}

func writeSignedJSON(path string, value any, key []byte, exclusive bool) error {
	if len(key) < 32 {
		return ErrIntegrity
	}
	unsigned, err := json.Marshal(value)
	if err != nil {
		return err
	}
	var fields map[string]any
	if err := json.Unmarshal(unsigned, &fields); err != nil {
		return err
	}
	delete(fields, "mac")
	unsigned, err = json.Marshal(fields)
	if err != nil {
		return err
	}
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write(unsigned)
	fields["mac"] = hex.EncodeToString(mac.Sum(nil))
	data, err := json.Marshal(fields)
	if err != nil {
		return err
	}
	flags := os.O_CREATE | os.O_WRONLY
	if exclusive {
		flags |= os.O_EXCL
	} else {
		flags |= os.O_TRUNC
	}
	temp := path + ".tmp"
	if exclusive {
		temp = path
	}
	file, err := os.OpenFile(temp, flags, 0o600)
	if err != nil {
		return err
	}
	if _, err = file.Write(data); err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		if !exclusive {
			_ = os.Remove(temp)
		}
		return err
	}
	if !exclusive {
		if err := replaceFile(temp, path); err != nil {
			_ = os.Remove(temp)
			return err
		}
	}
	return syncDirectory(filepath.Dir(path))
}

func readSignedJSON(path string, value any, key []byte) error {
	if len(key) < 32 {
		return ErrIntegrity
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := rejectDuplicateKeys(data); err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return ErrIntegrity
	}
	rawMAC, ok := fields["mac"]
	if !ok {
		return ErrIntegrity
	}
	var encodedMAC string
	if json.Unmarshal(rawMAC, &encodedMAC) != nil {
		return ErrIntegrity
	}
	delete(fields, "mac")
	unsigned, err := json.Marshal(fields)
	if err != nil {
		return ErrIntegrity
	}
	expected := hmac.New(sha256.New, key)
	_, _ = expected.Write(unsigned)
	actual, err := hex.DecodeString(encodedMAC)
	if err != nil || !hmac.Equal(actual, expected.Sum(nil)) {
		return ErrIntegrity
	}
	return json.Unmarshal(data, value)
}

func readHandoff(path string, key []byte) (Handoff, error) {
	var handoff Handoff
	if err := readSignedJSON(path, &handoff, key); err != nil {
		return Handoff{}, err
	}
	if handoff.Version != handoffVersion || !updateIDPattern.MatchString(handoff.ID) {
		return Handoff{}, ErrIntegrity
	}
	return handoff, nil
}

func readCurrent(path string, key []byte) (Current, bool, error) {
	var current Current
	if err := readSignedJSON(path, &current, key); err != nil {
		return Current{}, false, err
	}
	if current.Version != handoffVersion || !updateIDPattern.MatchString(current.UpdateID) {
		return Current{}, false, ErrIntegrity
	}
	return current, true, nil
}

func boundedError(err error) string {
	if err == nil {
		return ""
	}
	const limit = 2048
	text := err.Error()
	if len(text) > limit {
		return text[:limit]
	}
	return text
}

func bytesTrimSpace(value []byte) []byte {
	return []byte(strings.TrimSpace(string(value)))
}

func cloneByteSlices(values [][]byte) [][]byte {
	result := make([][]byte, len(values))
	for index := range values {
		result[index] = append([]byte(nil), values[index]...)
	}
	return result
}
