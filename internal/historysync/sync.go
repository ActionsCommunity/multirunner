// Package historysync backfills and reconciles GitHub Actions history.
package historysync

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	ghapi "github.com/GerardSmit/multirunner/internal/github"
	"github.com/GerardSmit/multirunner/internal/history"
)

const (
	perPage             = 100
	apiResultCap        = 1000
	defaultShard        = 7 * 24 * time.Hour
	defaultOverlap      = 6 * time.Hour
	defaultRateRetries  = 3
	cursorVersion       = 1
	defaultActionsStart = "2019-11-13T00:00:00Z"
)

// API is the repo-scoped subset of the GitHub history API used by Syncer.
type API interface {
	ListRepositoryWorkflowRuns(context.Context, ghapi.WorkflowRunListOptions) ([]ghapi.WorkflowRun, ghapi.ResponseMetadata, error)
	ListWorkflowJobs(context.Context, int64, ghapi.PageOptions) ([]ghapi.WorkflowJob, ghapi.ResponseMetadata, error)
}

// Store is the history persistence subset used by Syncer.
type Store interface {
	UpsertWorkflowRun(context.Context, history.WorkflowRun) error
	UpsertWorkflowJob(context.Context, history.WorkflowJob) error
	ListRunnerSessions(context.Context, history.ListOptions) ([]history.RunnerSession, error)
	SetSyncState(context.Context, history.SyncState) error
	SyncState(context.Context, string) (history.SyncState, error)
}

// Observer receives non-blocking synchronization telemetry.
type Observer interface {
	ObserveHistoryRepository(string)
	ObserveHistoryAPIRequest(string, int)
	ObserveHistorySyncSuccess(string, history.SyncState)
	ObserveHistorySyncError(string)
}

// Repository binds a configured repository name to its repo-scoped API.
type Repository struct {
	Name string
	API  API
}

// Options controls backfill and reconciliation. Zero values use conservative
// sequential defaults.
type Options struct {
	Backfill            bool
	LegacyPrefixes      []string
	StartTime           time.Time
	ShardDuration       time.Duration
	Overlap             time.Duration
	MaxRateLimitRetries int
	Now                 func() time.Time
	Sleep               func(context.Context, time.Duration) error
	Observer            Observer
}

// Report describes one best-effort synchronization pass.
type Report struct {
	Repositories []RepositoryReport
	Degraded     bool
}

// RepositoryReport describes one repository synchronization.
type RepositoryReport struct {
	Repository   string
	Runs         int
	Jobs         int
	ExactJobs    int
	InferredJobs int
	ExpiredRuns  int
	Error        error
}

// Syncer synchronizes repositories sequentially.
type Syncer struct {
	store Store
	repos []Repository
	opts  Options
}

// New constructs a synchronizer.
func New(store Store, repositories []Repository, options Options) (*Syncer, error) {
	if store == nil {
		return nil, errors.New("history sync store is required")
	}
	if options.ShardDuration == 0 {
		options.ShardDuration = defaultShard
	}
	if options.ShardDuration < time.Second {
		return nil, errors.New("history sync shard duration must be at least one second")
	}
	if options.Overlap == 0 {
		options.Overlap = defaultOverlap
	}
	if options.Overlap < 0 {
		return nil, errors.New("history sync overlap cannot be negative")
	}
	if options.MaxRateLimitRetries == 0 {
		options.MaxRateLimitRetries = defaultRateRetries
	}
	if options.MaxRateLimitRetries < 0 {
		return nil, errors.New("history sync rate-limit retries cannot be negative")
	}
	if options.Now == nil {
		options.Now = func() time.Time { return time.Now().UTC() }
	}
	if options.StartTime.IsZero() {
		options.StartTime, _ = time.Parse(time.RFC3339, defaultActionsStart)
	}
	if options.Sleep == nil {
		options.Sleep = sleepContext
	}
	for i := range repositories {
		repositories[i].Name = strings.TrimSpace(repositories[i].Name)
		if repositories[i].Name == "" || repositories[i].API == nil {
			return nil, fmt.Errorf("history sync repository %d requires a name and API", i)
		}
		if options.Observer != nil {
			options.Observer.ObserveHistoryRepository(repositories[i].Name)
		}
	}
	return &Syncer{store: store, repos: append([]Repository(nil), repositories...), opts: options}, nil
}

