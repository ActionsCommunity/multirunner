package history

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/GerardSmit/multirunner/internal/webhook"
)

// WebhookObserver persists normalized workflow_job deliveries for freshness.
// Periodic REST reconciliation remains the durable repair path.
type WebhookObserver struct {
	store          *Store
	legacyPrefixes []string
	logger         *slog.Logger
}

func NewWebhookObserver(store *Store, legacyPrefixes []string, logger *slog.Logger) *WebhookObserver {
	return &WebhookObserver{
		store:          store,
		legacyPrefixes: append([]string(nil), legacyPrefixes...),
		logger:         logger,
	}
}

func (o *WebhookObserver) OnWorkflowJob(
	ctx context.Context, event webhook.WorkflowJobEvent,
) {
	if o == nil || o.store == nil || event.Repository == "" || event.WorkflowJob.ID == 0 {
		return
	}
	now := time.Now().UTC()
	if event.DeliveryID != "" {
		inserted, err := o.store.RecordWebhookDelivery(ctx, WebhookDelivery{
			ID: event.DeliveryID, Event: "workflow_job", Action: event.Action,
			Repository: event.Repository, ReceivedAt: now,
		})
		if err != nil {
			o.logError("record history webhook", event, err)
			return
		}
		if !inserted {
			return
		}
	}

	session, source, confidence, matched, err := o.matchSession(ctx, event)
	if err != nil {
		o.logError("match history webhook", event, err)
		return
	}
	if !matched {
		if event.DeliveryID != "" {
			_ = o.store.MarkWebhookProcessed(ctx, event.DeliveryID, now)
		}
		return
	}

	job := event.WorkflowJob
	createdAt := dereferenceTime(job.CreatedAt, now)
	updatedAt := dereferenceTime(job.CompletedAt, dereferenceTime(job.StartedAt, now))
	run := WorkflowRun{
		ID: job.RunID, Repository: event.Repository, Name: job.WorkflowName,
		WorkflowName: job.WorkflowName, Status: job.Status, Conclusion: job.Conclusion,
		HeadBranch: job.HeadBranch, HeadSHA: job.HeadSHA, RunAttempt: job.RunAttempt,
		CreatedAt: createdAt, UpdatedAt: updatedAt,
	}
	if err := o.store.UpsertWorkflowRun(ctx, run); err != nil {
		o.logError("persist history webhook run", event, err)
		return
	}
	record := WorkflowJob{
		ID: job.ID, RunID: job.RunID, RunAttempt: job.RunAttempt,
		Repository: event.Repository, Name: job.Name, WorkflowName: job.WorkflowName,
		HeadBranch: job.HeadBranch, HeadSHA: job.HeadSHA, Status: job.Status,
		Conclusion: job.Conclusion, Labels: append([]string(nil), job.Labels...),
		RunnerID: job.RunnerID, RunnerName: job.RunnerName,
		RunnerGroup: job.RunnerGroupName, PoolName: session.PoolName,
		LocalSessionID: session.ID, AttributionSource: source,
		AttributionConfidence: confidence, APIURL: job.URL, HTMLURL: job.HTMLURL,
		CreatedAt: createdAt, StartedAt: job.StartedAt, CompletedAt: job.CompletedAt,
		UpdatedAt: updatedAt, Steps: make([]WorkflowStep, 0, len(job.Steps)),
	}
	for _, step := range job.Steps {
		record.Steps = append(record.Steps, WorkflowStep{
			Number: int(step.Number), Name: step.Name, Status: step.Status,
			Conclusion: step.Conclusion, StartedAt: step.StartedAt,
			CompletedAt: step.CompletedAt,
		})
	}
	if err := o.store.UpsertWorkflowJob(ctx, record); err != nil {
		o.logError("persist history webhook job", event, err)
		return
	}
	if session.ID != "" {
		session.Repository = event.Repository
		session.WorkflowRunID = job.RunID
		session.RunAttempt = job.RunAttempt
		session.WorkflowJobID = job.ID
		session.AttributionSource = source
		session.AttributionConfidence = confidence
		session.UpdatedAt = now
		if err := o.store.UpsertRunnerSession(ctx, session); err != nil {
			o.logError("correlate history runner session", event, err)
		}
	}
	if event.DeliveryID != "" {
		if err := o.store.MarkWebhookProcessed(ctx, event.DeliveryID, now); err != nil {
			o.logError("complete history webhook", event, err)
		}
	}
}

func (o *WebhookObserver) matchSession(ctx context.Context, event webhook.WorkflowJobEvent) (RunnerSession, string, int, bool, error) {
	var afterTime *time.Time
	var afterID string
	for {
		sessions, err := o.store.ListRunnerSessions(ctx, ListOptions{
			Limit: 1000, AfterTime: afterTime, AfterID: afterID,
		})
		if err != nil {
			return RunnerSession{}, "", 0, false, err
		}
		for _, session := range sessions {
			if session.WorkflowJobID != 0 && session.WorkflowJobID == event.WorkflowJob.ID {
				return session, "exact_job_id", 100, true, nil
			}
		}
		if event.WorkflowJob.RunnerName != "" {
			for _, session := range sessions {
				if session.RunnerName == event.WorkflowJob.RunnerName {
					return session, "exact_runner_name", 100, true, nil
				}
			}
		}
		if len(sessions) < 1000 {
			break
		}
		last := sessions[len(sessions)-1]
		afterTime, afterID = &last.StartedAt, last.ID
	}
	for _, prefix := range o.legacyPrefixes {
		if prefix != "" && strings.HasPrefix(event.WorkflowJob.RunnerName, prefix) {
			return RunnerSession{}, "inferred_legacy_prefix", 50, true, nil
		}
	}
	return RunnerSession{}, "", 0, false, nil
}

func (o *WebhookObserver) logError(message string, event webhook.WorkflowJobEvent, err error) {
	if o.logger != nil {
		o.logger.Error(message, "repository", event.Repository,
			"run_id", event.WorkflowJob.RunID, "job_id", event.WorkflowJob.ID, "err", err)
	}
}

func dereferenceTime(value *time.Time, fallback time.Time) time.Time {
	if value == nil || value.IsZero() {
		return fallback
	}
	return value.UTC()
}

var _ webhook.Observer = (*WebhookObserver)(nil)
