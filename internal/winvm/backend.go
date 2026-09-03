package winvm

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/GerardSmit/multirunner/internal/backend"
)

// Options configures the QEMU Windows-VM backend.
type Options struct {
	Golden  string // path to the golden qcow2 (built by `multirunner bake`)
	WorkDir string // where per-job overlays/ISOs are written
	MemMB   int
	CPUs    int
	Accel   string // "" = auto-detect (kvm/whpx/hvf on x86-64, tcg on ARM)
	QEMUBin string // qemu-system-x86_64 (default looked up on PATH)
	ImgBin  string // qemu-img (default looked up on PATH)
}

// Backend runs ephemeral Windows runners as QEMU VMs (golden image + per-job
// copy-on-write overlay + JIT config ISO). It implements backend.Backend, so it
// plugs into the pool / autoscaler unchanged.
type Backend struct {
	opt          Options
	accel        string
	ovmfCode     string // UEFI firmware code (empty => legacy BIOS golden)
	quitNamed    func(context.Context, string, string) error
	processAlive func(int) (bool, error)
}

var _ backend.OwnedRunnerStore = (*Backend)(nil)

// NewBackend builds the QEMU backend.
func NewBackend(opt Options) (*Backend, error) {
	if opt.Golden == "" {
		return nil, fmt.Errorf("qemu backend: golden image path is required (run: multirunner bake)")
	}
	if opt.WorkDir == "" {
		opt.WorkDir = filepath.Join(os.TempDir(), "multirunner-vm")
	}
	if opt.QEMUBin == "" {
		opt.QEMUBin = "qemu-system-x86_64"
	}
	if opt.ImgBin == "" {
		opt.ImgBin = "qemu-img"
	}
	accel := opt.Accel
	if accel == "" {
		accel = DetectAccel(runtime.GOOS, runtime.GOARCH)
	}
	code, _ := DetectOVMF(opt.QEMUBin)
	return &Backend{
		opt:          opt,
		accel:        accel,
		ovmfCode:     code,
		quitNamed:    QMPQuitNamed,
		processAlive: processAlive,
	}, nil
}

func (b *Backend) Name() string { return "qemu-windows" }

// Ping verifies qemu + qemu-img are present and the golden image exists.
func (b *Backend) Ping(ctx context.Context) error {
	if _, err := exec.LookPath(b.opt.QEMUBin); err != nil {
		return fmt.Errorf("%s not found on PATH: %w", b.opt.QEMUBin, err)
	}
	if _, err := exec.LookPath(b.opt.ImgBin); err != nil {
		return fmt.Errorf("%s not found on PATH: %w", b.opt.ImgBin, err)
	}
	if _, err := os.Stat(b.opt.Golden); err != nil {
		return fmt.Errorf("golden image %s missing (run: multirunner bake): %w", b.opt.Golden, err)
	}
	return nil
}

// OSType reports the guest OS this backend runs.
func (b *Backend) OSType(ctx context.Context) (string, error) { return "windows", nil }

// EnsureImage is a no-op for VMs (the golden image is produced by `bake`); it
// only checks the golden exists.
func (b *Backend) EnsureImage(ctx context.Context, _ string) error {
	if _, err := os.Stat(b.opt.Golden); err != nil {
		return fmt.Errorf("golden image %s missing (run: multirunner bake): %w", b.opt.Golden, err)
	}
	return nil
}

