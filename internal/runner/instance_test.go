package runner

import (
	"context"
	"encoding/json"
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

// stubHandle is a runner container that has already exited by the time the test
// looks at it, which is exactly the state RunOnce sees after Wait returns.
type stubHandle struct {
	code    int
	waitErr error
	killed  bool
	// onWait fires inside Wait, letting a test cancel the job context at the
	// exact moment production does: while the runner is in flight.
	onWait func()
}

func (h *stubHandle) ID() string { return "container-1" }
func (h *stubHandle) Wait(context.Context) (int, error) {
	if h.onWait != nil {
		h.onWait()
	}
	return h.code, h.waitErr
}
func (h *stubHandle) Logs(context.Context) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader("")), nil
}
func (h *stubHandle) Kill(context.Context) error { h.killed = true; return nil }

type stubBackend struct{ handle *stubHandle }

func (stubBackend) Name() string                              { return "stub" }
func (stubBackend) Ping(context.Context) error                { return nil }
func (stubBackend) OSType(context.Context) (string, error)    { return "linux", nil }
func (stubBackend) EnsureImage(context.Context, string) error { return nil }
func (b stubBackend) Launch(context.Context, backend.LaunchRequest) (backend.RunnerHandle, error) {
	return b.handle, nil
}
func (stubBackend) Close() error { return nil }

// jitServer is a stand-in GitHub API that hands out one JIT config and records
// every DELETE it receives, so a test can prove the registration was reclaimed.
func jitServer(t *testing.T, deleteStatus int) (*github.Client, func() []string) {
	t.Helper()
	return jitServerWithRunnerID(t, deleteStatus, 42)
}

func jitServerWithRunnerID(t *testing.T, deleteStatus int, runnerID int64) (*github.Client, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var deletes []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			mu.Lock()
			deletes = append(deletes, r.URL.Path)
			mu.Unlock()
			w.WriteHeader(deleteStatus)
			return
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"encoded_jit_config": "BASE64BLOB",
			"runner":             map[string]any{"id": runnerID, "name": "mr-1"},
		})
	}))
	t.Cleanup(srv.Close)

	c, err := github.New(context.Background(),
		config.GitHub{URL: srv.URL, Scope: config.ScopeRepo, Owner: "o", Repo: "r"},
		config.Auth{PAT: "test-token"})
	if err != nil {
		t.Fatalf("github.New: %v", err)
	}
	return c, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), deletes...)
	}
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// TestRunOnceDeregistersAfterExit is the regression test for the registration
// leak: GitHub only auto-removes an ephemeral runner that consumed a job, so a
// runner that exits without one used to stay registered as offline forever.
func TestRunOnceDeregistersAfterExit(t *testing.T) {
	gh, deletes := jitServer(t, http.StatusNoContent)

	code, err := RunOnce(context.Background(), gh, stubBackend{&stubHandle{}},
		Spec{Name: "mr-1", Image: "img"}, discardLogger())
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
	got := deletes()
	if len(got) != 1 || !strings.HasSuffix(got[0], "/repos/o/r/actions/runners/42") {
		t.Errorf("deletes = %v, want one delete of runner 42", got)
	}
}

// TestRunOnceDeregistersAfterWaitFailure covers the path that leaked most often
// in practice: the container died in a way the backend surfaced as an error, so
// the old code returned before it ever reached the cleanup.
func TestRunOnceDeregistersAfterWaitFailure(t *testing.T) {
	gh, deletes := jitServer(t, http.StatusNoContent)
	handle := &stubHandle{code: 1, waitErr: errors.New("container vanished")}

	if _, err := RunOnce(context.Background(), gh, stubBackend{handle},
		Spec{Name: "mr-1", Image: "img"}, discardLogger()); err == nil {
		t.Fatal("RunOnce should surface the wait failure")
	}
	if got := deletes(); len(got) != 1 {
		t.Errorf("deletes = %v, want the registration reclaimed even on failure", got)
	}
}

// TestRunOnceDeregistersOnShutdown pins that the shutdown path still kills the
// container and still cleans up. The context is cancelled while the runner is
// in flight, which is what a real shutdown looks like.
func TestRunOnceDeregistersOnShutdown(t *testing.T) {
	gh, deletes := jitServer(t, http.StatusNoContent)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	handle := &stubHandle{onWait: cancel}

	_, err := RunOnce(ctx, gh, stubBackend{handle}, Spec{Name: "mr-1", Image: "img"}, discardLogger())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("RunOnce error = %v, want context.Canceled", err)
	}
	if !handle.killed {
		t.Error("shutdown must kill the in-flight container")
	}
	if got := deletes(); len(got) != 1 {
		t.Errorf("deletes = %v, want the registration reclaimed on shutdown", got)
	}
}

// TestRunOnceToleratesAlreadyDeletedRunner covers the happy path in production:
// the runner took a job, GitHub already removed it, and our delete 404s. That
// is success, so it must not surface as a warning.
func TestRunOnceToleratesAlreadyDeletedRunner(t *testing.T) {
	gh, deletes := jitServer(t, http.StatusNotFound)
	var logs strings.Builder
	logger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))

	if _, err := RunOnce(context.Background(), gh, stubBackend{&stubHandle{}},
		Spec{Name: "mr-1", Image: "img"}, logger); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if got := deletes(); len(got) != 1 {
		t.Errorf("deletes = %v, want one attempt", got)
	}
	if strings.Contains(logs.String(), "deregister runner failed") {
		t.Errorf("an already-removed runner was reported as a failure:\n%s", logs.String())
	}
}

// TestRunOnceSkipsDeregisterWithoutRunnerID guards the case where GitHub hands
// back a JIT config with no runner object: there is no registration to reclaim,
// so calling DELETE on runner 0 would just be a bogus request.
func TestRunOnceSkipsDeregisterWithoutRunnerID(t *testing.T) {
	gh, deletes := jitServerWithRunnerID(t, http.StatusNoContent, 0)

	if _, err := RunOnce(context.Background(), gh, stubBackend{&stubHandle{}},
		Spec{Name: "mr-1", Image: "img"}, discardLogger()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if got := deletes(); len(got) != 0 {
		t.Errorf("deletes = %v, want none without a runner id", got)
	}
}

// TestRunOnceReportsDeregisterFailure pins that a real API failure is still
// surfaced, so the tolerance for 404 does not turn into blanket silence.
func TestRunOnceReportsDeregisterFailure(t *testing.T) {
	gh, _ := jitServer(t, http.StatusInternalServerError)
	var logs strings.Builder
	logger := slog.New(slog.NewTextHandler(&logs, nil))

	if _, err := RunOnce(context.Background(), gh, stubBackend{&stubHandle{}},
		Spec{Name: "mr-1", Image: "img"}, logger); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if !strings.Contains(logs.String(), "deregister runner failed") {
		t.Errorf("a failed cleanup went unreported:\n%s", logs.String())
	}
}
