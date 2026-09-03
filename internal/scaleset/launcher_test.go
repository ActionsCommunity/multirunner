package scaleset

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/actions/scaleset"

	"github.com/GerardSmit/multirunner/internal/backend"
)

// fakeJIT hands out predictable JIT blobs and records the names it was asked for.
type fakeJIT struct {
	mu               sync.Mutex
	removeMu         sync.Mutex
	settings         []scaleset.RunnerScaleSetJitRunnerSetting
	removeAttempts   []int64
	removed          []int64
	blockFirstRemove bool
	err              error
}

func (f *fakeJIT) GenerateJitRunnerConfig(
	_ context.Context,
	setting *scaleset.RunnerScaleSetJitRunnerSetting,
	_ int,
) (*scaleset.RunnerScaleSetJitRunnerConfig, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	f.settings = append(f.settings, *setting)
	return &scaleset.RunnerScaleSetJitRunnerConfig{
		EncodedJITConfig: "jit-for-" + setting.Name,
		Runner:           &scaleset.RunnerReference{ID: len(f.settings)},
	}, nil
}

func (f *fakeJIT) RemoveRunner(ctx context.Context, runnerID int64) error {
	f.removeMu.Lock()
	defer f.removeMu.Unlock()

	f.mu.Lock()
	first := len(f.removeAttempts) == 0
	f.removeAttempts = append(f.removeAttempts, runnerID)
	f.mu.Unlock()

	if first && f.blockFirstRemove {
		<-ctx.Done()
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	f.removed = append(f.removed, runnerID)
	return nil
}

// fakeHandle is a runner that exits when its channel is closed.
type fakeHandle struct {
	name      string
	done      chan struct{}
	closeOnce sync.Once
	killCount int
	blockKill bool
}

func (h *fakeHandle) Wait(ctx context.Context) (int, error) {
	select {
	case <-h.done:
		return 0, nil
	case <-ctx.Done():
		return 0, ctx.Err()
	}
}
func (h *fakeHandle) Logs(context.Context) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader("")), nil
}
func (h *fakeHandle) Kill(ctx context.Context) error {
	h.killCount++
	if h.blockKill {
		<-ctx.Done()
		h.complete()
		return ctx.Err()
	}
	h.complete()
	return nil
}
func (h *fakeHandle) ID() string { return h.name }
func (h *fakeHandle) complete()  { h.closeOnce.Do(func() { close(h.done) }) }

// fakeBackend records the LaunchRequests it receives.
type fakeBackend struct {
	mu       sync.Mutex
	launched []backend.LaunchRequest
	handles  []*fakeHandle
	err      error
}

func (b *fakeBackend) Name() string                              { return "fake" }
func (b *fakeBackend) Ping(context.Context) error                { return nil }
func (b *fakeBackend) OSType(context.Context) (string, error)    { return "linux", nil }
func (b *fakeBackend) EnsureImage(context.Context, string) error { return nil }
func (b *fakeBackend) Close() error                              { return nil }

func (b *fakeBackend) Launch(_ context.Context, req backend.LaunchRequest) (backend.RunnerHandle, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.err != nil {
		return nil, b.err
	}
	h := &fakeHandle{name: req.Name, done: make(chan struct{})}
	b.launched = append(b.launched, req)
	b.handles = append(b.handles, h)
	return h, nil
}

func (b *fakeBackend) requests() []backend.LaunchRequest {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]backend.LaunchRequest, len(b.launched))
	copy(out, b.launched)
	return out
}

func (b *fakeBackend) finish(i int) {
	b.mu.Lock()
	h := b.handles[i]
	b.mu.Unlock()
	h.complete()
}

// waitFor polls until cond holds or the deadline passes. Runner exit is
// observed on a goroutine, so the launcher's view updates asynchronously.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not met before deadline")
}

