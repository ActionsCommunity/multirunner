package winvm

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/GerardSmit/multirunner/internal/backend"
)

// TestQEMULifecycle exercises the real backend against an actual QEMU+accel using
// an empty "golden" disk (no OS): it verifies overlay + JIT-ISO creation, that
// the VM process starts, and that explicit ownership cleanup removes artifacts.
// Set MULTIRUNNER_TEST_QEMU=1
// (and have qemu-system-x86_64/qemu-img on PATH) to run.
func TestQEMULifecycle(t *testing.T) {
	if os.Getenv("MULTIRUNNER_TEST_QEMU") == "" {
		t.Skip("set MULTIRUNNER_TEST_QEMU=1 to run")
	}
	if _, err := exec.LookPath("qemu-img"); err != nil {
		t.Skip("qemu-img not on PATH")
	}
	dir := t.TempDir()
	golden := filepath.Join(dir, "golden.qcow2")
	if out, err := exec.Command("qemu-img", "create", "-f", "qcow2", golden, "64M").CombinedOutput(); err != nil {
		t.Fatalf("create golden: %v: %s", err, out)
	}

	accel := os.Getenv("MULTIRUNNER_TEST_ACCEL")
	be, err := NewBackend(Options{Golden: golden, WorkDir: filepath.Join(dir, "vm"), MemMB: 512, CPUs: 1, Accel: accel})
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	if err := be.Ping(ctx); err != nil {
		t.Fatalf("Ping: %v", err)
	}

	h, err := be.Launch(ctx, backend.LaunchRequest{Name: "vmtest-0", EncodedJITConfig: "FAKEJIT"})
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	vh := h.(*vmHandle)
	overlay := filepath.Join(be.opt.WorkDir, "vmtest-0.qcow2")
	iso := filepath.Join(be.opt.WorkDir, "vmtest-0.iso")
	if _, err := os.Stat(overlay); err != nil {
		t.Errorf("overlay not created: %v", err)
	}
	if _, err := os.Stat(iso); err != nil {
		t.Errorf("jit iso not created: %v", err)
	}
	t.Logf("VM launched: overlay=%s iso=%s accel=%s", filepath.Base(overlay), filepath.Base(iso), be.accel)

	// Let the VM run briefly, then stop it and confirm Wait returns.
	time.Sleep(2 * time.Second)
	if err := h.Kill(ctx); err != nil {
		t.Fatalf("Kill: %v", err)
	}
	done := make(chan struct{})
	go func() { _, _ = h.Wait(ctx); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Wait did not return after Kill")
	}
	if err := be.RemoveOwnedRunner(ctx, vh.ID()); err != nil {
		t.Fatalf("RemoveOwnedRunner: %v", err)
	}
	if _, err := os.Stat(overlay); !os.IsNotExist(err) {
		t.Errorf("overlay not cleaned up")
	}
	if _, err := os.Stat(iso); !os.IsNotExist(err) {
		t.Errorf("iso not cleaned up")
	}
}
