// Package hostrunner runs ephemeral GitHub Actions runners directly on the
// host, as the user multirunner runs as, with no container or VM. It exists for
// jobs that must use the host itself: its shell, its files and its signed-in
// tools. That is also why it is dangerous: a job runs with the user's full
// access, so config validation requires a workflow allowlist for host pools.
//
// One runner install is shared by every job of a pool (pools are size 1), and
// the job's work folder is deleted after each job so nothing leaks into the
// next one. Windows only: each runner runs inside a Job Object so a kill takes
// the whole process tree (cmd.exe, Runner.Listener, Runner.Worker, job steps).
package hostrunner

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	imageversions "github.com/GerardSmit/multirunner/images"
	"github.com/GerardSmit/multirunner/internal/backend"
)

// Options configures the host backend.
type Options struct {
	// Pool names the default directories, so pools never share an install.
	Pool string
	// RunnerDir holds the extracted runner, one subdirectory per version.
	RunnerDir string
	// StateDir holds ownership records, PID records and per-job logs.
	StateDir string
	// RunnerVersion and RunnerSHA256 pin the actions/runner release. Empty uses
	// the embedded image manifest; a custom version requires its SHA256.
	RunnerVersion string
	RunnerSHA256  string
	// KeepWorkspace keeps the job's work folder after the job (debugging only).
	KeepWorkspace bool
	// StripEnvNames and StripEnvValues remove multirunner's own secrets from the
	// environment the runner (and therefore every job step) inherits: variables
	// loaded from .env or referenced as ${VAR} in the config, and any variable
	// whose value is a resolved secret.
	StripEnvNames  []string
	StripEnvValues []string
	// AllowLocalSystem permits running jobs as NT AUTHORITY\SYSTEM.
	AllowLocalSystem bool
	// Logger receives cleanup problems that must not fail a finished job.
	Logger *slog.Logger
}

// Backend runs one ephemeral runner at a time on the host.
type Backend struct {
	opt      Options
	download func(ctx context.Context, url, dst string) error
	proc     processOps
	logger   *slog.Logger

	mu     sync.Mutex
	busy   bool
	active *hostHandle
}

var (
	_ backend.Backend          = (*Backend)(nil)
	_ backend.OwnedRunnerStore = (*Backend)(nil)
)

// processOps is the platform process control the backend needs; tests replace it.
type processOps interface {
	// start starts cmd so that kill() terminates its whole process tree.
	start(cmd *exec.Cmd) (processGroup, error)
	alive(pid int) (bool, error)
	// startTime identifies a process across PID reuse (0 when unknown).
	startTime(pid int) (int64, error)
	killTree(pid int) error
}

type processGroup interface {
	kill() error
	close() error
}

// NewBackend builds the host backend for the current OS.
func NewBackend(opt Options) (*Backend, error) {
	if runtime.GOOS != "windows" {
		return nil, fmt.Errorf("host backend: supported on Windows only (this host is %s)", runtime.GOOS)
	}
	if !opt.AllowLocalSystem {
		system, err := runningAsLocalSystem()
		if err != nil {
			return nil, fmt.Errorf("host backend: identify current user: %w", err)
		}
		if system {
			return nil, fmt.Errorf("host backend: refusing to run jobs as NT AUTHORITY\\SYSTEM " +
				"(the default `multirunner service install` identity); run multirunner as a normal user, " +
				"or set host.allow_local_system: true")
		}
	}
	if opt.RunnerDir == "" || opt.StateDir == "" {
		if !portableBaseName(opt.Pool) {
			return nil, fmt.Errorf("host backend: pool name %q cannot name a directory; set host.runner_dir and host.state_dir", opt.Pool)
		}
		runnerDir, stateDir := DefaultDirs(opt.Pool)
		if opt.RunnerDir == "" {
			opt.RunnerDir = runnerDir
		}
		if opt.StateDir == "" {
			opt.StateDir = stateDir
		}
	}
	version, digest, err := resolveRunner(opt.RunnerVersion, opt.RunnerSHA256)
	if err != nil {
		return nil, fmt.Errorf("host backend: %w", err)
	}
	opt.RunnerVersion, opt.RunnerSHA256 = version, digest
	logger := opt.Logger
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Backend{opt: opt, download: downloadFile, proc: platformProcessOps(), logger: logger}, nil
}

