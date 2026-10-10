package history

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/GerardSmit/multirunner/internal/operations"
	_ "modernc.org/sqlite"
)

// Store is a concurrency-safe SQLite history store.
type Store struct {
	db            *sql.DB
	path          string
	eventMu       sync.RWMutex
	eventListener func(operations.Event)
	commandEpoch  string
	summaryMu     sync.RWMutex
	summaryCache  map[string]summaryCacheEntry
}

// Open opens a history database, configures SQLite, and applies migrations.
func Open(ctx context.Context, path string) (*Store, error) {
	if path == "" {
		return nil, errors.New("history database path is required")
	}
	if dir := filepath.Dir(path); dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("create history database directory: %w", err)
		}
	}
	_, statErr := os.Stat(path)
	existingDatabase := statErr == nil
	if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
		return nil, fmt.Errorf("stat history database: %w", statErr)
	}
	dsn, err := sqliteDSN(path, false)
	if err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open history database: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	state, err := inspectMigrationLedger(ctx, db)
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	if existingDatabase && state.needsChanges() {
		if _, err := createPreMigrationBackup(ctx, path, state.latest); err != nil {
			_ = db.Close()
			return nil, err
		}
	}
	if err := migrate(ctx, db); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &Store{
		db: db, path: path, summaryCache: make(map[string]summaryCacheEntry),
	}, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// UpsertRunnerSession inserts or replaces the current state of a runner lifecycle.
