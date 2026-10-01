package scaleset

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"reflect"
	"strings"
	"sync"
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
	removeErr        error
	removeHook       func(int64)
	omitRunner       bool
	blockGenerate    bool
	generateStarted  chan struct{}
	generateOnce     sync.Once
}

func (f *fakeJIT) GenerateJitRunnerConfig(
	ctx context.Context,
	setting *scaleset.RunnerScaleSetJitRunnerSetting,
	_ int,
) (*scaleset.RunnerScaleSetJitRunnerConfig, error) {
	if f.blockGenerate {
		f.generateOnce.Do(func() { close(f.generateStarted) })
		<-ctx.Done()
		return nil, ctx.Err()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	f.settings = append(f.settings, *setting)
	config := &scaleset.RunnerScaleSetJitRunnerConfig{
		EncodedJITConfig: "jit-for-" + setting.Name,
		Runner:           &scaleset.RunnerReference{ID: len(f.settings)},
	}
	if f.omitRunner {
		config.Runner = nil
	}
	return config, nil
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
	if f.removeErr != nil {
		return f.removeErr
	}
	if f.removeHook != nil {
		f.removeHook(runnerID)
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
	mu        sync.Mutex
	killCount int
	blockKill bool
	exitCode  int
}

func (h *fakeHandle) Wait(ctx context.Context) (int, error) {
	select {
	case <-h.done:
		return h.exitCode, nil
	case <-ctx.Done():
		return 0, ctx.Err()
	}
}
func (h *fakeHandle) Logs(context.Context) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader("")), nil
}
func (h *fakeHandle) Kill(ctx context.Context) error {
	h.mu.Lock()
	h.killCount++
	block := h.blockKill
	h.mu.Unlock()
	if block {
		<-ctx.Done()
		return ctx.Err()
	}
	h.complete()
	return nil
}
func (h *fakeHandle) ID() string { return h.name }
func (h *fakeHandle) complete()  { h.closeOnce.Do(func() { close(h.done) }) }
func (h *fakeHandle) kills() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.killCount
}
func (h *fakeHandle) setBlockKill(block bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.blockKill = block
}

// fakeBackend records the LaunchRequests it receives.
type fakeBackend struct {
	mu                  sync.Mutex
	launched            []backend.LaunchRequest
	handles             []*fakeHandle
	err                 error
	handleOnError       bool
	owned               []backend.OwnedRunner
	listOwnership       backend.RunnerOwnership
	listErr             error
	removeOwnedAttempts []string
	removeOwnedErr      error
	blockOwnedRemoval   bool
	removeOwnedHook     func(string)
	blockLaunch         bool
	launchStarted       chan struct{}
	launchOnce          sync.Once
	blockList           bool
	listStarted         chan struct{}
	listOnce            sync.Once
	listCalls           int
}

type unsupportedBackend struct{}

func (unsupportedBackend) Name() string                              { return "unsupported" }
func (unsupportedBackend) Ping(context.Context) error                { return nil }
func (unsupportedBackend) OSType(context.Context) (string, error)    { return "linux", nil }
func (unsupportedBackend) EnsureImage(context.Context, string) error { return nil }
func (unsupportedBackend) Launch(context.Context, backend.LaunchRequest) (backend.RunnerHandle, error) {
	return nil, errors.New("not implemented")
}
func (unsupportedBackend) Close() error { return nil }

func (b *fakeBackend) Name() string                              { return "fake" }
func (b *fakeBackend) Ping(context.Context) error                { return nil }
func (b *fakeBackend) OSType(context.Context) (string, error)    { return "linux", nil }
func (b *fakeBackend) EnsureImage(context.Context, string) error { return nil }
func (b *fakeBackend) Close() error                              { return nil }

