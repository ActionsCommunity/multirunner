//go:build windows

package hostrunner

import (
	"context"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/GerardSmit/multirunner/internal/backend"
)

// tookJob is what a fake run.cmd prints so the backend sees a job was run.
const tookJob = "echo 2026-01-01 00:00:00Z: Running job: test"

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
		logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	root := b.runnerRoot()
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(b.opt.StateDir, 0o700); err != nil {
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

func waitCtx(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// hiddenReadOnly writes a file the way the runner leaves its credentials.
func hiddenReadOnly(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("{}"), 0o400); err != nil {
		t.Fatal(err)
	}
	p, _ := syscall.UTF16PtrFromString(path)
	if err := syscall.SetFileAttributes(p, syscall.FILE_ATTRIBUTE_HIDDEN|syscall.FILE_ATTRIBUTE_READONLY); err != nil {
		t.Fatal(err)
	}
}

func TestLaunchRunsJobAndCleansWorkspace(t *testing.T) {
	b := fakeRunner(t, `if not "%1"=="--jitconfig" exit /b 9`+"\r\n"+tookJob+"\r\n"+
		`mkdir _work\repo & echo %MR_TEST%> _work\repo\out.txt`+"\r\n"+
		`mkdir _diag & echo worker> _diag\Worker_1.log & exit /b 3`)
	h := launch(t, b, "job-1", map[string]string{"MR_TEST": "hello"})
	code, err := h.Wait(waitCtx(t))
	if err != nil || code != 3 {
		t.Fatalf("Wait = %d, %v; want 3, nil", code, err)
	}
	root := b.runnerRoot()
	if exists(filepath.Join(root, "_work")) || exists(filepath.Join(root, "_diag")) {
		t.Fatal("work folder and _diag must not stay in the install")
	}
	if !exists(filepath.Join(b.opt.StateDir, "job-1.diag", "Worker_1.log")) {
		t.Fatal("_diag should be archived to the state dir")
	}
	if exists(filepath.Join(b.opt.StateDir, "job-1.runner.json")) || !exists(filepath.Join(b.opt.StateDir, "job-1.log")) {
		t.Fatal("record removed, log kept")
	}
	// The install is free again for the next job.
	if code, err := launch(t, b, "job-2", nil).Wait(waitCtx(t)); err != nil || code != 3 {
		t.Fatalf("job-2 Wait = %d, %v", code, err)
	}
}

func TestLaunchClearsStateFromAnInterruptedRunner(t *testing.T) {
	// The runner refuses to start over hidden credentials it left behind.
	b := fakeRunner(t, `if exist .runner exit /b 0`+"\r\n"+tookJob+"\r\n"+`exit /b 0`)
	root := b.runnerRoot()
	for _, name := range []string{".runner", ".credentials", ".credentials_rsaparams"} {
		hiddenReadOnly(t, filepath.Join(root, name))
	}
	if err := os.MkdirAll(filepath.Join(root, "_work", "old"), 0o700); err != nil {
		t.Fatal(err)
	}
	code, err := launch(t, b, "job-1", nil).Wait(waitCtx(t))
	if err != nil || code != 0 {
		t.Fatalf("Wait = %d, %v; stale state should have been cleared first", code, err)
	}
	if exists(filepath.Join(root, ".runner")) || exists(filepath.Join(root, "_work")) {
		t.Fatal("stale runner state must be gone")
	}
}

func TestRunnerThatNeverTookAJobIsAFailure(t *testing.T) {
	b := fakeRunner(t, `echo Runner listener exit with terminated error & exit /b 0`)
	code, err := launch(t, b, "job-1", nil).Wait(waitCtx(t))
	if err == nil || !strings.Contains(err.Error(), "without running a job") {
		t.Fatalf("Wait = %d, %v; want a no-job error", code, err)
	}
}

func TestLaunchStripsMultirunnerSecrets(t *testing.T) {
	t.Setenv("GITHUB_PAT", "pat-value")
	t.Setenv("SOME_OTHER", "pat-value")
	t.Setenv("MULTIRUNNER_CACHE_ACCESS_TOKEN", "cache")
	t.Setenv("HARMLESS", "ok")
	b := fakeRunner(t, tookJob+"\r\n"+`set > env.txt & exit /b 0`)
	b.Configure([]string{"GITHUB_PAT"}, []string{"pat-value"}, nil)
	if _, err := launch(t, b, "job-1", nil).Wait(waitCtx(t)); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(b.runnerRoot(), "env.txt"))
	if err != nil {
		t.Fatal(err)
	}
	env := string(data)
	for _, leaked := range []string{"GITHUB_PAT=", "SOME_OTHER=", "MULTIRUNNER_CACHE_ACCESS_TOKEN=", "pat-value"} {
		if strings.Contains(env, leaked) {
			t.Fatalf("%q reached the job environment", leaked)
		}
	}
	if !strings.Contains(env, "HARMLESS=ok") {
		t.Fatal("ordinary host variables should still reach the job")
	}
}