// Configure sets what needs the loaded config: the secrets to keep out of the
// job environment and where to log cleanup problems.
func (b *Backend) Configure(stripNames, stripValues []string, logger *slog.Logger) {
	b.opt.StripEnvNames = append([]string(nil), stripNames...)
	b.opt.StripEnvValues = append([]string(nil), stripValues...)
	if logger != nil {
		b.logger = logger
	}
}

// DefaultDirs returns a pool's default runner and state directories. Config
// validation uses it too, so explicit and default directories are compared as
// the paths they really resolve to.
func DefaultDirs(pool string) (runnerDir, stateDir string) {
	base := os.Getenv("LOCALAPPDATA")
	if base == "" {
		base = os.TempDir()
	}
	root := filepath.Join(base, "multirunner", "host", pool)
	return filepath.Join(root, "runner"), filepath.Join(root, "state")
}

func resolveRunner(version, digest string) (string, string, error) {
	manifest := imageversions.MustEmbedded()
	if version == "" {
		version = manifest.Minimal.Runner.Version
	}
	if digest == "" {
		if version != manifest.Minimal.Runner.Version {
			return "", "", fmt.Errorf("runner_sha256 is required for custom runner_version %s", version)
		}
		digest = manifest.Minimal.Runner.WindowsX64SHA256
	}
	if b, err := hex.DecodeString(digest); err != nil || len(b) != sha256.Size {
		return "", "", fmt.Errorf("runner_sha256 must be 64 hex characters")
	}
	return version, strings.ToLower(digest), nil
}

func (b *Backend) Name() string { return "host-windows" }

// runnerRoot is the extracted runner for the pinned version.
func (b *Backend) runnerRoot() string {
	return filepath.Join(b.opt.RunnerDir, b.opt.RunnerVersion)
}

// Ping checks that the runner and state directory paths are reachable without
// creating them. doctor relies on backend pings being read-only; startup creates
// the runner directory in EnsureImage and the state directory in Launch.
func (b *Backend) Ping(ctx context.Context) error {
	for _, dir := range []string{b.opt.RunnerDir, b.opt.StateDir} {
		if err := reachableDirectoryPath(dir); err != nil {
			return fmt.Errorf("host backend directory %s: %w", dir, err)
		}
	}
	return nil
}