func (b *fakeBackend) ListOwnedRunners(ctx context.Context, ownership backend.RunnerOwnership) ([]backend.OwnedRunner, error) {
	b.mu.Lock()
	b.listOwnership = ownership
	b.listCalls++
	block := b.blockList
	owned := append([]backend.OwnedRunner(nil), b.owned...)
	err := b.listErr
	b.mu.Unlock()
	if block {
		b.listOnce.Do(func() { close(b.listStarted) })
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return owned, err
}

func (b *fakeBackend) RemoveOwnedRunner(ctx context.Context, resourceID string) error {
	b.mu.Lock()
	b.removeOwnedAttempts = append(b.removeOwnedAttempts, resourceID)
	block := b.blockOwnedRemoval
	err := b.removeOwnedErr
	b.mu.Unlock()
	if block {
		<-ctx.Done()
		return ctx.Err()
	}
	if b.removeOwnedHook != nil {
		b.removeOwnedHook(resourceID)
	}
	return err
}

func (b *fakeBackend) Launch(ctx context.Context, req backend.LaunchRequest) (backend.RunnerHandle, error) {
	if b.blockLaunch {
		b.launchOnce.Do(func() { close(b.launchStarted) })
		<-ctx.Done()
		return nil, ctx.Err()
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	h := &fakeHandle{name: req.Name, done: make(chan struct{})}
	if b.err != nil {
		if b.handleOnError {
			b.launched = append(b.launched, req)
			b.handles = append(b.handles, h)
			return h, b.err
		}
		return nil, b.err
	}
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

// fail ends a runner with a non-zero exit code, the shape of a runner that never
// took a job: a rejected image, a runner build GitHub refuses, a bad JIT config.
func (b *fakeBackend) fail(i, code int) {
	b.mu.Lock()
	h := b.handles[i]
	h.exitCode = code
	b.mu.Unlock()
	h.complete()
}

func (b *fakeBackend) ownedRemovalAttempts() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.removeOwnedAttempts...)
}

func (b *fakeBackend) setOwnedRemovalError(err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.removeOwnedErr = err
}

func (b *fakeBackend) ownedListCalls() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.listCalls
}

func (f *fakeJIT) removedIDs() []int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]int64(nil), f.removed...)
}

func (f *fakeJIT) removalAttempts() []int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]int64(nil), f.removeAttempts...)
}

func (f *fakeJIT) setRemovalError(err error) {
	f.removeMu.Lock()
	defer f.removeMu.Unlock()
	f.removeErr = err
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
	settings := backend.ContainerSettings{
		CPUCount:        4,
		MemoryBytes:     4_294_967_296,
		MemorySwapBytes: 8_589_934_592,
		DNS:             []string{"1.1.1.1"},
	}
	l := New(t.Context(), jit, be, Options{
		ScaleSetID: 7,
		Image:      "runner:latest",
		WorkFolder: "_work",
		Container:  settings,
		Ownership: backend.RunnerOwnership{
			Instance: "host-a",
			Target:   "https://github.com/o/r",
			Pool:     "linux",
		},
	})

	got, err := l.HandleDesiredRunnerCount(t.Context(), 3)
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
		if !reflect.DeepEqual(r.Container, settings) {
			t.Errorf("runner %s container settings = %+v, want %+v", r.Name, r.Container, settings)
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
		if r.Ownership.Instance != "host-a" || r.Ownership.ScaleSetID != 7 || r.Ownership.RunnerID == 0 {
			t.Errorf("runner %s ownership = %+v", r.Name, r.Ownership)
		}
	}
}

func TestDesiredCountPreservesOmittedContainerSettings(t *testing.T) {
	be := &fakeBackend{}
	l := New(t.Context(), &fakeJIT{}, be, Options{ScaleSetID: 7})

	if _, err := l.HandleDesiredRunnerCount(context.Background(), 1); err != nil {
		t.Fatalf("HandleDesiredRunnerCount: %v", err)
	}
	reqs := be.requests()
	if len(reqs) != 1 {
		t.Fatalf("launched %d runners, want 1", len(reqs))
	}
	if !reflect.DeepEqual(reqs[0].Container, backend.ContainerSettings{}) {
		t.Errorf("omitted container settings = %+v, want zero value", reqs[0].Container)
	}
}

