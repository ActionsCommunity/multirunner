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

func TestLifecycleStoppedPreservesIntentionalConclusion(t *testing.T) {
	for _, conclusion := range []string{"terminated", "shutdown", "reconciled"} {
		t.Run(conclusion, func(t *testing.T) {
			store := openTestStore(t)
			NewLifecycleObserver(store, nil).ObserveRunnerLifecycle(t.Context(), runner.LifecycleEvent{
				Type: runner.LifecycleStopped, LocalSessionID: "stopped", Conclusion: conclusion,
				Pool: "linux", RunnerName: "runner", Timestamp: time.Now().UTC(),
			})
			sessions, err := store.ListRunnerSessions(t.Context(), ListOptions{})
			if err != nil || len(sessions) != 1 || sessions[0].Conclusion != conclusion {
				t.Fatalf("intentional conclusion = %+v, %v", sessions, err)
			}
		})
	}
}

func TestLifecycleUpdatesPreserveWebhookCorrelation(t *testing.T) {
	store := openTestStore(t)
	lifecycle := NewLifecycleObserver(store, nil)
	webhooks := NewWebhookObserver(store, nil, nil)
	now := time.Date(2026, 10, 9, 16, 0, 0, 0, time.UTC)
	lifecycle.ObserveRunnerLifecycle(t.Context(), runner.LifecycleEvent{
		Type: runner.LifecyclePlanned, LocalSessionID: "session-1",
		Pool: "linux", Target: "owner", Repository: "owner/trigger-repo",
		RunnerName: "runner-1", Timestamp: now,
		QueuedRunID: 99, QueuedRunAttempt: 1, QueuedJobID: 199,
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
		Pool: "linux", Target: "owner", Repository: "owner/trigger-repo",
		RunnerName: "runner-1", Timestamp: now.Add(time.Minute),
	})
	lifecycle.ObserveRunnerLifecycle(t.Context(), runner.LifecycleEvent{
		Type: runner.LifecycleStopped, LocalSessionID: "session-1",
		Pool: "linux", Target: "owner", Repository: "owner/trigger-repo",
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
		session.Repository != "owner/repo" ||
		session.RunAttempt != 2 || session.AttributionSource != "exact_runner_name" ||
		session.AttributionConfidence != 100 {
		t.Fatalf("runner correlation was overwritten: %+v", session)
	}
}

func TestLifecycleTriggerIsNotAssignment(t *testing.T) {
	store := openTestStore(t)
	NewLifecycleObserver(store, nil).ObserveRunnerLifecycle(t.Context(), runner.LifecycleEvent{
		Type: runner.LifecyclePlanned, LocalSessionID: "warm-session",
		Pool: "linux", Target: "owner/repo", RunnerName: "warm-runner",
		QueuedRunID: 100, QueuedJobID: 200, QueuedRunAttempt: 2,
		Timestamp: time.Now().UTC(),
	})
	sessions, err := store.ListRunnerSessions(t.Context(), ListOptions{})
	if err != nil || len(sessions) != 1 {
		t.Fatalf("sessions = %+v, %v", sessions, err)
	}
	if got := sessions[0]; got.WorkflowJobID != 0 || got.WorkflowRunID != 0 ||
		got.AttributionConfidence != 0 || got.AttributionSource != "" {
		t.Fatalf("trigger was treated as assignment: %+v", got)
	}
}

func TestWebhookMatchingUsesConfirmedRunnerIdentity(t *testing.T) {
	store := openTestStore(t)
	now := time.Now().UTC()
	for _, session := range []RunnerSession{
		{ID: "trigger", RunnerName: "wrong-runner", WorkflowJobID: 200,
			AttributionSource: "exact_job_id", AttributionConfidence: 100},
		{ID: "actual", RunnerName: "actual-runner"},
	} {
		session.StartedAt, session.PlannedAt = now, now
		if err := store.UpsertRunnerSession(t.Context(), session); err != nil {
			t.Fatal(err)
		}
	}
	observer := NewWebhookObserver(store, nil, nil)
	event := webhook.WorkflowJobEvent{
		Repository:  "owner/repo",
		WorkflowJob: webhook.WorkflowJob{ID: 200, RunID: 100, RunnerName: "actual-runner"},
	}
	got, source, _, matched, err := observer.matchSession(t.Context(), event)
	if err != nil || !matched || got.ID != "actual" || source != "exact_runner_name" {
		t.Fatalf("contradictory hint match = %+v, %s, %v, %v", got, source, matched, err)
	}
	event.WorkflowJob.RunnerName = ""
	if _, _, _, matched, err := observer.matchSession(t.Context(), event); err != nil || matched {
		t.Fatalf("unconfirmed nameless match = %v, %v", matched, err)
	}
	event.WorkflowJob.RunnerName = "actual-runner"
	observer.OnWorkflowJob(t.Context(), event)
	event.WorkflowJob.RunnerName = ""
	got, source, _, matched, err = observer.matchSession(t.Context(), event)
	if err != nil || !matched || got.ID != "actual" || source != "exact_runner_name" {
		t.Fatalf("confirmed nameless match = %+v, %s, %v, %v", got, source, matched, err)
	}
	event.WorkflowJob.RunnerName = "unrelated"
	if _, _, _, matched, err := observer.matchSession(t.Context(), event); err != nil || matched {
		t.Fatalf("contradictory confirmed match = %v, %v", matched, err)
	}
}

func TestCorrelationPreservesConcurrentTerminalSession(t *testing.T) {
	store := openTestStore(t)
	now := time.Now().UTC()
	if err := store.UpsertRunnerSession(t.Context(), RunnerSession{
		ID: "session", RunnerName: "runner", StartedAt: now, Status: "launched",
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.CompleteRunnerSession(t.Context(), "session", "completed", "failure", "exit", now); err != nil {
		t.Fatal(err)
	}
	if err := store.CorrelateRunnerSession(t.Context(), WorkflowJob{
		ID: 200, RunID: 100, RunAttempt: 2, Repository: "owner/repo",
		LocalSessionID: "session", AttributionSource: "exact_runner_name", AttributionConfidence: 100,
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.CorrelateRunnerSession(t.Context(), WorkflowJob{
		ID: 200, RunID: 100, Repository: "owner/repo",
		LocalSessionID: "session", AttributionSource: "exact_runner_name", AttributionConfidence: 100,
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.CorrelateRunnerSession(t.Context(), WorkflowJob{
		ID: 201, RunID: 101, RunAttempt: 3, Repository: "owner/other",
		LocalSessionID: "session", AttributionSource: "inferred_legacy_prefix", AttributionConfidence: 50,
	}); err != nil {
		t.Fatal(err)
	}
	sessions, err := store.ListRunnerSessions(t.Context(), ListOptions{Repository: "owner/repo", RunID: 100})
	if err != nil || len(sessions) != 1 {
		t.Fatalf("run sessions = %+v, %v", sessions, err)
	}
	if got := sessions[0]; got.Status != "completed" || got.Conclusion != "failure" ||
		got.Error != "exit" || got.CompletedAt == nil || got.WorkflowJobID != 200 || got.RunAttempt != 2 {
		t.Fatalf("correlation lost terminal state: %+v", got)
	}
}
