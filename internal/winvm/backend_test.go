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

	"github.com/GerardSmit/multirunner/internal/backend"
)

func TestVMHandleKillWaitsForExitAndSupportsConcurrentWaiters(t *testing.T) {
	cmd := helperProcess(t)
	h := newVMHandle(cmd, "runner.runner.json", nil)

	const waiters = 2
	results := make(chan error, waiters)
	var ready sync.WaitGroup
	ready.Add(waiters)
	for range waiters {
		go func() {
			ready.Done()
			_, err := h.Wait(t.Context())
			results <- err
		}()
	}
	ready.Wait()

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
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
	if h.ID() != "runner.runner.json" {
		t.Fatalf("resource ID = %q, want ownership record", h.ID())
	}
}

func TestVMHandleRunsPoolCleanupAfterExit(t *testing.T) {
	cmd := helperProcess(t)
	cleaned := make(chan struct{}, 1)
	h := newVMHandle(cmd, "runner.runner.json", func() error {
		cleaned <- struct{}{}
		return nil
	})
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := h.Kill(ctx); err != nil {
		t.Fatalf("Kill: %v", err)
	}
	select {
	case <-cleaned:
	default:
		t.Fatal("pool cleanup did not run")
	}
}

func TestOwnedRunnerRemovalPreservesRecordOnCleanupFailure(t *testing.T) {
	dir := t.TempDir()
	overlay := filepath.Join(dir, "non-empty-overlay")
	if err := os.Mkdir(overlay, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(overlay, "keep"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	recordPath := filepath.Join(dir, "runner.runner.json")
	record := runnerRecord{Name: "runner", Overlay: filepath.Base(overlay)}
	if err := writeRunnerRecord(recordPath, record); err != nil {
		t.Fatal(err)
	}
	be := &Backend{opt: Options{WorkDir: dir}, processAlive: processAlive, quitNamed: QMPQuitNamed}
	err := be.RemoveOwnedRunner(t.Context(), filepath.Base(recordPath))
	if err == nil || !strings.Contains(err.Error(), "remove qemu artifact") {
		t.Fatalf("RemoveOwnedRunner error = %v, want cleanup failure", err)
	}
	if _, statErr := os.Stat(recordPath); statErr != nil {
		t.Fatalf("ownership record was removed after cleanup failure: %v", statErr)
	}
}

func TestVMHandlePropagatesKillAndWaitFailures(t *testing.T) {
	t.Run("kill", func(t *testing.T) {
		h := &vmHandle{cmd: &exec.Cmd{}, done: make(chan struct{})}
		err := h.Kill(t.Context())
		if err == nil || !strings.Contains(err.Error(), "process is unavailable") {
			t.Fatalf("Kill error = %v, want unavailable process", err)
		}
	})

	t.Run("wait", func(t *testing.T) {
		h := &vmHandle{done: make(chan struct{}), waitErr: errors.New("wait failed")}
		close(h.done)
		if _, err := h.Wait(t.Context()); err == nil ||
			!strings.Contains(err.Error(), "wait failed") {
			t.Fatalf("Wait error = %v, want wait failure", err)
		}
	})
}

func TestQEMUOwnedRunnerStoreUsesCompleteOwnershipBoundary(t *testing.T) {
	dir := t.TempDir()
	ownership := backend.RunnerOwnership{
		Instance: "host-a", Target: "https://github.com/o/r", Pool: "windows", ScaleSetID: 7,
	}
	owned := runnerRecord{
		Name: "owned", Ownership: ownership, Overlay: "owned.qcow2", ISO: "owned.iso",
	}
	owned.Ownership.ScaleSetID = 6
	owned.Ownership.RunnerID = 41
	foreign := owned
	foreign.Name = "foreign"
	foreign.Ownership.Instance = "host-b"
	foreign.Ownership.RunnerID = 42
	ownedRecord := runnerRecordName("owned", owned.Ownership)
	foreignRecord := runnerRecordName("foreign", foreign.Ownership)
	for name, record := range map[string]runnerRecord{
		ownedRecord:   owned,
		foreignRecord: foreign,
	} {
		if err := writeRunnerRecord(filepath.Join(dir, name), record); err != nil {
			t.Fatal(err)
		}
		for _, artifact := range []string{record.Overlay, record.ISO} {
			createArtifact(t, dir, artifact)
		}
	}
	if err := os.WriteFile(
		filepath.Join(dir, ownershipNamespace(foreign.Ownership)+".broken.runner.json"),
		[]byte("{"), 0o600,
	); err != nil {
		t.Fatal(err)
	}
	be := &Backend{opt: Options{WorkDir: dir}, processAlive: processAlive, quitNamed: QMPQuitNamed}

	got, err := be.ListOwnedRunners(t.Context(), ownership)
	if err != nil {
		t.Fatalf("ListOwnedRunners: %v", err)
	}
	if len(got) != 1 || got[0].Name != "owned" || got[0].RunnerID != 41 {
		t.Fatalf("owned runners = %+v", got)
	}
	if err := be.RemoveOwnedRunner(t.Context(), got[0].ResourceID); err != nil {
		t.Fatalf("RemoveOwnedRunner: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ownedRecord)); !os.IsNotExist(err) {
		t.Fatalf("owned record still exists: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, foreignRecord)); err != nil {
		t.Fatalf("foreign record was removed: %v", err)
	}
	if err := be.RemoveOwnedRunner(t.Context(), got[0].ResourceID); err != nil {
		t.Fatalf("duplicate RemoveOwnedRunner: %v", err)
	}
}

func TestRunnerRecordReplacementIsReadable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runner.runner.json")
	if err := writeRunnerRecord(path, runnerRecord{Name: "first"}); err != nil {
		t.Fatal(err)
	}
	if err := writeRunnerRecord(path, runnerRecord{Name: "second"}); err != nil {
		t.Fatal(err)
	}
	record, err := readRunnerRecord(path)
	if err != nil {
		t.Fatal(err)
	}
	if record.Name != "second" {
		t.Fatalf("record name = %q, want second", record.Name)
	}
}

func TestRunnerRecordNameUsesStableOwnershipNamespace(t *testing.T) {
	if got := runnerRecordName("runner", backend.RunnerOwnership{}); got != "runner.runner.json" {
		t.Fatalf("unowned record name = %q", got)
	}
	ownership := backend.RunnerOwnership{
		Instance: "host-a", Target: "https://github.com/o/r", Pool: "windows", ScaleSetID: 7,
	}
	first := runnerRecordName("runner", ownership)
	ownership.ScaleSetID = 8
	second := runnerRecordName("runner", ownership)
	if first != second {
		t.Fatalf("record namespace changed with scale-set ID: %q != %q", first, second)
	}
	if first == "runner.runner.json" {
		t.Fatal("owned record did not receive an ownership namespace")
	}
}

func TestQEMULaunchRejectsNonPortableRunnerName(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value string
	}{
		{name: "slash traversal", value: "../runner"},
		{name: "backslash traversal", value: `..\runner`},
		{name: "drive relative", value: "C:runner"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			be := &Backend{}
			if _, err := be.Launch(t.Context(), backend.LaunchRequest{Name: tc.value}); err == nil {
				t.Fatalf("Launch accepted invalid runner name %q", tc.value)
			}
		})
	}
}

func TestQEMUOwnedRunnerRemovalRequiresVerifiedProcessIdentity(t *testing.T) {
	dir := t.TempDir()
	recordPath := filepath.Join(dir, "runner.runner.json")
	record := runnerRecord{Name: "runner", PID: 123, Started: true}
	if err := writeRunnerRecord(recordPath, record); err != nil {
		t.Fatal(err)
	}
	be := &Backend{
		opt:          Options{WorkDir: dir},
		processAlive: func(int) (bool, error) { return true, nil },
		quitNamed: func(context.Context, string, string) error {
			return errors.New("qemu identity mismatch")
		},
	}

	err := be.RemoveOwnedRunner(t.Context(), filepath.Base(recordPath))
	if err == nil || !strings.Contains(err.Error(), "identity mismatch") {
		t.Fatalf("RemoveOwnedRunner error = %v, want identity mismatch", err)
	}
	if _, err := os.Stat(recordPath); err != nil {
		t.Fatalf("record removed without verified process identity: %v", err)
	}
}

func TestQEMUOwnedRunnerRemovalStopsVerifiedProcess(t *testing.T) {
	dir := t.TempDir()
	recordPath := filepath.Join(dir, "runner.runner.json")
	record := runnerRecord{Name: "runner", PID: 123, QMPAddr: "127.0.0.1:1234", Started: true}
	if err := writeRunnerRecord(recordPath, record); err != nil {
		t.Fatal(err)
	}
	var checks int
	var quitName string
	be := &Backend{
		opt: Options{WorkDir: dir},
		processAlive: func(int) (bool, error) {
			checks++
			return checks == 1, nil
		},
		quitNamed: func(_ context.Context, _ string, name string) error {
			quitName = name
			return nil
		},
	}
	if err := be.RemoveOwnedRunner(t.Context(), filepath.Base(recordPath)); err != nil {
		t.Fatalf("RemoveOwnedRunner: %v", err)
	}
	if quitName != "runner" {
		t.Fatalf("quit name = %q, want runner", quitName)
	}
}

func TestQEMUOwnedRunnerRemovalRejectsTraversal(t *testing.T) {
	for _, tc := range []struct {
		name       string
		resourceID string
		valid      bool
	}{
		{name: "empty"},
		{name: "dot", resourceID: "."},
		{name: "dot dot", resourceID: ".."},
		{name: "parent slash", resourceID: "../foreign.runner.json"},
		{name: "parent backslash", resourceID: `..\foreign.runner.json`},
		{name: "nested slash", resourceID: "nested/foreign.runner.json"},
		{name: "nested backslash", resourceID: `nested\foreign.runner.json`},
		{name: "mixed separators", resourceID: `nested/..\foreign.runner.json`},
		{name: "absolute unix", resourceID: "/tmp/foreign.runner.json"},
		{name: "absolute windows slash", resourceID: "C:/temp/foreign.runner.json"},
		{name: "absolute windows backslash", resourceID: `C:\temp\foreign.runner.json`},
		{name: "drive relative", resourceID: "C:foreign.runner.json"},
		{name: "drive relative traversal", resourceID: `C:..\foreign.runner.json`},
		{name: "rooted windows", resourceID: `\foreign.runner.json`},
		{name: "UNC backslash", resourceID: `\\server\share\foreign.runner.json`},
		{name: "UNC slash", resourceID: "//server/share/foreign.runner.json"},
		{name: "valid simple", resourceID: "runner.runner.json", valid: true},
		{name: "valid owned", resourceID: "0123456789abcdef.mr-scaleset-7-a1b2c3d4.runner.json", valid: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			be := &Backend{opt: Options{WorkDir: t.TempDir()}}
			err := be.RemoveOwnedRunner(t.Context(), tc.resourceID)
			if tc.valid && err != nil {
				t.Fatalf("RemoveOwnedRunner(%q): %v", tc.resourceID, err)
			}
			if !tc.valid && err == nil {
				t.Fatalf("RemoveOwnedRunner accepted invalid resource ID %q", tc.resourceID)
			}
		})
	}
}

