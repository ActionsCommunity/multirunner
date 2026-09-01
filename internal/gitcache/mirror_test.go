package gitcache

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func mustGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	// Point global config at an empty file so a developer's signing or hook
	// settings cannot break the fixture commits.
	emptyConfig := filepath.Join(t.TempDir(), "gitconfig")
	if err := os.WriteFile(emptyConfig, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com",
		"GIT_CONFIG_GLOBAL="+emptyConfig, "GIT_CONFIG_NOSYSTEM=1",
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
// duplicate semantics that os/exec applies. Keys are upper-cased because
// Windows resolves environment names case-insensitively.
func resolveEnv(env []string) map[string]string {
	out := map[string]string{}
	for _, kv := range env {
		if i := strings.IndexByte(kv, '='); i > 0 {
			out[strings.ToUpper(kv[:i])] = kv[i+1:]
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
	if got := env["GIT_CONFIG_KEY_2"]; got != "http./.extraHeader" {
		t.Errorf("GIT_CONFIG_KEY_2 = %q, want scoped extraHeader", got)
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
	if got := env["GIT_CONFIG_KEY_0"]; got != "http./.extraHeader" {
		t.Errorf("GIT_CONFIG_KEY_0 = %q, want scoped extraHeader", got)
	}
}

func TestGitEnvClampsOversizedInheritedConfig(t *testing.T) {
	// A huge count must not drive a huge allocation, but the entries that do
	// exist within the clamp (such as a hardening setting) must survive.
	t.Setenv("GIT_CONFIG_COUNT", "1000000000")
	t.Setenv("GIT_CONFIG_KEY_0", "safe.bareRepository")
	t.Setenv("GIT_CONFIG_VALUE_0", "explicit")

	m := &Manager{token: "test-token", logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	env := resolveEnv(m.gitEnv())

	if got := env["GIT_CONFIG_COUNT"]; got != "2" {
		t.Errorf("GIT_CONFIG_COUNT = %q, want the inherited entry plus the scoped auth entry", got)
	}
	if got := env["GIT_CONFIG_KEY_0"]; got != "safe.bareRepository" {
		t.Errorf("GIT_CONFIG_KEY_0 = %q, want inherited safe.bareRepository", got)
	}
	if got := env["GIT_CONFIG_KEY_1"]; got != "http./.extraHeader" {
		t.Errorf("GIT_CONFIG_KEY_1 = %q, want scoped extraHeader", got)
	}
}

func TestGitEnvDropsInheritedConfigParameters(t *testing.T) {
	// GIT_CONFIG_PARAMETERS is read by git at the same precedence as the
	// indexed list, so an inherited rewrite there would bypass the filter.
	t.Setenv("GIT_CONFIG_PARAMETERS", "'url.https://evil.example/.insteadof=https://github.com/'")

	m := &Manager{token: "test-token", baseURL: "https://github.com", logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	for name, env := range map[string][]string{"gitEnv": m.gitEnv(), "localGitEnv": m.localGitEnv()} {
		if _, ok := resolveEnv(env)["GIT_CONFIG_PARAMETERS"]; ok {
			t.Errorf("%s: GIT_CONFIG_PARAMETERS survived into the subprocess environment", name)
		}
	}
}

func TestStripIndexedGitConfigIsCaseInsensitive(t *testing.T) {
	env := []string{
		"PATH=/bin",
		"git_config_count=1",
		"Git_Config_Key_0=http.https://github.com/.extraHeader",
		"GIT_CONFIG_VALUE_0=Authorization: bearer inherited",
		"git_config_parameters='x=y'",
		"GIT_TERMINAL_PROMPT=0",
	}
	got := stripIndexedGitConfig(env)
	want := []string{"PATH=/bin", "GIT_TERMINAL_PROMPT=0"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("stripIndexedGitConfig = %q, want %q", got, want)
	}
}

func TestSweepSkipsBareRepositoriesItDidNotCreate(t *testing.T) {
	base := setupSourceRepo(t)
	mirrorRoot := t.TempDir()
	m, err := New(mirrorRoot, base, "", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	mirror, err := m.EnsureMirror(ctx, "repo")
	if err != nil {
		t.Fatal(err)
	}
	foreign := filepath.Join(mirrorRoot, "archive.git")
	mustGit(t, mirrorRoot, "init", "--bare", foreign)

	old := time.Now().Add(-48 * time.Hour)
	for _, p := range []string{filepath.Join(mirror, lastUsedFile), foreign} {
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatal(err)
		}
	}

	removed, err := m.Sweep(ctx, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 1 {
		t.Errorf("removed = %d, want only the stale mirror", removed)
	}
	if mirrorExists(mirror) {
		t.Error("stale mirror was not swept")
	}
	if !mirrorExists(foreign) {
		t.Error("bare repository without the last-used marker was deleted")
	}
}

func TestLocalGitEnvOmitsCredentials(t *testing.T) {
	t.Setenv("GIT_CONFIG_COUNT", "2")
	t.Setenv("GIT_CONFIG_KEY_0", "safe.bareRepository")
	t.Setenv("GIT_CONFIG_VALUE_0", "explicit")
	t.Setenv("GIT_CONFIG_KEY_1", "http.https://github.com/.extraHeader")
	t.Setenv("GIT_CONFIG_VALUE_1", "Authorization: bearer inherited")

	m := &Manager{
		baseURL: "https://github.com",
		token:   "manager-secret",
		logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	env := resolveEnv(m.localGitEnv())
	if got := env["GIT_CONFIG_COUNT"]; got != "1" {
		t.Fatalf("GIT_CONFIG_COUNT = %q, want only safe inherited config", got)
	}
	if got := env["GIT_CONFIG_KEY_0"]; got != "safe.bareRepository" {
		t.Errorf("GIT_CONFIG_KEY_0 = %q, want safe.bareRepository", got)
	}
	for key, value := range env {
		if strings.Contains(strings.ToLower(key+"="+value), "authorization:") ||
			strings.Contains(value, "manager-secret") {
			t.Fatalf("local git environment contains credentials in %s", key)
		}
	}
}

// GitHub App auth leaves the mirror PAT empty, so the unauthenticated manager is
// a normal configuration — not a degenerate one. Scrubbing must still happen.
func TestLocalGitEnvOmitsCredentialsWithoutToken(t *testing.T) {
	t.Setenv("GIT_CONFIG_COUNT", "2")
	t.Setenv("GIT_CONFIG_KEY_0", "safe.bareRepository")
	t.Setenv("GIT_CONFIG_VALUE_0", "explicit")
	t.Setenv("GIT_CONFIG_KEY_1", "http.https://github.com/.extraHeader")
	t.Setenv("GIT_CONFIG_VALUE_1", "Authorization: bearer inherited")

	m := &Manager{
		baseURL: "https://github.com",
		logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	env := resolveEnv(m.localGitEnv())
	if got := env["GIT_CONFIG_COUNT"]; got != "1" {
		t.Fatalf("GIT_CONFIG_COUNT = %q, want only safe inherited config", got)
	}
	if got := env["GIT_CONFIG_KEY_0"]; got != "safe.bareRepository" {
		t.Errorf("GIT_CONFIG_KEY_0 = %q, want safe.bareRepository", got)
	}
	// The dropped entry must be absent outright, not shadowed by a lower count.
	if _, ok := env["GIT_CONFIG_KEY_1"]; ok {
		t.Error("inherited Authorization entry survived in the environment")
	}
	for key, value := range env {
		if strings.Contains(strings.ToLower(key+"="+value), "authorization:") {
			t.Fatalf("local git environment contains credentials in %s", key)
		}
	}
}

// Without a token there is no credential to protect, so an inherited URL rewrite
// stays in force rather than being silently dropped.
func TestGitEnvWithoutTokenKeepsUrlRewrite(t *testing.T) {
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "url.https://mirror.invalid/.insteadOf")
	t.Setenv("GIT_CONFIG_VALUE_0", "https://github.com/")

	m := &Manager{
		baseURL: "https://github.com",
		logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	env := resolveEnv(m.gitEnv())
	if got := env["GIT_CONFIG_COUNT"]; got != "1" {
		t.Fatalf("GIT_CONFIG_COUNT = %q, want 1", got)
	}
	if got := env["GIT_CONFIG_KEY_0"]; got != "url.https://mirror.invalid/.insteadOf" {
		t.Errorf("GIT_CONFIG_KEY_0 = %q, want the inherited rewrite", got)
	}
	for key, value := range env {
		if strings.Contains(strings.ToUpper(key+"="+value), "AUTHORIZATION") {
			t.Fatalf("unauthenticated environment gained an auth header in %s", key)
		}
	}
}

func TestGitEnvDropsInheritedAuthorizationAndRewrite(t *testing.T) {
	t.Setenv("GIT_CONFIG_COUNT", "3")
	t.Setenv("GIT_CONFIG_KEY_0", "safe.bareRepository")
	t.Setenv("GIT_CONFIG_VALUE_0", "explicit")
	t.Setenv("GIT_CONFIG_KEY_1", "http.https://github.com/.extraHeader")
	t.Setenv("GIT_CONFIG_VALUE_1", "Authorization: bearer inherited")
	t.Setenv("GIT_CONFIG_KEY_2", "url.https://attacker.invalid/.insteadOf")
	t.Setenv("GIT_CONFIG_VALUE_2", "https://github.com/")

	m := &Manager{
		baseURL: "https://github.com",
		token:   "test-token",
		logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	env := resolveEnv(m.gitEnv())
	if got := env["GIT_CONFIG_COUNT"]; got != "2" {
		t.Fatalf("GIT_CONFIG_COUNT = %q, want safe config plus one auth header", got)
	}
	if got := env["GIT_CONFIG_KEY_1"]; got != "http.https://github.com/.extraHeader" {
		t.Errorf("auth key = %q, want GitHub-scoped header", got)
	}
	if strings.Contains(strings.ToLower(env["GIT_CONFIG_VALUE_0"]), "authorization:") {
		t.Error("inherited Authorization header survived sanitization")
	}
}

func TestGitAuthCannotBeRewrittenToAnotherHost(t *testing.T) {
	var primaryRequests, rewrittenRequests int
	var primaryAuth string
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		primaryRequests++
		primaryAuth = r.Header.Get("Authorization")
		http.NotFound(w, r)
	}))
	defer primary.Close()
	rewritten := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rewrittenRequests++
		http.NotFound(w, r)
	}))
	defer rewritten.Close()

	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", fmt.Sprintf("url.%s/.insteadOf", rewritten.URL))
	t.Setenv("GIT_CONFIG_VALUE_0", primary.URL+"/")
	m := &Manager{
		baseURL: primary.URL,
		token:   "test-token",
		logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	_ = m.git(context.Background(), "", "ls-remote", m.cloneURL("o/r"))
	if primaryRequests == 0 {
		t.Fatal("git did not contact the configured GitHub host")
	}
	if rewrittenRequests != 0 {
		t.Fatalf("credentialed request was rewritten to another host %d time(s)", rewrittenRequests)
	}
	if len(primaryAuth) < len("Basic ") || !strings.EqualFold(primaryAuth[:len("Basic ")], "Basic ") {
		t.Errorf("configured host Authorization = %q, want Basic credentials", primaryAuth)
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