// This is the claim the whole design rests on: the blob the scale set client
// returns is carried straight through on LaunchRequest, so no backend changes.
func TestDesiredCountLaunchesRunnersCarryingTheJITConfig(t *testing.T) {
	jit := &fakeJIT{}
	be := &fakeBackend{}
	l := New(jit, be, Options{ScaleSetID: 7, Image: "runner:latest", WorkFolder: "_work"})

	got, err := l.HandleDesiredRunnerCount(context.Background(), 3)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != 3 {
		t.Fatalf("reported %d runners, want 3", got)
	}

	reqs := be.requests()
	if len(reqs) != 3 {
		t.Fatalf("launched %d runners, want 3", len(reqs))
	}
	for _, r := range reqs {
		if want := "jit-for-" + r.Name; r.EncodedJITConfig != want {
			t.Errorf("runner %s carried JIT %q, want %q", r.Name, r.EncodedJITConfig, want)
		}
		if len(jit.settings) != 3 {
			t.Fatalf("generated %d JIT settings, want 3", len(jit.settings))
		}
		for _, setting := range jit.settings {
			if setting.WorkFolder != "_work" {
				t.Errorf("runner %s got JIT work folder %q, want _work", setting.Name, setting.WorkFolder)
			}
		}
		if r.Image != "runner:latest" {
			t.Errorf("runner %s got image %q, want runner:latest", r.Name, r.Image)
		}
		if r.WorkFolder != "_work" {
			t.Errorf("runner %s got work folder %q, want _work", r.Name, r.WorkFolder)
		}
	}
}

func TestDesiredCountIsIdempotentWhileRunnersAreUp(t *testing.T) {
	l := New(&fakeJIT{}, &fakeBackend{}, Options{ScaleSetID: 1})

	if _, err := l.HandleDesiredRunnerCount(context.Background(), 2); err != nil {
		t.Fatalf("first call: %v", err)
	}
	// GitHub repeats the desired count; it must not stack up more runners.
	got, err := l.HandleDesiredRunnerCount(context.Background(), 2)
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if got != 2 {
		t.Fatalf("reported %d runners, want 2", got)
	}
	if n := l.Running(); n != 2 {
		t.Fatalf("running %d, want 2", n)
	}
}

// TestExitedRunnerIsReplacedImmediately pins that a freed slot is refilled as
// soon as the runner exits, without waiting for the listener's next message.
// Waiting for it halves throughput for the rest of a queue: with a cap of 2, two
// runners finishing moments apart left one slot idle for a whole poll interval,
// so six queued jobs ran one at a time instead of two.
func TestExitedRunnerIsReplacedImmediately(t *testing.T) {
	be := &fakeBackend{}
	l := New(&fakeJIT{}, be, Options{ScaleSetID: 1})

	if _, err := l.HandleDesiredRunnerCount(context.Background(), 2); err != nil {
		t.Fatalf("launch: %v", err)
	}
	be.finish(0)

	// No second HandleDesiredRunnerCount: the replacement must come from the exit.
	waitFor(t, func() bool { return len(be.requests()) == 3 })
	waitFor(t, func() bool { return l.Running() == 2 })
}

// TestExitedRunnerIsNotReplacedPastDemand pins the bound on that refill: GitHub
// asking for one runner must not be turned into an endless supply of them as
// each finishes.
func TestExitedRunnerIsNotReplacedPastDemand(t *testing.T) {
	be := &fakeBackend{}
	l := New(&fakeJIT{}, be, Options{ScaleSetID: 1})

	if _, err := l.HandleDesiredRunnerCount(context.Background(), 1); err != nil {
		t.Fatalf("launch: %v", err)
	}
	// The queue drains: GitHub now wants nothing.
	if _, err := l.HandleDesiredRunnerCount(context.Background(), 0); err != nil {
		t.Fatalf("drain: %v", err)
	}
	be.finish(0)

	waitFor(t, func() bool { return l.Running() == 0 })
	if n := len(be.requests()); n != 1 {
		t.Errorf("launched %d runners, want 1 (no refill once demand is gone)", n)
	}
}

// TestShutdownStopsRefilling pins that a runner exiting during shutdown does not
// start a replacement that would outlive the shutdown.
func TestShutdownStopsRefilling(t *testing.T) {
	be := &fakeBackend{}
	l := New(&fakeJIT{}, be, Options{ScaleSetID: 1})

	if _, err := l.HandleDesiredRunnerCount(context.Background(), 2); err != nil {
		t.Fatalf("launch: %v", err)
	}
	if err := l.Shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	waitFor(t, func() bool { return l.Running() == 0 })

	if n := len(be.requests()); n != 2 {
		t.Errorf("launched %d runners, want the original 2 and no refill", n)
	}
	if _, err := l.HandleDesiredRunnerCount(context.Background(), 4); err != nil {
		t.Fatalf("post-shutdown call: %v", err)
	}
	if n := len(be.requests()); n != 2 {
		t.Errorf("launched %d runners after shutdown, want no new ones", n)
	}
}

