package gitcache

import (
	"context"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func mustGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// setupSourceRepo creates <baseDir>/repo.git with one commit and returns the
// base dir (as a clone base) using forward slashes.
func setupSourceRepo(t *testing.T) string {
	t.Helper()
	base := t.TempDir()
	repo := filepath.Join(base, "repo.git")
	mustGit(t, base, "init", "-b", "main", repo)
	if err := os.WriteFile(filepath.Join(repo, "a.txt"), []byte("one"), 0o600); err != nil {
		t.Fatal(err)
	}
	mustGit(t, repo, "add", "a.txt")
	mustGit(t, repo, "commit", "-m", "first")
	return filepath.ToSlash(base)
}

func TestEnsureMirrorCloneThenFetch(t *testing.T) {
	base := setupSourceRepo(t)
	repoDir := filepath.FromSlash(base + "/repo.git")
	mirrorRoot := t.TempDir()

	m, err := New(mirrorRoot, base, "", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	// First call clones the mirror.
	path, err := m.EnsureMirror(ctx, "repo")
	if err != nil {
		t.Fatalf("EnsureMirror clone: %v", err)
	}
	if !mirrorExists(path) {
		t.Fatalf("mirror not created at %s", path)
	}
	if n := commitCount(t, path); n != 1 {
		t.Fatalf("mirror commit count = %d, want 1", n)
	}

	// Add a second commit to the source.
	if err := os.WriteFile(filepath.Join(repoDir, "b.txt"), []byte("two"), 0o600); err != nil {
		t.Fatal(err)
	}
	mustGit(t, repoDir, "add", "b.txt")
	mustGit(t, repoDir, "commit", "-m", "second")

	// Second call fetches the update.
	if _, err := m.EnsureMirror(ctx, "repo"); err != nil {
		t.Fatalf("EnsureMirror fetch: %v", err)
	}
	if n := commitCount(t, path); n != 2 {
		t.Fatalf("mirror commit count after fetch = %d, want 2", n)
	}
}

func TestEnsureMirrorConcurrent(t *testing.T) {
	base := setupSourceRepo(t)
	m, err := New(t.TempDir(), base, "", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	var wg sync.WaitGroup
	errs := make([]error, 4)
	for i := range errs {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			_, errs[idx] = m.EnsureMirror(ctx, "repo")
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("goroutine %d: %v", i, err)
		}
	}
}

func TestContainerPath(t *testing.T) {
	m := &Manager{}
	if got := m.ContainerPath("octo/hello", "linux"); got != "/gitcache/octo/hello.git" {
		t.Errorf("linux ContainerPath = %q", got)
	}
	if got := m.ContainerPath("octo/hello", "windows"); got != `C:\gitcache\octo\hello.git` {
		t.Errorf("windows ContainerPath = %q", got)
	}
}

// forceExplicitBareRepository makes git refuse bare repositories discovered from
// the working directory, so tests exercise the same hardening control an org or
// CI sandbox may set. Only paths named via --git-dir/GIT_DIR stay usable.
func forceExplicitBareRepository(t *testing.T) {
	t.Helper()
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "safe.bareRepository")
	t.Setenv("GIT_CONFIG_VALUE_0", "explicit")
}

// resolveEnv collapses an environment slice into a map using the last-wins
// duplicate semantics that os/exec applies.
func resolveEnv(env []string) map[string]string {
	out := map[string]string{}
	for _, kv := range env {
		if i := strings.IndexByte(kv, '='); i > 0 {
			out[kv[:i]] = kv[i+1:]
		}
	}
	return out
}

// Mirrors are bare, so every git call must name them explicitly. Discovering
// them from the working directory fails under safe.bareRepository=explicit.
func TestMirrorOpsUnderExplicitBareRepository(t *testing.T) {
	forceExplicitBareRepository(t)

	base := setupSourceRepo(t)
	repoDir := filepath.FromSlash(base + "/repo.git")
	m, err := New(t.TempDir(), base, "", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	path, err := m.EnsureMirror(ctx, "repo")
	if err != nil {
		t.Fatalf("EnsureMirror clone: %v", err)
	}

	mustGit(t, repoDir, "commit", "--allow-empty", "-m", "second")

	// The fetch path is the one that regressed: it reuses an existing bare mirror.
	if _, err := m.EnsureMirror(ctx, "repo"); err != nil {
		t.Fatalf("EnsureMirror fetch: %v", err)
	}
	if n := commitCount(t, path); n != 2 {
		t.Fatalf("mirror commit count after fetch = %d, want 2", n)
	}
	if err := m.Bundle(ctx, "repo", io.Discard); err != nil {
		t.Fatalf("Bundle: %v", err)
	}
}

// A token must not cost the operator their own env-based git config. This is
// the combined case: the auth header is injected while safe.bareRepository is
// still in force, so both the header indexing and --git-dir have to be right.
func TestMirrorOpsWithTokenUnderExplicitBareRepository(t *testing.T) {
	forceExplicitBareRepository(t)

	base := setupSourceRepo(t)
	m, err := New(t.TempDir(), base, "test-token", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	if _, err := m.EnsureMirror(ctx, "repo"); err != nil {
		t.Fatalf("EnsureMirror clone: %v", err)
	}
	if _, err := m.EnsureMirror(ctx, "repo"); err != nil {
		t.Fatalf("EnsureMirror fetch: %v", err)
	}
	if err := m.Bundle(ctx, "repo", io.Discard); err != nil {
		t.Fatalf("Bundle: %v", err)
	}
}

func TestGitEnvAppendsHeaderAfterInheritedConfig(t *testing.T) {
	t.Setenv("GIT_CONFIG_COUNT", "2")
	t.Setenv("GIT_CONFIG_KEY_0", "safe.bareRepository")
	t.Setenv("GIT_CONFIG_VALUE_0", "explicit")
	t.Setenv("GIT_CONFIG_KEY_1", "credential.interactive")
	t.Setenv("GIT_CONFIG_VALUE_1", "never")

	m := &Manager{token: "test-token", logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	env := resolveEnv(m.gitEnv())

	if got := env["GIT_CONFIG_COUNT"]; got != "3" {
		t.Errorf("GIT_CONFIG_COUNT = %q, want 3", got)
	}
	if got := env["GIT_CONFIG_KEY_2"]; got != "http.extraHeader" {
		t.Errorf("GIT_CONFIG_KEY_2 = %q, want http.extraHeader", got)
	}
	if got := env["GIT_CONFIG_VALUE_2"]; !strings.HasPrefix(got, "AUTHORIZATION: basic ") {
		t.Errorf("GIT_CONFIG_VALUE_2 = %q, want an authorization header", got)
	}
	// The inherited entries must survive untouched.
	if got := env["GIT_CONFIG_KEY_0"]; got != "safe.bareRepository" {
		t.Errorf("GIT_CONFIG_KEY_0 = %q, want safe.bareRepository", got)
	}
	if got := env["GIT_CONFIG_KEY_1"]; got != "credential.interactive" {
		t.Errorf("GIT_CONFIG_KEY_1 = %q, want credential.interactive", got)
	}
}

func TestGitEnvWithoutTokenLeavesConfigAlone(t *testing.T) {
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "safe.bareRepository")
	t.Setenv("GIT_CONFIG_VALUE_0", "explicit")

	m := &Manager{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	env := resolveEnv(m.gitEnv())

	if got := env["GIT_CONFIG_COUNT"]; got != "1" {
		t.Errorf("GIT_CONFIG_COUNT = %q, want 1 (unchanged)", got)
	}
	if got := env["GIT_CONFIG_KEY_0"]; got != "safe.bareRepository" {
		t.Errorf("GIT_CONFIG_KEY_0 = %q, want safe.bareRepository", got)
	}
	if got := env["GIT_TERMINAL_PROMPT"]; got != "0" {
		t.Errorf("GIT_TERMINAL_PROMPT = %q, want 0", got)
	}
}

func TestGitEnvIgnoresBogusCount(t *testing.T) {
	t.Setenv("GIT_CONFIG_COUNT", "not-a-number")

	m := &Manager{token: "test-token", logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	env := resolveEnv(m.gitEnv())

	// Git rejects a bogus count, so fall back to a fresh single-entry list.
	if got := env["GIT_CONFIG_COUNT"]; got != "1" {
		t.Errorf("GIT_CONFIG_COUNT = %q, want 1", got)
	}
	if got := env["GIT_CONFIG_KEY_0"]; got != "http.extraHeader" {
		t.Errorf("GIT_CONFIG_KEY_0 = %q, want http.extraHeader", got)
	}
}

func commitCount(t *testing.T, mirrorPath string) int {
	t.Helper()
	// --git-dir, not cmd.Dir: the mirror is bare, and git refuses to discover a
	// bare repo from the working directory under safe.bareRepository=explicit.
	out := mustGit(t, "", "--git-dir="+mirrorPath, "rev-list", "--count", "main")
	n := 0
	for _, c := range out {
		if c < '0' || c > '9' {
			continue
		}
		n = n*10 + int(c-'0')
	}
	return n
}
