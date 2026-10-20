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
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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
}

// Backend runs one ephemeral runner at a time on the host.
type Backend struct {
	opt      Options
	download func(ctx context.Context, url, dst string) error
	proc     processOps

	mu   sync.Mutex
	busy bool
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
	if opt.RunnerDir == "" || opt.StateDir == "" {
		if !portableBaseName(opt.Pool) {
			return nil, fmt.Errorf("host backend: pool name %q cannot name a directory; set host.runner_dir and host.state_dir", opt.Pool)
		}
		base := os.Getenv("LOCALAPPDATA")
		if base == "" {
			base = os.TempDir()
		}
		if opt.RunnerDir == "" {
			opt.RunnerDir = filepath.Join(base, "multirunner", "host", opt.Pool, "runner")
		}
		if opt.StateDir == "" {
			opt.StateDir = filepath.Join(base, "multirunner", "host", opt.Pool, "state")
		}
	}
	version, digest, err := resolveRunner(opt.RunnerVersion, opt.RunnerSHA256)
	if err != nil {
		return nil, fmt.Errorf("host backend: %w", err)
	}
	opt.RunnerVersion, opt.RunnerSHA256 = version, digest
	return &Backend{opt: opt, download: downloadFile, proc: platformProcessOps()}, nil
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

// Ping checks the runner and state directories are usable.
func (b *Backend) Ping(ctx context.Context) error {
	for _, dir := range []string{b.opt.RunnerDir, b.opt.StateDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("host backend directory %s: %w", dir, err)
		}
	}
	return nil
}

// OSType is the host OS; it always matches a validated host pool.
func (b *Backend) OSType(ctx context.Context) (string, error) { return runtime.GOOS, nil }

// EnsureImage downloads, verifies and extracts the pinned runner release once.
// The image reference is ignored: the host has no image.
func (b *Backend) EnsureImage(ctx context.Context, _ string) error {
	root := b.runnerRoot()
	if _, err := os.Stat(filepath.Join(root, "run.cmd")); err == nil {
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
	staging, err := os.MkdirTemp(b.opt.RunnerDir, ".extract-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(staging)
	if err := extractZip(archivePath, staging); err != nil {
		return fmt.Errorf("extract runner: %w", err)
	}
	if _, err := os.Stat(filepath.Join(staging, "run.cmd")); err != nil {
		return fmt.Errorf("runner archive has no run.cmd")
	}
	if err := os.Rename(staging, root); err != nil {
		return fmt.Errorf("install runner: %w", err)
	}
	return nil
}

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
		b.mu.Unlock()
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
	cmd.Env = hostEnv(os.Environ(), req.Env)
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
	record.StartTime, _ = b.proc.startTime(record.PID)
	if err := writeRunnerRecord(recordPath, record); err != nil {
		_ = group.kill()
		_ = cmd.Wait()
		_ = group.close()
		_ = logFile.Close()
		return failed(fmt.Errorf("record host runner process: %w", err))
	}

	cleanup := func() error {
		defer release()
		errs := []error{group.close(), logFile.Close()}
		if !b.opt.KeepWorkspace {
			errs = append(errs, removeWorkFolder(root, workFolder))
		}
		if err := os.Remove(recordPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, err)
		}
		return errors.Join(errs...)
	}
	return newHostHandle(cmd, group, filepath.Base(recordPath), cleanup), nil
}

func (b *Backend) Close() error { return nil }

// hostEnv layers the pool's runner env over the host environment. Later
// entries win on Windows, but a duplicate key is still ambiguous to tools, so
// overridden host keys are dropped.
func hostEnv(base []string, extra map[string]string) []string {
	out := make([]string, 0, len(base)+len(extra))
	for _, kv := range base {
		key, _, _ := strings.Cut(kv, "=")
		if _, override := lookupFold(extra, key); override && key != "" {
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

func removeWorkFolder(root, workFolder string) error {
	if !portableBaseName(workFolder) {
		return fmt.Errorf("refusing to remove work folder %q", workFolder)
	}
	path := filepath.Join(root, workFolder)
	var err error
	// Antivirus and indexers briefly hold files a job just wrote; retry a little.
	for attempt := 0; attempt < 5; attempt++ {
		if err = os.RemoveAll(path); err == nil {
			return nil
		}
		time.Sleep(time.Duration(attempt+1) * 200 * time.Millisecond)
	}
	return fmt.Errorf("remove host work folder %s: %w", path, err)
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
	if record.PID != 0 {
		alive, err := b.proc.alive(record.PID)
		if err != nil {
			return fmt.Errorf("inspect host runner %d: %w", record.PID, err)
		}
		same := alive
		if alive && record.StartTime != 0 {
			started, err := b.proc.startTime(record.PID)
			same = err == nil && started == record.StartTime
		}
		if same {
			if err := b.proc.killTree(record.PID); err != nil {
				return fmt.Errorf("stop host runner %s: %w", record.Name, err)
			}
			if err := waitForExit(ctx, record.PID, b.proc.alive); err != nil {
				return fmt.Errorf("wait for host runner %s: %w", record.Name, err)
			}
		}
	} else if record.Started {
		return fmt.Errorf("host runner record %s has no process identity", resourceID)
	}
	var errs []error
	if record.Root == b.runnerRoot() && !b.opt.KeepWorkspace && record.WorkFolder != "" {
		errs = append(errs, removeWorkFolder(record.Root, record.WorkFolder))
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

	done       chan struct{}
	waitErr    error
	cleanupErr error
	killMu     sync.Mutex
}

func newHostHandle(cmd *exec.Cmd, group processGroup, resourceID string, cleanup func() error) *hostHandle {
	h := &hostHandle{cmd: cmd, group: group, resourceID: resourceID, done: make(chan struct{})}
	go func() {
		h.waitErr = cmd.Wait()
		h.cleanupErr = cleanup()
		close(h.done)
	}()
	return h
}

func (h *hostHandle) ID() string { return h.resourceID + " pid=" + strconv.Itoa(h.cmd.Process.Pid) }

func (h *hostHandle) Wait(ctx context.Context) (int, error) {
	select {
	case <-h.done:
		if h.waitErr != nil {
			var exitErr *exec.ExitError
			if errors.As(h.waitErr, &exitErr) {
				return exitErr.ExitCode(), h.cleanupErr
			}
			return -1, errors.Join(h.waitErr, h.cleanupErr)
		}
		return 0, h.cleanupErr
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
		return h.cleanupErr
	default:
	}
	if err := h.group.kill(); err != nil {
		select {
		case <-h.done: // exited (and closed its job) between the check and the kill
			return h.cleanupErr
		default:
			return fmt.Errorf("kill host runner: %w", err)
		}
	}
	select {
	case <-h.done:
		return h.cleanupErr
	case <-ctx.Done():
		return fmt.Errorf("wait for host runner exit: %w", ctx.Err())
	}
}