// reachableDirectoryPath accepts an existing directory or a path whose nearest
// existing ancestor is a directory.
func reachableDirectoryPath(path string) error {
	current := path
	for {
		info, err := os.Stat(current)
		if err == nil {
			if !info.IsDir() {
				return fmt.Errorf("%s is not a directory", current)
			}
			return nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		parent := filepath.Dir(current)
		if parent == current {
			return err
		}
		current = parent
	}
}

// OSType is the host OS; it always matches a validated host pool.
func (b *Backend) OSType(ctx context.Context) (string, error) { return runtime.GOOS, nil }

// EnsureImage downloads, verifies and extracts the pinned runner release once.
// The image reference is ignored: the host has no image.
func (b *Backend) EnsureImage(ctx context.Context, _ string) error {
	root := b.runnerRoot()
	marker := filepath.Join(root, installedMarker)
	if data, err := os.ReadFile(marker); err == nil && strings.TrimSpace(string(data)) == b.opt.RunnerSHA256 {
		return nil
	}
	if err := os.MkdirAll(b.opt.RunnerDir, 0o700); err != nil {
		return err
	}
	url := fmt.Sprintf("https://github.com/actions/runner/releases/download/v%s/actions-runner-win-x64-%s.zip",
		b.opt.RunnerVersion, b.opt.RunnerVersion)
	archive, err := os.CreateTemp(b.opt.RunnerDir, ".runner-*.zip")
	if err != nil {
		return err
	}
	archivePath := archive.Name()
	_ = archive.Close()
	defer os.Remove(archivePath)
	if err := b.download(ctx, url, archivePath); err != nil {
		return fmt.Errorf("download runner %s: %w", b.opt.RunnerVersion, err)
	}
	got, err := fileSHA256(archivePath)
	if err != nil {
		return err
	}
	if got != b.opt.RunnerSHA256 {
		return fmt.Errorf("runner %s digest mismatch: got %s, want %s", b.opt.RunnerVersion, got, b.opt.RunnerSHA256)
	}
	// Extract in place and mark the install complete last. Renaming a freshly
	// extracted tree fails on Windows while antivirus still holds its files.
	if err := os.RemoveAll(root); err != nil {
		return fmt.Errorf("clear partial runner install: %w", err)
	}
	if err := extractZip(archivePath, root); err != nil {
		return fmt.Errorf("extract runner: %w", err)
	}
	if _, err := os.Stat(filepath.Join(root, "run.cmd")); err != nil {
		return fmt.Errorf("runner archive has no run.cmd")
	}
	if err := os.WriteFile(marker, []byte(b.opt.RunnerSHA256+"\n"), 0o600); err != nil {
		return fmt.Errorf("mark runner installed: %w", err)
	}
	return nil
}

// installedMarker records the verified digest of a complete install, so an
// interrupted extraction is redone instead of trusted.
const installedMarker = ".multirunner-installed"

// Launch starts one ephemeral runner from the shared install. Only one job can
// run at a time per install; a second launch fails instead of sharing it.
func (b *Backend) Launch(ctx context.Context, req backend.LaunchRequest) (backend.RunnerHandle, error) {
	if err := req.Ownership.Validate(); err != nil {
		return nil, fmt.Errorf("host: %w", err)
	}
	if !req.Ownership.IsZero() && req.Ownership.RunnerID == 0 {
		return nil, fmt.Errorf("host: runner ID is required for owned launches")
	}
	if !portableBaseName(req.Name) {
		return nil, fmt.Errorf("invalid host runner name %q", req.Name)
	}
	if req.EncodedJITConfig == "" {
		return nil, fmt.Errorf("host: JIT config is required")
	}
	if len(req.Mounts) != 0 {
		return nil, fmt.Errorf("host: mounts are not supported (the job already sees the host filesystem)")
	}
	workFolder := req.WorkFolder
	if workFolder == "" {
		workFolder = "_work"
	}
	if !portableBaseName(workFolder) {
		return nil, fmt.Errorf("host: work_folder must be one directory name, got %q", workFolder)
	}
	root := b.runnerRoot()
	if _, err := os.Stat(filepath.Join(root, "run.cmd")); err != nil {
		return nil, fmt.Errorf("host: runner %s is not installed: %w", b.opt.RunnerVersion, err)
	}
	if err := os.MkdirAll(b.opt.StateDir, 0o700); err != nil {
		return nil, err
	}

	b.mu.Lock()
	if b.busy {
		b.mu.Unlock()
		return nil, fmt.Errorf("host: a runner is already running from %s (host pools run one job at a time)", root)
	}
	b.busy = true
	b.mu.Unlock()
	release := func() {
		b.mu.Lock()
		b.busy = false
		b.active = nil
		b.mu.Unlock()
	}

	// Whatever the previous job left (an interrupted runner's hidden
	// .runner/.credentials, a work folder, diag logs) is cleared first: the
	// runner cannot start over its own stale state, and a job must never see
	// the previous one's files.
	if err := b.resetInstall(root, workFolder, ""); err != nil {
		release()
		return nil, fmt.Errorf("host: reset runner install: %w", err)
	}

	recordPath := filepath.Join(b.opt.StateDir, runnerRecordName(req.Name, req.Ownership))
	logPath := filepath.Join(b.opt.StateDir, req.Name+".log")
	record := runnerRecord{
		Name: req.Name, Ownership: req.Ownership,
		Root: root, WorkFolder: workFolder, Log: filepath.Base(logPath),
	}
	if err := writeRunnerRecord(recordPath, record); err != nil {
		release()
		return nil, fmt.Errorf("write runner ownership: %w", err)
	}
	failed := func(err error) (backend.RunnerHandle, error) {
		release()
		return nil, errors.Join(err, os.Remove(recordPath))
	}

	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return failed(fmt.Errorf("open runner log: %w", err))
	}
	// --jitconfig makes the runner ephemeral: it takes one job and exits.
	cmd := exec.Command(filepath.Join(root, "run.cmd"), "--jitconfig", req.EncodedJITConfig)
	cmd.Dir = root
	cmd.Env = hostEnv(os.Environ(), req.Env, b.opt.StripEnvNames, b.opt.StripEnvValues)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	group, err := b.proc.start(cmd)
	if err != nil {
		_ = logFile.Close()
		if cmd.Process != nil {
			// Started but not contained: kill what we can see before giving up.
			_ = b.proc.killTree(cmd.Process.Pid)
			_ = cmd.Wait()
		}
		return failed(fmt.Errorf("start host runner: %w", err))
	}
	record.Started = true
	record.PID = cmd.Process.Pid
	// Recovery only kills a PID whose start time still matches, so a runner
	// without a known identity is not allowed to run at all.
	started, err := b.proc.startTime(record.PID)
	if err == nil && started == 0 {
		err = errors.New("start time is zero")
	}
	if err == nil {
		record.StartTime = started
		err = writeRunnerRecord(recordPath, record)
	}
	if err != nil {
		_ = group.kill()
		_ = cmd.Wait()
		_ = group.close()
		_ = logFile.Close()
		_ = b.resetInstall(root, workFolder, "")
		return failed(fmt.Errorf("record host runner process: %w", err))
	}

	// cleanup runs after the runner exits. Its problems are logged, never
	// returned: they must not turn a finished job into a failure. Anything it
	// could not remove is retried by the next Launch, which refuses to start
	// over it.
	cleanup := func() {
		defer release()
		errs := []error{group.close(), logFile.Close()}
		errs = append(errs, b.resetInstall(root, workFolder, req.Name))
		if err := os.Remove(recordPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, err)
		}
		if err := errors.Join(errs...); err != nil {
			b.logger.Warn("host runner cleanup incomplete; the next launch retries it",
				"runner", req.Name, "err", err)
		}
		b.pruneState()
	}
	h := newHostHandle(cmd, group, filepath.Base(recordPath), logPath, cleanup)
	b.mu.Lock()
	b.active = h
	b.mu.Unlock()
	return h, nil
}