func TestQEMUOwnedRunnerRemovalRejectsSymlinkEscape(t *testing.T) {
	workDir := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside.runner.json")
	if err := writeRunnerRecord(outside, runnerRecord{Name: "outside"}); err != nil {
		t.Fatal(err)
	}
	resourceID := "linked.runner.json"
	if err := os.Symlink(outside, filepath.Join(workDir, resourceID)); err != nil {
		t.Skipf("symlink creation unavailable: %v", err)
	}

	be := &Backend{opt: Options{WorkDir: workDir}}
	err := be.RemoveOwnedRunner(t.Context(), resourceID)
	if err == nil || !strings.Contains(err.Error(), "escapes work directory") {
		t.Fatalf("RemoveOwnedRunner error = %v, want symlink escape rejection", err)
	}
	if _, err := os.Stat(outside); err != nil {
		t.Fatalf("outside runner record was changed: %v", err)
	}
}

func TestProcessAliveDetectsRunningAndExitedProcess(t *testing.T) {
	cmd := helperProcess(t)
	alive, err := processAlive(cmd.Process.Pid)
	if err != nil || !alive {
		t.Fatalf("running process = (%v, %v), want alive", alive, err)
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err == nil {
		t.Fatal("killed helper returned no exit error")
	}
	alive, err = processAlive(cmd.Process.Pid)
	if err != nil {
		t.Fatalf("inspect exited process: %v", err)
	}
	if alive {
		t.Fatal("exited process reported alive")
	}
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