// Launch creates a clean overlay + JIT ISO and boots the VM. The guest runs its
// one job then powers off, which terminates QEMU (see -no-reboot).
func (b *Backend) Launch(ctx context.Context, req backend.LaunchRequest) (backend.RunnerHandle, error) {
	if err := req.Ownership.Validate(); err != nil {
		return nil, fmt.Errorf("qemu: %w", err)
	}
	if !req.Ownership.IsZero() && req.Ownership.RunnerID == 0 {
		return nil, fmt.Errorf("qemu: runner ID is required for owned launches")
	}
	if !portableBaseName(req.Name) {
		return nil, fmt.Errorf("invalid qemu runner name %q", req.Name)
	}
	if err := os.MkdirAll(b.opt.WorkDir, 0o755); err != nil {
		return nil, err
	}
	overlay := filepath.Join(b.opt.WorkDir, req.Name+".qcow2")
	isoPath := filepath.Join(b.opt.WorkDir, req.Name+".iso")
	varsCopy := filepath.Join(b.opt.WorkDir, req.Name+".vars.fd")
	serialPath := overlay + ".serial.log"
	pidPath := filepath.Join(b.opt.WorkDir, req.Name+".pid")
	recordPath := filepath.Join(b.opt.WorkDir, runnerRecordName(req.Name, req.Ownership))

	qmpAddr, err := reserveLoopbackAddress()
	if err != nil {
		return nil, fmt.Errorf("reserve qmp endpoint: %w", err)
	}
	record := runnerRecord{
		Name: req.Name, Ownership: req.Ownership, QMPAddr: qmpAddr,
		Overlay: filepath.Base(overlay), ISO: filepath.Base(isoPath),
		Vars: filepath.Base(varsCopy), Serial: filepath.Base(serialPath),
		PIDFile: filepath.Base(pidPath),
	}
	if err := writeRunnerRecord(recordPath, record); err != nil {
		return nil, fmt.Errorf("write runner ownership: %w", err)
	}
	cleanupFailedLaunch := func() error {
		return removeRunnerRecord(b.opt.WorkDir, recordPath, record)
	}

	if out, err := exec.CommandContext(ctx, b.opt.ImgBin, OverlayCreateArgs(b.opt.Golden, overlay)...).CombinedOutput(); err != nil {
		return nil, errors.Join(
			fmt.Errorf("create overlay: %w: %s", err, out),
			cleanupFailedLaunch(),
		)
	}
	if err := BuildJITISO(isoPath, req.EncodedJITConfig, vmEnv(req.Env)); err != nil {
		return nil, errors.Join(
			fmt.Errorf("build jit iso: %w", err),
			cleanupFailedLaunch(),
		)
	}

	// UEFI: each VM needs its own writable NVRAM, seeded from the golden's vars
	// (which holds the Windows Boot Manager entry created during install).
	var fw Firmware
	goldenVars := GoldenVarsPath(b.opt.Golden)
	if b.ovmfCode != "" {
		if _, err := os.Stat(goldenVars); err == nil {
			if err := copyFile(varsCopy, goldenVars); err != nil {
				return nil, errors.Join(
					fmt.Errorf("copy nvram: %w", err),
					cleanupFailedLaunch(),
				)
			}
			fw = Firmware{CodeFD: b.ovmfCode, VarsFD: varsCopy}
		}
	}

	args := QEMUArgs(LaunchOpts{
		Overlay: overlay, JITISOPath: isoPath,
		Name: req.Name, QMPAddr: qmpAddr, PIDFile: pidPath,
		MemMB: b.opt.MemMB, CPUs: b.opt.CPUs, Accel: b.accel, Firmware: fw,
	})
	cmd := exec.Command(b.opt.QEMUBin, args...)
	if err := cmd.Start(); err != nil {
		return nil, errors.Join(
			fmt.Errorf("start qemu: %w", err),
			cleanupFailedLaunch(),
		)
	}
	record.Started = true
	record.PID = cmd.Process.Pid
	if err := writeRunnerRecord(recordPath, record); err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return nil, errors.Join(
			fmt.Errorf("record qemu process: %w", err),
			cleanupFailedLaunch(),
		)
	}
	var cleanup func() error
	if req.Ownership.IsZero() {
		cleanup = func() error {
			return b.RemoveOwnedRunner(context.Background(), filepath.Base(recordPath))
		}
	}
	return newVMHandle(cmd, filepath.Base(recordPath), cleanup), nil
}

func (b *Backend) Close() error { return nil }

type runnerRecord struct {
	Name      string                  `json:"name"`
	Ownership backend.RunnerOwnership `json:"ownership"`
	QMPAddr   string                  `json:"qmpAddr"`
	PID       int                     `json:"pid,omitempty"`
	PIDFile   string                  `json:"pidFile"`
	Overlay   string                  `json:"overlay"`
	ISO       string                  `json:"iso"`
	Vars      string                  `json:"vars,omitempty"`
	Serial    string                  `json:"serial"`
	Started   bool                    `json:"started"`
}

