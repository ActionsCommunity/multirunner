package pool

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/GerardSmit/multirunner/internal/backend"
	"github.com/GerardSmit/multirunner/internal/config"
	"github.com/GerardSmit/multirunner/internal/github"
)

type failImageBackend struct{}

func (failImageBackend) Name() string                              { return "fail-image" }
func (failImageBackend) Ping(context.Context) error                { return nil }
func (failImageBackend) OSType(context.Context) (string, error)    { return "linux", nil }
func (failImageBackend) EnsureImage(context.Context, string) error { return errors.New("pull failed") }
func (failImageBackend) Launch(context.Context, backend.LaunchRequest) (backend.RunnerHandle, error) {
	return nil, errors.New("not reached")
}
func (failImageBackend) Close() error { return nil }

func TestManagerReturnsPoolStartupError(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	l := NewLauncher(
		config.Pool{Name: "linux", OS: "linux", Size: 1, MaxConsecutiveFailures: 1},
		"missing:image",
		failImageBackend{},
		nil,
		nil,
		nil,
		logger,
		Hooks{},
	)
	err := NewManager([]*Pool{NewPool(l, logger)}, logger).Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "pull failed") {
		t.Fatalf("Run error = %v, want pull failure", err)
	}
}

// countingProvider records how often rotation was consulted, so a test can prove
// a demand-driven launch used the repo it was handed instead of drifting.
type countingProvider struct {
	next  *github.Client
	calls int
}

func (p *countingProvider) NextClient() *github.Client                             { p.calls++; return p.next }
func (p *countingProvider) ClientFor(string) *github.Client                        { return nil }
func (p *countingProvider) QueuedJobs(context.Context) ([]github.QueuedJob, error) { return nil, nil }
func (p *countingProvider) Scope() config.Scope                                    { return config.ScopeRepos }

// apiRecorder is a stand-in GitHub API that records the paths it was called on
// and then fails, so a launch stops right after registration is attempted.
func apiRecorder(t *testing.T) (*httptest.Server, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		http.Error(w, "stop the launch here", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	return srv, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), paths...)
	}
}

func testClient(t *testing.T, url, repo string) *github.Client {
	t.Helper()
	c, err := github.New(context.Background(),
		config.GitHub{URL: url, Scope: config.ScopeRepo, Owner: "o", Repo: repo},
		config.Auth{PAT: "test-token"})
	if err != nil {
		t.Fatalf("github.New(%s): %v", repo, err)
	}
	return c
}

func testLauncher(gh github.ClientProvider, hooks Hooks) *Launcher {
	return NewLauncher(
		config.Pool{Name: "linux", OS: "linux", Size: 1, NamePrefix: "mr"},
		"img", failImageBackend{}, gh, nil, nil,
		slog.New(slog.NewTextHandler(io.Discard, nil)), hooks,
	)
}

// TestRunOneOnRegistersToGivenRepo is the launcher-side regression test for the
// placement bug: a runner launched because repoQueued had work must register to
// repoQueued, not to whatever repo rotation happens to point at.
func TestRunOneOnRegistersToGivenRepo(t *testing.T) {
	srv, recorded := apiRecorder(t)
	p := &countingProvider{next: testClient(t, srv.URL, "repoRotation")}
	l := testLauncher(p, Hooks{})

	if _, err := l.RunOneOn(context.Background(), testClient(t, srv.URL, "repoQueued")); err == nil {
		t.Fatal("RunOneOn should surface the stub API failure")
	}
	if p.calls != 0 {
		t.Errorf("rotation consulted %d times, want 0: a queued job must go to its own repo", p.calls)
	}
	paths := recorded()
	if len(paths) != 1 || !strings.Contains(paths[0], "/repos/o/repoQueued/") {
		t.Errorf("registered against %v, want a repoQueued path", paths)
	}
}

// TestRunOneUsesRotation pins the warm-capacity contract: with no queued job to
// place against, rotation is still the right choice.
func TestRunOneUsesRotation(t *testing.T) {
	srv, recorded := apiRecorder(t)
	p := &countingProvider{next: testClient(t, srv.URL, "repoRotation")}
	l := testLauncher(p, Hooks{})

	if _, err := l.RunOne(context.Background()); err == nil {
		t.Fatal("RunOne should surface the stub API failure")
	}
	if p.calls != 1 {
		t.Errorf("rotation consulted %d times, want 1", p.calls)
	}
	paths := recorded()
	if len(paths) != 1 || !strings.Contains(paths[0], "/repos/o/repoRotation/") {
		t.Errorf("registered against %v, want a repoRotation path", paths)
	}
}

// TestRunOneOnWithoutAnyClientFailsCleanly covers the reachable nil path:
// ClientFor returns nil for an unmanaged repo, and rotation can be empty too.
// That must be a clean error, not a nil dereference, and it must not leave an
// unmatched start behind in the metrics.
func TestRunOneOnWithoutAnyClientFailsCleanly(t *testing.T) {
	var starts, stops int
	l := testLauncher(&countingProvider{next: nil}, Hooks{
		OnStart: func(string) { starts++ },
		OnStop:  func(string, int, error) { stops++ },
	})

	_, err := l.RunOneOn(context.Background(), nil)
	if err == nil || !strings.Contains(err.Error(), "no github client") {
		t.Fatalf("RunOneOn error = %v, want a no-client failure", err)
	}
	if starts != 0 || stops != 0 {
		t.Errorf("hooks fired start=%d stop=%d, want 0/0 for a launch that never began", starts, stops)
	}
}
