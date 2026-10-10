package update

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/GerardSmit/multirunner/internal/backup"
)

const updateRestartOptionsEnv = "MULTIRUNNER_TEST_UPDATE_RESTART_OPTIONS"

type updateRestartOptions struct {
	Root        string
	HandoffKey  []byte
	TrustedRoot []byte
	Installed   Installed
	Policy      Policy
	Now         time.Time
}

func TestUpdatedWorkerRestartHelper(t *testing.T) {
	optionsPath := os.Getenv(updateRestartOptionsEnv)
	if optionsPath == "" {
		return
	}
	data, err := os.ReadFile(optionsPath)
	if err != nil {
		t.Fatal(err)
	}
	var saved updateRestartOptions
	if err := json.Unmarshal(data, &saved); err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	_, committed, err := CommitHealthy(t.Context(), ActivationOptions{
		Root: saved.Root, FallbackPath: executable,
		HandoffKey: saved.HandoffKey, TrustedRoot: saved.TrustedRoot,
		Installed: saved.Installed, Policy: saved.Policy,
		Now: func() time.Time { return saved.Now },
	}, &fakeStore{}, saved.Now)
	if err != nil || !committed {
		t.Fatalf("CommitHealthy after restart = %v, %v", committed, err)
	}
}

type fakeSource struct {
	bundle      MetadataBundle
	artifact    []byte
	bundleCalls int
	targetCalls int
}

func (s *fakeSource) Bundle(context.Context, int) (MetadataBundle, error) {
	s.bundleCalls++
	return s.bundle, nil
}

func (s *fakeSource) OpenTarget(context.Context, string, TargetFile) (io.ReadCloser, error) {
	s.targetCalls++
	return io.NopCloser(bytes.NewReader(s.artifact)), nil
}

type fakeStore struct {
	items map[string]Metadata
}

func (s *fakeStore) CreateUpdate(_ context.Context, metadata Metadata) error {
	if s.items == nil {
		s.items = map[string]Metadata{}
	}
	if _, exists := s.items[metadata.ID]; exists {
		return ErrConflict
	}
	s.items[metadata.ID] = metadata
	return nil
}

func (s *fakeStore) UpdateUpdate(_ context.Context, metadata Metadata) error {
	if _, exists := s.items[metadata.ID]; !exists {
		return ErrNotFound
	}
	s.items[metadata.ID] = metadata
	return nil
}

func (s *fakeStore) Update(_ context.Context, id string) (Metadata, error) {
	metadata, ok := s.items[id]
	if !ok {
		return Metadata{}, ErrNotFound
	}
	return metadata, nil
}

func (s *fakeStore) ListUpdates(_ context.Context, limit int) ([]Metadata, error) {
	items := make([]Metadata, 0, len(s.items))
	for _, metadata := range s.items {
		items = append(items, metadata)
	}
	sort.Slice(items, func(i, j int) bool {
		return items[i].CreatedAt.After(items[j].CreatedAt)
	})
	if len(items) > limit {
		items = items[:limit]
	}
	return items, nil
}

