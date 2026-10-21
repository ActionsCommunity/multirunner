package autoscale

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GerardSmit/multirunner/internal/backend"
	"github.com/GerardSmit/multirunner/internal/config"
	"github.com/GerardSmit/multirunner/internal/github"
	"github.com/GerardSmit/multirunner/internal/pool"
	"github.com/GerardSmit/multirunner/internal/runner"
	"github.com/GerardSmit/multirunner/internal/webhook"
)

type demandBackend struct {
	launched  chan *demandHandle
	count     atomic.Int32
	launchErr error
}

func (*demandBackend) Name() string                              { return "demand" }
func (*demandBackend) Ping(context.Context) error                { return nil }
func (*demandBackend) OSType(context.Context) (string, error)    { return "linux", nil }
func (*demandBackend) EnsureImage(context.Context, string) error { return nil }
func (*demandBackend) Close() error                              { return nil }
func (b *demandBackend) Launch(_ context.Context, request backend.LaunchRequest) (backend.RunnerHandle, error) {
	if b.launchErr != nil {
		return nil, b.launchErr
	}
	h := &demandHandle{id: request.Name, done: make(chan struct{})}
	b.count.Add(1)
	b.launched <- h
	return h, nil
}

type demandHandle struct {
	id   string
	done chan struct{}
	once sync.Once
}

func (h *demandHandle) ID() string { return h.id }
func (h *demandHandle) Wait(ctx context.Context) (int, error) {
	select {
	case <-ctx.Done():
		return -1, ctx.Err()
	case <-h.done:
		return 0, nil
	}
}
func (*demandHandle) Logs(context.Context) (io.ReadCloser, error) { return nil, nil }
func (h *demandHandle) Kill(context.Context) error {
	h.complete()
	return nil
}
func (h *demandHandle) complete() { h.once.Do(func() { close(h.done) }) }

func demandTestScaler(t *testing.T, scope config.Scope, size int) (*Scaler, *pool.Launcher, *demandBackend, <-chan int64) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
		case strings.Contains(r.URL.Path, "/actions/runs/"):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": 100, "run_attempt": 1, "status": "in_progress", "event": "push",
				"path": ".github/workflows/ci.yml", "head_branch": "main",
				"triggering_actor": map[string]any{"login": "trusted"},
			})
		case strings.Contains(r.URL.Path, "/actions/jobs/"):
			status := "queued"
			if strings.HasSuffix(r.URL.Path, "/5") {
				status = "completed"
			}
			id, _ := strconv.ParseInt(path.Base(r.URL.Path), 10, 64)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": id, "run_id": 100,
				"status": status, "labels": []string{"self-hosted", "linux"}})
		case r.Method == http.MethodPost:
			_, _ = io.WriteString(w, `{"encoded_jit_config":"jit","runner":{"id":42}}`)
		default:
			_, _ = io.WriteString(w, `{"busy":false}`)
		}
	}))
	t.Cleanup(server.Close)
	repo := ""
	if scope == config.ScopeRepo {
		repo = "r"
	}
	client, err := github.New(t.Context(),
		config.GitHub{URL: server.URL, Scope: scope, Owner: "o", Repo: repo},
		config.Auth{PAT: "test"})
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.DiscardHandler)
	be := &demandBackend{launched: make(chan *demandHandle, 20)}
	jobs := make(chan int64, 20)
	launcher := pool.NewLauncher(config.Pool{Name: "linux", Size: size,
		Labels: []string{"self-hosted", "linux"}, NamePrefix: "mr", OS: "linux"},
		"image", be, client, nil, nil, logger, pool.Hooks{Observer: runner.LifecycleObserverFunc(
			func(_ context.Context, event runner.LifecycleEvent) {
				if event.Type == runner.LifecycleLaunched {
					jobs <- event.QueuedJobID
				}
			},
		)})
	scaler := New([]*pool.Launcher{launcher}, client, scope, 0, logger)
	return scaler, launcher, be, jobs
}

func demandEvent(id int64) webhook.WorkflowJobEvent {
	return webhook.WorkflowJobEvent{Repository: "o/r", Action: "queued",
		WorkflowJob: webhook.WorkflowJob{ID: id, RunID: 100, RunAttempt: 1,
			Status: "queued", Labels: []string{"self-hosted", "linux"}}}
}

func waitDemand(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("demand state did not converge")
}

func pendingDemand(s *Scaler) int {
	s.demandMu.Lock()
	defer s.demandMu.Unlock()
	return len(s.pending)
}

