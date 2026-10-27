package history

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/GerardSmit/multirunner/internal/alerts"
)

// PendingRunnerSessions returns incomplete sessions, oldest first.
func (s *Store) PendingRunnerSessions(ctx context.Context, limit int) ([]RunnerSession, error) {
	if limit <= 0 {
		limit = 100
	}
	if limit > 1000 {
		limit = 1000
	}
	rows, err := s.db.QueryContext(ctx, runnerSessionSelect+
		` WHERE completed_at IS NULL ORDER BY started_at ASC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("list pending runner sessions: %w", err)
	}
	return scanRunnerSessions(rows)
}

// ListRunnerSessions lists sessions newest first.
func (s *Store) ListRunnerSessions(ctx context.Context, options ListOptions) ([]RunnerSession, error) {
	where, args := runnerSessionWhere(options)
	where, args = addCursorClause(where, args, "started_at", options, false)
	query := runnerSessionSelect + where + ` ORDER BY started_at DESC, id DESC LIMIT ?`
	args = append(args, listLimit(options.Limit))
	if options.AfterTime == nil {
		query += ` OFFSET ?`
		args = append(args, nonnegative(options.Offset))
	}
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list runner sessions: %w", err)
	}
	return scanRunnerSessions(rows)
}

// ListWorkflowRuns lists workflow runs newest first.
func (s *Store) ListWorkflowRuns(ctx context.Context, options ListOptions) ([]WorkflowRun, error) {
	where, args := workflowRunWhere(options)
	where, args = addCursorClause(where, args, "created_at", options, true)
	args = append(args, listLimit(options.Limit))
	offset := ""
	if options.AfterTime == nil {
		offset = ` OFFSET ?`
		args = append(args, nonnegative(options.Offset))
	}
	rows, err := s.db.QueryContext(ctx, `SELECT
		id, repository, name, workflow_name, event, status, conclusion, head_branch,
		head_sha, actor, html_url, run_number, run_attempt, created_at, updated_at,
		started_at, completed_at, workflow_id, workflow_path, display_title,
		triggering_actor, api_url
		FROM workflow_runs`+where+` ORDER BY created_at DESC, id DESC LIMIT ?`+offset, args...)
	if err != nil {
		return nil, fmt.Errorf("list workflow runs: %w", err)
	}
	defer rows.Close()
	var result []WorkflowRun
	for rows.Next() {
		var run WorkflowRun
		var created, updated int64
		var started, completed sql.NullInt64
		if err := rows.Scan(
			&run.ID, &run.Repository, &run.Name, &run.WorkflowName, &run.Event,
			&run.Status, &run.Conclusion, &run.HeadBranch, &run.HeadSHA, &run.Actor,
			&run.HTMLURL, &run.RunNumber, &run.RunAttempt, &created, &updated,
			&started, &completed, &run.WorkflowID, &run.WorkflowPath,
			&run.DisplayTitle, &run.TriggeringActor, &run.APIURL,
		); err != nil {
			return nil, fmt.Errorf("scan workflow run: %w", err)
		}
		run.CreatedAt = millisTime(created)
		run.UpdatedAt = millisTime(updated)
		run.StartedAt = pointerTime(started)
		run.CompletedAt = pointerTime(completed)
		result = append(result, run)
	}
	return result, rows.Err()
}

func (s *Store) WorkflowRun(
	ctx context.Context, repository string, runID int64,
) (WorkflowRun, error) {
	if repository == "" || runID <= 0 {
		return WorkflowRun{}, errors.New("workflow run repository and ID are required")
	}
	runs, err := s.ListWorkflowRuns(ctx, ListOptions{
		Repository: repository, RunID: runID, Limit: 1,
	})
	if err != nil {
		return WorkflowRun{}, err
	}
	if len(runs) == 0 {
		return WorkflowRun{}, ErrNotFound
	}
	return runs[0], nil
}

// ListWorkflowJobs lists jobs newest first and includes their ordered steps.
func (s *Store) ListWorkflowJobs(ctx context.Context, options ListOptions) ([]WorkflowJob, error) {
	where, args := workflowJobWhere(options)
	args = append(args, listLimit(options.Limit), nonnegative(options.Offset))
	rows, err := s.db.QueryContext(ctx, `SELECT
		id, run_id, repository, name, status, conclusion, runner_name, runner_group,
		html_url, created_at, started_at, completed_at, updated_at, run_attempt,
		workflow_name, head_branch, head_sha, labels_json, runner_id, pool_name,
		local_session_id, attribution_source, attribution_confidence, api_url
		FROM workflow_jobs`+where+` ORDER BY created_at DESC LIMIT ? OFFSET ?`, args...)
	if err != nil {
		return nil, fmt.Errorf("list workflow jobs: %w", err)
	}
	var jobs []WorkflowJob
	for rows.Next() {
		var job WorkflowJob
		var created, updated int64
		var started, completed sql.NullInt64
		var labelsJSON string
		if err := rows.Scan(
			&job.ID, &job.RunID, &job.Repository, &job.Name, &job.Status,
			&job.Conclusion, &job.RunnerName, &job.RunnerGroup, &job.HTMLURL,
			&created, &started, &completed, &updated, &job.RunAttempt,
			&job.WorkflowName, &job.HeadBranch, &job.HeadSHA, &labelsJSON,
			&job.RunnerID, &job.PoolName, &job.LocalSessionID,
			&job.AttributionSource, &job.AttributionConfidence, &job.APIURL,
		); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan workflow job: %w", err)
		}
		job.CreatedAt = millisTime(created)
		job.UpdatedAt = millisTime(updated)
		job.StartedAt = pointerTime(started)
		job.CompletedAt = pointerTime(completed)
		if err := json.Unmarshal([]byte(labelsJSON), &job.Labels); err != nil {
			rows.Close()
			return nil, fmt.Errorf("decode workflow job labels: %w", err)
		}
		jobs = append(jobs, job)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	steps, err := s.workflowStepsForJobs(ctx, jobs)
	if err != nil {
		return nil, err
	}
	for index := range jobs {
		jobs[index].Steps = steps[workflowJobKey{
			repository: jobs[index].Repository,
			id:         jobs[index].ID,
		}]
	}
	return jobs, nil
}

func (s *Store) WorkflowJob(ctx context.Context, jobID int64) (WorkflowJob, error) {
	if jobID <= 0 {
		return WorkflowJob{}, errors.New("workflow job ID must be positive")
	}
	jobs, err := s.ListWorkflowJobs(ctx, ListOptions{JobID: jobID, Limit: 2})
	if err != nil {
		return WorkflowJob{}, err
	}
	switch len(jobs) {
	case 0:
		return WorkflowJob{}, ErrNotFound
	case 1:
		return jobs[0], nil
	default:
		return WorkflowJob{}, ErrAmbiguous
	}
}

const summaryCacheTTL = 30 * time.Second

type summaryCacheEntry struct {
	summary   Summary
	expiresAt time.Time
}

// Summary returns aggregate counts, optionally limited to one repository.
func (s *Store) Summary(ctx context.Context, repository string) (Summary, error) {
	now := nowUTC()
	s.summaryMu.RLock()
	cached, ok := s.summaryCache[repository]
	s.summaryMu.RUnlock()
	if ok && now.Before(cached.expiresAt) {
		return cached.summary, nil
	}

	s.summaryMu.Lock()
	defer s.summaryMu.Unlock()
	cached, ok = s.summaryCache[repository]
	if ok && now.Before(cached.expiresAt) {
		return cached.summary, nil
	}

	var summary Summary
	query := `SELECT
		(SELECT COUNT(*) FROM runner_sessions),
		(SELECT COUNT(*) FROM runner_sessions WHERE completed_at IS NULL),
		(SELECT COUNT(*) FROM workflow_runs),
		(SELECT COUNT(*) FROM workflow_jobs),
		(SELECT COUNT(*) FROM workflow_steps),
		(SELECT COUNT(*) FROM workflow_jobs WHERE conclusion='success'),
		(SELECT COUNT(*) FROM workflow_jobs WHERE conclusion='failure'),
		(SELECT COUNT(*) FROM workflow_jobs WHERE conclusion='cancelled'),
		(SELECT COALESCE(AVG((completed_at-started_at)/1000.0), 0)
			FROM workflow_jobs WHERE started_at IS NOT NULL AND completed_at IS NOT NULL),
		(SELECT COUNT(*) FROM webhook_deliveries)`
	var args []any
	if repository != "" {
		query = `SELECT
			(SELECT COUNT(*) FROM runner_sessions WHERE repository=?),
			(SELECT COUNT(*) FROM runner_sessions WHERE repository=? AND completed_at IS NULL),
			(SELECT COUNT(*) FROM workflow_runs WHERE repository=?),
			(SELECT COUNT(*) FROM workflow_jobs WHERE repository=?),
			(SELECT COUNT(*) FROM workflow_steps WHERE repository=?),
			(SELECT COUNT(*) FROM workflow_jobs WHERE repository=? AND conclusion='success'),
			(SELECT COUNT(*) FROM workflow_jobs WHERE repository=? AND conclusion='failure'),
			(SELECT COUNT(*) FROM workflow_jobs WHERE repository=? AND conclusion='cancelled'),
			(SELECT COALESCE(AVG((completed_at-started_at)/1000.0), 0)
				FROM workflow_jobs
				WHERE repository=? AND started_at IS NOT NULL AND completed_at IS NOT NULL),
			(SELECT COUNT(*) FROM webhook_deliveries WHERE repository=?)`
		for range 10 {
			args = append(args, repository)
		}
	}
	if err := s.db.QueryRowContext(ctx, query, args...).Scan(
		&summary.RunnerSessions, &summary.PendingSessions, &summary.WorkflowRuns,
		&summary.WorkflowJobs, &summary.WorkflowSteps, &summary.SuccessfulJobs,
		&summary.FailedJobs, &summary.CancelledJobs,
		&summary.AverageDurationSeconds, &summary.WebhookDeliveries,
	); err != nil {
		return Summary{}, fmt.Errorf("summarize history: %w", err)
	}
	s.summaryCache[repository] = summaryCacheEntry{
		summary: summary, expiresAt: now.Add(summaryCacheTTL),
	}
	return summary, nil
}

func (s *Store) invalidateSummaryCache() {
	s.summaryMu.Lock()
	clear(s.summaryCache)
	s.summaryMu.Unlock()
}

func (s *Store) ListAlertAnnotations(
	ctx context.Context, alertID string, limit, offset int,
) ([]alerts.Annotation, error) {
	limit = listLimit(limit)
	offset = nonnegative(offset)
	query := `SELECT id, alert_id, body, created_at, created_by, correlation_id
		FROM alert_annotations`
	var args []any
	if alertID != "" {
		query += ` WHERE alert_id=?`
		args = append(args, alertID)
	}
	query += ` ORDER BY created_at DESC LIMIT ? OFFSET ?`
	args = append(args, limit, offset)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list alert annotations: %w", err)
	}
	defer rows.Close()
	var result []alerts.Annotation
	for rows.Next() {
		var item alerts.Annotation
		var createdAt int64
		if err := rows.Scan(
			&item.ID, &item.AlertID, &item.Body, &createdAt,
			&item.CreatedBy, &item.CorrelationID,
		); err != nil {
			return nil, fmt.Errorf("scan alert annotation: %w", err)
		}
		item.CreatedAt = millisTime(createdAt)
		result = append(result, item)
	}
	return result, rows.Err()
}

func (s *Store) ListAuditRecords(
	ctx context.Context, limit, offset int,
) ([]AuditRecord, error) {
	limit = listLimit(limit)
	offset = nonnegative(offset)
	rows, err := s.db.QueryContext(ctx, `SELECT id, source, occurred_at,
		actor_kind, actor_id, action, target_type, target_id, correlation_id,
		outcome, payload_json FROM (
			SELECT id, 'command' AS source, occurred_at, actor_kind, actor_id,
				action, target_type, target_id, correlation_id, '' AS outcome,
				payload_json
			FROM audit_events
			UNION ALL
			SELECT id, 'access', occurred_at, actor_kind, actor_id, action,
				target_type, target_id, correlation_id, outcome, NULL AS payload_json
			FROM access_audit_events
			UNION ALL
			SELECT id, 'alert', occurred_at, actor_kind, actor_id, action,
				'alert', alert_id, correlation_id, '', payload_json
			FROM alert_audit_events
		) ORDER BY occurred_at DESC LIMIT ? OFFSET ?`, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("list audit records: %w", err)
	}
	defer rows.Close()
	var result []AuditRecord
	for rows.Next() {
		var item AuditRecord
		var occurredAt int64
		var payload []byte
		if err := rows.Scan(
			&item.ID, &item.Source, &occurredAt, &item.ActorKind, &item.ActorID,
			&item.Action, &item.TargetType, &item.TargetID, &item.CorrelationID,
			&item.Outcome, &payload,
		); err != nil {
			return nil, fmt.Errorf("scan audit record: %w", err)
		}
		if len(payload) > 0 {
			item.Payload = json.RawMessage(payload)
		}
		item.OccurredAt = millisTime(occurredAt)
		result = append(result, item)
	}
	return result, rows.Err()
}

// PruneBefore removes completed history older than cutoff in one transaction.
func (s *Store) PruneBefore(ctx context.Context, cutoff time.Time) (PruneResult, error) {
	if cutoff.IsZero() {
		return PruneResult{}, errors.New("prune cutoff is required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return PruneResult{}, fmt.Errorf("begin history prune: %w", err)
	}
	defer tx.Rollback()
	var result PruneResult
	cutoffMillis := timeMillis(cutoff)
	for _, count := range []struct {
		query string
		dest  *int64
	}{
		{`SELECT COUNT(*) FROM workflow_steps WHERE (repository, job_id) IN (
			SELECT repository, id FROM workflow_jobs WHERE completed_at IS NOT NULL AND completed_at < ?
		)`, &result.WorkflowSteps},
		{`SELECT COUNT(*) FROM workflow_jobs WHERE completed_at IS NOT NULL AND completed_at < ?`, &result.WorkflowJobs},
		{`SELECT COUNT(*) FROM workflow_runs WHERE completed_at IS NOT NULL AND completed_at < ?`, &result.WorkflowRuns},
	} {
		if err := tx.QueryRowContext(ctx, count.query, cutoffMillis).Scan(count.dest); err != nil {
			return PruneResult{}, fmt.Errorf("count history prune: %w", err)
		}
	}
	deletes := []struct {
		query string
		dest  *int64
	}{
		{`DELETE FROM runner_sessions WHERE completed_at IS NOT NULL AND completed_at < ?`, &result.RunnerSessions},
		{`DELETE FROM workflow_runs WHERE completed_at IS NOT NULL AND completed_at < ?`, nil},
		{`DELETE FROM webhook_deliveries WHERE received_at < ?`, &result.WebhookDeliveries},
	}
	for _, deletion := range deletes {
		execResult, err := tx.ExecContext(ctx, deletion.query, cutoffMillis)
		if err != nil {
			return PruneResult{}, fmt.Errorf("prune history: %w", err)
		}
		if deletion.dest != nil {
			if *deletion.dest, err = execResult.RowsAffected(); err != nil {
				return PruneResult{}, fmt.Errorf("count pruned history: %w", err)
			}
		}
	}
	if err := advanceAnalyticsVersionTx(ctx, tx); err != nil {
		return PruneResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return PruneResult{}, fmt.Errorf("commit history prune: %w", err)
	}
	s.invalidateSummaryCache()
	return result, nil
}

const runnerSessionSelect = `SELECT
	id, runner_name, pool_name, repository, workflow_run_id, workflow_job_id,
	status, conclusion, started_at, completed_at, updated_at, error, target,
	run_attempt, github_registration_id, backend_id, attribution_source,
	attribution_confidence, planned_at, registered_at, launched_at, exit_code
	FROM runner_sessions`

func scanRunnerSessions(rows *sql.Rows) ([]RunnerSession, error) {
	defer rows.Close()
	var result []RunnerSession
	for rows.Next() {
		var session RunnerSession
		var started, updated, planned int64
		var completed, registered, launched sql.NullInt64
		if err := rows.Scan(
			&session.ID, &session.RunnerName, &session.PoolName, &session.Repository,
			&session.WorkflowRunID, &session.WorkflowJobID, &session.Status,
			&session.Conclusion, &started, &completed, &updated, &session.Error,
			&session.Target, &session.RunAttempt, &session.GitHubRegistrationID,
			&session.BackendID, &session.AttributionSource,
			&session.AttributionConfidence, &planned, &registered, &launched,
			&session.ExitCode,
		); err != nil {
			return nil, fmt.Errorf("scan runner session: %w", err)
		}
		session.StartedAt = millisTime(started)
		session.CompletedAt = pointerTime(completed)
		session.UpdatedAt = millisTime(updated)
		session.PlannedAt = millisTime(planned)
		session.RegisteredAt = pointerTime(registered)
		session.LaunchedAt = pointerTime(launched)
		result = append(result, session)
	}
	return result, rows.Err()
}

type workflowJobKey struct {
	repository string
	id         int64
}

func (s *Store) workflowStepsForJobs(
	ctx context.Context, jobs []WorkflowJob,
) (map[workflowJobKey][]WorkflowStep, error) {
	result := make(map[workflowJobKey][]WorkflowStep, len(jobs))
	if len(jobs) == 0 {
		return result, nil
	}
	query, args := workflowStepsBatchQuery(jobs)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list workflow steps batch: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var key workflowJobKey
		var step WorkflowStep
		var started, completed sql.NullInt64
		if err := rows.Scan(
			&key.repository, &key.id, &step.Number, &step.Name, &step.Status,
			&step.Conclusion, &started, &completed,
		); err != nil {
			return nil, fmt.Errorf("scan workflow step batch: %w", err)
		}
		step.StartedAt = pointerTime(started)
		step.CompletedAt = pointerTime(completed)
		result[key] = append(result[key], step)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list workflow steps batch: %w", err)
	}
	return result, nil
}

func workflowStepsBatchQuery(jobs []WorkflowJob) (string, []any) {
	var query strings.Builder
	query.WriteString(`WITH requested(repository, job_id) AS (VALUES `)
	args := make([]any, 0, len(jobs)*2)
	for index, job := range jobs {
		if index > 0 {
			query.WriteByte(',')
		}
		query.WriteString("(?, ?)")
		args = append(args, job.Repository, job.ID)
	}
	query.WriteString(`)
		SELECT step.repository, step.job_id, step.number, step.name, step.status,
			step.conclusion, step.started_at, step.completed_at
		FROM workflow_steps step
		INNER JOIN requested
			ON requested.repository=step.repository AND requested.job_id=step.job_id
		ORDER BY step.repository, step.job_id, step.number`)
	return query.String(), args
}

func runnerSessionWhere(options ListOptions) (string, []any) {
	var clauses []string
	var args []any
	if options.Repository != "" {
		clauses = append(clauses, "repository = ?")
		args = append(args, options.Repository)
	}
	if options.RunID > 0 {
		clauses = append(clauses, "workflow_run_id = ?")
		args = append(args, options.RunID)
	}
	if options.Status != "" {
		clauses = append(clauses, "status = ?")
		args = append(args, options.Status)
	}
	if options.Pool != "" {
		clauses = append(clauses, "pool_name = ?")
		args = append(args, options.Pool)
	}
	if options.Attribution != "" {
		clauses = append(clauses, "attribution_source = ?")
		args = append(args, options.Attribution)
	}
	if options.Query != "" {
		clauses = append(clauses, "(runner_name LIKE ? OR workflow_job_id = ? OR workflow_run_id = ?)")
		like := "%" + options.Query + "%"
		args = append(args, like, options.Query, options.Query)
	}
	addTimeClauses(&clauses, &args, "started_at", options)
	return whereClause(clauses), args
}

func workflowRunWhere(options ListOptions) (string, []any) {
	var clauses []string
	var args []any
	addCommonClauses(&clauses, &args, options)
	if options.RunID > 0 {
		clauses = append(clauses, "id = ?")
		args = append(args, options.RunID)
	}
	if options.Workflow != "" {
		clauses = append(clauses, "(workflow_name = ? OR name = ?)")
		args = append(args, options.Workflow, options.Workflow)
	}
	if options.Branch != "" {
		clauses = append(clauses, "head_branch = ?")
		args = append(args, options.Branch)
	}
	if options.Actor != "" {
		clauses = append(clauses, "(actor = ? OR triggering_actor = ?)")
		args = append(args, options.Actor, options.Actor)
	}
	if options.Query != "" {
		clauses = append(clauses, "(name LIKE ? OR workflow_name LIKE ? OR head_sha LIKE ?)")
		like := "%" + options.Query + "%"
		args = append(args, like, like, like)
	}
	addTimeClauses(&clauses, &args, "created_at", options)
	return whereClause(clauses), args
}

func workflowJobWhere(options ListOptions) (string, []any) {
	var clauses []string
	var args []any
	addCommonClauses(&clauses, &args, options)
	if options.RunID > 0 {
		clauses = append(clauses, "run_id = ?")
		args = append(args, options.RunID)
	}
	if options.JobID > 0 {
		clauses = append(clauses, "id = ?")
		args = append(args, options.JobID)
	}
	if options.Workflow != "" {
		clauses = append(clauses, "workflow_name = ?")
		args = append(args, options.Workflow)
	}
	if options.Job != "" {
		clauses = append(clauses, "name = ?")
		args = append(args, options.Job)
	}
	if options.Pool != "" {
		clauses = append(clauses, "pool_name = ?")
		args = append(args, options.Pool)
	}
	if options.Branch != "" {
		clauses = append(clauses, "head_branch = ?")
		args = append(args, options.Branch)
	}
	if options.Attribution != "" {
		clauses = append(clauses, "attribution_source = ?")
		args = append(args, options.Attribution)
	}
	if options.Query != "" {
		clauses = append(clauses, "(name LIKE ? OR workflow_name LIKE ? OR head_sha LIKE ? OR runner_name LIKE ?)")
		like := "%" + options.Query + "%"
		args = append(args, like, like, like, like)
	}
	addTimeClauses(&clauses, &args, "created_at", options)
	return whereClause(clauses), args
}

func addCommonClauses(clauses *[]string, args *[]any, options ListOptions) {
	if options.Repository != "" {
		*clauses = append(*clauses, "repository = ?")
		*args = append(*args, options.Repository)
	}
	if options.Status != "" {
		*clauses = append(*clauses, "status = ?")
		*args = append(*args, options.Status)
	}
	if options.Conclusion != "" {
		*clauses = append(*clauses, "conclusion = ?")
		*args = append(*args, options.Conclusion)
	}
}

func addTimeClauses(clauses *[]string, args *[]any, column string, options ListOptions) {
	if options.Since != nil && !options.Since.IsZero() {
		*clauses = append(*clauses, column+" >= ?")
		*args = append(*args, timeMillis(*options.Since))
	}
	if options.Until != nil && !options.Until.IsZero() {
		*clauses = append(*clauses, column+" <= ?")
		*args = append(*args, timeMillis(*options.Until))
	}
}

func whereClause(clauses []string) string {
	if len(clauses) == 0 {
		return ""
	}
	return " WHERE " + strings.Join(clauses, " AND ")
}

func addCursorClause(
	where string, args []any, column string, options ListOptions, numericID bool,
) (string, []any) {
	if options.AfterTime == nil {
		return where, args
	}
	idArg := any(options.AfterID)
	if numericID {
		id, err := strconv.ParseInt(options.AfterID, 10, 64)
		if err != nil {
			id = 0
		}
		idArg = id
	}
	clause := `(` + column + ` < ? OR (` + column + ` = ? AND id < ?))`
	if where == "" {
		where = " WHERE " + clause
	} else {
		where += " AND " + clause
	}
	value := timeMillis(*options.AfterTime)
	return where, append(args, value, value, idArg)
}

func listLimit(limit int) int {
	if limit <= 0 {
		return 100
	}
	if limit > 1000 {
		return 1000
	}
	return limit
}

func nonnegative(value int) int {
	if value < 0 {
		return 0
	}
	return value
}
