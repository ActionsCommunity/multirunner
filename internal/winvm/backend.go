package winvm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"

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
	opt      Options
	accel    string
	ovmfCode string // UEFI firmware code (empty => legacy BIOS golden)
}

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
	sweepOrphans(opt.WorkDir)
	return &Backend{opt: opt, accel: accel, ovmfCode: code}, nil
}

// sweepOrphans clears leftover per-VM artifacts in the work dir at startup. A
// clean shutdown removes them (vmHandle.cleanup); these remain only after a hard
// kill/crash. Safe to delete at startup: no VM from this process is running yet.
func sweepOrphans(workDir string) {
	if workDir == "" {
		return
	}
	for _, pat := range []string{"*.qcow2", "*.iso", "*.vars.fd", "*.serial.log"} {
		matches, _ := filepath.Glob(filepath.Join(workDir, pat))
		for _, p := range matches {
			_ = os.Remove(p)
		}
	}
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
	if err := os.MkdirAll(b.opt.WorkDir, 0o755); err != nil {
		return nil, err
	}
	overlay := filepath.Join(b.opt.WorkDir, req.Name+".qcow2")
	isoPath := filepath.Join(b.opt.WorkDir, req.Name+".iso")

	if out, err := exec.CommandContext(ctx, b.opt.ImgBin, OverlayCreateArgs(b.opt.Golden, overlay)...).CombinedOutput(); err != nil {
		return nil, fmt.Errorf("create overlay: %w: %s", err, out)
	}
	if err := BuildJITISO(isoPath, req.EncodedJITConfig, vmEnv(req.Env)); err != nil {
		os.Remove(overlay)
		return nil, fmt.Errorf("build jit iso: %w", err)
	}

	// UEFI: each VM needs its own writable NVRAM, seeded from the golden's vars
	// (which holds the Windows Boot Manager entry created during install).
	var fw Firmware
	varsCopy := ""
	goldenVars := GoldenVarsPath(b.opt.Golden)
	if b.ovmfCode != "" {
		if _, err := os.Stat(goldenVars); err == nil {
			varsCopy = filepath.Join(b.opt.WorkDir, req.Name+".vars.fd")
			if err := copyFile(varsCopy, goldenVars); err != nil {
				os.Remove(overlay)
				os.Remove(isoPath)
				return nil, fmt.Errorf("copy nvram: %w", err)
			}
			fw = Firmware{CodeFD: b.ovmfCode, VarsFD: varsCopy}
		}
	}

	args := QEMUArgs(LaunchOpts{
		Overlay: overlay, JITISOPath: isoPath,
		MemMB: b.opt.MemMB, CPUs: b.opt.CPUs, Accel: b.accel, Firmware: fw,
	})
	cmd := exec.Command(b.opt.QEMUBin, args...)
	if err := cmd.Start(); err != nil {
		os.Remove(overlay)
		os.Remove(isoPath)
		if varsCopy != "" {
			os.Remove(varsCopy)
		}
		return nil, fmt.Errorf("start qemu: %w", err)
	}
	return newVMHandle(cmd, overlay, isoPath, varsCopy), nil
}

func (b *Backend) Close() error { return nil }

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
	cmd     *exec.Cmd
	overlay string
	iso     string
	vars    string // per-VM UEFI NVRAM copy (empty for BIOS)

	done       chan struct{}
	waitErr    error
	cleanupErr error
	killOnce   sync.Once
	killErr    error
}

func newVMHandle(cmd *exec.Cmd, overlay, iso, vars string) *vmHandle {
	h := &vmHandle{
		cmd: cmd, overlay: overlay, iso: iso, vars: vars,
		done: make(chan struct{}),
	}
	go func() {
		h.waitErr = cmd.Wait()
		h.cleanupErr = h.cleanup()
		close(h.done)
	}()
	return h
}

func (h *vmHandle) ID() string { return filepath.Base(h.overlay) }

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
	h.killOnce.Do(func() {
		if h.cmd.Process == nil {
			h.killErr = fmt.Errorf("qemu process is unavailable")
			return
		}
		h.killErr = h.cmd.Process.Kill()
		if errors.Is(h.killErr, os.ErrProcessDone) {
			h.killErr = nil
		}
	})
	if h.killErr != nil {
		return fmt.Errorf("kill qemu: %w", h.killErr)
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

func (h *vmHandle) cleanup() error {
	var errs []error
	remove := func(kind, path string) {
		if path == "" {
			return
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, fmt.Errorf("remove %s %s: %w", kind, path, err))
		}
	}
	remove("overlay", h.overlay)
	remove("JIT ISO", h.iso)
	if h.vars != "" {
		remove("NVRAM", h.vars)
	}
	return errors.Join(errs...)
}
