package scaleset

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/actions/scaleset"

	"github.com/GerardSmit/multirunner/internal/backend"
)

// fakeJIT hands out predictable JIT blobs and records the names it was asked for.
type fakeJIT struct {
	mu    sync.Mutex
	names []string
	err   error
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
	f.names = append(f.names, setting.Name)
	return &scaleset.RunnerScaleSetJitRunnerConfig{
		EncodedJITConfig: "jit-for-" + setting.Name,
	}, nil
}

// fakeHandle is a runner that exits when its channel is closed.
type fakeHandle struct {
	name string
	done chan struct{}
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
func (h *fakeHandle) Kill(context.Context) error { return nil }
func (h *fakeHandle) ID() string                 { return h.name }

// fakeBackend records the LaunchRequests it receives.
type fakeBackend struct {
	mu       sync.Mutex
	launched []backend.LaunchRequest
	handles  []*fakeHandle
	err      error
}

func (b *fakeBackend) Name() string                                { return "fake" }
func (b *fakeBackend) Ping(context.Context) error                  { return nil }
func (b *fakeBackend) OSType(context.Context) (string, error)      { return "linux", nil }
func (b *fakeBackend) EnsureImage(context.Context, string) error   { return nil }
func (b *fakeBackend) Close() error                                { return nil }

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
	close(h.done)
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

func TestExitedRunnerFreesItsSlot(t *testing.T) {
	be := &fakeBackend{}
	l := New(&fakeJIT{}, be, Options{ScaleSetID: 1})

	if _, err := l.HandleDesiredRunnerCount(context.Background(), 2); err != nil {
		t.Fatalf("launch: %v", err)
	}
	be.finish(0)
	waitFor(t, func() bool { return l.Running() == 1 })

	// The slot is free, so asking for 2 again starts exactly one replacement.
	if _, err := l.HandleDesiredRunnerCount(context.Background(), 2); err != nil {
		t.Fatalf("relaunch: %v", err)
	}
	if n := len(be.requests()); n != 3 {
		t.Fatalf("launched %d runners in total, want 3", n)
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
	be := &fakeBackend{err: errors.New("daemon unreachable")}
	l := New(&fakeJIT{}, be, Options{ScaleSetID: 1})

	got, err := l.HandleDesiredRunnerCount(context.Background(), 3)
	if err == nil {
		t.Fatal("expected an error when the backend refuses to launch")
	}
	if got != 0 {
		t.Fatalf("reported %d runners, want 0", got)
	}
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
