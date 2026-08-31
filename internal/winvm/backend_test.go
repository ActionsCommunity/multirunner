package winvm

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestVMHandleKillWaitsForExitAndSupportsConcurrentWaiters(t *testing.T) {
	dir := t.TempDir()
	overlay := createArtifact(t, dir, "runner.qcow2")
	iso := createArtifact(t, dir, "runner.iso")
	vars := createArtifact(t, dir, "runner.vars.fd")
	cmd := helperProcess(t)
	h := newVMHandle(cmd, overlay, iso, vars)

	const waiters = 2
	results := make(chan error, waiters)
	var ready sync.WaitGroup
	ready.Add(waiters)
	for range waiters {
		go func() {
			ready.Done()
			_, err := h.Wait(context.Background())
			results <- err
		}()
	}
	ready.Wait()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := h.Kill(ctx); err != nil {
		t.Fatalf("Kill: %v", err)
	}
	if h.cmd.ProcessState == nil {
		t.Fatal("Kill returned before cmd.Wait observed process exit")
	}
	for range waiters {
		if err := <-results; err != nil {
			t.Errorf("concurrent Wait returned %v", err)
		}
	}
	for _, path := range []string{overlay, iso, vars} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("artifact still exists after process exit: %s", path)
		}
	}
}

func TestVMHandlePropagatesCleanupFailure(t *testing.T) {
	dir := t.TempDir()
	overlay := filepath.Join(dir, "non-empty-overlay")
	if err := os.Mkdir(overlay, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(overlay, "keep"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := helperProcess(t)
	h := newVMHandle(cmd, overlay, "", "")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := h.Kill(ctx)
	if err == nil || !strings.Contains(err.Error(), "remove overlay") {
		t.Fatalf("Kill error = %v, want cleanup failure", err)
	}
	if _, waitErr := h.Wait(context.Background()); waitErr == nil ||
		!strings.Contains(waitErr.Error(), "remove overlay") {
		t.Fatalf("Wait error = %v, want cleanup failure", waitErr)
	}
}

func TestVMHandlePropagatesKillAndWaitFailures(t *testing.T) {
	t.Run("kill", func(t *testing.T) {
		h := &vmHandle{cmd: &exec.Cmd{}, done: make(chan struct{})}
		err := h.Kill(context.Background())
		if err == nil || !strings.Contains(err.Error(), "process is unavailable") {
			t.Fatalf("Kill error = %v, want unavailable process", err)
		}
	})

	t.Run("wait", func(t *testing.T) {
		h := &vmHandle{done: make(chan struct{}), waitErr: errors.New("wait failed")}
		close(h.done)
		if _, err := h.Wait(context.Background()); err == nil ||
			!strings.Contains(err.Error(), "wait failed") {
			t.Fatalf("Wait error = %v, want wait failure", err)
		}
	})
}

func helperProcess(t *testing.T) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=TestVMHandleHelperProcess")
	cmd.Env = append(os.Environ(), "MULTIRUNNER_VM_HANDLE_HELPER=1")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start helper process: %v", err)
	}
	t.Cleanup(func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
	})
	return cmd
}

func createArtifact(t *testing.T, dir, name string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestVMHandleHelperProcess(t *testing.T) {
	if os.Getenv("MULTIRUNNER_VM_HANDLE_HELPER") != "1" {
		return
	}
	for {
		time.Sleep(time.Hour)
	}
}