func TestStageActivateCommitAndResolveWorker(t *testing.T) {
	fixture := newUpdateServiceFixture(t)
	metadata, err := fixture.service.Stage(
		t.Context(), "update-1", Request{Version: "v1.2.0"},
	)
	if err != nil {
		t.Fatal(err)
	}
	if metadata.State != StateStaged || fixture.source.targetCalls != 1 {
		t.Fatalf("staged metadata = %+v, target calls = %d", metadata, fixture.source.targetCalls)
	}
	if active, err := HasActiveHandoff(fixture.root, fixture.key); err != nil || !active {
		t.Fatalf("active staged handoff = %v, %v", active, err)
	}
	if _, err := os.Stat(metadata.ArtifactPath); err != nil {
		t.Fatal(err)
	}

	options := fixture.activationOptions()
	options.Now = func() time.Time { return fixture.now.Add(time.Minute) }
	handoff, workerPath, activated, err := ActivatePending(t.Context(), options)
	if err != nil || !activated {
		t.Fatalf("activate = %v, %v", activated, err)
	}
	if workerPath != metadata.ArtifactPath || handoff.RollbackPath != fixture.fallback {
		t.Fatalf("worker = %q, handoff = %+v", workerPath, handoff)
	}

	committed, ok, err := CommitHealthy(
		t.Context(), fixture.healthyOptions(metadata.ArtifactPath),
		fixture.store, fixture.now.Add(2*time.Minute),
	)
	if err != nil || !ok {
		t.Fatalf("commit = %v, %v", ok, err)
	}
	if committed.State != StateSucceeded {
		t.Fatalf("committed state = %s", committed.State)
	}
	if active, err := HasActiveHandoff(fixture.root, fixture.key); err != nil || active {
		t.Fatalf("active committed handoff = %v, %v", active, err)
	}
	resolveOptions := fixture.activationOptions()
	resolveOptions.Now = func() time.Time { return fixture.now.Add(3 * time.Minute) }
	resolved, err := ResolveWorker(resolveOptions)
	if err != nil {
		t.Fatal(err)
	}
	if resolved != metadata.ArtifactPath {
		t.Fatalf("resolved worker = %q", resolved)
	}
}

func TestUpdatedWorkerConfirmsThroughRealRestartPath(t *testing.T) {
	fixture := newUpdateServiceFixture(t)
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	artifact, err := os.ReadFile(executable)
	if err != nil {
		t.Fatal(err)
	}
	fixture.setArtifact(t, artifact)
	metadata, err := fixture.service.Stage(t.Context(), "restart-update", Request{})
	if err != nil {
		t.Fatal(err)
	}
	handoff, _, activated, err := ActivatePending(t.Context(), fixture.activationOptions())
	if err != nil || !activated {
		t.Fatalf("ActivatePending = %v, %v", activated, err)
	}
	running := fixture.service.installed
	running.Version = handoff.VersionName
	running.Commit = handoff.Commit
	saved := updateRestartOptions{
		Root: fixture.root, HandoffKey: fixture.key,
		TrustedRoot: fixture.service.trustedRoot,
		Installed:   running, Policy: fixture.service.policy,
		Now: fixture.now.Add(time.Minute),
	}
	optionsData, err := json.Marshal(saved)
	if err != nil {
		t.Fatal(err)
	}
	optionsPath := filepath.Join(t.TempDir(), "restart-options.json")
	if err := os.WriteFile(optionsPath, optionsData, 0o600); err != nil {
		t.Fatal(err)
	}
	command := exec.Command(metadata.ArtifactPath,
		"-test.run=^TestUpdatedWorkerRestartHelper$")
	command.Env = append(os.Environ(), updateRestartOptionsEnv+"="+optionsPath)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("updated worker restart: %v\n%s", err, output)
	}
	if _, err := os.Stat(filepath.Join(
		fixture.root, "restart-update.succeeded.json",
	)); err != nil {
		t.Fatalf("successful restart handoff was not archived: %v", err)
	}
}

func TestCommitHealthyRejectsWrongRunningBuildOrExecutable(t *testing.T) {
	fixture := newUpdateServiceFixture(t)
	metadata, err := fixture.service.Stage(t.Context(), "wrong-build", Request{})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, activated, err := ActivatePending(
		t.Context(), fixture.activationOptions(),
	); err != nil || !activated {
		t.Fatalf("ActivatePending = %v, %v", activated, err)
	}

	wrongBuild := fixture.healthyOptions(metadata.ArtifactPath)
	wrongBuild.Installed.Version = fixture.service.installed.Version
	wrongExecutable := fixture.healthyOptions(metadata.ArtifactPath)
	wrongExecutable.FallbackPath = fixture.fallback
	for name, options := range map[string]ActivationOptions{
		"build":      wrongBuild,
		"executable": wrongExecutable,
	} {
		t.Run(name, func(t *testing.T) {
			if _, committed, err := CommitHealthy(
				t.Context(), options, fixture.store, fixture.now,
			); !errors.Is(err, ErrIntegrity) || committed {
				t.Fatalf("CommitHealthy = %v, %v", committed, err)
			}
		})
	}
}

