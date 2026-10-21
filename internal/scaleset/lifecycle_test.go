package scaleset

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/GerardSmit/multirunner/internal/backend"
	"github.com/GerardSmit/multirunner/internal/runner"
)

type lifecycleEvents struct {
	mu     sync.Mutex
	events []runner.LifecycleEvent
}

func (r *lifecycleEvents) ObserveRunnerLifecycle(ctx context.Context, event runner.LifecycleEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, event)
	if ctx.Err() != nil {
		panic("lifecycle callback received cancelled context")
	}
	if _, ok := ctx.Deadline(); !ok {
		panic("lifecycle callback received unbounded context")
	}
}

func (r *lifecycleEvents) snapshot() []runner.LifecycleEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]runner.LifecycleEvent(nil), r.events...)
}

func TestStructuredLifecycleSuccessFailurePartialAndShutdown(t *testing.T) {
	for _, tc := range []struct {
		name       string
		jitErr     error
		launchErr  error
		partial    bool
		shutdown   bool
		cleanupErr error
		want       []runner.LifecycleEventType
	}{
		{name: "success", want: []runner.LifecycleEventType{runner.LifecyclePlanned, runner.LifecycleRegistered, runner.LifecycleLaunched, runner.LifecycleStopped}},
		{name: "jit failure", jitErr: errors.New("jit failed"), want: []runner.LifecycleEventType{runner.LifecyclePlanned, runner.LifecycleFailed}},
		{name: "launch failure", launchErr: errors.New("launch failed"), want: []runner.LifecycleEventType{runner.LifecyclePlanned, runner.LifecycleRegistered, runner.LifecycleFailed}},
		{name: "partial launch", launchErr: errors.New("partial launch"), partial: true, want: []runner.LifecycleEventType{runner.LifecyclePlanned, runner.LifecycleRegistered, runner.LifecycleFailed}},
		{name: "partial cleanup failure", launchErr: errors.New("partial launch"), partial: true, cleanupErr: errors.New("backend removal failed"), want: []runner.LifecycleEventType{runner.LifecyclePlanned, runner.LifecycleRegistered, runner.LifecycleFailed}},
		{name: "shutdown", shutdown: true, want: []runner.LifecycleEventType{runner.LifecyclePlanned, runner.LifecycleRegistered, runner.LifecycleLaunched, runner.LifecycleStopped}},
		{name: "cleanup failure", cleanupErr: errors.New("backend removal failed"), want: []runner.LifecycleEventType{runner.LifecyclePlanned, runner.LifecycleRegistered, runner.LifecycleLaunched, runner.LifecycleFailed}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			jit := &fakeJIT{err: tc.jitErr}
			be := &fakeBackend{err: tc.launchErr, handleOnError: tc.partial, removeOwnedErr: tc.cleanupErr}
			events := &lifecycleEvents{}
			var legacy atomic.Int32
			launcher := New(ctx, jit, be, Options{
				ScaleSetID: 42, Image: "image", Observer: events,
				Ownership: backend.RunnerOwnership{Pool: "linux", Target: "https://github.com/o/r"},
				OnStart:   func() { legacy.Add(1) }, OnStop: func(int, error) { legacy.Add(1) },
			})
			if _, err := launcher.HandleDesiredRunnerCount(t.Context(), 1); err != nil {
				t.Fatal(err)
			}
			if tc.jitErr == nil && tc.launchErr == nil {
				if tc.shutdown {
					cancel()
					if err := launcher.Shutdown(ctx); err != nil {
						t.Fatal(err)
					}
				} else {
					be.mu.Lock()
					handle := be.handles[0]
					be.mu.Unlock()
					handle.complete()
				}
			}
			waitFor(t, func() bool { return len(events.snapshot()) == len(tc.want) })
			snapshot := events.snapshot()
			types := make([]runner.LifecycleEventType, len(snapshot))
			for i, event := range snapshot {
				types[i] = event.Type
				if event.LocalSessionID == "" || event.LocalSessionID != snapshot[0].LocalSessionID ||
					event.Pool != "linux" || event.Target != "o/r" ||
					event.Repository != "o/r" || event.RunnerName != snapshot[0].RunnerName {
					t.Fatalf("event identity = %+v", event)
				}
				if i > 0 && tc.jitErr == nil && event.GitHubRegistrationID != 1 {
					t.Fatalf("registration missing: %+v", event)
				}
			}
			if !reflect.DeepEqual(types, tc.want) {
				t.Fatalf("event types = %v, want %v", types, tc.want)
			}
			terminal := snapshot[len(snapshot)-1]
			if tc.cleanupErr != nil && !strings.Contains(terminal.Error, tc.cleanupErr.Error()) {
				t.Fatalf("terminal lost cleanup error: %+v", terminal)
			}
			if tc.partial || tc.launchErr == nil && tc.jitErr == nil {
				if terminal.BackendInstanceID == "" {
					t.Fatalf("backend identity missing: %+v", terminal)
				}
			}
			if tc.shutdown && terminal.Conclusion != "shutdown" {
				t.Fatalf("shutdown conclusion = %+v", terminal)
			}
			if legacy.Load() != 0 {
				t.Fatal("structured observer double-counted legacy hooks")
			}
			be.mu.Lock()
			be.removeOwnedErr = nil
			be.mu.Unlock()
			if err := launcher.Shutdown(context.Background()); err != nil {
				t.Fatal(err)
			}
			if got := len(events.snapshot()); got != len(tc.want) {
				t.Fatalf("cleanup emitted duplicate terminal event: %d", got)
			}
		})
	}
}