func TestPausedWebhookDemandExceedingCapacityResumesWithoutRedelivery(t *testing.T) {
	for _, scope := range []config.Scope{config.ScopeRepo, config.ScopeOrg, config.ScopeEnterprise} {
		t.Run(string(scope), func(t *testing.T) {
			scaler, launcher, be, jobs := demandTestScaler(t, scope, 2)
			launcher.Pause()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- scaler.Run(ctx) }()
			waitDemand(t, func() bool { return scaler.context() == ctx })
			for id := int64(1); id <= 7; id++ {
				scaler.OnQueued(demandEvent(id))
				scaler.OnQueued(demandEvent(id))
			}
			if pendingDemand(scaler) != 7 || len(scaler.states[0].sem) != 0 || be.count.Load() != 0 {
				t.Fatalf("paused demand = %d, slots = %d, launches = %d",
					pendingDemand(scaler), len(scaler.states[0].sem), be.count.Load())
			}
			completed := demandEvent(4)
			completed.Action = "completed"
			scaler.OnWorkflowJob(ctx, completed)
			// The run remains active after one job completes. Revalidation
			// must inspect the job rather than trusting the run's active status.
			want := 5
			launcher.Resume()
			first := <-be.launched
			second := <-be.launched
			if len(scaler.states[0].sem) != 2 {
				t.Fatal("capacity was not filled")
			}
			select {
			case <-be.launched:
				t.Fatal("launch exceeded pool capacity")
			default:
			}
			first.complete()
			second.complete()
			for i := 2; i < want; i++ {
				select {
				case handle := <-be.launched:
					handle.complete()
				case <-time.After(2 * time.Second):
					t.Fatal("pending work was lost on capacity release")
				}
			}
			seen := make(map[int64]bool)
			for range want {
				select {
				case id := <-jobs:
					if seen[id] || id == 4 || id == 5 {
						t.Fatalf("launched duplicate or stale job %d", id)
					}
					seen[id] = true
				case <-time.After(time.Second):
					t.Fatal("missing launched event")
				}
			}
			waitDemand(t, func() bool { return pendingDemand(scaler) == 0 && len(scaler.states[0].sem) == 0 })
			scaler.OnQueued(demandEvent(1))
			// A direct dispatch makes the no-redelivery assertion deterministic.
			scaler.dispatchDemand(ctx)
			if be.count.Load() != int32(want) {
				t.Fatal("completed demand was launched again")
			}
			cancel()
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(time.Second):
				t.Fatal("autoscaler did not stop")
			}
		})
	}
}

func TestPauseDuringRevalidationKeepsDemandWithoutHoldingSlots(t *testing.T) {
	scaler, launcher, be, _ := demandTestScaler(t, config.ScopeOrg, 1)
	scaler.setContext(t.Context())
	resolve := scaler.resolveMetadata
	scaler.resolveMetadata = func(ctx context.Context, client *github.Client, job github.QueuedJob) (github.QueuedJob, error) {
		launcher.Pause()
		return resolve(ctx, client, job)
	}
	scaler.OnQueued(demandEvent(1))
	scaler.dispatchDemand(t.Context())
	if len(scaler.states[0].sem) != 0 || pendingDemand(scaler) != 1 || be.count.Load() != 0 {
		t.Fatal("pause race consumed a slot or lost demand")
	}
	scaler.resolveMetadata = resolve
	launcher.Resume()
	scaler.dispatchDemand(t.Context())
	handle := <-be.launched
	handle.complete()
	scaler.runnerWG.Wait()
}

