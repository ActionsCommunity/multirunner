package pool

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/GerardSmit/multirunner/internal/backend"
	"github.com/GerardSmit/multirunner/internal/config"
	"github.com/GerardSmit/multirunner/internal/github"
	"github.com/GerardSmit/multirunner/internal/runner"
)

type intentBackend struct {
	launched chan struct{}
	killErr  error
}

func (*intentBackend) Name() string                              { return "intent" }
func (*intentBackend) Ping(context.Context) error                { return nil }
func (*intentBackend) OSType(context.Context) (string, error)    { return "linux", nil }
func (*intentBackend) EnsureImage(context.Context, string) error { return nil }
func (*intentBackend) Close() error                              { return nil }
func (b *intentBackend) Launch(context.Context, backend.LaunchRequest) (backend.RunnerHandle, error) {
	b.launched <- struct{}{}
	return intentHandle{killErr: b.killErr}, nil
}

type intentHandle struct{ killErr error }

func (intentHandle) ID() string { return "intent" }
func (intentHandle) Wait(ctx context.Context) (int, error) {
	<-ctx.Done()
	return 137, ctx.Err()
}
func (intentHandle) Logs(context.Context) (io.ReadCloser, error) { return nil, nil }
func (h intentHandle) Kill(context.Context) error                { return h.killErr }

func TestIntentionalTerminateDoesNotBackoffPoolSlot(t *testing.T) {
	server := successfulJITServer(t)
	client := testClient(t, server.URL, "repo")
	logger := slog.New(slog.DiscardHandler)
	be := &intentBackend{launched: make(chan struct{}, 10)}
	events := make(chan runner.LifecycleEvent, 20)
	launcher := NewLauncher(config.Pool{
		Name: "linux", OS: "linux", Size: 1, NamePrefix: "mr", MaxConsecutiveFailures: 1,
	}, "image", be, client, nil, nil, logger, Hooks{Observer: runner.LifecycleObserverFunc(
		func(ctx context.Context, event runner.LifecycleEvent) { events <- event },
	)})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- NewPool(launcher, logger).Run(ctx) }()
	var id string
	select {
	case <-be.launched:
		id = launcher.ActiveSessions()[0]
	case <-time.After(time.Second):
		t.Fatal("runner did not launch")
	}
	if err := launcher.Terminate(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	select {
	case <-be.launched:
		// backoffBase is two seconds; a clean intentional stop must refill now.
	case <-time.After(time.Second):
		t.Fatal("intentional stop entered failure backoff")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	close(events)
	terminalCount := 0
	for event := range events {
		if event.LocalSessionID == id && (event.Type == runner.LifecycleFailed || event.Type == runner.LifecycleStopped) {
			terminalCount++
			if event.Type != runner.LifecycleStopped || event.Conclusion != "terminated" || event.Error != "" {
				t.Fatalf("intentional terminal = %+v", event)
			}
		}
	}
	if terminalCount != 1 {
		t.Fatalf("terminal events = %d", terminalCount)
	}
}

func TestTerminateReturnsRealCleanupFailure(t *testing.T) {
	server := successfulJITServer(t)
	client := testClient(t, server.URL, "repo")
	be := &intentBackend{launched: make(chan struct{}, 1), killErr: errors.New("kill denied")}
	launcher := NewLauncher(config.Pool{Name: "linux", OS: "linux", Size: 1},
		"image", be, client, nil, nil, slog.New(slog.DiscardHandler), Hooks{})
	done := make(chan error, 1)
	go func() { _, err := launcher.RunOne(t.Context()); done <- err }()
	<-be.launched
	err := launcher.Terminate(t.Context(), launcher.ActiveSessions()[0])
	if err == nil || !strings.Contains(err.Error(), "kill denied") {
		t.Fatalf("Terminate error = %v", err)
	}
	if err := <-done; err == nil || !strings.Contains(err.Error(), "kill denied") {
		t.Fatalf("RunOne error = %v", err)
	}
}

func TestRunJobIfUnpausedDoesNotWaitOrRegister(t *testing.T) {
	server, paths := apiRecorder(t)
	client := testClient(t, server.URL, "repo")
	launcher := NewLauncher(config.Pool{Name: "linux", Size: 1}, "image",
		&captureBackend{}, client, nil, nil, slog.New(slog.DiscardHandler), Hooks{})
	launcher.Pause()
	_, err := launcher.RunJobIfUnpaused(t.Context(), github.QueuedJob{Client: client})
	if !errors.Is(err, ErrProvisioningPaused) || len(paths()) != 0 {
		t.Fatalf("paused launch = %v, requests = %v", err, paths())
	}
}
