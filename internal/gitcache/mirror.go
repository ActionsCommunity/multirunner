// Package gitcache maintains host-resident bare git mirrors so ephemeral runners
// can seed a workspace from a local clone (git alternates / --reference) instead
// of full-cloning from GitHub every job. The heavy object history comes from the
// local mirror; the per-job checkout still fetches the tip delta from GitHub with
// its own token, so private-repo auth is unaffected.
package gitcache

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// lastUsedFile marks when a mirror was last cloned/fetched/bundled, for GC.
const lastUsedFile = ".mr-lastused"

// Manager owns the mirror directory and serializes updates per repo.
type Manager struct {
	root    string
	baseURL string // e.g. https://github.com
	token   string // optional, for private mirror fetches
	logger  *slog.Logger

	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

// New creates a Manager rooted at dir. baseURL is the GitHub base (github.com or
// GHES); token (optional) authenticates mirror fetches for private repos.
func New(dir, baseURL, token string, logger *slog.Logger) (*Manager, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create git mirror dir: %w", err)
	}
	if baseURL == "" {
		baseURL = "https://github.com"
	}
	return &Manager{
		root:    dir,
		baseURL: strings.TrimRight(baseURL, "/"),
		token:   token,
		logger:  logger.With("component", "gitcache"),
		locks:   map[string]*sync.Mutex{},
	}, nil
}

// MirrorPath returns the host path of a repo's bare mirror.
func (m *Manager) MirrorPath(repoSlug string) string {
	return filepath.Join(m.root, filepath.FromSlash(repoSlug)+".git")
}

