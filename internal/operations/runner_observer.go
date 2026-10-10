package operations

import (
	"context"
	"encoding/json"

	"github.com/GerardSmit/multirunner/internal/runner"
)

type EventAppender interface {
	TryAppend(EventInput) bool
}

type RunnerLifecycleObserver struct {
	journal EventAppender
}

func NewRunnerLifecycleObserver(journal EventAppender) *RunnerLifecycleObserver {
	return &RunnerLifecycleObserver{journal: journal}
}

func (o *RunnerLifecycleObserver) ObserveRunnerLifecycle(
	_ context.Context, event runner.LifecycleEvent,
) {
	if o == nil || o.journal == nil || event.LocalSessionID == "" {
		return
	}
	payload, err := json.Marshal(map[string]any{
		"pool":                   event.Pool,
		"target":                 event.Target,
		"repository":             event.Repository,
		"runner_name":            event.RunnerName,
		"queued_run_id":          event.QueuedRunID,
		"queued_run_attempt":     event.QueuedRunAttempt,
		"queued_job_id":          event.QueuedJobID,
		"github_registration_id": event.GitHubRegistrationID,
		"backend_instance_id":    event.BackendInstanceID,
		"exit_code":              event.ExitCode,
		"error":                  event.Error,
	})
	if err != nil {
		return
	}
	o.journal.TryAppend(EventInput{
		Type:          "runner." + string(event.Type),
		EntityType:    "runner_session",
		EntityID:      event.LocalSessionID,
		Timestamp:     event.Timestamp,
		CorrelationID: event.LocalSessionID,
		ActorKind:     ActorSystem,
		ActorID:       "multirunner",
		Payload:       payload,
	})
}

var _ runner.LifecycleObserver = (*RunnerLifecycleObserver)(nil)