// Close stops a runner that is still running (shutdown can return before the
// pool's launch goroutine waits for it) and waits for its cleanup, so a
// restart never finds a half-used install.
func (b *Backend) Close() error {
	b.mu.Lock()
	h := b.active
	b.mu.Unlock()
	if h == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	return h.Kill(ctx)
}

// CleanupOrphans recovers from a previous multirunner process that stopped
// mid-job, in every provisioning mode: it stops leftover runners it can prove
// are its own, then resets the install. Scale-set records are left for the
// scale-set reconciler, which also releases their registrations.
func (b *Backend) CleanupOrphans() error {
	paths, err := filepath.Glob(filepath.Join(b.opt.StateDir, "*.runner.json"))
	if err != nil {
		return err
	}
	var errs []error
	for _, path := range paths {
		record, err := readRunnerRecord(path)
		if err != nil {
			errs = append(errs, fmt.Errorf("read host runner record %s: %w", filepath.Base(path), err))
			continue
		}
		if !record.Ownership.IsZero() {
			continue
		}
		if err := b.removeRunner(context.Background(), path, record); err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) != 0 {
		return errors.Join(errs...)
	}
	// No live runner remains: clear stale state even without a record.
	return b.resetInstall(b.runnerRoot(), "_work", "orphan")
}

// hostEnv layers the pool's runner env over the host environment, minus
// multirunner's own secrets. Later entries win on Windows, but a duplicate key
// is still ambiguous to tools, so overridden host keys are dropped.
func hostEnv(base []string, extra map[string]string, stripNames, stripValues []string) []string {
	out := make([]string, 0, len(base)+len(extra))
	for _, kv := range base {
		key, value, _ := strings.Cut(kv, "=")
		if _, override := lookupFold(extra, key); override && key != "" {
			continue
		}
		if strippedEnv(key, value, stripNames, stripValues) {
			continue
		}
		out = append(out, kv)
	}
	for k, v := range extra {
		out = append(out, k+"="+v)
	}
	return out
}

func lookupFold(m map[string]string, key string) (string, bool) {
	for k, v := range m {
		if strings.EqualFold(k, key) {
			return v, true
		}
	}
	return "", false
}

// strippedEnv reports whether a host variable is one of multirunner's own
// secrets, by name or by value, or multirunner's own namespace.
func strippedEnv(key, value string, names, values []string) bool {
	if strings.HasPrefix(strings.ToUpper(key), "MULTIRUNNER_") {
		return true
	}
	for _, name := range names {
		if strings.EqualFold(name, key) {
			return true
		}
	}
	if value != "" {
		for _, secret := range values {
			if secret != "" && value == secret {
				return true
			}
		}
	}
	return false
}

