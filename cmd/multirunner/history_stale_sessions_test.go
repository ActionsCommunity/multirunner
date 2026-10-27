package main

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/GerardSmit/multirunner/internal/config"
	"github.com/GerardSmit/multirunner/internal/history"
)

func TestHistorySetupCannotRecoverAnotherDaemonsLiveSessions(t *testing.T) {
	database := filepath.Join(t.TempDir(), "history.db")
	owner, err := history.AcquireRuntimeLock(database)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	store, err := history.Open(t.Context(), database)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.UpsertRunnerSession(t.Context(), history.RunnerSession{
		ID: "still-live", PoolName: "linux", RunnerName: "live-runner", Status: "launched",
		StartedAt: time.Now().Add(-time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{History: config.History{DatabasePath: database}}
	if runtime, err := setupPersistentHistory(t.Context(), "", cfg, nil, nil, slog.New(slog.DiscardHandler)); err == nil {
		runtime.Close()
		t.Fatal("second runtime took over another daemon's live history")
	}
	pending, err := store.PendingRunnerSessions(t.Context(), 10)
	if err != nil || len(pending) != 1 || pending[0].ID != "still-live" {
		t.Fatalf("second runtime closed a live session: %+v, %v", pending, err)
	}
}

func TestReconcileStaleRunnerSessionsPaginatesAndPreservesCurrentAndTerminal(t *testing.T) {
	directory := t.TempDir()
	store, err := history.Open(t.Context(), filepath.Join(directory, "history.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Now().UTC().Truncate(time.Millisecond)
	for i := range 1005 {
		if err := store.UpsertRunnerSession(t.Context(), history.RunnerSession{
			ID: fmt.Sprintf("old-%04d", i), RunnerName: "old", PoolName: "pool",
			Status: "launched", StartedAt: now.Add(-time.Hour), PlannedAt: now.Add(-time.Hour),
		}); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []string{"current", "terminal"} {
		started := now
		if id == "terminal" {
			started = now.Add(-time.Hour)
		}
		if err := store.UpsertRunnerSession(t.Context(), history.RunnerSession{
			ID: id, RunnerName: id, PoolName: "pool", Status: "launched",
			StartedAt: started, PlannedAt: started,
		}); err != nil {
			t.Fatal(err)
		}
	}
	completedAt := now.Add(-30 * time.Minute)
	if err := store.CompleteRunnerSession(t.Context(), "terminal", "completed", "success", "", completedAt); err != nil {
		t.Fatal(err)
	}
	if err := reconcileStaleRunnerSessions(t.Context(), store, now.Add(500*time.Microsecond)); err != nil {
		t.Fatal(err)
	}
	pending, err := store.PendingRunnerSessions(t.Context(), 1000)
	if err != nil || len(pending) != 1 || pending[0].ID != "current" {
		t.Fatalf("pending = %+v, err = %v", pending, err)
	}
	terminal, err := store.ListRunnerSessions(t.Context(), history.ListOptions{Query: "terminal"})
	if err != nil || len(terminal) != 1 || terminal[0].Conclusion != "success" ||
		terminal[0].CompletedAt == nil || !terminal[0].CompletedAt.Equal(completedAt) {
		t.Fatalf("terminal overwritten: %+v, %v", terminal, err)
	}
	stale, err := store.ListRunnerSessions(t.Context(), history.ListOptions{Limit: 1000})
	if err != nil {
		t.Fatal(err)
	}
	for _, session := range stale {
		if session.ID == "current" || session.ID == "terminal" {
			continue
		}
		if session.Status != "completed" || session.Conclusion != "interrupted" || session.CompletedAt == nil {
			t.Fatalf("stale session not finalized: %+v", session)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := reconcileStaleRunnerSessions(ctx, store, now); err == nil {
		t.Fatal("cancelled startup reconciliation succeeded")
	}
}
