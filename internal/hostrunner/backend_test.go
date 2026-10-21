package hostrunner

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
	// Installed: a second call does not download again.
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