// runnerStateFiles are what the runner writes into its own root while
// registered. A clean exit removes them; an interrupted one leaves them hidden,
// and the next runner then fails with "Access to the path '.runner' is denied".
var runnerStateFiles = []string{
	".runner", ".credentials", ".credentials_rsaparams",
	".runner_migrated", ".credentials_migrated", ".service",
}

// resetInstall returns the shared install to a clean state: runner state files
// removed, the work folder removed (unless keep_workspace), and _diag moved
// into the state dir as <archiveAs>.diag (or deleted when archiveAs is empty)
// so one job can't read another's diagnostics from the install.
func (b *Backend) resetInstall(root, workFolder, archiveAs string) error {
	if !portableBaseName(workFolder) {
		return fmt.Errorf("refusing to reset work folder %q", workFolder)
	}
	var errs []error
	for _, name := range runnerStateFiles {
		errs = append(errs, removeForced(filepath.Join(root, name)))
	}
	diag := filepath.Join(root, "_diag")
	if _, err := os.Stat(diag); err == nil {
		moved := false
		if archiveAs != "" && portableBaseName(archiveAs) {
			target := filepath.Join(b.opt.StateDir, archiveAs+".diag")
			_ = removeTree(target)
			moved = os.Rename(diag, target) == nil
		}
		if !moved {
			errs = append(errs, removeTree(diag))
		}
	}
	if !b.opt.KeepWorkspace {
		errs = append(errs, removeTree(filepath.Join(root, workFolder)))
	}
	return errors.Join(errs...)
}

// removeForced deletes one file, clearing the read-only attribute the runner
// sets on its credential files.
func removeForced(path string) error {
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	_ = os.Chmod(path, 0o600)
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove %s: %w", path, err)
	}
	return nil
}

// removeTree deletes a directory, retrying while antivirus or the indexer still
// holds files a job just wrote, and clearing read-only files (git objects).
func removeTree(path string) error {
	var err error
	for attempt := 0; attempt < 5; attempt++ {
		if err = os.RemoveAll(path); err == nil {
			return nil
		}
		_ = filepath.WalkDir(path, func(p string, _ os.DirEntry, _ error) error {
			_ = os.Chmod(p, 0o700)
			return nil
		})
		time.Sleep(time.Duration(attempt+1) * 200 * time.Millisecond)
	}
	return fmt.Errorf("remove %s: %w", path, err)
}

// stateRetention is how many per-job logs and diag folders the state dir keeps.
const stateRetention = 20

// pruneState keeps the newest per-job logs and diag folders and deletes the rest.
func (b *Backend) pruneState() {
	entries, err := os.ReadDir(b.opt.StateDir)
	if err != nil {
		return
	}
	type item struct {
		path string
		mod  time.Time
	}
	var items []item
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".log") && !strings.HasSuffix(e.Name(), ".diag") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		items = append(items, item{filepath.Join(b.opt.StateDir, e.Name()), info.ModTime()})
	}
	if len(items) <= stateRetention*2 {
		return
	}
	sort.Slice(items, func(i, j int) bool { return items[i].mod.After(items[j].mod) })
	for _, it := range items[stateRetention*2:] {
		_ = removeTree(it.path)
	}
}

type runnerRecord struct {
	Name       string                  `json:"name"`
	Ownership  backend.RunnerOwnership `json:"ownership"`
	Root       string                  `json:"root"`
	WorkFolder string                  `json:"workFolder"`
	Log        string                  `json:"log"`
	PID        int                     `json:"pid,omitempty"`
	StartTime  int64                   `json:"startTime,omitempty"`
	Started    bool                    `json:"started"`
}

// ListOwnedRunners returns the records a previous multirunner process left for
// this ownership boundary.
func (b *Backend) ListOwnedRunners(_ context.Context, ownership backend.RunnerOwnership) ([]backend.OwnedRunner, error) {
	if err := ownership.Validate(); err != nil || ownership.IsZero() {
		return nil, fmt.Errorf("host: complete runner ownership is required")
	}
	paths, err := filepath.Glob(filepath.Join(b.opt.StateDir, ownershipNamespace(ownership)+".*.runner.json"))
	if err != nil {
		return nil, fmt.Errorf("list host runner records: %w", err)
	}
	owned := make([]backend.OwnedRunner, 0, len(paths))
	for _, path := range paths {
		record, err := readRunnerRecord(path)
		if err != nil {
			return nil, fmt.Errorf("read host runner record %s: %w", filepath.Base(path), err)
		}
		if !ownershipMatches(record.Ownership, ownership) {
			continue
		}
		if record.Name == "" || record.Ownership.RunnerID == 0 {
			return nil, fmt.Errorf("host runner record %s is incomplete", filepath.Base(path))
		}
		owned = append(owned, backend.OwnedRunner{
			ResourceID: filepath.Base(path), Name: record.Name, RunnerID: record.Ownership.RunnerID,
		})
	}
	return owned, nil
}