// Sync performs one sequential best-effort pass. Repository failures are
// reported as degraded synchronization; context cancellation is returned.
func (s *Syncer) Sync(ctx context.Context) (Report, error) {
	report := Report{Repositories: make([]RepositoryReport, 0, len(s.repos))}
	for _, repo := range s.repos {
		if err := ctx.Err(); err != nil {
			return report, err
		}
		item := RepositoryReport{Repository: repo.Name}
		if err := s.syncRepository(ctx, repo, &item); err != nil {
			if ctx.Err() != nil {
				return report, ctx.Err()
			}
			item.Error = err
			report.Degraded = true
			if s.opts.Observer != nil {
				s.opts.Observer.ObserveHistorySyncError(repo.Name)
			}
		}
		report.Repositories = append(report.Repositories, item)
	}
	return report, nil
}

type syncCursor struct {
	Version int       `json:"version"`
	Phase   string    `json:"phase"`
	Next    time.Time `json:"next,omitempty"`
	Through time.Time `json:"through,omitempty"`
}

func (s *Syncer) syncRepository(ctx context.Context, repo Repository, report *RepositoryReport) error {
	sessions, err := s.runnerSessions(ctx, repo.Name)
	if err != nil {
		return err
	}
	cursor, found, err := s.loadCursor(ctx, repo.Name)
	if err != nil {
		return err
	}
	now := s.opts.Now().UTC().Truncate(time.Second)
	if !found {
		if s.opts.Backfill {
			cursor = syncCursor{Version: cursorVersion, Phase: "backfill", Next: now}
		} else {
			cursor = syncCursor{Version: cursorVersion, Phase: "reconcile", Through: now}
		}
	}

	if cursor.Phase == "backfill" {
		start := s.opts.StartTime.UTC().Truncate(time.Second)
		for !cursor.Next.Before(start) {
			to := cursor.Next
			from := to.Add(-s.opts.ShardDuration + time.Second)
			if from.Before(start) {
				from = start
			}
			if err := s.syncRange(ctx, repo, from, to, sessions, report); err != nil {
				return err
			}
			cursor.Next = from.Add(-time.Second)
			if err := s.saveCursor(ctx, repo.Name, cursor); err != nil {
				return err
			}
		}
		cursor.Phase = "reconcile"
		cursor.Through = now
		cursor.Next = time.Time{}
		return s.saveCursor(ctx, repo.Name, cursor)
	}

	from := cursor.Through.Add(-s.opts.Overlap)
	if cursor.Through.IsZero() {
		from = now.Add(-s.opts.Overlap)
	}
	if from.After(now) {
		from = now
	}
	if err := s.syncRange(ctx, repo, from, now, sessions, report); err != nil {
		return err
	}
	cursor.Phase = "reconcile"
	cursor.Through = now
	cursor.Next = time.Time{}
	return s.saveCursor(ctx, repo.Name, cursor)
}