// ContainerPath returns the in-container mount target for a repo's mirror, in
// the path style of the target container OS ("windows" => C:\gitcache\..., else
// /gitcache/...).
func (m *Manager) ContainerPath(repoSlug, os string) string {
	if os == "windows" {
		return `C:\gitcache\` + strings.ReplaceAll(repoSlug, "/", `\`) + ".git"
	}
	return "/gitcache/" + repoSlug + ".git"
}

// EnsureMirror clones the repo as a bare mirror on first use, or fetches updates
// if it already exists. Concurrent calls for the same repo are serialized.
func (m *Manager) EnsureMirror(ctx context.Context, repoSlug string) (string, error) {
	lock := m.repoLock(repoSlug)
	lock.Lock()
	defer lock.Unlock()

	path := m.MirrorPath(repoSlug)
	cloneURL := m.cloneURL(repoSlug)

	if mirrorExists(path) {
		m.logger.Debug("updating mirror", "repo", repoSlug)
		if err := m.git(ctx, path, "fetch", "--prune", "origin"); err != nil {
			return "", fmt.Errorf("mirror fetch %s: %w", repoSlug, err)
		}
		m.touch(path)
		return path, nil
	}

	m.logger.Info("creating mirror", "repo", repoSlug)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	if err := m.git(ctx, "", "clone", "--mirror", cloneURL, path); err != nil {
		return "", fmt.Errorf("mirror clone %s: %w", repoSlug, err)
	}
	m.touch(path)
	return path, nil
}

// touch records that a mirror was just used (created/fetched/bundled), for GC.
func (m *Manager) touch(mirrorPath string) {
	_ = os.WriteFile(filepath.Join(mirrorPath, lastUsedFile), nil, 0o644)
}

// lastUsed returns when a mirror was last touched, falling back to the mirror
// directory's own mtime if the marker is missing (older mirrors).
func lastUsed(mirrorPath string) time.Time {
	if fi, err := os.Stat(filepath.Join(mirrorPath, lastUsedFile)); err == nil {
		return fi.ModTime()
	}
	if fi, err := os.Stat(mirrorPath); err == nil {
		return fi.ModTime()
	}
	return time.Time{}
}

// Sweep removes bare mirrors not used within maxAge. maxAge <= 0 disables it.
// Returns the number of mirrors removed.
func (m *Manager) Sweep(ctx context.Context, maxAge time.Duration) (int, error) {
	if maxAge <= 0 {
		return 0, nil
	}
	cutoff := time.Now().Add(-maxAge)
	removed := 0
	err := filepath.Walk(m.root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() || !strings.HasSuffix(path, ".git") || !mirrorExists(path) {
			return nil
		}
		if lastUsed(path).Before(cutoff) {
			lock := m.repoLock(m.slugFor(path))
			lock.Lock()
			rmErr := os.RemoveAll(path)
			lock.Unlock()
			if rmErr != nil {
				m.logger.Warn("mirror sweep remove failed", "path", path, "err", rmErr)
			} else {
				m.logger.Info("mirror swept (stale)", "path", path)
				removed++
			}
		}
		return filepath.SkipDir // a mirror dir has no nested mirrors
	})
	return removed, err
}

// slugFor reverses MirrorPath: the repo slug for a mirror directory.
func (m *Manager) slugFor(mirrorPath string) string {
	rel, err := filepath.Rel(m.root, mirrorPath)
	if err != nil {
		rel = mirrorPath
	}
	return filepath.ToSlash(strings.TrimSuffix(rel, ".git"))
}

// Bundle ensures the mirror is current, then writes a git bundle of all refs to
// w. A runner job-started hook clones the workspace from this bundle (bulk
// objects, no GitHub bandwidth) and re-points origin at GitHub so checkout
// fetches only the delta. The bundle carries objects only — no credentials.
func (m *Manager) Bundle(ctx context.Context, repoSlug string, w io.Writer) error {
	path, err := m.EnsureMirror(ctx, repoSlug)
	if err != nil {
		return err
	}
	lock := m.repoLock(repoSlug)
	lock.Lock()
	defer lock.Unlock()
	cmd := exec.CommandContext(ctx, "git", "--git-dir="+path, "bundle", "create", "-", "--all")
	cmd.Env = m.gitEnv()
	cmd.Stdout = w
	var errb strings.Builder
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git bundle %s: %w: %s", repoSlug, err, strings.TrimSpace(errb.String()))
	}
	return nil
}

func (m *Manager) repoLock(repoSlug string) *sync.Mutex {
	m.mu.Lock()
	defer m.mu.Unlock()
	l, ok := m.locks[repoSlug]
	if !ok {
		l = &sync.Mutex{}
		m.locks[repoSlug] = l
	}
	return l
}

func (m *Manager) cloneURL(repoSlug string) string {
	return m.baseURL + "/" + repoSlug + ".git"
}

// git runs a git command, optionally against the bare repository at gitDir.
//
// gitDir is passed as --git-dir rather than -C because every repository this
// package operates on is bare. Git refuses to *discover* a bare repository from
// the working directory when safe.bareRepository is "explicit" (a hardening
// control some orgs and CI sandboxes set); it only accepts one named explicitly
// via --git-dir or GIT_DIR. Using -C would break the cache for those users.
func (m *Manager) git(ctx context.Context, gitDir string, args ...string) error {
	full := make([]string, 0, len(args)+1)
	if gitDir != "" {
		full = append(full, "--git-dir="+gitDir)
	}
	full = append(full, args...)

	cmd := exec.CommandContext(ctx, "git", full...)
	cmd.Env = m.gitEnv()
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git %s: %w: %s", redact(args), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// gitEnv builds the environment for a git subprocess: the parent environment
// plus an auth header injected through Git's env-based config when a token is
// configured. Keeping the token out of argv avoids exposing it in command-line
// process listings.
//
// Git's env-based config is an indexed list. Inherited entries are rebuilt into
// a dense, validated list before the credential is appended. URL rewrites are
// excluded because they could redirect the configured GitHub URL to another
// host after authentication was attached. Inherited Authorization headers are
// also excluded to prevent duplicate credentials.
func (m *Manager) gitEnv() []string {
	env := append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	if m.token == "" {
		return env
	}

	count := 0
	if raw := os.Getenv("GIT_CONFIG_COUNT"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 0 {
			m.logger.Warn("ignoring unusable GIT_CONFIG_COUNT", "value", raw)
		} else {
			count = parsed
		}
	}

	type entry struct{ key, value string }
	entries := make([]entry, 0, count+1)
	for i := 0; i < count; i++ {
		key, keyOK := os.LookupEnv(fmt.Sprintf("GIT_CONFIG_KEY_%d", i))
		value, valueOK := os.LookupEnv(fmt.Sprintf("GIT_CONFIG_VALUE_%d", i))
		if !keyOK || !valueOK || strings.TrimSpace(key) == "" {
			m.logger.Warn("ignoring incomplete inherited git config", "index", i)
			continue
		}
		lowerKey := strings.ToLower(key)
		if strings.HasPrefix(lowerKey, "url.") &&
			(strings.HasSuffix(lowerKey, ".insteadof") || strings.HasSuffix(lowerKey, ".pushinsteadof")) {
			m.logger.Warn("ignoring inherited git URL rewrite while authenticated", "key", key)
			continue
		}
		if strings.HasPrefix(lowerKey, "http.") && strings.HasSuffix(lowerKey, ".extraheader") &&
			strings.HasPrefix(strings.ToLower(strings.TrimSpace(value)), "authorization:") {
			m.logger.Warn("ignoring inherited git Authorization header", "key", key)
			continue
		}
		entries = append(entries, entry{key: key, value: value})
	}

	hdr := "AUTHORIZATION: basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:"+m.token))
	base := strings.TrimRight(m.baseURL, "/") + "/"
	entries = append(entries, entry{key: "http." + base + ".extraHeader", value: hdr})
	env = append(env, fmt.Sprintf("GIT_CONFIG_COUNT=%d", len(entries)))
	for i, item := range entries {
		env = append(env,
			fmt.Sprintf("GIT_CONFIG_KEY_%d=%s", i, item.key),
			fmt.Sprintf("GIT_CONFIG_VALUE_%d=%s", i, item.value))
	}
	return env
}

func mirrorExists(path string) bool {
	// A bare mirror has a HEAD file at its root.
	if _, err := os.Stat(filepath.Join(path, "HEAD")); err == nil {
		return true
	}
	return false
}

func redact(args []string) string { return strings.Join(args, " ") }
