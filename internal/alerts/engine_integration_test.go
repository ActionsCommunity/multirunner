package alerts_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/GerardSmit/multirunner/internal/alerts"
	"github.com/GerardSmit/multirunner/internal/history"
	"github.com/GerardSmit/multirunner/internal/operations"
)

func TestEngineReplaysDurableEventsAndResumesFromWatermark(t *testing.T) {
	store, err := history.Open(t.Context(), filepath.Join(t.TempDir(), "history.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	host, err := store.EnsureOperationalHost(t.Context(), "installation-1", "host")
	if err != nil {
		t.Fatal(err)
	}
	epoch, err := store.StartHostEpoch(t.Context(), host.ID, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	store.SetCommandEpoch(epoch.ID)
	journal, err := operations.NewJournal(
		t.Context(), store, epoch.ID, operations.JournalOptions{},
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(journal.Close)
	event, err := journal.Append(t.Context(), operations.EventInput{
		Type: "runner.failed", EntityType: "runner_session", EntityID: "session-1",
		ActorKind: operations.ActorSystem, ActorID: "multirunner",
		Payload: json.RawMessage(`{"pool":"linux","error":"backend unavailable"}`),
	})
	if err != nil {
		t.Fatal(err)
	}

	runEngine := func() {
		t.Helper()
		ctx, cancel := context.WithCancel(t.Context())
		engine, err := alerts.NewEngine(alerts.Options{
			Store: store, Source: journal, EpochID: epoch.ID,
			Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		})
		if err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() { done <- engine.Run(ctx) }()
		waitForAlert(t, store)
		cancel()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("alert engine did not stop")
		}
	}

	runEngine()
	cursor, err := store.AlertEventCursor(t.Context(), epoch.ID)
	if err != nil {
		t.Fatal(err)
	}
	if cursor < event.Sequence {
		t.Fatalf("alert cursor = %d, want at least %d", cursor, event.Sequence)
	}
	runEngine()
	instances, err := store.ListAlerts(t.Context(), alerts.ListOptions{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(instances) != 1 || instances[0].OccurrenceCount != 1 {
		t.Fatalf("alerts after restart = %+v", instances)
	}
}

func TestEngineReplaysUnfinishedPriorEpoch(t *testing.T) {
	store, err := history.Open(t.Context(), filepath.Join(t.TempDir(), "history.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	host, err := store.EnsureOperationalHost(t.Context(), "installation-1", "host")
	if err != nil {
		t.Fatal(err)
	}
	unfinished, err := store.StartHostEpoch(t.Context(), host.ID, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AppendOperationalEvent(
		t.Context(), unfinished.ID, operations.EventInput{
			Type: "runner.failed", EntityType: "runner_session", EntityID: "session-1",
			ActorKind: operations.ActorSystem, ActorID: "multirunner",
			Payload: json.RawMessage(`{"pool":"linux","error":"backend unavailable"}`),
		},
	); err != nil {
		t.Fatal(err)
	}
	current, err := store.StartHostEpoch(t.Context(), host.ID, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	store.SetCommandEpoch(current.ID)
	journal, err := operations.NewJournal(
		t.Context(), store, current.ID, operations.JournalOptions{},
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(journal.Close)
	engine, err := alerts.NewEngine(alerts.Options{
		Store: store, Source: journal, EpochID: unfinished.ID,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.Replay(t.Context()); err != nil {
		t.Fatal(err)
	}
	instances, err := store.ListAlerts(t.Context(), alerts.ListOptions{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(instances) != 1 || instances[0].RuleID != "runner-failed" {
		t.Fatalf("replayed alerts = %+v", instances)
	}
	cursor, err := store.AlertEventCursor(t.Context(), unfinished.ID)
	if err != nil {
		t.Fatal(err)
	}
	if cursor != 1 {
		t.Fatalf("unfinished epoch cursor = %d, want 1", cursor)
	}
}

func waitForAlert(t *testing.T, store *history.Store) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		instances, err := store.ListAlerts(t.Context(), alerts.ListOptions{Limit: 10})
		if err == nil && len(instances) == 1 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("durable event was not replayed into an alert")
}