func (b *Backend) ListOwnedRunners(_ context.Context, ownership backend.RunnerOwnership) ([]backend.OwnedRunner, error) {
	if err := ownership.Validate(); err != nil || ownership.IsZero() {
		return nil, fmt.Errorf("qemu: complete runner ownership is required")
	}
	pattern := ownershipNamespace(ownership) + ".*.runner.json"
	paths, err := filepath.Glob(filepath.Join(b.opt.WorkDir, pattern))
	if err != nil {
		return nil, fmt.Errorf("list qemu runner records: %w", err)
	}
	owned := make([]backend.OwnedRunner, 0, len(paths))
	for _, path := range paths {
		record, err := readRunnerRecord(path)
		if err != nil {
			return nil, fmt.Errorf("read qemu runner record %s: %w", filepath.Base(path), err)
		}
		if !ownershipMatches(record.Ownership, ownership) {
			continue
		}
		if record.Name == "" || record.Ownership.RunnerID == 0 {
			return nil, fmt.Errorf("qemu runner record %s is incomplete", filepath.Base(path))
		}
		owned = append(owned, backend.OwnedRunner{
			ResourceID: filepath.Base(path),
			Name:       record.Name,
			RunnerID:   record.Ownership.RunnerID,
		})
	}
	return owned, nil
}

func (b *Backend) RemoveOwnedRunner(ctx context.Context, resourceID string) error {
	if !strings.HasSuffix(resourceID, ".runner.json") {
		return fmt.Errorf("invalid qemu runner resource ID %q", resourceID)
	}
	recordPath, err := containedRunnerPath(b.opt.WorkDir, resourceID)
	if err != nil {
		return fmt.Errorf("invalid qemu runner resource ID %q: %w", resourceID, err)
	}
	record, err := readRunnerRecord(recordPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read qemu runner record: %w", err)
	}

	pid, err := runnerPID(b.opt.WorkDir, record)
	if err != nil {
		return err
	}
	if pid != 0 {
		alive, err := b.processAlive(pid)
		if err != nil {
			return fmt.Errorf("inspect qemu process %d: %w", pid, err)
		}
		if alive {
			if err := b.quitNamed(ctx, record.QMPAddr, record.Name); err != nil {
				return fmt.Errorf("stop owned qemu %s: %w", record.Name, err)
			}
			if err := waitForProcessExit(ctx, pid, b.processAlive); err != nil {
				return fmt.Errorf("wait for owned qemu %s: %w", record.Name, err)
			}
		}
	} else if record.Started {
		return fmt.Errorf("qemu runner record %s has no process identity", resourceID)
	}

	return removeRunnerRecord(b.opt.WorkDir, recordPath, record)
}

func reserveLoopbackAddress() (string, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	addr := listener.Addr().String()
	if err := listener.Close(); err != nil {
		return "", err
	}
	return addr, nil
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
	if err := temp.Chmod(0o600); err != nil {
		_ = temp.Close()
		return err
	}
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

func runnerPID(workDir string, record runnerRecord) (int, error) {
	pid := record.PID
	if record.PIDFile == "" {
		return pid, nil
	}
	path, err := runnerArtifactPath(workDir, record.PIDFile)
	if err != nil {
		return 0, err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return pid, nil
	}
	if err != nil {
		return 0, fmt.Errorf("read qemu pid file: %w", err)
	}
	value, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || value <= 0 {
		return 0, fmt.Errorf("invalid qemu pid file %s", record.PIDFile)
	}
	return value, nil
}

func waitForProcessExit(ctx context.Context, pid int, alive func(int) (bool, error)) error {
	ticker := time.NewTicker(25 * time.Millisecond)
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

func removeRunnerArtifacts(workDir string, record runnerRecord) error {
	var errs []error
	for _, name := range []string{record.Overlay, record.ISO, record.Vars, record.Serial, record.PIDFile} {
		if name == "" {
			continue
		}
		path, err := runnerArtifactPath(workDir, name)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, fmt.Errorf("remove qemu artifact %s: %w", name, err))
		}
	}
	return errors.Join(errs...)
}

func removeRunnerRecord(workDir, recordPath string, record runnerRecord) error {
	if err := removeRunnerArtifacts(workDir, record); err != nil {
		return err
	}
	if err := os.Remove(recordPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove qemu runner record: %w", err)
	}
	return nil
}

