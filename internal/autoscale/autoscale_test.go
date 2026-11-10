package autoscale

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GerardSmit/multirunner/internal/backend"
	"github.com/GerardSmit/multirunner/internal/config"
	"github.com/GerardSmit/multirunner/internal/github"
	"github.com/GerardSmit/multirunner/internal/pool"
)

type scalerProvider struct {
	client    *github.Client
	jobs      []github.QueuedJob
	queuedErr error
	calls     atomic.Int32
}

func (p *scalerProvider) ClientForSlot(int) *github.Client { return p.client }
func (p *scalerProvider) ClientFor(string) *github.Client {
	p.calls.Add(1)
	return p.client
}
func (p *scalerProvider) QueuedJobs(context.Context) ([]github.QueuedJob, error) {
	return append([]github.QueuedJob(nil), p.jobs...), p.queuedErr
}
func (p *scalerProvider) Scope() config.Scope { return config.ScopeRepo }

type scalerBackend struct {
	ensureErr error
	ensures   atomic.Int32
	seq       atomic.Int32
	launched  chan *scalerHandle
}

func (*scalerBackend) Name() string                           { return "test" }
func (*scalerBackend) Ping(context.Context) error             { return nil }
func (*scalerBackend) OSType(context.Context) (string, error) { return "linux", nil }
func (b *scalerBackend) EnsureImage(context.Context, string) error {
	b.ensures.Add(1)
	return b.ensureErr
}
func (b *scalerBackend) Launch(context.Context, backend.LaunchRequest) (backend.RunnerHandle, error) {
	h := &scalerHandle{
		id:     fmt.Sprintf("runner-%d", b.seq.Add(1)),
		done:   make(chan struct{}),
		killed: make(chan struct{}),
	}
	b.launched <- h
	return h, nil
}
func (*scalerBackend) Close() error { return nil }

type scalerHandle struct {
	id       string
	done     chan struct{}
	killed   chan struct{}
	doneOnce sync.Once
	killOnce sync.Once
}

func (h *scalerHandle) ID() string { return h.id }
func (h *scalerHandle) Wait(ctx context.Context) (int, error) {
	select {
	case <-h.done:
		return 0, nil
	case <-ctx.Done():
		return -1, ctx.Err()
	}
}
func (*scalerHandle) Logs(context.Context) (io.ReadCloser, error) { return nil, nil }
func (h *scalerHandle) Kill(context.Context) error {
	h.killOnce.Do(func() { close(h.killed) })
	h.complete()
	return nil
}
func (h *scalerHandle) complete() { h.doneOnce.Do(func() { close(h.done) }) }