func (s *Syncer) syncRange(ctx context.Context, repo Repository, from, to time.Time, sessions sessionIndex, report *RepositoryReport) error {
	runs, capped, err := s.workflowRuns(ctx, repo, from, to)
	if err != nil {
		return err
	}
	if capped {
		leftTo, rightFrom, ok := splitRange(from, to)
		if !ok {
			return fmt.Errorf("created range %s..%s reached GitHub's %d-result cap and cannot be split further",
				from.Format(time.RFC3339), to.Format(time.RFC3339), apiResultCap)
		}
		if err := s.syncRange(ctx, repo, from, leftTo, sessions, report); err != nil {
			return err
		}
		return s.syncRange(ctx, repo, rightFrom, to, sessions, report)
	}
	for _, run := range runs {
		jobs, expired, err := s.workflowJobs(ctx, repo, run.ID)
		if err != nil {
			return err
		}
		if expired {
			report.ExpiredRuns++
			continue
		}
		retained := make([]history.WorkflowJob, 0, len(jobs))
		for _, job := range jobs {
			attribution := sessions.match(job)
			if attribution.kind == attributionNone && hasLegacyPrefix(job.RunnerName, s.opts.LegacyPrefixes) {
				attribution = jobAttribution{
					kind: attributionInferred, source: "inferred_legacy_prefix", confidence: 50,
				}
			}
			if attribution.kind == attributionNone {
				continue
			}
			if attribution.kind == attributionExact {
				report.ExactJobs++
			} else {
				report.InferredJobs++
			}
			retained = append(retained, historyJob(repo.Name, job, attribution))
		}
		if len(retained) == 0 {
			continue
		}
		if err := s.store.UpsertWorkflowRun(ctx, historyRun(repo.Name, run)); err != nil {
			return err
		}
		report.Runs++
		for _, job := range retained {
			if err := s.store.UpsertWorkflowJob(ctx, job); err != nil {
				return err
			}
			report.Jobs++
		}
	}
	return nil
}

func (s *Syncer) workflowRuns(ctx context.Context, repo Repository, from, to time.Time) ([]ghapi.WorkflowRun, bool, error) {
	var result []ghapi.WorkflowRun
	for page := 1; page <= apiResultCap/perPage; page++ {
		var (
			items []ghapi.WorkflowRun
			meta  ghapi.ResponseMetadata
			err   error
		)
		for attempt := 0; ; attempt++ {
			items, meta, err = repo.API.ListRepositoryWorkflowRuns(ctx, ghapi.WorkflowRunListOptions{
				CreatedFrom: from, CreatedTo: to,
				PageOptions: ghapi.PageOptions{Page: page, PerPage: perPage},
			})
			s.observeAPIRequest(repo.Name, meta.StatusCode)
			retry, waitErr := s.handleRateLimit(ctx, meta, err, attempt)
			if waitErr != nil {
				return nil, false, waitErr
			}
			if !retry {
				break
			}
		}
		if err != nil {
			return nil, false, err
		}
		result = append(result, items...)
		if len(result) >= apiResultCap {
			return result, true, nil
		}
		if meta.NextPage == 0 || len(items) < perPage {
			return result, false, nil
		}
	}
	return result, len(result) >= apiResultCap, nil
}

func (s *Syncer) workflowJobs(ctx context.Context, repo Repository, runID int64) ([]ghapi.WorkflowJob, bool, error) {
	var result []ghapi.WorkflowJob
	for page := 1; ; page++ {
		var (
			items []ghapi.WorkflowJob
			meta  ghapi.ResponseMetadata
			err   error
		)
		for attempt := 0; ; attempt++ {
			items, meta, err = repo.API.ListWorkflowJobs(ctx, runID, ghapi.PageOptions{Page: page, PerPage: perPage})
			s.observeAPIRequest(repo.Name, meta.StatusCode)
			if err != nil && meta.StatusCode == http.StatusNotFound {
				return nil, true, nil
			}
			retry, waitErr := s.handleRateLimit(ctx, meta, err, attempt)
			if waitErr != nil {
				return nil, false, waitErr
			}
			if !retry {
				break
			}
		}
		if err != nil {
			return nil, false, err
		}
		result = append(result, items...)
		if meta.NextPage == 0 || len(items) < perPage {
			return result, false, nil
		}
	}
}

func (s *Syncer) handleRateLimit(ctx context.Context, meta ghapi.ResponseMetadata, callErr error, attempt int) (bool, error) {
	rateResponse := meta.StatusCode == http.StatusForbidden || meta.StatusCode == http.StatusTooManyRequests
	if callErr != nil && rateResponse && meta.RetryAfter > 0 {
		if err := s.opts.Sleep(ctx, meta.RetryAfter); err != nil {
			return false, err
		}
		return attempt < s.opts.MaxRateLimitRetries, nil
	}
	exhausted := meta.RateLimit.Remaining == 0 && !meta.RateLimit.Reset.IsZero()
	if !exhausted {
		return false, nil
	}
	delay := meta.RateLimit.Reset.Sub(s.opts.Now())
	if delay < 0 {
		delay = 0
	}
	if delay > 0 {
		if err := s.opts.Sleep(ctx, delay); err != nil {
			return false, err
		}
	}
	if callErr == nil {
		return false, nil
	}
	return rateResponse && attempt < s.opts.MaxRateLimitRetries, nil
}