func TestLifecycleTargetFallbackDoesNotInventOrgOrEnterpriseRepositories(t *testing.T) {
	for _, tc := range []struct {
		target string
		want   string
	}{
		{target: "https://github.com/acme", want: "acme"},
		{target: "https://github.com/enterprises/acme", want: "acme"},
	} {
		launcher := New(t.Context(), &fakeJIT{}, &fakeBackend{}, Options{
			ScaleSetID: 42, Ownership: backend.RunnerOwnership{Target: tc.target},
		})
		if launcher.opts.Repository != "" || launcher.lifecycleTarget() != tc.want ||
			launcher.opts.Ownership.Pool != "scaleset-42" {
			t.Fatalf("fallback = %+v, target = %q", launcher.opts, launcher.lifecycleTarget())
		}
	}
}

func TestStructuredReconciliationReportsOnceWithStableIdentity(t *testing.T) {
	events := &lifecycleEvents{}
	be := &fakeBackend{owned: []backend.OwnedRunner{{ResourceID: "orphan", RunnerID: 123, Name: "old"}}}
	launcher := New(t.Context(), &fakeJIT{}, be, Options{
		ScaleSetID: 42, Observer: events,
		Ownership: backend.RunnerOwnership{Instance: "host", Pool: "linux", Target: "o/r"},
	})
	for range 2 {
		if _, err := launcher.Reconcile(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	snapshot := events.snapshot()
	if len(snapshot) != 2 || snapshot[0].Type != runner.LifecyclePlanned ||
		snapshot[1].Type != runner.LifecycleStopped || snapshot[1].Conclusion != "reconciled" ||
		snapshot[0].LocalSessionID != snapshot[1].LocalSessionID ||
		snapshot[1].GitHubRegistrationID != 123 || snapshot[1].BackendInstanceID != "orphan" {
		t.Fatalf("reconciliation lifecycle = %+v", snapshot)
	}
	other := New(t.Context(), &fakeJIT{}, be, launcher.opts)
	event, _ := other.reconciliationEvent(be.owned[0])
	if event.LocalSessionID != snapshot[0].LocalSessionID {
		t.Fatal("reconciliation identity changed across listener restart")
	}
}
