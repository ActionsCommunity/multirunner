package hostrunner

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/GerardSmit/multirunner/internal/backend"
)

func TestResolveRunnerRequiresDigestForCustomVersion(t *testing.T) {
	if _, _, err := resolveRunner("1.2.3", ""); err == nil {
		t.Fatal("custom version without digest should fail")
	}
	if _, _, err := resolveRunner("1.2.3", "abc"); err == nil {
		t.Fatal("short digest should fail")
	}
	v, d, err := resolveRunner("", "")
	if err != nil || v == "" || len(d) != 64 {
		t.Fatalf("default runner = %q %q %v", v, d, err)
	}
}

func TestHostEnvOverridesCaseInsensitively(t *testing.T) {
	got := hostEnv([]string{"Path=C:\\x", "HOME=h"}, map[string]string{"PATH": "C:\\y"}, nil, nil)
	joined := strings.Join(got, "|")
	if strings.Contains(joined, `Path=C:\x`) || !strings.Contains(joined, `PATH=C:\y`) || !strings.Contains(joined, "HOME=h") {
		t.Fatalf("hostEnv = %v", got)
	}
}

func TestHostEnvStripsSecretsByNameValueAndPrefix(t *testing.T) {
	got := strings.Join(hostEnv([]string{
		"github_pat=p1", "COPY=p1", "MULTIRUNNER_X=1", "OK=1",
	}, map[string]string{"MULTIRUNNER_GIT_MIRROR": "m"}, []string{"GITHUB_PAT"}, []string{"p1"}), "|")
	if strings.Contains(got, "p1") || strings.Contains(got, "MULTIRUNNER_X") {
		t.Fatalf("secret leaked: %s", got)
	}
	if !strings.Contains(got, "OK=1") || !strings.Contains(got, "MULTIRUNNER_GIT_MIRROR=m") {
		t.Fatalf("pool env and ordinary vars must survive: %s", got)
	}
}

