package history

import (
	"context"
	"testing"
	"time"

	"github.com/GerardSmit/multirunner/internal/runner"
	"github.com/GerardSmit/multirunner/internal/webhook"
)

func TestLifecycleObserverHonorsCancelledContext(t *testing.T) {
	store := openTestStore(t)
	observer := NewLifecycleObserver(store, nil)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	observer.ObserveRunnerLifecycle(ctx, runner.LifecycleEvent{
		Type: runner.LifecyclePlanned, LocalSessionID: "cancelled-session",
		Pool: "linux", RunnerName: "runner-1", Timestamp: time.Now().UTC(),
	})
	sessions, err := store.ListRunnerSessions(t.Context(), ListOptions{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 0 {
		t.Fatalf("cancelled observer persisted %d sessions", len(sessions))
	}
}

func TestLifecycleUpdatesPreserveWebhookCorrelation(t *testing.T) {
	store := openTestStore(t)
	lifecycle := NewLifecycleObserver(store, nil)
	webhooks := NewWebhookObserver(store, nil, nil)
	now := time.Date(2026, 10, 9, 16, 0, 0, 0, time.UTC)
	lifecycle.ObserveRunnerLifecycle(t.Context(), runner.LifecycleEvent{
		Type: runner.LifecyclePlanned, LocalSessionID: "session-1",
		Pool: "linux", Target: "owner/repo", Repository: "owner/repo",
		RunnerName: "runner-1", Timestamp: now,
	})
	webhooks.OnWorkflowJob(t.Context(), webhook.WorkflowJobEvent{
		DeliveryID: "delivery-1", Action: "in_progress", Repository: "owner/repo",
		WorkflowJob: webhook.WorkflowJob{
			ID: 200, RunID: 100, RunAttempt: 2, Name: "test",
			WorkflowName: "CI", RunnerName: "runner-1", Status: "in_progress",
			CreatedAt: &now, StartedAt: &now,
		},
	})
	lifecycle.ObserveRunnerLifecycle(t.Context(), runner.LifecycleEvent{
		Type: runner.LifecycleLaunched, LocalSessionID: "session-1",
		Pool: "linux", Target: "owner/repo", Repository: "owner/repo",
		RunnerName: "runner-1", Timestamp: now.Add(time.Minute),
	})
	lifecycle.ObserveRunnerLifecycle(t.Context(), runner.LifecycleEvent{
		Type: runner.LifecycleStopped, LocalSessionID: "session-1",
		Pool: "linux", Target: "owner/repo", Repository: "owner/repo",
		RunnerName: "runner-1", Timestamp: now.Add(2 * time.Minute),
	})

	sessions, err := store.ListRunnerSessions(t.Context(), ListOptions{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 {
		t.Fatalf("runner sessions = %d", len(sessions))
	}
	session := sessions[0]
	if session.WorkflowRunID != 100 || session.WorkflowJobID != 200 ||
		session.RunAttempt != 2 || session.AttributionSource != "exact_runner_name" ||
		session.AttributionConfidence != 100 {
		t.Fatalf("runner correlation was overwritten: %+v", session)
	}
}