// RemoveOwnedRunner kills a leftover runner's process tree (only if the PID
// still belongs to that runner) and removes its work folder and record.
func (b *Backend) RemoveOwnedRunner(ctx context.Context, resourceID string) error {
	if !strings.HasSuffix(resourceID, ".runner.json") || !portableBaseName(resourceID) {
		return fmt.Errorf("invalid host runner resource ID %q", resourceID)
	}
	recordPath := filepath.Join(b.opt.StateDir, resourceID)
	record, err := readRunnerRecord(recordPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read host runner record: %w", err)
	}
	return b.removeRunner(ctx, recordPath, record)
}

// removeRunner stops a recorded runner (only when its PID provably still
// belongs to it), resets the install and deletes the record.
func (b *Backend) removeRunner(ctx context.Context, recordPath string, record runnerRecord) error {
	b.mu.Lock()
	inUse, active := b.busy, b.active
	b.mu.Unlock()
	if inUse {
		// The record may be this process's own running job (a scale-set
		// reconcile): stop it through its handle, whose cleanup resets the
		// install and removes the record.
		if active != nil && active.resourceID == filepath.Base(recordPath) {
			return active.Kill(ctx)
		}
		return fmt.Errorf("host runner %s: install is in use by a running job", record.Name)
	}
	if record.PID != 0 {
		alive, err := b.proc.alive(record.PID)
		if err != nil {
			return fmt.Errorf("inspect host runner %d: %w", record.PID, err)
		}
		if alive {
			if record.StartTime == 0 {
				return fmt.Errorf("host runner %s: PID %d is alive but its identity was never recorded; "+
					"refusing to kill it (stop it by hand, then delete %s)", record.Name, record.PID, recordPath)
			}
			started, err := b.proc.startTime(record.PID)
			if err == nil && started == record.StartTime {
				if err := b.proc.killTree(record.PID); err != nil {
					return fmt.Errorf("stop host runner %s: %w", record.Name, err)
				}
				if err := waitForExit(ctx, record.PID, b.proc.alive); err != nil {
					return fmt.Errorf("wait for host runner %s: %w", record.Name, err)
				}
			}
		}
	} else if record.Started {
		return fmt.Errorf("host runner record %s has no process identity", filepath.Base(recordPath))
	}
	var errs []error
	if record.Root == b.runnerRoot() && portableBaseName(record.WorkFolder) {
		errs = append(errs, b.resetInstall(record.Root, record.WorkFolder, record.Name))
	}
	if err := os.Remove(recordPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

func waitForExit(ctx context.Context, pid int, alive func(int) (bool, error)) error {
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		running, err := alive(pid)
		if err != nil {
			return err
		}
		if !running {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func writeRunnerRecord(path string, record runnerRecord) error {
	data, err := json.Marshal(record)
	if err != nil {
		return err
	}
	temp, err := os.CreateTemp(filepath.Dir(path), ".runner-record-*")
	if err != nil {
		return err
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)
	if _, err := temp.Write(data); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	return os.Rename(tempPath, path)
}

func readRunnerRecord(path string) (runnerRecord, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return runnerRecord{}, err
	}
	var record runnerRecord
	if err := json.Unmarshal(data, &record); err != nil {
		return runnerRecord{}, err
	}
	return record, nil
}

func portableBaseName(name string) bool {
	return name != "" && name != "." && name != ".." &&
		!strings.ContainsAny(name, `/\:`) && !filepath.IsAbs(name) && filepath.VolumeName(name) == ""
}

func runnerRecordName(name string, ownership backend.RunnerOwnership) string {
	if ownership.IsZero() {
		return name + ".runner.json"
	}
	return ownershipNamespace(ownership) + "." + name + ".runner.json"
}

func ownershipNamespace(ownership backend.RunnerOwnership) string {
	sum := sha256.Sum256([]byte(ownership.Instance + "\x00" + ownership.Target + "\x00" + ownership.Pool))
	return fmt.Sprintf("%x", sum[:8])
}

func ownershipMatches(actual, expected backend.RunnerOwnership) bool {
	return actual.Instance == expected.Instance && actual.Target == expected.Target && actual.Pool == expected.Pool
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func downloadFile(ctx context.Context, url, dst string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download %s: status %d", url, resp.StatusCode)
	}
	f, err := os.Create(dst)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(f, resp.Body)
	return errors.Join(copyErr, f.Close())
}

// extractZip unpacks archive into dst, rejecting entries that escape it.
func extractZip(archive, dst string) error {
	r, err := zip.OpenReader(archive)
	if err != nil {
		return err
	}
	defer r.Close()
	root, err := filepath.Abs(dst)
	if err != nil {
		return err
	}
	for _, f := range r.File {
		target := filepath.Join(root, filepath.FromSlash(f.Name))
		if rel, err := filepath.Rel(root, target); err != nil || rel == ".." ||
			strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
			return fmt.Errorf("archive entry %q escapes the destination", f.Name)
		}
		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0o700); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			return err
		}
		if err := extractFile(f, target); err != nil {
			return err
		}
	}
	return nil
}