func TestMaxRunnersCapsWhatTheHostAdvertises(t *testing.T) {
	be := &fakeBackend{}
	l := New(&fakeJIT{}, be, Options{ScaleSetID: 1, MaxRunners: 2})

	got, err := l.HandleDesiredRunnerCount(context.Background(), 5)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != 2 {
		t.Fatalf("reported %d runners, want 2 (the cap)", got)
	}
	if n := len(be.requests()); n != 2 {
		t.Fatalf("launched %d runners, want 2", n)
	}
}

func TestPartialLaunchReportsWhatStarted(t *testing.T) {
	jit := &fakeJIT{}
	be := &fakeBackend{err: errors.New("daemon unreachable")}
	l := New(jit, be, Options{ScaleSetID: 1})

	got, err := l.HandleDesiredRunnerCount(context.Background(), 3)
	if err == nil {
		t.Fatal("expected an error when the backend refuses to launch")
	}
	if got != 0 {
		t.Fatalf("reported %d runners, want 0", got)
	}
	if len(jit.removed) != 1 {
		t.Fatalf("removed %d unused registrations, want 1", len(jit.removed))
	}
}

func TestRunnerNamesAreUniqueAcrossLaunchers(t *testing.T) {
	be1 := &fakeBackend{}
	be2 := &fakeBackend{}
	l1 := New(&fakeJIT{}, be1, Options{ScaleSetID: 1})
	l2 := New(&fakeJIT{}, be2, Options{ScaleSetID: 1})

	if _, err := l1.HandleDesiredRunnerCount(context.Background(), 1); err != nil {
		t.Fatalf("first launcher: %v", err)
	}
	if _, err := l2.HandleDesiredRunnerCount(context.Background(), 1); err != nil {
		t.Fatalf("second launcher: %v", err)
	}
	if got1, got2 := be1.requests()[0].Name, be2.requests()[0].Name; got1 == got2 {
		t.Fatalf("runner names collided: %q", got1)
	}
}

func TestShutdownKillsAndDeregistersRunners(t *testing.T) {
	jit := &fakeJIT{}
	be := &fakeBackend{}
	l := New(jit, be, Options{ScaleSetID: 1})

	if _, err := l.HandleDesiredRunnerCount(context.Background(), 2); err != nil {
		t.Fatalf("launch: %v", err)
	}
	if err := l.Shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	waitFor(t, func() bool { return l.Running() == 0 })
	if len(jit.removed) != 2 {
		t.Fatalf("removed %d registrations, want 2", len(jit.removed))
	}
	for _, handle := range be.handles {
		if handle.killCount != 1 {
			t.Errorf("kill count = %d, want 1", handle.killCount)
		}
	}
}

func TestShutdownGivesEveryRunnerIndependentCleanupContexts(t *testing.T) {
	jit := &fakeJIT{}
	be := &fakeBackend{}
	l := New(jit, be, Options{ScaleSetID: 1, cleanupTimeout: 20 * time.Millisecond})

	if _, err := l.HandleDesiredRunnerCount(context.Background(), 3); err != nil {
		t.Fatalf("launch: %v", err)
	}
	be.handles[0].blockKill = true

	err := l.Shutdown(context.Background())
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("shutdown error = %v, want deadline exceeded from blocked kill", err)
	}
	waitFor(t, func() bool { return l.Running() == 0 })

	if len(jit.removed) != 3 {
		t.Fatalf("removed %d registrations, want 3", len(jit.removed))
	}
	for i, handle := range be.handles {
		if handle.killCount != 1 {
			t.Errorf("handle %d kill count = %d, want 1", i, handle.killCount)
		}
	}
}