func TestRollbackRestoresVerifiedDatabaseWhenSchemaBecameIncompatible(t *testing.T) {
	fixture := newUpdateServiceFixture(t)
	fixture.setTargetSchemaMax(t, 13)
	databasePath := filepath.Join(t.TempDir(), "history.db")
	if err := os.WriteFile(databasePath, []byte("schema-12"), 0o600); err != nil {
		t.Fatal(err)
	}
	restores := 0
	options := fixture.activationOptions()
	options.DatabasePath = databasePath
	options.BackupDatabase = func(
		_ context.Context, destination string,
	) (backup.Validation, error) {
		data, err := os.ReadFile(databasePath)
		if err == nil {
			err = os.WriteFile(destination, data, 0o600)
		}
		return backup.Validation{
			QuickCheck: "ok", SchemaVersion: 12, HostID: "host-1",
		}, err
	}
	options.ValidateDatabase = fixtureDatabaseValidator
	options.RestoreDatabase = func(_ context.Context, source *os.File, destination string) error {
		restores++
		if _, err := source.Seek(0, 0); err != nil {
			return err
		}
		data, err := io.ReadAll(source)
		if err != nil {
			return err
		}
		return os.WriteFile(destination, data, 0o600)
	}
	if _, err := fixture.service.Stage(t.Context(), "schema-update", Request{}); err != nil {
		t.Fatal(err)
	}
	handoff, _, activated, err := ActivatePending(t.Context(), options)
	if err != nil || !activated {
		t.Fatalf("ActivatePending = %v, %v", activated, err)
	}
	if handoff.DatabaseSHA256 == "" || handoff.DatabaseSchema != 12 {
		t.Fatalf("database backup evidence = %+v", handoff)
	}
	if err := os.WriteFile(databasePath, []byte("schema-13"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, rolledBack, err := RollbackPending(
		options, "updated worker failed", fixture.now.Add(time.Minute),
	); err != nil || !rolledBack {
		t.Fatalf("RollbackPending = %v, %v", rolledBack, err)
	}
	data, err := os.ReadFile(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	if restores != 1 || string(data) != "schema-12" {
		t.Fatalf("database restores=%d contents=%q", restores, data)
	}
}

func TestActivationRollbackKeepsPreviousWorker(t *testing.T) {
	fixture := newUpdateServiceFixture(t)
	first, err := fixture.service.Stage(t.Context(), "update-1", Request{})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := ActivatePending(t.Context(), fixture.activationOptions()); err != nil {
		t.Fatal(err)
	}
	handoff, worker, rolledBack, err := RollbackPending(
		fixture.activationOptions(),
		"worker exited before update health confirmation",
		fixture.now.Add(time.Minute),
	)
	if err != nil || !rolledBack {
		t.Fatalf("rollback = %v, %v", rolledBack, err)
	}
	if worker != fixture.fallback || handoff.State != StateRolledBack {
		t.Fatalf("worker = %q, handoff = %+v", worker, handoff)
	}
	if _, err := os.Stat(first.ArtifactPath); err != nil {
		t.Fatalf("staged artifact should remain for evidence: %v", err)
	}
	if _, ok, err := CommitHealthy(
		t.Context(), fixture.activationOptions(), fixture.store, fixture.now.Add(2*time.Minute),
	); err != nil || !ok {
		t.Fatalf("record rollback = %v, %v", ok, err)
	}
	resolved, err := ResolveWorker(fixture.activationOptions())
	if err != nil || resolved != fixture.fallback {
		t.Fatalf("resolved worker = %q, %v", resolved, err)
	}
}

func TestStageWithoutTrustNeverFetchesArtifact(t *testing.T) {
	fixture := newUpdateServiceFixture(t)
	fixture.service.trustedRoot = nil
	_, err := fixture.service.Stage(t.Context(), "update-1", Request{})
	if !errors.Is(err, ErrTrustMissing) {
		t.Fatalf("stage error = %v", err)
	}
	if fixture.source.bundleCalls != 0 || fixture.source.targetCalls != 0 {
		t.Fatalf("trustless stage fetched bundle=%d target=%d",
			fixture.source.bundleCalls, fixture.source.targetCalls)
	}
}

func TestStageRejectsTamperedArtifact(t *testing.T) {
	fixture := newUpdateServiceFixture(t)
	fixture.source.artifact = []byte("tampered")
	_, err := fixture.service.Stage(t.Context(), "update-1", Request{})
	if !errors.Is(err, ErrIntegrity) {
		t.Fatalf("stage error = %v", err)
	}
	if fixture.store.items["update-1"].State != StateFailed {
		t.Fatalf("metadata = %+v", fixture.store.items["update-1"])
	}
	if _, statErr := os.Stat(filepath.Join(fixture.root, "active.json")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("active handoff exists after failed stage: %v", statErr)
	}
}

func TestResolveWorkerRejectsTamperedCurrentArtifact(t *testing.T) {
	fixture := newUpdateServiceFixture(t)
	metadata, err := fixture.service.Stage(t.Context(), "update-1", Request{})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := ActivatePending(t.Context(), fixture.activationOptions()); err != nil {
		t.Fatal(err)
	}
	if _, _, err := CommitHealthy(
		t.Context(), fixture.healthyOptions(metadata.ArtifactPath), fixture.store, fixture.now,
	); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(metadata.ArtifactPath, []byte("tampered"), 0o700); err != nil {
		t.Fatal(err)
	}
	_, err = ResolveWorker(fixture.activationOptions())
	if !errors.Is(err, ErrIntegrity) {
		t.Fatalf("resolve error = %v", err)
	}
}

func TestVerifiedArtifactHandlePreventsWindowsReplacement(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows share-mode guarantee")
	}
	fixture := newUpdateServiceFixture(t)
	metadata, err := fixture.service.Stage(t.Context(), "update-1", Request{})
	if err != nil {
		t.Fatal(err)
	}
	file, err := OpenVerifiedArtifact(
		metadata.ArtifactPath, metadata.SizeBytes, metadata.SHA256,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(metadata.ArtifactPath, []byte("replacement"), 0o700); err == nil {
		_ = file.Close()
		t.Fatal("verified artifact was replaceable while its execution handle was held")
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestActivationRejectsLocallyResignedForgedHandoff(t *testing.T) {
	fixture := newUpdateServiceFixture(t)
	if _, err := fixture.service.Stage(t.Context(), "update-1", Request{}); err != nil {
		t.Fatal(err)
	}
	handoff, err := readHandoff(activePath(fixture.root), fixture.key)
	if err != nil {
		t.Fatal(err)
	}
	handoff.VersionName = "v9.9.9"
	if err := writeSignedJSON(activePath(fixture.root), &handoff, fixture.key, false); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := ActivatePending(
		t.Context(), fixture.activationOptions(),
	); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("forged handoff activation error = %v", err)
	}
}

func TestResolveWorkerRejectsLocallyResignedForgedCurrent(t *testing.T) {
	fixture := newUpdateServiceFixture(t)
	metadata, err := fixture.service.Stage(t.Context(), "update-1", Request{})
	if err != nil {
		t.Fatal(err)
	}
	options := fixture.activationOptions()
	if _, _, _, err := ActivatePending(t.Context(), options); err != nil {
		t.Fatal(err)
	}
	if _, _, err := CommitHealthy(
		t.Context(), fixture.healthyOptions(metadata.ArtifactPath),
		fixture.store, fixture.now.Add(time.Minute),
	); err != nil {
		t.Fatal(err)
	}
	current, _, err := readCurrent(currentPath(fixture.root), fixture.key)
	if err != nil {
		t.Fatal(err)
	}
	current.Commit = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	if err := writeSignedJSON(currentPath(fixture.root), &current, fixture.key, false); err != nil {
		t.Fatal(err)
	}
	if _, err := ResolveWorker(options); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("forged current resolution error = %v", err)
	}
}

func TestActivationVerifiesFullRootRotationChain(t *testing.T) {
	fixture := newUpdateServiceFixture(t)
	oldRoot := decodeRoot(t, fixture.repository.root)
	newRootSigner := newSigner(t)
	nextRoot := oldRoot.Signed
	nextRoot.Version = 2
	nextRoot.Keys[newRootSigner.id] = signerKey(newRootSigner)
	nextRoot.Roles["root"] = Role{KeyIDs: []string{newRootSigner.id}, Threshold: 1}
	fixture.source.bundle.RootUpdates = [][]byte{
		signEnvelope(t, nextRoot, fixture.repository.signers["root"], newRootSigner),
	}

	if _, err := fixture.service.Stage(t.Context(), "update-1", Request{}); err != nil {
		t.Fatal(err)
	}
	handoff, _, activated, err := ActivatePending(t.Context(), fixture.activationOptions())
	if err != nil || !activated {
		t.Fatalf("rotated activation = %v, %v", activated, err)
	}
	if handoff.Trust.RootVersion != 2 || len(handoff.Bundle.RootUpdates) != 1 {
		t.Fatalf("rotated handoff trust = %+v, roots = %d",
			handoff.Trust, len(handoff.Bundle.RootUpdates))
	}
}

type updateServiceFixture struct {
	service    *Service
	source     *fakeSource
	store      *fakeStore
	repository *testRepository
	root       string
	fallback   string
	key        []byte
	now        time.Time
}

func (f updateServiceFixture) activationOptions() ActivationOptions {
	return ActivationOptions{
		Root: f.root, FallbackPath: f.fallback, HandoffKey: f.key,
		TrustedRoot: f.service.trustedRoot, Installed: f.service.installed,
		Policy: f.service.policy, Now: func() time.Time { return f.now },
	}
}

func (f updateServiceFixture) healthyOptions(artifactPath string) ActivationOptions {
	options := f.activationOptions()
	options.FallbackPath = artifactPath
	options.Installed.Version = "v1.2.0"
	options.Installed.Commit = strings.Repeat("1", 40)
	return options
}

func (f updateServiceFixture) setArtifact(t *testing.T, artifact []byte) {
	t.Helper()
	f.source.artifact = artifact
	var targets Envelope[Targets]
	mustJSON(t, f.source.bundle.Targets, &targets)
	digest := sha256.Sum256(artifact)
	for path, target := range targets.Signed.Targets {
		target.Length = int64(len(artifact))
		target.Hashes["sha256"] = hex.EncodeToString(digest[:])
		targets.Signed.Targets[path] = target
	}
	f.source.bundle.Targets = signEnvelope(t, targets.Signed, f.repository.signers["targets"])
	f.relinkBundle(t)
}

func (f updateServiceFixture) setTargetSchemaMax(t *testing.T, schemaMax int) {
	t.Helper()
	var targets Envelope[Targets]
	mustJSON(t, f.source.bundle.Targets, &targets)
	for path, target := range targets.Signed.Targets {
		target.Custom.SchemaMax = schemaMax
		targets.Signed.Targets[path] = target
	}
	f.source.bundle.Targets = signEnvelope(t, targets.Signed, f.repository.signers["targets"])
	f.relinkBundle(t)
}

func (f updateServiceFixture) relinkBundle(t *testing.T) {
	t.Helper()
	var snapshot Envelope[Snapshot]
	mustJSON(t, f.source.bundle.Snapshot, &snapshot)
	snapshot.Signed.Meta["targets.json"] = linkedMeta(5, f.source.bundle.Targets)
	f.source.bundle.Snapshot = signEnvelope(t, snapshot.Signed, f.repository.signers["snapshot"])
	var timestamp Envelope[Timestamp]
	mustJSON(t, f.source.bundle.Timestamp, &timestamp)
	timestamp.Signed.Meta["snapshot.json"] = linkedMeta(4, f.source.bundle.Snapshot)
	f.source.bundle.Timestamp = signEnvelope(t, timestamp.Signed, f.repository.signers["timestamp"])
}

func fixtureDatabaseValidator(
	_ context.Context, path string, _ bool,
) (backup.Validation, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return backup.Validation{}, err
	}
	schema := 0
	switch string(data) {
	case "schema-12":
		schema = 12
	case "schema-13":
		schema = 13
	default:
		return backup.Validation{}, errors.New("invalid fixture database")
	}
	return backup.Validation{
		QuickCheck: "ok", SchemaVersion: schema, HostID: "host-1",
	}, nil
}

func newUpdateServiceFixture(t *testing.T) updateServiceFixture {
	t.Helper()
	repository := newTestRepository(t)
	root := t.TempDir()
	fallback := filepath.Join(t.TempDir(), "multirunner")
	if runtime.GOOS == "windows" {
		fallback += ".exe"
	}
	if err := os.WriteFile(fallback, []byte("installed executable"), 0o700); err != nil {
		t.Fatal(err)
	}
	source := &fakeSource{
		bundle: MetadataBundle{
			Timestamp: repository.timestamp,
			Snapshot:  repository.snapshot,
			Targets:   repository.targets,
		},
		artifact: repository.artifact,
	}
	store := &fakeStore{}
	key := bytes.Repeat([]byte{7}, 32)
	installed := repository.installed
	installed.OS = runtime.GOOS
	installed.Arch = runtime.GOARCH
	var targets Envelope[Targets]
	mustJSON(t, repository.targets, &targets)
	for path, target := range targets.Signed.Targets {
		delete(targets.Signed.Targets, path)
		target.Custom.OS = installed.OS
		target.Custom.Arch = installed.Arch
		targetPath := "multirunner_v1.2.0_" + installed.OS + "_" + installed.Arch
		if installed.OS == "windows" {
			targetPath += ".exe"
		}
		targets.Signed.Targets[targetPath] = target
	}
	source.bundle.Targets = signEnvelope(t, targets.Signed, repository.signers["targets"])
	var snapshot Envelope[Snapshot]
	mustJSON(t, source.bundle.Snapshot, &snapshot)
	snapshot.Signed.Meta["targets.json"] = linkedMeta(5, source.bundle.Targets)
	source.bundle.Snapshot = signEnvelope(t, snapshot.Signed, repository.signers["snapshot"])
	var timestamp Envelope[Timestamp]
	mustJSON(t, source.bundle.Timestamp, &timestamp)
	timestamp.Signed.Meta["snapshot.json"] = linkedMeta(4, source.bundle.Snapshot)
	source.bundle.Timestamp = signEnvelope(t, timestamp.Signed, repository.signers["timestamp"])

	service, err := New(Options{
		Root: root, TrustedRoot: repository.root,
		Installed: installed, HostID: "host-1", Policy: repository.policy,
		Source: source, Store: store, HandoffKey: key,
		Now: func() time.Time { return repository.now },
	})
	if err != nil {
		t.Fatal(err)
	}
	return updateServiceFixture{
		service: service, source: source, store: store,
		repository: repository, root: root, fallback: fallback,
		key: key, now: repository.now,
	}
}
