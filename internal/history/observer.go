package history

import (
	"context"
	"log/slog"
	"strings"
	"sync"

	"github.com/GerardSmit/multirunner/internal/runner"
)

// LifecycleObserver persists local runner lifecycle state without allowing
// history failures to stop runner provisioning.
type LifecycleObserver struct {
	store  *Store
	logger *slog.Logger

	mu       sync.Mutex
	sessions map[string]RunnerSession
}

func NewLifecycleObserver(store *Store, logger *slog.Logger) *LifecycleObserver {
	return &LifecycleObserver{
		store:    store,
		logger:   logger,
		sessions: make(map[string]RunnerSession),
	}
}

func (o *LifecycleObserver) ObserveRunnerLifecycle(
	ctx context.Context, event runner.LifecycleEvent,
) {
	if o == nil || o.store == nil || event.LocalSessionID == "" {
		return
	}

	o.mu.Lock()
	session := o.sessions[event.LocalSessionID]
	if session.ID == "" {
		repository := event.Repository
		if repository == "" && strings.Contains(event.Target, "/") {
			repository = event.Target
		}
		session = RunnerSession{
			ID:            event.LocalSessionID,
			RunnerName:    event.RunnerName,
			PoolName:      event.Pool,
			Target:        event.Target,
			Repository:    repository,
			WorkflowRunID: event.QueuedRunID,
			RunAttempt:    event.QueuedRunAttempt,
			WorkflowJobID: event.QueuedJobID,
			PlannedAt:     event.Timestamp,
			StartedAt:     event.Timestamp,
		}
		if event.QueuedJobID != 0 {
			session.AttributionSource = "exact_job_id"
			session.AttributionConfidence = 100
		}
	}
	session.UpdatedAt = event.Timestamp
	session.GitHubRegistrationID = event.GitHubRegistrationID
	session.BackendID = event.BackendInstanceID
	session.ExitCode = event.ExitCode
	session.Error = event.Error

	switch event.Type {
	case runner.LifecyclePlanned:
		session.Status = "planned"
	case runner.LifecycleRegistered:
		session.Status = "registered"
		timestamp := event.Timestamp
		session.RegisteredAt = &timestamp
	case runner.LifecycleLaunched:
		session.Status = "launched"
		timestamp := event.Timestamp
		session.LaunchedAt = &timestamp
	case runner.LifecycleStopped:
		session.Status = "completed"
		session.Conclusion = "success"
		timestamp := event.Timestamp
		session.CompletedAt = &timestamp
	case runner.LifecycleFailed:
		session.Status = "completed"
		session.Conclusion = "failure"
		timestamp := event.Timestamp
		session.CompletedAt = &timestamp
	default:
		o.mu.Unlock()
		return
	}

	o.sessions[event.LocalSessionID] = session
	if session.CompletedAt != nil {
		delete(o.sessions, event.LocalSessionID)
	}
	o.mu.Unlock()

	if err := o.store.UpsertRunnerSession(ctx, session); err != nil && o.logger != nil {
		o.logger.Error("persist runner history", "session_id", event.LocalSessionID, "err", err)
	}
}

var _ runner.LifecycleObserver = (*LifecycleObserver)(nil)