func TestPingDoesNotCreateDirectories(t *testing.T) {
	base := t.TempDir()
	parent := filepath.Join(base, "missing")
	b := &Backend{opt: Options{
		RunnerDir: filepath.Join(parent, "runner"),
		StateDir:  filepath.Join(parent, "state"),
	}}
	if err := b.Ping(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(parent); !os.IsNotExist(err) {
		t.Fatalf("Ping created %s or returned an unexpected error: %v", parent, err)
	}

	file := filepath.Join(base, "not-a-directory")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	b.opt.RunnerDir = filepath.Join(file, "runner")
	if err := b.Ping(context.Background()); err == nil {
		t.Fatal("Ping should reject a path beneath a file")
	}
}

type identityErrorProcessOps struct{ err error }

func (p identityErrorProcessOps) start(*exec.Cmd) (processGroup, error) { return nil, p.err }
func (identityErrorProcessOps) alive(int) (bool, error)                 { return true, nil }
func (p identityErrorProcessOps) startTime(int) (int64, error)          { return 0, p.err }
func (p identityErrorProcessOps) killTree(int) error                    { return p.err }

func TestRemoveOwnedRunnerPreservesRecordWhenIdentityLookupFails(t *testing.T) {
	dir := t.TempDir()
	b := &Backend{
		opt: Options{
			RunnerDir: filepath.Join(dir, "runner"),
			StateDir:  filepath.Join(dir, "state"),
			Pool:      "pool",
		},
		proc: identityErrorProcessOps{err: errors.New("access denied")},
	}
	if err := os.MkdirAll(b.opt.StateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	ownership := backend.RunnerOwnership{
		Instance: "instance", Target: "acme/site", Pool: "pool", ScaleSetID: 1, RunnerID: 7,
	}
	resourceID := runnerRecordName("runner", ownership)
	recordPath := filepath.Join(b.opt.StateDir, resourceID)
	if err := writeRunnerRecord(recordPath, runnerRecord{
		Name: "runner", Ownership: ownership, Root: b.runnerRoot(), WorkFolder: "_work",
		PID: 1234, StartTime: 5678, Started: true,
	}); err != nil {
		t.Fatal(err)
	}

	err := b.RemoveOwnedRunner(context.Background(), resourceID)
	if err == nil || !strings.Contains(err.Error(), "identify live PID") {
		t.Fatalf("RemoveOwnedRunner error = %v, want identity lookup failure", err)
	}
	if _, err := os.Stat(recordPath); err != nil {
		t.Fatalf("retry record was removed after an inconclusive identity check: %v", err)
	}
}

func TestPruneStateKeepsNewestLogsAndDiagnosticsIndependently(t *testing.T) {
	stateDir := t.TempDir()
	b := &Backend{opt: Options{StateDir: stateDir}}
	for i := 0; i < 25; i++ {
		path := filepath.Join(stateDir, fmt.Sprintf("job-%02d.log", i))
		if err := os.WriteFile(path, []byte("log"), 0o600); err != nil {
			t.Fatal(err)
		}
		mod := time.Unix(int64(i+1), 0)
		if err := os.Chtimes(path, mod, mod); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 23; i++ {
		path := filepath.Join(stateDir, fmt.Sprintf("job-%02d.diag", i))
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
		mod := time.Unix(int64(i+1), 0)
		if err := os.Chtimes(path, mod, mod); err != nil {
			t.Fatal(err)
		}
	}

	b.pruneState()

	entries, err := os.ReadDir(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	logs, diagnostics := 0, 0
	for _, entry := range entries {
		switch {
		case strings.HasSuffix(entry.Name(), ".log"):
			logs++
		case strings.HasSuffix(entry.Name(), ".diag"):
			diagnostics++
		}
	}
	if logs != stateRetention || diagnostics != stateRetention {
		t.Fatalf("retained logs=%d diagnostics=%d, want %d each", logs, diagnostics, stateRetention)
	}
	for _, path := range []string{"job-00.log", "job-04.log", "job-00.diag", "job-02.diag"} {
		if _, err := os.Stat(filepath.Join(stateDir, path)); !os.IsNotExist(err) {
			t.Fatalf("old state %s was not pruned: %v", path, err)
		}
	}
	for _, path := range []string{"job-05.log", "job-24.log", "job-03.diag", "job-22.diag"} {
		if _, err := os.Stat(filepath.Join(stateDir, path)); err != nil {
			t.Fatalf("new state %s was pruned: %v", path, err)
		}
	}
}

func zipWith(t *testing.T, files map[string]string) (string, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "runner.zip")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	w := zip.NewWriter(f)
	for name, body := range files {
		fw, err := w.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := fw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	sum := sha256.Sum256(data)
	return path, hex.EncodeToString(sum[:])
}

func newTestBackend(t *testing.T, archive, digest string) *Backend {
	dir := t.TempDir()
	return &Backend{
		opt: Options{RunnerDir: filepath.Join(dir, "r"), StateDir: filepath.Join(dir, "s"), RunnerVersion: "1.0.0", RunnerSHA256: digest},
		download: func(_ context.Context, _ string, dst string) error {
			data, err := os.ReadFile(archive)
			if err != nil {
				return err
			}
			return os.WriteFile(dst, data, 0o600)
		},
		proc: platformProcessOps(),
	}
}

func TestEnsureImageVerifiesAndExtracts(t *testing.T) {
	archive, digest := zipWith(t, map[string]string{"run.cmd": "@echo off", "bin/Runner.Listener.dll": "x"})
	b := newTestBackend(t, archive, digest)
	if err := b.EnsureImage(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(b.runnerRoot(), "bin", "Runner.Listener.dll")); err != nil {
		t.Fatal(err)
	}
	// A partial install (no marker) is redone; a complete one is trusted.
	if err := os.Remove(filepath.Join(b.runnerRoot(), installedMarker)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(b.runnerRoot(), "stale.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := b.EnsureImage(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(b.runnerRoot(), "stale.txt")); !os.IsNotExist(err) {
		t.Fatal("a partial install should be cleared before re-extracting")
	}
	// A valid marker with a missing runner binary is repaired rather than trusted.
	listener := filepath.Join(b.runnerRoot(), "bin", "Runner.Listener.dll")
	if err := os.Remove(listener); err != nil {
		t.Fatal(err)
	}
	repairs := 0
	b.download = func(_ context.Context, _ string, dst string) error {
		repairs++
		data, err := os.ReadFile(archive)
		if err != nil {
			return err
		}
		return os.WriteFile(dst, data, 0o600)
	}
	if err := b.EnsureImage(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	if repairs != 1 {
		t.Fatalf("repair downloads = %d, want 1", repairs)
	}
	if _, err := os.Stat(listener); err != nil {
		t.Fatalf("Runner.Listener.dll was not repaired: %v", err)
	}

	// Complete: another call does not download again.
	b.download = func(context.Context, string, string) error { t.Fatal("downloaded twice"); return nil }
	if err := b.EnsureImage(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
}

func TestEnsureImageRejectsDigestMismatch(t *testing.T) {
	archive, _ := zipWith(t, map[string]string{"run.cmd": "@echo off"})
	b := newTestBackend(t, archive, strings.Repeat("a", 64))
	if err := b.EnsureImage(context.Background(), ""); err == nil || !strings.Contains(err.Error(), "digest mismatch") {
		t.Fatalf("want digest mismatch, got %v", err)
	}
	if _, err := os.Stat(b.runnerRoot()); !os.IsNotExist(err) {
		t.Fatal("nothing should be installed after a digest mismatch")
	}
}

func TestExtractZipRejectsTraversal(t *testing.T) {
	archive, _ := zipWith(t, map[string]string{"../evil.txt": "x"})
	if err := extractZip(archive, t.TempDir()); err == nil {
		t.Fatal("traversal entry should be rejected")
	}
}
