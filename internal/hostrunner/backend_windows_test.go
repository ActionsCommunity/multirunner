//go:build windows

package hostrunner

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/GerardSmit/multirunner/internal/backend"
)

// fakeRunner installs a run.cmd that stands in for the actions runner.
func fakeRunner(t *testing.T, script string) *Backend {
	t.Helper()
	dir := t.TempDir()
	b := &Backend{
		opt: Options{
			RunnerDir: filepath.Join(dir, "runner"), StateDir: filepath.Join(dir, "state"),
			RunnerVersion: "9.9.9", RunnerSHA256: strings.Repeat("0", 64),
		},
		download: func(context.Context, string, string) error { t.Fatal("unexpected download"); return nil },
		proc:     platformProcessOps(),
	}
	root := b.runnerRoot()
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "run.cmd"), []byte("@echo off\r\n"+script+"\r\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := b.Ping(context.Background()); err != nil {
		t.Fatal(err)
	}
	return b
}

func launch(t *testing.T, b *Backend, name string, env map[string]string) backend.RunnerHandle {
	t.Helper()
	h, err := b.Launch(context.Background(), backend.LaunchRequest{
		Name: name, EncodedJITConfig: "aml0", WorkFolder: "_work", Env: env,
	})
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func TestLaunchRunsJobAndCleansWorkspace(t *testing.T) {
	b := fakeRunner(t, `if not "%1"=="--jitconfig" exit /b 9`+"\r\n"+
		`mkdir _work\repo & echo %MR_TEST%> _work\repo\out.txt & exit /b 3`)
	h := launch(t, b, "job-1", map[string]string{"MR_TEST": "hello"})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	code, err := h.Wait(ctx)
	if err != nil || code != 3 {
		t.Fatalf("Wait = %d, %v; want 3, nil", code, err)
	}
	if _, err := os.Stat(filepath.Join(b.runnerRoot(), "_work")); !os.IsNotExist(err) {
		t.Fatalf("work folder should be removed after the job, stat err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(b.opt.StateDir, "job-1.runner.json")); !os.IsNotExist(err) {
		t.Fatalf("runner record should be removed, stat err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(b.opt.StateDir, "job-1.log")); err != nil {
		t.Fatalf("runner log should be kept: %v", err)
	}
	// The install is free again for the next job.
	h2 := launch(t, b, "job-2", nil)
	if _, err := h2.Wait(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestLaunchRefusesConcurrentJobs(t *testing.T) {
	b := fakeRunner(t, `ping -n 30 127.0.0.1 >nul`)
	h := launch(t, b, "job-1", nil)
	defer h.Kill(context.Background())
	_, err := b.Launch(context.Background(), backend.LaunchRequest{Name: "job-2", EncodedJITConfig: "aml0"})
	if err == nil || !strings.Contains(err.Error(), "one job at a time") {
		t.Fatalf("second launch should be refused, got %v", err)
	}
}

func TestKillTerminatesWholeProcessTree(t *testing.T) {
	// run.cmd starts a grandchild that records its own PID, then sleeps.
	b := fakeRunner(t, `powershell -NoProfile -Command "Set-Content child.pid $PID; Start-Sleep 60"`)
	h := launch(t, b, "job-1", nil)

	var pid int
	deadline := time.Now().Add(20 * time.Second)
	for pid == 0 && time.Now().Before(deadline) {
		if data, err := os.ReadFile(filepath.Join(b.runnerRoot(), "child.pid")); err == nil {
			pid, _ = strconv.Atoi(strings.TrimSpace(string(data)))
		}
		time.Sleep(100 * time.Millisecond)
	}
	if pid == 0 {
		t.Fatal("grandchild ping never started")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := h.Kill(ctx); err != nil {
		t.Fatal(err)
	}
	if err := waitForExit(ctx, pid, b.proc.alive); err != nil {
		t.Fatalf("grandchild %d survived Kill: %v", pid, err)
	}
}

func TestRemoveOwnedRunnerSkipsReusedPID(t *testing.T) {
	b := fakeRunner(t, `exit /b 0`)
	own := backend.RunnerOwnership{Instance: "i", Target: "acme/site", Pool: "sync", ScaleSetID: 1, RunnerID: 7}
	record := runnerRecord{
		Name: "job-1", Ownership: own, Root: b.runnerRoot(), WorkFolder: "_work",
		PID: os.Getpid(), StartTime: 1, Started: true, // our PID, but a different start time
	}
	path := filepath.Join(b.opt.StateDir, runnerRecordName("job-1", own))
	if err := writeRunnerRecord(path, record); err != nil {
		t.Fatal(err)
	}
	owned, err := b.ListOwnedRunners(context.Background(), own)
	if err != nil || len(owned) != 1 || owned[0].RunnerID != 7 {
		t.Fatalf("ListOwnedRunners = %+v, %v", owned, err)
	}
	if err := b.RemoveOwnedRunner(context.Background(), owned[0].ResourceID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("record should be removed, stat err = %v", err)
	}
	// Reaching here means the test process was not killed.
}

func TestLaunchRejectsMountsAndUnsafeNames(t *testing.T) {
	b := fakeRunner(t, `exit /b 0`)
	for _, req := range []backend.LaunchRequest{
		{Name: `..\x`, EncodedJITConfig: "a"},
		{Name: "ok", EncodedJITConfig: "a", WorkFolder: `..`},
		{Name: "ok", EncodedJITConfig: "a", Mounts: []backend.Mount{{Source: "s", Target: "t"}}},
		{Name: "ok"},
	} {
		if _, err := b.Launch(context.Background(), req); err == nil {
			t.Fatalf("Launch(%+v) should fail", req)
		}
	}
}