func scalerClient(t *testing.T) *github.Client {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"encoded_jit_config":"jit","runner":{"id":42,"name":"mr"}}`)
	}))
	t.Cleanup(server.Close)
	client, err := github.New(context.Background(),
		config.GitHub{URL: server.URL, Scope: config.ScopeRepo, Owner: "o", Repo: "r"},
		config.Auth{PAT: "test-token"})
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func TestEnsureImagesReturnsPoolFailure(t *testing.T) {
	be := &scalerBackend{ensureErr: errors.New("pull failed"), launched: make(chan *scalerHandle, 1)}
	launcher := pool.NewLauncher(
		config.Pool{Name: "linux", OS: "linux", Size: 1},
		"missing:image", be, &scalerProvider{}, nil, nil,
		slog.New(slog.NewTextHandler(io.Discard, nil)), pool.Hooks{},
	)
	s := New(t.Context(), []*pool.Launcher{launcher}, &scalerProvider{}, config.ScopeRepo, 0,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	err := s.EnsureImages(t.Context())
	if err == nil || !strings.Contains(err.Error(), "pool linux") || !strings.Contains(err.Error(), "pull failed") {
		t.Fatalf("EnsureImages error = %v", err)
	}
}

func TestOnQueuedAfterCancellationDoesNotConsultProvider(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	provider := &scalerProvider{}
	s := New(ctx, nil, provider, config.ScopeRepo, 0, slog.New(slog.NewTextHandler(io.Discard, nil)))
	s.OnQueued("o/r", 42, []string{"self-hosted"})
	if got := provider.calls.Load(); got != 0 {
		t.Fatalf("provider calls after shutdown = %d, want 0", got)
	}
}

func TestOnQueuedLaunchesManagedRepository(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client := scalerClient(t)
	provider := &scalerProvider{client: client}
	be := &scalerBackend{launched: make(chan *scalerHandle, 1)}
	labels := []string{"self-hosted", "linux"}
	launcher := pool.NewLauncher(
		config.Pool{Name: "linux", OS: "linux", Size: 1, NamePrefix: "mr", Labels: labels},
		"image", be, provider, nil, nil,
		slog.New(slog.NewTextHandler(io.Discard, nil)), pool.Hooks{},
	)
	s := New(ctx, []*pool.Launcher{launcher}, provider, config.ScopeRepo, 0,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := s.EnsureImages(ctx); err != nil {
		t.Fatal(err)
	}

	s.OnQueued("o/r", 42, labels)
	select {
	case h := <-be.launched:
		h.complete()
	case <-time.After(time.Second):
		t.Fatal("queued job did not launch a runner")
	}
	if got := provider.calls.Load(); got != 1 {
		t.Fatalf("ClientFor calls = %d, want 1", got)
	}
}

func TestReconcileUsesPartialResultsAndStopsOnTotalFailure(t *testing.T) {
	client := scalerClient(t)
	labels := []string{"self-hosted", "linux"}
	job := github.QueuedJob{Client: client, Repository: "o/r", Labels: labels}

	t.Run("partial results", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		provider := &scalerProvider{
			client: client,
			jobs:   []github.QueuedJob{job},
			queuedErr: &github.RepoPollError{
				Failures: map[string]error{"o/down": errors.New("unavailable")},
				Total:    2,
			},
		}
		be := &scalerBackend{launched: make(chan *scalerHandle, 1)}
		launcher := pool.NewLauncher(
			config.Pool{Name: "linux", OS: "linux", Size: 1, NamePrefix: "mr", Labels: labels},
			"image", be, provider, nil, nil,
			slog.New(slog.NewTextHandler(io.Discard, nil)), pool.Hooks{},
		)
		s := New(ctx, []*pool.Launcher{launcher}, provider, config.ScopeRepos, 0,
			slog.New(slog.NewTextHandler(io.Discard, nil)))
		if err := s.EnsureImages(ctx); err != nil {
			t.Fatal(err)
		}
		s.reconcile()
		select {
		case h := <-be.launched:
			h.complete()
		case <-time.After(time.Second):
			t.Fatal("healthy repository result was discarded after a partial poll failure")
		}
	})

	t.Run("all failed", func(t *testing.T) {
		provider := &scalerProvider{
			client: client,
			jobs:   []github.QueuedJob{job},
			queuedErr: &github.RepoPollError{
				Failures: map[string]error{"o/r": errors.New("unavailable")},
				Total:    1,
			},
		}
		be := &scalerBackend{launched: make(chan *scalerHandle, 1)}
		launcher := pool.NewLauncher(
			config.Pool{Name: "linux", OS: "linux", Size: 1, NamePrefix: "mr", Labels: labels},
			"image", be, provider, nil, nil,
			slog.New(slog.NewTextHandler(io.Discard, nil)), pool.Hooks{},
		)
		s := New(t.Context(), []*pool.Launcher{launcher}, provider, config.ScopeRepo, 0,
			slog.New(slog.NewTextHandler(io.Discard, nil)))
		s.reconcile()
		select {
		case <-be.launched:
			t.Fatal("all-failed poll launched a runner from unusable results")
		default:
		}
	})
}

func TestRunPreparedStopsOnCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	s := New(ctx, nil, &scalerProvider{}, config.ScopeOrg, 0,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	done := make(chan error, 1)
	go func() { done <- s.RunPrepared(ctx) }()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("RunPrepared = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("RunPrepared ignored cancellation")
	}
}

func TestTryLaunchRespectsCapacityAndCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client := scalerClient(t)
	provider := &scalerProvider{client: client}
	be := &scalerBackend{launched: make(chan *scalerHandle, 2)}
	stopped := make(chan struct{}, 2)
	labels := []string{"self-hosted", "linux"}
	launcher := pool.NewLauncher(
		config.Pool{Name: "linux", OS: "linux", Size: 1, NamePrefix: "mr", Labels: labels},
		"image", be, provider, nil, nil,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		pool.Hooks{OnStop: func(string, int, error) { stopped <- struct{}{} }},
	)
	s := New(ctx, []*pool.Launcher{launcher}, provider, config.ScopeRepo, 0,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := s.EnsureImages(ctx); err != nil {
		t.Fatal(err)
	}
	if got := be.ensures.Load(); got != 1 {
		t.Fatalf("image ensures = %d, want 1", got)
	}

	job := github.QueuedJob{Client: client, Repository: "o/r", Labels: labels}
	if !s.tryLaunch(s.states[0], job) {
		t.Fatal("first launch was rejected")
	}
	first := <-be.launched
	if s.tryLaunch(s.states[0], job) {
		t.Fatal("second launch exceeded capacity")
	}

	first.complete()
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("completed runner did not report stop")
	}
	deadline := time.Now().Add(time.Second)
	for len(s.states[0].sem) != 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if len(s.states[0].sem) != 0 {
		t.Fatal("completed runner did not release capacity")
	}
	if !s.tryLaunch(s.states[0], job) {
		t.Fatal("replacement launch was rejected after capacity release")
	}
	second := <-be.launched

	cancel()
	select {
	case <-second.killed:
	case <-time.After(time.Second):
		t.Fatal("autoscaler cancellation did not kill the active runner")
	}
}

func TestLabelsMatch(t *testing.T) {
	pool := []string{"self-hosted", "linux", "x64"}
	cases := []struct {
		job  []string
		want bool
	}{
		{[]string{"self-hosted", "linux"}, true},
		{[]string{"self-hosted", "linux", "x64"}, true},
		{[]string{"self-hosted"}, true},
		{[]string{"self-hosted", "windows"}, false}, // windows not on pool
		{[]string{"gpu"}, false},
		{nil, true}, // no requested labels -> any runner matches
		// GitHub's default self-hosted labels are capitalized. Matching must be
		// case-insensitive or autoscale never launches for a standard workflow.
		{[]string{"self-hosted", "Linux", "X64"}, true},
		{[]string{"Self-Hosted", "LINUX"}, true},
	}
	for _, c := range cases {
		if got := labelsMatch(pool, c.job); got != c.want {
			t.Errorf("labelsMatch(%v, %v) = %v, want %v", pool, c.job, got, c.want)
		}
	}
}

// TestLabelsMatchGitHubWindowsCasing pins the exact live regression: a Windows
// pool configured with lowercase labels must serve `runs-on: [self-hosted,
// Windows, X64]`, which is what GitHub reports for every standard Windows job.
func TestLabelsMatchGitHubWindowsCasing(t *testing.T) {
	pool := []string{"self-hosted", "windows", "x64"}
	job := []string{"self-hosted", "Windows", "X64"}
	if !labelsMatch(pool, job) {
		t.Fatalf("labelsMatch(%v, %v) = false, want true", pool, job)
	}
}

// TestLabelsMatchStillRejectsWrongOS guards against the fix over-matching: a
// Linux pool must not pick up a Windows job just because casing is ignored.
func TestLabelsMatchStillRejectsWrongOS(t *testing.T) {
	pool := []string{"self-hosted", "linux", "x64"}
	job := []string{"self-hosted", "Windows", "X64"}
	if labelsMatch(pool, job) {
		t.Fatalf("labelsMatch(%v, %v) = true, want false", pool, job)
	}
}

func TestContainerBuildLabelDoesNotMatchGenericLinuxJob(t *testing.T) {
	pool := []string{"container-build"}
	if labelsMatch(pool, []string{"self-hosted", "Linux", "X64"}) {
		t.Fatal("custom-only container-build pool matched a generic Linux job")
	}
	if !labelsMatch(pool, []string{"container-build"}) {
		t.Fatal("custom-only container-build pool rejected its explicit label")
	}
}