func (s *Syncer) runnerSessions(ctx context.Context, repository string) (sessionIndex, error) {
	index := sessionIndex{
		jobIDs: map[int64]history.RunnerSession{}, runnerNames: map[string]history.RunnerSession{},
	}
	var afterTime *time.Time
	var afterID string
	for {
		items, err := s.store.ListRunnerSessions(ctx, history.ListOptions{
			Repository: repository, Limit: 1000,
			AfterTime: afterTime, AfterID: afterID,
		})
		if err != nil {
			return sessionIndex{}, err
		}
		for _, item := range items {
			if item.WorkflowJobID != 0 {
				index.jobIDs[item.WorkflowJobID] = item
			}
			if item.RunnerName != "" {
				index.runnerNames[item.RunnerName] = item
			}
		}
		if len(items) < 1000 {
			return index, nil
		}
		last := items[len(items)-1]
		afterTime, afterID = &last.StartedAt, last.ID
	}
}

func (s *Syncer) loadCursor(ctx context.Context, repository string) (syncCursor, bool, error) {
	state, err := s.store.SyncState(ctx, cursorKey(repository))
	if errors.Is(err, history.ErrNotFound) {
		return syncCursor{}, false, nil
	}
	if err != nil {
		return syncCursor{}, false, err
	}
	var cursor syncCursor
	if err := json.Unmarshal([]byte(state.Cursor), &cursor); err != nil {
		return syncCursor{}, false, fmt.Errorf("decode history sync cursor for %s: %w", repository, err)
	}
	if cursor.Version != cursorVersion || (cursor.Phase != "backfill" && cursor.Phase != "reconcile") {
		return syncCursor{}, false, fmt.Errorf("unsupported history sync cursor for %s", repository)
	}
	if s.opts.Observer != nil {
		s.opts.Observer.ObserveHistorySyncSuccess(repository, state)
	}
	return cursor, true, nil
}

func (s *Syncer) saveCursor(ctx context.Context, repository string, cursor syncCursor) error {
	data, err := json.Marshal(cursor)
	if err != nil {
		return err
	}
	state := history.SyncState{
		Key: cursorKey(repository), Repository: repository, Phase: cursor.Phase,
		Cursor: string(data), LastSuccessAt: timePointer(s.opts.Now().UTC()),
		BackfillComplete: cursor.Phase == "reconcile", UpdatedAt: s.opts.Now().UTC(),
	}
	if err := s.store.SetSyncState(ctx, state); err != nil {
		return err
	}
	if s.opts.Observer != nil {
		s.opts.Observer.ObserveHistorySyncSuccess(repository, state)
	}
	return nil
}

func (s *Syncer) observeAPIRequest(repository string, status int) {
	if s.opts.Observer != nil {
		s.opts.Observer.ObserveHistoryAPIRequest(repository, status)
	}
}

func cursorKey(repository string) string { return "historysync:" + repository }

type attribution int

const (
	attributionNone attribution = iota
	attributionExact
	attributionInferred
)

type sessionIndex struct {
	jobIDs      map[int64]history.RunnerSession
	runnerNames map[string]history.RunnerSession
}

type jobAttribution struct {
	kind       attribution
	source     string
	confidence int
	session    history.RunnerSession
}

func (s sessionIndex) match(job ghapi.WorkflowJob) jobAttribution {
	if session, ok := s.jobIDs[job.ID]; ok {
		return jobAttribution{
			kind: attributionExact, source: "exact_job_id", confidence: 100, session: session,
		}
	}
	if job.RunnerName != "" {
		if session, ok := s.runnerNames[job.RunnerName]; ok {
			return jobAttribution{
				kind: attributionExact, source: "exact_runner_name", confidence: 100, session: session,
			}
		}
	}
	return jobAttribution{}
}