func TestPendingDemandShutdownAndOverflowAreBounded(t *testing.T) {
	scaler, launcher, be, _ := demandTestScaler(t, config.ScopeOrg, 1)
	var output bytes.Buffer
	scaler.logger = slog.New(slog.NewTextHandler(&output, nil))
	launcher.Pause()
	for id := int64(1); id <= maxPendingJobs+10; id++ {
		scaler.OnQueued(demandEvent(id))
	}
	if pendingDemand(scaler) != maxPendingJobs || len(scaler.states[0].sem) != 0 {
		t.Fatalf("pending = %d, slots = %d", pendingDemand(scaler), len(scaler.states[0].sem))
	}
	if !strings.Contains(output.String(), "queue is full") || !strings.Contains(output.String(), "queue_capacity=4096") {
		t.Fatalf("overflow was not logged: %s", output.String())
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := scaler.Run(ctx); err != nil {
		t.Fatal(err)
	}

	if pendingDemand(scaler) != 0 || be.count.Load() != 0 {
		t.Fatal("shutdown retained or launched pending demand")
	}
}

func TestCompletionDuringDemandRevalidationPreventsLaunch(t *testing.T) {
	scaler, _, be, _ := demandTestScaler(t, config.ScopeOrg, 1)
	resolve := scaler.resolveMetadata
	scaler.resolveMetadata = func(ctx context.Context, client *github.Client, job github.QueuedJob) (github.QueuedJob, error) {
		event := demandEvent(job.JobID)
		event.Action = "completed"
		scaler.OnWorkflowJob(ctx, event)
		return resolve(ctx, client, job)
	}
	scaler.OnQueued(demandEvent(1))
	scaler.dispatchDemand(t.Context())
	if pendingDemand(scaler) != 0 || be.count.Load() != 0 || len(scaler.states[0].sem) != 0 {
		t.Fatal("completed work was launched after revalidation")
	}
}

func TestRevalidationRechecksAuthorizationAndLabels(t *testing.T) {
	for _, change := range []string{"actor", "labels"} {
		t.Run(change, func(t *testing.T) {
			scaler, _, be, _ := demandTestScaler(t, config.ScopeOrg, 1)
			client := scaler.gh.ClientFor("o/r")
			launcher := pool.NewLauncher(config.Pool{
				Name: "privileged", Size: 1, Labels: []string{"self-hosted", "linux"},
				Repository: "o/r", Workflows: []string{".github/workflows/ci.yml"},
				WorkflowEvent: "push", WorkflowActor: "trusted", WorkflowRef: "main",
			}, "image", be, scaler.gh, nil, nil, slog.New(slog.DiscardHandler), pool.Hooks{})
			scaler.states[0].l = launcher
			job := github.QueuedJob{Client: client, Repository: "o/r", RunID: 100, JobID: 1,
				Labels: []string{"self-hosted", "linux"}, WorkflowPath: ".github/workflows/ci.yml",
				Actor: "trusted", Event: "push", Ref: "main", JobStatus: "queued"}
			scaler.launchFor(job)
			scaler.resolveMetadata = func(context.Context, *github.Client, github.QueuedJob) (github.QueuedJob, error) {
				if change == "actor" {
					job.Actor = "untrusted"
				} else {
					job.Labels = []string{"privileged-label-not-present"}
				}
				return job, nil
			}
			scaler.dispatchDemand(t.Context())
			if be.count.Load() != 0 || pendingDemand(scaler) != 0 {
				t.Fatal("revalidation authorized changed job metadata")
			}
		})
	}
}

func TestDemandRevalidationRetainsTransientErrorsAndDropsInactiveJobs(t *testing.T) {
	scaler, _, be, _ := demandTestScaler(t, config.ScopeOrg, 1)
	resolve := scaler.resolveMetadata
	scaler.resolveMetadata = func(context.Context, *github.Client, github.QueuedJob) (github.QueuedJob, error) {
		return github.QueuedJob{}, errors.New("temporary API outage")
	}
	scaler.OnQueued(demandEvent(1))
	scaler.dispatchDemand(t.Context())
	if pendingDemand(scaler) != 1 || len(scaler.states[0].sem) != 0 {
		t.Fatal("transient API outage lost demand or occupied a slot")
	}
	scaler.resolveMetadata = resolve
	scaler.dispatchDemand(t.Context())
	if be.count.Load() != 0 {
		t.Fatal("transient failure retry was not paced")
	}
	scaler.demandMu.Lock()
	clear(scaler.retryAfter)
	scaler.demandMu.Unlock()
	scaler.dispatchDemand(t.Context())
	handle := <-be.launched
	handle.complete()
	scaler.runnerWG.Wait()

	for _, field := range []string{"job", "run"} {
		scaler.OnQueued(demandEvent(2))
		scaler.resolveMetadata = func(_ context.Context, _ *github.Client, job github.QueuedJob) (github.QueuedJob, error) {
			if field == "job" {
				job.JobStatus = "completed"
			} else {
				job.Status = "completed"
			}
			return job, nil
		}
		scaler.dispatchDemand(t.Context())
		if pendingDemand(scaler) != 0 || be.count.Load() != 1 {
			t.Fatal("inactive demand was retained or launched")
		}
		scaler.demandMu.Lock()
		clear(scaler.recent)
		scaler.recentOrder = nil
		scaler.demandMu.Unlock()
	}
}

func TestFailedProvisioningRetainsDemandForPacedRetry(t *testing.T) {
	scaler, _, be, _ := demandTestScaler(t, config.ScopeOrg, 1)
	be.launchErr = errors.New("backend temporarily unavailable")
	scaler.OnQueued(demandEvent(1))
	scaler.dispatchDemand(t.Context())
	scaler.runnerWG.Wait()
	if pendingDemand(scaler) != 1 || len(scaler.states[0].sem) != 0 {
		t.Fatal("failed provisioning lost queued demand or retained capacity")
	}
	be.launchErr = nil
	scaler.demandMu.Lock()
	clear(scaler.retryAfter)
	scaler.demandMu.Unlock()
	scaler.dispatchDemand(t.Context())
	handle := <-be.launched
	handle.complete()
	scaler.runnerWG.Wait()
	if be.count.Load() != 1 || pendingDemand(scaler) != 0 {
		t.Fatal("failed demand was not retried exactly once")
	}
}

func TestActiveDemandShutdownWaitsForCleanup(t *testing.T) {
	scaler, _, be, _ := demandTestScaler(t, config.ScopeOrg, 1)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- scaler.Run(ctx) }()
	waitDemand(t, func() bool { return scaler.context() == ctx })
	scaler.OnQueued(demandEvent(1))
	var handle *demandHandle
	select {
	case handle = <-be.launched:
	case <-time.After(time.Second):
		t.Fatal("runner did not launch")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("shutdown did not wait for runner cleanup")
	}
	select {
	case <-handle.done:
	default:
		t.Fatal("shutdown returned before runner cleanup")
	}
	if len(scaler.states[0].sem) != 0 {
		t.Fatal("shutdown left an occupied slot")
	}
}