func TestShutdownStartsRemovalTimeoutAfterClientLockIsAvailable(t *testing.T) {
	jit := &fakeJIT{blockFirstRemove: true}
	be := &fakeBackend{}
	l := New(jit, be, Options{ScaleSetID: 1, cleanupTimeout: 20 * time.Millisecond})

	if _, err := l.HandleDesiredRunnerCount(context.Background(), 3); err != nil {
		t.Fatalf("launch: %v", err)
	}

	err := l.Shutdown(context.Background())
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("shutdown error = %v, want deadline exceeded from first removal", err)
	}
	waitFor(t, func() bool { return l.Running() == 0 })

	if len(jit.removeAttempts) != 3 {
		t.Fatalf("attempted %d removals, want 3", len(jit.removeAttempts))
	}
	if len(jit.removed) != 2 {
		t.Fatalf("completed %d removals, want 2 after the first timed out", len(jit.removed))
	}
}

func TestLifecycleCallbacksTrackRunner(t *testing.T) {
	be := &fakeBackend{}
	var starts, stops atomic.Int32
	l := New(&fakeJIT{}, be, Options{
		ScaleSetID: 1,
		OnStart:    func() { starts.Add(1) },
		OnStop:     func(int, error) { stops.Add(1) },
	})

	if _, err := l.HandleDesiredRunnerCount(context.Background(), 1); err != nil {
		t.Fatalf("launch: %v", err)
	}
	if got := starts.Load(); got != 1 {
		t.Fatalf("start callbacks = %d, want 1", got)
	}
	be.finish(0)
	waitFor(t, func() bool { return stops.Load() == 1 })
}

func TestJobCallbacksDoNotProvision(t *testing.T) {
	be := &fakeBackend{}
	l := New(&fakeJIT{}, be, Options{ScaleSetID: 1})

	if err := l.HandleJobStarted(context.Background(), &scaleset.JobStarted{}); err != nil {
		t.Fatalf("HandleJobStarted: %v", err)
	}
	if err := l.HandleJobCompleted(context.Background(), &scaleset.JobCompleted{}); err != nil {
		t.Fatalf("HandleJobCompleted: %v", err)
	}
	if n := len(be.requests()); n != 0 {
		t.Fatalf("job callbacks launched %d runners, want 0", n)
	}
}

// TestAwaitExitReportsAShortRun pins that a runner which dies before taking a
// job is reported. Silence here is the failure that reads as "the job just sits
// queued": the listener re-launches on the next poll and nothing says why.
func TestAwaitExitReportsAShortRun(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	l := New(&fakeJIT{}, &fakeBackend{}, Options{ScaleSetID: 1, Image: "img", Logger: logger})
	l.awaitExit("mr-scaleset-1-dead", &instantHandle{code: 1})

	out := buf.String()
	if !strings.Contains(out, "runner exited without completing a job") {
		t.Errorf("a non-zero exit was not reported:\n%s", out)
	}
	if !strings.Contains(out, "mr-scaleset-1-dead") {
		t.Errorf("the report does not name the runner:\n%s", out)
	}
	if l.Running() != 0 {
		t.Errorf("running = %d, want the slot freed", l.Running())
	}
}

// TestAwaitExitReportsACleanExitQuietly pins that a runner which exited 0 is not
// reported as a failure however briefly it lived. An ephemeral runner can finish
// a small job in seconds, so a duration heuristic here produces false alarms on
// every fast job - which is exactly what it did the first time.
func TestAwaitExitReportsACleanExitQuietly(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	l := New(&fakeJIT{}, &fakeBackend{}, Options{ScaleSetID: 1, Image: "img", Logger: logger})
	l.awaitExit("mr-scaleset-1-ok", &slowHandle{})

	if out := buf.String(); strings.Contains(out, "level=ERROR") || strings.Contains(out, "level=WARN") {
		t.Errorf("a clean exit was reported as a problem:\n%s", out)
	}
}

// instantHandle is a runner that exits immediately with a code.
type instantHandle struct{ code int }

func (h *instantHandle) Wait(context.Context) (int, error) { return h.code, nil }
func (h *instantHandle) Kill(context.Context) error        { return nil }
func (h *instantHandle) ID() string                        { return "instant" }
func (h *instantHandle) Logs(context.Context) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader("")), nil
}

// slowHandle is a runner that lived long enough to have taken a job.
type slowHandle struct{}

func (h *slowHandle) Wait(context.Context) (int, error) {
	time.Sleep(10 * time.Millisecond)
	return 0, nil
}
func (h *slowHandle) Kill(context.Context) error { return nil }
func (h *slowHandle) ID() string                 { return "slow" }
func (h *slowHandle) Logs(context.Context) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader("")), nil
}
