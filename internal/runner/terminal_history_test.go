package runner_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/GerardSmit/multirunner/internal/backend"
	"github.com/GerardSmit/multirunner/internal/config"
	"github.com/GerardSmit/multirunner/internal/github"
	"github.com/GerardSmit/multirunner/internal/history"
	"github.com/GerardSmit/multirunner/internal/runner"
)

type cancelledHistoryBackend struct{ cancel context.CancelFunc }

func (cancelledHistoryBackend) Name() string                              { return "history-test" }
func (cancelledHistoryBackend) Ping(context.Context) error                { return nil }
func (cancelledHistoryBackend) OSType(context.Context) (string, error)    { return "linux", nil }
func (cancelledHistoryBackend) EnsureImage(context.Context, string) error { return nil }
func (cancelledHistoryBackend) Close() error                              { return nil }
func (b cancelledHistoryBackend) Launch(context.Context, backend.LaunchRequest) (backend.RunnerHandle, error) {
	return cancelledHistoryHandle{cancel: b.cancel}, nil
}

type cancelledHistoryHandle struct{ cancel context.CancelFunc }

func (cancelledHistoryHandle) ID() string { return "cancelled-backend" }
func (h cancelledHistoryHandle) Wait(context.Context) (int, error) {
	h.cancel()
	return -1, context.Canceled
}
func (cancelledHistoryHandle) Kill(context.Context) error { return nil }
func (cancelledHistoryHandle) Logs(context.Context) (io.ReadCloser, error) {
	return nil, nil
}

func TestCancelledRunnerPersistsTerminalSessionInSQLite(t *testing.T) {
	directory := filepath.Join(".", ".test-history-"+runner.NewLocalSessionID())
	if err := os.Mkdir(directory, 0700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	store, err := history.Open(t.Context(), filepath.Join(directory, "history.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"encoded_jit_config":"jit","runner":{"id":42}}`)
	}))
	defer server.Close()
	client, err := github.New(t.Context(),
		config.GitHub{URL: server.URL, Scope: config.ScopeRepo, Owner: "o", Repo: "r"},
		config.Auth{PAT: "test"})
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.DiscardHandler)
	ctx, cancel := context.WithCancel(t.Context())
	_, err = runner.RunOnce(ctx, client, cancelledHistoryBackend{cancel: cancel}, runner.Spec{
		Name: "cancelled", LocalSessionID: "cancelled", Pool: "linux", Target: "o/r",
		Observer: history.NewLifecycleObserver(store, logger),
	}, logger)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("RunOnce error = %v", err)
	}
	sessions, err := store.ListRunnerSessions(t.Context(), history.ListOptions{})
	if err != nil || len(sessions) != 1 {
		t.Fatalf("sessions = %+v, %v", sessions, err)
	}
	if session := sessions[0]; session.CompletedAt == nil || session.Status != "completed" ||
		session.Conclusion != "failure" || session.BackendID != "cancelled-backend" {
		t.Fatalf("terminal session was not persisted: %+v", session)
	}
	pending, err := store.PendingRunnerSessions(t.Context(), 100)
	if err != nil || len(pending) != 0 {
		t.Fatalf("pending = %+v, %v", pending, err)
	}
}