func hasLegacyPrefix(runnerName string, prefixes []string) bool {
	if runnerName == "" {
		return false
	}
	for _, prefix := range prefixes {
		if prefix != "" && strings.HasPrefix(runnerName, prefix) {
			return true
		}
	}
	return false
}

func splitRange(from, to time.Time) (time.Time, time.Time, bool) {
	from = from.UTC().Truncate(time.Second)
	to = to.UTC().Truncate(time.Second)
	if !to.After(from) {
		return time.Time{}, time.Time{}, false
	}
	seconds := int64(to.Sub(from) / time.Second)
	leftTo := from.Add(time.Duration(seconds/2) * time.Second)
	rightFrom := leftTo.Add(time.Second)
	if rightFrom.After(to) {
		return time.Time{}, time.Time{}, false
	}
	return leftTo, rightFrom, true
}

func historyRun(repository string, run ghapi.WorkflowRun) history.WorkflowRun {
	out := history.WorkflowRun{
		ID: run.ID, Repository: repository, Name: run.DisplayTitle,
		WorkflowName: run.Name, WorkflowID: run.WorkflowID, WorkflowPath: run.WorkflowPath,
		DisplayTitle: run.DisplayTitle, Event: run.Event, Status: run.Status,
		Conclusion: run.Conclusion, HeadBranch: run.HeadBranch, HeadSHA: run.HeadSHA,
		Actor: run.Actor, TriggeringActor: run.TriggeringActor, HTMLURL: run.HTMLURL,
		RunNumber:  int64(run.RunNumber),
		RunAttempt: run.RunAttempt, CreatedAt: run.CreatedAt, UpdatedAt: run.UpdatedAt,
	}
	if !run.RunStartedAt.IsZero() {
		value := run.RunStartedAt
		out.StartedAt = &value
	}
	if run.Status == "completed" && !run.UpdatedAt.IsZero() {
		value := run.UpdatedAt
		out.CompletedAt = &value
	}
	return out
}

func historyJob(repository string, job ghapi.WorkflowJob, attribution jobAttribution) history.WorkflowJob {
	updated := job.CompletedAt
	if updated.IsZero() {
		updated = job.StartedAt
	}
	if updated.IsZero() {
		updated = job.CreatedAt
	}
	out := history.WorkflowJob{
		ID: job.ID, RunID: job.RunID, Repository: repository, Name: job.Name,
		RunAttempt: int(job.RunAttempt), WorkflowName: job.WorkflowName,
		HeadBranch: job.HeadBranch, HeadSHA: job.HeadSHA,
		Status: job.Status, Conclusion: job.Conclusion, Labels: append([]string(nil), job.Labels...),
		RunnerID: job.RunnerID, RunnerName: job.RunnerName,
		RunnerGroup: job.RunnerGroupName, PoolName: attribution.session.PoolName,
		LocalSessionID: attribution.session.ID, AttributionSource: attribution.source,
		AttributionConfidence: attribution.confidence, HTMLURL: job.HTMLURL,
		CreatedAt: job.CreatedAt, UpdatedAt: updated,
		Steps: make([]history.WorkflowStep, 0, len(job.Steps)),
	}

	if !job.StartedAt.IsZero() {
		value := job.StartedAt
		out.StartedAt = &value
	}
	if !job.CompletedAt.IsZero() {
		value := job.CompletedAt
		out.CompletedAt = &value
	}
	for _, step := range job.Steps {
		item := history.WorkflowStep{
			Number: int(step.Number), Name: step.Name, Status: step.Status,
			Conclusion: step.Conclusion,
		}
		if !step.StartedAt.IsZero() {
			value := step.StartedAt
			item.StartedAt = &value
		}
		if !step.CompletedAt.IsZero() {
			value := step.CompletedAt
			item.CompletedAt = &value
		}
		out.Steps = append(out.Steps, item)
	}
	return out
}

func timePointer(value time.Time) *time.Time { return &value }

func sleepContext(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