func TestDesiredCountIsIdempotentWhileRunnersAreUp(t *testing.T) {
	l := New(t.Context(), &fakeJIT{}, &fakeBackend{}, Options{ScaleSetID: 1})

	if _, err := l.HandleDesiredRunnerCount(t.Context(), 2); err != nil {
		t.Fatalf("first call: %v", err)
	}
	// GitHub repeats the desired count; it must not stack up more runners.
	got, err := l.HandleDesiredRunnerCount(t.Context(), 2)
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
	jit := &fakeJIT{}
	l := New(t.Context(), jit, be, Options{ScaleSetID: 1, MaxRunners: 2})

	if _, err := l.HandleDesiredRunnerCount(t.Context(), 6); err != nil {
		t.Fatalf("launch: %v", err)
	}
	be.finish(0)
	be.finish(1)

	// No second HandleDesiredRunnerCount: the replacement must come from the exit.
	waitFor(t, func() bool { return len(be.requests()) == 4 })
	waitFor(t, func() bool { return l.Running() == 2 })
	waitFor(t, func() bool { return len(jit.removedIDs()) == 2 })
}

// TestExitedRunnerIsNotReplacedPastDemand pins the bound on that refill: GitHub
// asking for one runner must not be turned into an endless supply of them as
// each finishes.
func TestExitedRunnerIsNotReplacedPastDemand(t *testing.T) {
	be := &fakeBackend{}
	l := New(t.Context(), &fakeJIT{}, be, Options{ScaleSetID: 1})

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
	l := New(t.Context(), &fakeJIT{}, be, Options{ScaleSetID: 1})

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

func TestBusyRunnerExitDoesNotDeregister(t *testing.T) {
	jit := &fakeJIT{}
	be := &fakeBackend{}
	l := New(t.Context(), jit, be, Options{ScaleSetID: 1})

	if _, err := l.HandleDesiredRunnerCount(t.Context(), 1); err != nil {
		t.Fatalf("launch: %v", err)
	}
	req := be.requests()[0]
	if err := l.HandleJobStarted(t.Context(), &scaleset.JobStarted{
		RunnerID:   1,
		RunnerName: req.Name,
	}); err != nil {
		t.Fatalf("HandleJobStarted: %v", err)
	}
	if err := l.HandleJobStarted(t.Context(), &scaleset.JobStarted{
		RunnerID:   1,
		RunnerName: req.Name,
	}); err != nil {
		t.Fatalf("duplicate HandleJobStarted: %v", err)
	}
	if _, err := l.HandleDesiredRunnerCount(t.Context(), 0); err != nil {
		t.Fatal(err)
	}
	be.finish(0)
	// Capacity is freed after termination, before registration and backend
	// cleanup. Wait for the tracked record to disappear before asserting cleanup.
	waitFor(t, func() bool {
		l.mu.Lock()
		defer l.mu.Unlock()
		return len(l.running) == 0
	})
	if removed := jit.removedIDs(); len(removed) != 0 {
		t.Fatalf("removed busy runner registrations %v, want none", removed)
	}
}

func TestMaxRunnersCapsWhatTheHostAdvertises(t *testing.T) {
	be := &fakeBackend{}
	l := New(t.Context(), &fakeJIT{}, be, Options{ScaleSetID: 1, MaxRunners: 2})

	got, err := l.HandleDesiredRunnerCount(t.Context(), 5)
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
	l := New(t.Context(), jit, be, Options{ScaleSetID: 1})

	got, err := l.HandleDesiredRunnerCount(t.Context(), 3)
	if err != nil {
		t.Fatalf("transient launch failure stopped listener callback: %v", err)
	}
	if got != 0 {
		t.Fatalf("reported %d runners, want 0", got)
	}
	if removed := jit.removedIDs(); len(removed) != 1 {
		t.Fatalf("removed %d unused registrations, want 1", len(removed))
	}
}

func TestPartialLaunchHandleStaysCountedUntilCleanupConverges(t *testing.T) {
	jit := &fakeJIT{}
	be := &fakeBackend{
		err:           errors.New("launch response lost"),
		handleOnError: true,
	}
	l := New(t.Context(), jit, be, Options{
		ScaleSetID: 1,
		Ownership: backend.RunnerOwnership{
			Instance: "host", Target: "https://github.com/o/r", Pool: "linux", ScaleSetID: 1,
		},
	})

	got, err := l.HandleDesiredRunnerCount(t.Context(), 1)
	if err != nil {
		t.Fatalf("partial launch failure stopped listener callback: %v", err)
	}
	if got != 1 {
		t.Fatalf("reported %d runners, want uncertain launch counted until cleanup", got)
	}

	// Capacity is freed after termination, before registration and backend
	// cleanup. Wait for the tracked record to disappear before asserting cleanup.
	waitFor(t, func() bool {
		l.mu.Lock()
		defer l.mu.Unlock()
		return len(l.running) == 0
	})
	if removed := jit.removedIDs(); len(removed) != 1 {
		t.Fatalf("removed registrations = %v, want one", removed)
	}
	if attempts := be.ownedRemovalAttempts(); len(attempts) != 1 {
		t.Fatalf("backend cleanup attempts = %v, want one", attempts)
	}
}

func TestMissingRunnerIdentityDoesNotLaunchBackend(t *testing.T) {
	jit := &fakeJIT{omitRunner: true}
	be := &fakeBackend{}
	l := New(t.Context(), jit, be, Options{ScaleSetID: 1})

	got, err := l.HandleDesiredRunnerCount(t.Context(), 1)
	if err != nil {
		t.Fatalf("HandleDesiredRunnerCount: %v", err)
	}
	if got != 0 {
		t.Fatalf("running = %d, want zero", got)
	}
	if requests := be.requests(); len(requests) != 0 {
		t.Fatalf("backend launches = %d, want zero", len(requests))
	}
}

func TestFailedLaunchRegistrationCleanupRetriesWithoutStoppingListener(t *testing.T) {
	jit := &fakeJIT{removeErr: errors.New("service unavailable")}
	be := &fakeBackend{err: errors.New("daemon unreachable")}
	l := New(t.Context(), jit, be, Options{ScaleSetID: 1})

	got, err := l.HandleDesiredRunnerCount(t.Context(), 1)
	if err != nil || got != 0 {
		t.Fatalf("first desired count = (%d, %v), want zero without listener error", got, err)
	}
	if attempts := jit.removalAttempts(); len(attempts) != 1 {
		t.Fatalf("registration removal attempts = %v, want one", attempts)
	}

	jit.setRemovalError(nil)
	if _, err := l.HandleDesiredRunnerCount(t.Context(), 0); err != nil {
		t.Fatalf("retry desired count: %v", err)
	}
	if removed := jit.removedIDs(); len(removed) != 1 {
		t.Fatalf("removed registrations = %v, want one after retry", removed)
	}
}

func TestPermanentLaunchFailureStopsListenerCallback(t *testing.T) {
	jit := &fakeJIT{err: errors.New(`request failed(status="400 Bad Request")`)}
	l := New(t.Context(), jit, &fakeBackend{}, Options{ScaleSetID: 1})

	got, err := l.HandleDesiredRunnerCount(t.Context(), 1)
	if err == nil {
		t.Fatal("permanent configuration failure was suppressed")
	}
	if got != 0 {
		t.Fatalf("reported %d runners, want 0", got)
	}
}

func TestRunnerNamesAreUniqueAcrossLaunchers(t *testing.T) {
	be1 := &fakeBackend{}
	be2 := &fakeBackend{}
	l1 := New(t.Context(), &fakeJIT{}, be1, Options{ScaleSetID: 1})
	l2 := New(t.Context(), &fakeJIT{}, be2, Options{ScaleSetID: 1})

	if _, err := l1.HandleDesiredRunnerCount(t.Context(), 1); err != nil {
		t.Fatalf("first launcher: %v", err)
	}
	if _, err := l2.HandleDesiredRunnerCount(t.Context(), 1); err != nil {
		t.Fatalf("second launcher: %v", err)
	}
	if got1, got2 := be1.requests()[0].Name, be2.requests()[0].Name; got1 == got2 {
		t.Fatalf("runner names collided: %q", got1)
	}
}

func TestHungJITLaunchStopsWhenSessionIsCanceled(t *testing.T) {
	sessionCtx, cancel := context.WithCancel(t.Context())
	jit := &fakeJIT{blockGenerate: true, generateStarted: make(chan struct{})}
	l := New(sessionCtx, jit, &fakeBackend{}, Options{
		ScaleSetID:    1,
		launchTimeout: time.Hour,
	})

	done := make(chan error, 1)
	go func() {
		_, err := l.HandleDesiredRunnerCount(context.WithoutCancel(sessionCtx), 1)
		done <- err
	}()
	<-jit.generateStarted
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("desired count after cancellation: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("hung JIT generation ignored session cancellation")
	}
	if running := l.Running(); running != 0 {
		t.Fatalf("running after cancellation = %d, want zero", running)
	}
	if err := l.Shutdown(t.Context()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
}

func TestHungBackendLaunchStopsWhenSessionIsCanceled(t *testing.T) {
	sessionCtx, cancel := context.WithCancel(t.Context())
	jit := &fakeJIT{}
	be := &fakeBackend{blockLaunch: true, launchStarted: make(chan struct{})}
	l := New(sessionCtx, jit, be, Options{
		ScaleSetID:    1,
		launchTimeout: time.Hour,
	})

	done := make(chan error, 1)
	go func() {
		_, err := l.HandleDesiredRunnerCount(context.WithoutCancel(sessionCtx), 1)
		done <- err
	}()
	<-be.launchStarted
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("desired count after cancellation: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("hung backend launch ignored session cancellation")
	}
	if removed := jit.removedIDs(); len(removed) != 1 {
		t.Fatalf("removed registrations = %v, want timed-out launch registration cleaned", removed)
	}
	if err := l.Shutdown(t.Context()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
}

func TestHungLaunchOperationsHaveBoundedTimeouts(t *testing.T) {
	tests := []struct {
		name string
		jit  *fakeJIT
		be   *fakeBackend
	}{
		{
			name: "JIT generation",
			jit:  &fakeJIT{blockGenerate: true, generateStarted: make(chan struct{})},
			be:   &fakeBackend{},
		},
		{
			name: "backend launch",
			jit:  &fakeJIT{},
			be:   &fakeBackend{blockLaunch: true, launchStarted: make(chan struct{})},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			l := New(t.Context(), test.jit, test.be, Options{
				ScaleSetID:    1,
				launchTimeout: 20 * time.Millisecond,
			})
			started := time.Now()
			if _, err := l.HandleDesiredRunnerCount(context.WithoutCancel(t.Context()), 1); err != nil {
				t.Fatalf("HandleDesiredRunnerCount: %v", err)
			}
			if elapsed := time.Since(started); elapsed > time.Second {
				t.Fatalf("hung operation returned after %s, want bounded timeout", elapsed)
			}
		})
	}
}

// TestAwaitExitReportsAShortRun pins that a runner which dies before taking a
// job is reported. Silence here is the failure that reads as "the job just sits
// queued": the listener re-launches on the next poll and nothing says why.
func TestAwaitExitReportsAShortRun(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	l := New(t.Context(), &fakeJIT{}, &fakeBackend{}, Options{ScaleSetID: 1, Image: "img", Logger: logger})
	l.awaitExit("mr-scaleset-1-dead", &runnerState{handle: &instantHandle{code: 1}})

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

	l := New(t.Context(), &fakeJIT{}, &fakeBackend{}, Options{ScaleSetID: 1, Image: "img", Logger: logger})
	l.awaitExit("mr-scaleset-1-ok", &runnerState{handle: &slowHandle{}})

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

// TestFailedRunnerIsNotReplacedImmediately pins the bound on refilling that the
// throughput fix needs. A runner that completed a job earns an instant
// replacement; one that died without taking a job must not, or a rejected image
// becomes a launch loop paced only by container start time, each iteration
// registering a fresh runner with GitHub. Those wait for the listener's next
// message instead.
func TestFailedRunnerIsNotReplacedImmediately(t *testing.T) {
	be := &fakeBackend{}
	l := New(t.Context(), &fakeJIT{}, be, Options{ScaleSetID: 1, MaxRunners: 2})

	if _, err := l.HandleDesiredRunnerCount(context.Background(), 2); err != nil {
		t.Fatalf("launch: %v", err)
	}
	be.fail(0, 1)

	waitFor(t, func() bool { return l.Running() == 1 })
	// Give a refill the same window a successful exit's replacement gets.
	time.Sleep(100 * time.Millisecond)
	if n := len(be.requests()); n != 2 {
		t.Errorf("launched %d runners, want the original 2: a runner that died must not be replaced until the listener asks again", n)
	}

	// The listener asking again is still honoured, so the pool recovers once
	// GitHub confirms the work is still there.
	if _, err := l.HandleDesiredRunnerCount(context.Background(), 2); err != nil {
		t.Fatalf("relaunch: %v", err)
	}
	if n := len(be.requests()); n != 3 {
		t.Errorf("launched %d runners, want a replacement on the next message", n)
	}
}