func (s *Store) UpsertRunnerSession(ctx context.Context, session RunnerSession) error {
	if session.ID == "" {
		return errors.New("runner session ID is required")
	}
	if session.StartedAt.IsZero() {
		session.StartedAt = session.PlannedAt
	}
	if session.PlannedAt.IsZero() {
		session.PlannedAt = session.StartedAt
	}
	if session.StartedAt.IsZero() {
		return errors.New("runner session start time is required")
	}
	if session.UpdatedAt.IsZero() {
		session.UpdatedAt = nowUTC()
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO runner_sessions (
		id, runner_name, pool_name, repository, workflow_run_id, workflow_job_id,
		status, conclusion, started_at, completed_at, updated_at, error,
		target, run_attempt, github_registration_id, backend_id,
		attribution_source, attribution_confidence, planned_at, registered_at,
		launched_at, exit_code
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	ON CONFLICT(id) DO UPDATE SET
		runner_name=excluded.runner_name, pool_name=excluded.pool_name,
		repository=CASE WHEN excluded.repository<>'' THEN excluded.repository
			ELSE runner_sessions.repository END,
		workflow_run_id=CASE WHEN excluded.workflow_run_id<>0 THEN excluded.workflow_run_id
			ELSE runner_sessions.workflow_run_id END,
		workflow_job_id=CASE WHEN excluded.workflow_job_id<>0 THEN excluded.workflow_job_id
			ELSE runner_sessions.workflow_job_id END, status=excluded.status,
		conclusion=excluded.conclusion, started_at=excluded.started_at,
		completed_at=excluded.completed_at, updated_at=excluded.updated_at,
		error=excluded.error, target=excluded.target,
		run_attempt=CASE WHEN excluded.run_attempt<>0 THEN excluded.run_attempt
			ELSE runner_sessions.run_attempt END,
		github_registration_id=excluded.github_registration_id,
		backend_id=excluded.backend_id,
		attribution_source=CASE WHEN excluded.attribution_source<>'' THEN excluded.attribution_source
			ELSE runner_sessions.attribution_source END,
		attribution_confidence=CASE WHEN excluded.attribution_confidence<>0
			THEN excluded.attribution_confidence
			ELSE runner_sessions.attribution_confidence END,
		planned_at=excluded.planned_at, registered_at=excluded.registered_at,
		launched_at=excluded.launched_at, exit_code=excluded.exit_code`,
		session.ID, session.RunnerName, session.PoolName, session.Repository,
		session.WorkflowRunID, session.WorkflowJobID, session.Status, session.Conclusion,
		timeMillis(session.StartedAt), nullableTime(session.CompletedAt),
		timeMillis(session.UpdatedAt), session.Error, session.Target, session.RunAttempt,
		session.GitHubRegistrationID, session.BackendID, session.AttributionSource,
		session.AttributionConfidence, timeMillis(session.PlannedAt),
		nullableTime(session.RegisteredAt), nullableTime(session.LaunchedAt), session.ExitCode)
	if err != nil {
		return fmt.Errorf("upsert runner session: %w", err)
	}
	s.invalidateSummaryCache()
	return nil
}

// CompleteRunnerSession records the terminal state of an existing session.
func (s *Store) CompleteRunnerSession(ctx context.Context, id, status, conclusion, errorText string, completedAt time.Time) error {
	if completedAt.IsZero() {
		completedAt = nowUTC()
	}
	result, err := s.db.ExecContext(ctx, `UPDATE runner_sessions
		SET status=?, conclusion=?, error=?, completed_at=?, updated_at=? WHERE id=?`,
		status, conclusion, errorText, timeMillis(completedAt), timeMillis(completedAt), id)
	if err != nil {
		return fmt.Errorf("complete runner session: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("complete runner session: %w", err)
	}
	if affected == 0 {
		return ErrNotFound
	}
	s.invalidateSummaryCache()
	return nil
}

// UpsertWorkflowRun inserts or updates a workflow run.
func (s *Store) UpsertWorkflowRun(ctx context.Context, run WorkflowRun) error {
	if run.ID == 0 || run.Repository == "" {
		return errors.New("workflow run repository and ID are required")
	}
	if run.CreatedAt.IsZero() {
		run.CreatedAt = nowUTC()
	}
	if run.UpdatedAt.IsZero() {
		run.UpdatedAt = run.CreatedAt
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO workflow_runs (
		repository, id, name, workflow_name, event, status, conclusion, head_branch,
		head_sha, actor, html_url, run_number, run_attempt, created_at, updated_at,
		started_at, completed_at, workflow_id, workflow_path, display_title,
		triggering_actor, api_url
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	ON CONFLICT(repository, id) DO UPDATE SET
		name=excluded.name, workflow_name=excluded.workflow_name, event=excluded.event,
		status=excluded.status, conclusion=excluded.conclusion,
		head_branch=excluded.head_branch, head_sha=excluded.head_sha, actor=excluded.actor,
		html_url=excluded.html_url, run_number=excluded.run_number,
		run_attempt=excluded.run_attempt, created_at=excluded.created_at,
		updated_at=excluded.updated_at, started_at=excluded.started_at,
		completed_at=excluded.completed_at, workflow_id=excluded.workflow_id,
		workflow_path=excluded.workflow_path, display_title=excluded.display_title,
		triggering_actor=excluded.triggering_actor, api_url=excluded.api_url`,
		run.Repository, run.ID, run.Name, run.WorkflowName, run.Event, run.Status,
		run.Conclusion, run.HeadBranch, run.HeadSHA, run.Actor, run.HTMLURL,
		run.RunNumber, run.RunAttempt, timeMillis(run.CreatedAt), timeMillis(run.UpdatedAt),
		nullableTime(run.StartedAt), nullableTime(run.CompletedAt), run.WorkflowID,
		run.WorkflowPath, run.DisplayTitle, run.TriggeringActor, run.APIURL)
	if err != nil {
		return fmt.Errorf("upsert workflow run: %w", err)
	}
	s.invalidateSummaryCache()
	return nil
}

// UpsertWorkflowJob atomically upserts a job and replaces its step snapshot.
func (s *Store) UpsertWorkflowJob(ctx context.Context, job WorkflowJob) error {
	if job.ID == 0 || job.RunID == 0 || job.Repository == "" {
		return errors.New("workflow job repository, run ID, and ID are required")
	}
	if job.CreatedAt.IsZero() {
		job.CreatedAt = nowUTC()
	}
	if job.UpdatedAt.IsZero() {
		job.UpdatedAt = job.CreatedAt
	}
	labels, err := json.Marshal(job.Labels)
	if err != nil {
		return fmt.Errorf("encode workflow job labels: %w", err)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin workflow job upsert: %w", err)
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, `INSERT INTO workflow_jobs (
		repository, id, run_id, name, status, conclusion, runner_name, runner_group,
		html_url, created_at, started_at, completed_at, updated_at, run_attempt,
		workflow_name, head_branch, head_sha, labels_json, runner_id, pool_name,
		local_session_id, attribution_source, attribution_confidence, api_url
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	ON CONFLICT(repository, id) DO UPDATE SET
		run_id=excluded.run_id, name=excluded.name, status=excluded.status,
		conclusion=excluded.conclusion, runner_name=excluded.runner_name,
		runner_group=excluded.runner_group, html_url=excluded.html_url,
		created_at=excluded.created_at, started_at=excluded.started_at,
		completed_at=excluded.completed_at, updated_at=excluded.updated_at,
		run_attempt=excluded.run_attempt, workflow_name=excluded.workflow_name,
		head_branch=excluded.head_branch, head_sha=excluded.head_sha,
		labels_json=excluded.labels_json, runner_id=excluded.runner_id,
		pool_name=excluded.pool_name, local_session_id=excluded.local_session_id,
		attribution_source=excluded.attribution_source,
		attribution_confidence=excluded.attribution_confidence, api_url=excluded.api_url`,
		job.Repository, job.ID, job.RunID, job.Name, job.Status, job.Conclusion,
		job.RunnerName, job.RunnerGroup, job.HTMLURL, timeMillis(job.CreatedAt),
		nullableTime(job.StartedAt), nullableTime(job.CompletedAt), timeMillis(job.UpdatedAt),
		job.RunAttempt, job.WorkflowName, job.HeadBranch, job.HeadSHA, string(labels),
		job.RunnerID, job.PoolName, job.LocalSessionID, job.AttributionSource,
		job.AttributionConfidence, job.APIURL)
	if err != nil {
		return fmt.Errorf("upsert workflow job: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM workflow_steps WHERE repository=? AND job_id=?`,
		job.Repository, job.ID); err != nil {
		return fmt.Errorf("replace workflow steps: %w", err)
	}
	if len(job.Steps) > 0 {
		query, args, err := workflowStepBatch(job)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, query, args...); err != nil {
			return fmt.Errorf("insert workflow step batch: %w", err)
		}
	}
	if err := advanceAnalyticsVersionTx(ctx, tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit workflow job upsert: %w", err)
	}
	s.invalidateSummaryCache()
	return nil
}

func workflowStepBatch(job WorkflowJob) (string, []any, error) {
	const prefix = `INSERT INTO workflow_steps (
		repository, job_id, number, name, status, conclusion, started_at, completed_at
	) VALUES `
	var query strings.Builder
	query.Grow(len(prefix) + len(job.Steps)*24)
	query.WriteString(prefix)
	args := make([]any, 0, len(job.Steps)*8)
	for index, step := range job.Steps {
		if step.Number < 1 {
			return "", nil, errors.New("workflow step number must be positive")
		}
		if index > 0 {
			query.WriteByte(',')
		}
		query.WriteString("(?, ?, ?, ?, ?, ?, ?, ?)")
		args = append(args,
			job.Repository, job.ID, step.Number, step.Name, step.Status, step.Conclusion,
			nullableTime(step.StartedAt), nullableTime(step.CompletedAt))
	}
	return query.String(), args, nil
}

// RecordWebhookDelivery inserts a delivery once. inserted is false for a duplicate ID.
func (s *Store) RecordWebhookDelivery(ctx context.Context, delivery WebhookDelivery) (inserted bool, err error) {
	if delivery.ID == "" {
		return false, errors.New("webhook delivery ID is required")
	}
	if delivery.ReceivedAt.IsZero() {
		delivery.ReceivedAt = nowUTC()
	}
	result, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO webhook_deliveries (
		id, event, action, repository, received_at, processed_at, payload
	) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		delivery.ID, delivery.Event, delivery.Action, delivery.Repository,
		timeMillis(delivery.ReceivedAt), nullableTime(delivery.ProcessedAt), delivery.Payload)
	if err != nil {
		return false, fmt.Errorf("record webhook delivery: %w", err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	if n == 1 {
		s.invalidateSummaryCache()
	}
	return n == 1, nil
}

// MarkWebhookProcessed sets a delivery's processed timestamp.
func (s *Store) MarkWebhookProcessed(ctx context.Context, id string, processedAt time.Time) error {
	if processedAt.IsZero() {
		processedAt = nowUTC()
	}
	result, err := s.db.ExecContext(ctx,
		`UPDATE webhook_deliveries SET processed_at=? WHERE id=?`,
		timeMillis(processedAt), id)
	if err != nil {
		return fmt.Errorf("mark webhook processed: %w", err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// SetSyncState idempotently stores a named synchronization cursor.
func (s *Store) SetSyncState(ctx context.Context, state SyncState) error {
	if state.Key == "" {
		return errors.New("sync state key is required")
	}
	if state.UpdatedAt.IsZero() {
		state.UpdatedAt = nowUTC()
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO sync_state(
		key, cursor, updated_at, repository, phase, last_success_at, last_error,
		retry_after, backfill_complete, rate_remaining, rate_reset_at
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT(key) DO UPDATE SET
		cursor=excluded.cursor, updated_at=excluded.updated_at,
		repository=excluded.repository, phase=excluded.phase,
		last_success_at=excluded.last_success_at, last_error=excluded.last_error,
		retry_after=excluded.retry_after, backfill_complete=excluded.backfill_complete,
		rate_remaining=excluded.rate_remaining, rate_reset_at=excluded.rate_reset_at`,
		state.Key, state.Cursor, timeMillis(state.UpdatedAt), state.Repository, state.Phase,
		nullableTime(state.LastSuccessAt), state.LastError, nullableTime(state.RetryAfter),
		boolInt(state.BackfillComplete), state.RateRemaining, nullableTime(state.RateResetAt))
	if err != nil {
		return fmt.Errorf("set sync state: %w", err)
	}
	return nil
}

// SyncState returns a named synchronization cursor.
func (s *Store) SyncState(ctx context.Context, key string) (SyncState, error) {
	var state SyncState
	var updated int64
	var lastSuccess, retryAfter, rateReset sql.NullInt64
	var backfillComplete int
	err := s.db.QueryRowContext(ctx,
		`SELECT key, cursor, updated_at, repository, phase, last_success_at,
		last_error, retry_after, backfill_complete, rate_remaining, rate_reset_at
		FROM sync_state WHERE key=?`, key).
		Scan(&state.Key, &state.Cursor, &updated, &state.Repository, &state.Phase,
			&lastSuccess, &state.LastError, &retryAfter, &backfillComplete,
			&state.RateRemaining, &rateReset)
	if errors.Is(err, sql.ErrNoRows) {
		return SyncState{}, ErrNotFound
	}
	if err != nil {
		return SyncState{}, fmt.Errorf("get sync state: %w", err)
	}
	state.UpdatedAt = millisTime(updated)
	state.LastSuccessAt = pointerTime(lastSuccess)
	state.RetryAfter = pointerTime(retryAfter)
	state.BackfillComplete = backfillComplete != 0
	state.RateResetAt = pointerTime(rateReset)
	return state, nil
}

func nowUTC() time.Time { return time.Now().UTC() }

func timeMillis(t time.Time) int64 { return t.UTC().UnixMilli() }

func nullableTime(t *time.Time) any {
	if t == nil || t.IsZero() {
		return nil
	}
	return timeMillis(*t)
}

func millisTime(ms int64) time.Time { return time.UnixMilli(ms).UTC() }

func pointerTime(ms sql.NullInt64) *time.Time {
	if !ms.Valid {
		return nil
	}
	t := millisTime(ms.Int64)
	return &t
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}