func runnerArtifactPath(workDir, name string) (string, error) {
	path, err := containedRunnerPath(workDir, name)
	if err != nil {
		return "", fmt.Errorf("invalid qemu artifact name %q: %w", name, err)
	}
	return path, nil
}

func containedRunnerPath(workDir, name string) (string, error) {
	if !portableBaseName(name) {
		return "", errors.New("must be a portable basename")
	}
	root, err := filepath.Abs(workDir)
	if err != nil {
		return "", fmt.Errorf("resolve work directory: %w", err)
	}
	candidate := filepath.Join(root, name)
	if _, err := os.Lstat(candidate); errors.Is(err, os.ErrNotExist) {
		return candidate, nil
	} else if err != nil {
		return "", fmt.Errorf("inspect path: %w", err)
	}

	canonicalRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", fmt.Errorf("resolve work directory links: %w", err)
	}
	canonicalCandidate, err := filepath.EvalSymlinks(candidate)
	if err != nil {
		return "", fmt.Errorf("resolve path links: %w", err)
	}
	relative, err := filepath.Rel(canonicalRoot, canonicalCandidate)
	if err != nil || filepath.IsAbs(relative) ||
		relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("canonical path escapes work directory")
	}
	return candidate, nil
}

func portableBaseName(name string) bool {
	if name == "" || name == "." || name == ".." ||
		strings.ContainsAny(name, `/\:`) ||
		filepath.IsAbs(name) || filepath.VolumeName(name) != "" {
		return false
	}
	return true
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
	return actual.Instance == expected.Instance &&
		actual.Target == expected.Target &&
		actual.Pool == expected.Pool
}

// slirpHost is the host's address as seen from a QEMU user-net (SLIRP) guest.
const slirpHost = "10.0.2.2"

// vmEnv copies env, rewriting the cache server URLs so their hostname targets the
// SLIRP host alias — the cache server runs on the host, and host.docker.internal
// does not exist inside the VM (the host is reachable at 10.0.2.2 over user-net).
func vmEnv(env map[string]string) map[string]string {
	return backend.RewriteCacheHost(env, slirpHost)
}

// vmHandle tracks one running VM and its throwaway artifacts.
type vmHandle struct {
	cmd        *exec.Cmd
	resourceID string

	done       chan struct{}
	waitErr    error
	cleanupErr error
	cleanup    func() error
	killMu     sync.Mutex
}

func newVMHandle(cmd *exec.Cmd, resourceID string, cleanup func() error) *vmHandle {
	h := &vmHandle{
		cmd: cmd, resourceID: resourceID, cleanup: cleanup,
		done: make(chan struct{}),
	}
	go func() {
		h.waitErr = cmd.Wait()
		if h.cleanup != nil {
			h.cleanupErr = h.cleanup()
		}
		close(h.done)
	}()
	return h
}

func (h *vmHandle) ID() string { return h.resourceID }

func (h *vmHandle) Wait(ctx context.Context) (int, error) {
	select {
	case <-h.done:
		if h.waitErr != nil {
			if ee, ok := h.waitErr.(*exec.ExitError); ok {
				return ee.ExitCode(), h.cleanupErr
			}
			return -1, errors.Join(h.waitErr, h.cleanupErr)
		}
		return 0, h.cleanupErr
	case <-ctx.Done():
		return -1, ctx.Err()
	}
}

func (h *vmHandle) Logs(ctx context.Context) (io.ReadCloser, error) { return nil, nil }

func (h *vmHandle) Kill(ctx context.Context) error {
	h.killMu.Lock()
	defer h.killMu.Unlock()
	if h.cmd.Process == nil {
		return fmt.Errorf("qemu process is unavailable")
	}
	if err := h.cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return fmt.Errorf("kill qemu: %w", err)
	}
	select {
	case <-h.done:
		var waitErr error
		if h.waitErr != nil {
			if _, ok := h.waitErr.(*exec.ExitError); !ok {
				waitErr = fmt.Errorf("wait for qemu exit: %w", h.waitErr)
			}
		}
		return errors.Join(waitErr, h.cleanupErr)
	case <-ctx.Done():
		return fmt.Errorf("wait for qemu exit: %w", ctx.Err())
	}
}