func extractFile(f *zip.File, target string) error {
	src, err := f.Open()
	if err != nil {
		return err
	}
	defer src.Close()
	out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o700)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, src)
	return errors.Join(copyErr, out.Close())
}

// hostHandle tracks one running host runner.
type hostHandle struct {
	cmd        *exec.Cmd
	group      processGroup
	resourceID string
	logPath    string

	done    chan struct{}
	code    int
	waitErr error
	killMu  sync.Mutex
}

// ranJobMarker is what Runner.Listener prints when it takes a job. run.cmd
// exits 0 even when the listener never got that far (it failed to configure,
// or exited with "terminated error"), so the exit code alone can't tell a
// finished job from a runner that never worked.
const ranJobMarker = "Running job:"

func newHostHandle(cmd *exec.Cmd, group processGroup, resourceID, logPath string, cleanup func()) *hostHandle {
	h := &hostHandle{cmd: cmd, group: group, resourceID: resourceID, logPath: logPath, done: make(chan struct{})}
	go func() {
		err := cmd.Wait()
		var exitErr *exec.ExitError
		switch {
		case err == nil:
			h.code = 0
		case errors.As(err, &exitErr):
			h.code = exitErr.ExitCode()
		default:
			h.code, h.waitErr = -1, err
		}
		if h.waitErr == nil && h.code == 0 && !logContains(logPath, ranJobMarker) {
			h.waitErr = fmt.Errorf("runner exited without running a job (see %s)", logPath)
		}
		// Cleanup problems are logged by cleanup itself and never change the
		// job's result.
		cleanup()
		close(h.done)
	}()
	return h
}

func logContains(path, marker string) bool {
	data, err := os.ReadFile(path)
	return err == nil && strings.Contains(string(data), marker)
}

func (h *hostHandle) ID() string { return h.resourceID + " pid=" + strconv.Itoa(h.cmd.Process.Pid) }

// Wait returns the runner's exit code. A runner that exited without ever
// taking a job is an error, so the pool counts it as a failure and backs off
// instead of re-registering every poll.
func (h *hostHandle) Wait(ctx context.Context) (int, error) {
	select {
	case <-h.done:
		return h.code, h.waitErr
	case <-ctx.Done():
		return -1, ctx.Err()
	}
}

// Logs returns nil: the runner's output goes to <state_dir>/<name>.log, which
// outlives the job for diagnosis (like the VM backend's serial log).
func (h *hostHandle) Logs(ctx context.Context) (io.ReadCloser, error) { return nil, nil }

// Kill terminates the runner's whole process tree, then waits for cleanup.
func (h *hostHandle) Kill(ctx context.Context) error {
	h.killMu.Lock()
	defer h.killMu.Unlock()
	select {
	case <-h.done:
		return nil
	default:
	}
	if err := h.group.kill(); err != nil {
		select {
		case <-h.done: // exited (and closed its job) between the check and the kill
			return nil
		default:
			return fmt.Errorf("kill host runner: %w", err)
		}
	}
	select {
	case <-h.done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("wait for host runner exit: %w", ctx.Err())
	}
}