func TestLaunchRefusesConcurrentJobs(t *testing.T) {
	b := fakeRunner(t, tookJob+"\r\n"+`ping -n 30 127.0.0.1 >nul`)
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
	pid := waitForPIDFile(t, filepath.Join(b.runnerRoot(), "child.pid"))
	ctx := waitCtx(t)
	if err := h.Kill(ctx); err != nil {
		t.Fatal(err)
	}
	if err := waitForExit(ctx, pid, b.proc.alive); err != nil {
		t.Fatalf("grandchild %d survived Kill: %v", pid, err)
	}
}

func TestCloseStopsTheActiveRunnerAndCleansUp(t *testing.T) {
	b := fakeRunner(t, tookJob+"\r\n"+`mkdir _work & ping -n 60 127.0.0.1 >nul`)
	h := launch(t, b, "job-1", nil)
	deadline := time.Now().Add(10 * time.Second)
	for !exists(filepath.Join(b.runnerRoot(), "_work")) && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Wait(waitCtx(t)); err == nil {
		t.Log("killed runner reported success; acceptable, cleanup is what matters")
	}
	if exists(filepath.Join(b.runnerRoot(), "_work")) || exists(filepath.Join(b.opt.StateDir, "job-1.runner.json")) {
		t.Fatal("Close must wait for cleanup")
	}
}

func TestCleanupOrphansRecoversAfterACrashMidJob(t *testing.T) {
	b := fakeRunner(t, `exit /b 0`)
	root := b.runnerRoot()
	// A "previous multirunner" left a live runner process, its record, stale
	// credentials and a work folder.
	orphan := exec.Command("ping", "-n", "60", "127.0.0.1")
	if err := orphan.Start(); err != nil {
		t.Fatal(err)
	}
	defer orphan.Process.Kill()
	started, err := b.proc.startTime(orphan.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	record := runnerRecord{Name: "job-1", Root: root, WorkFolder: "_work",
		PID: orphan.Process.Pid, StartTime: started, Started: true}
	if err := writeRunnerRecord(filepath.Join(b.opt.StateDir, "job-1.runner.json"), record); err != nil {
		t.Fatal(err)
	}
	hiddenReadOnly(t, filepath.Join(root, ".credentials"))
	if err := os.MkdirAll(filepath.Join(root, "_work", "repo"), 0o700); err != nil {
		t.Fatal(err)
	}

	if err := b.CleanupOrphans(); err != nil {
		t.Fatal(err)
	}
	if err := waitForExit(waitCtx(t), orphan.Process.Pid, b.proc.alive); err != nil {
		t.Fatal("orphaned runner should be stopped")
	}
	if exists(filepath.Join(root, ".credentials")) || exists(filepath.Join(root, "_work")) ||
		exists(filepath.Join(b.opt.StateDir, "job-1.runner.json")) {
		t.Fatal("orphaned state should be cleared")
	}
}

func TestRecoveryRefusesToKillAnUnidentifiedPID(t *testing.T) {
	b := fakeRunner(t, `exit /b 0`)
	record := runnerRecord{Name: "job-1", Root: b.runnerRoot(), WorkFolder: "_work",
		PID: os.Getpid(), Started: true} // alive, but no recorded start time
	if err := writeRunnerRecord(filepath.Join(b.opt.StateDir, "job-1.runner.json"), record); err != nil {
		t.Fatal(err)
	}
	if err := b.CleanupOrphans(); err == nil || !strings.Contains(err.Error(), "refusing to kill") {
		t.Fatalf("want a refusal, got %v", err)
	}
	// Reaching here means the test process was not killed.
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
	if exists(path) {
		t.Fatal("record should be removed")
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

func TestNotRunningAsLocalSystemInTests(t *testing.T) {
	system, err := runningAsLocalSystem()
	if err != nil {
		t.Fatal(err)
	}
	if system {
		t.Skip("test runner is LocalSystem")
	}
}

func waitForPIDFile(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(path); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil && pid > 0 {
				return pid
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("child never recorded its PID")
	return 0
}
