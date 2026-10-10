package historysync

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"testing"
	"time"

	ghapi "github.com/GerardSmit/multirunner/internal/github"
	"github.com/GerardSmit/multirunner/internal/history"
)

var testNow = time.Date(2026, 10, 8, 1, 0, 0, 0, time.UTC)

type fakeAPI struct {
	listRuns func(context.Context, ghapi.WorkflowRunListOptions) ([]ghapi.WorkflowRun, ghapi.ResponseMetadata, error)
	listJobs func(context.Context, int64, ghapi.PageOptions) ([]ghapi.WorkflowJob, ghapi.ResponseMetadata, error)
	runCalls []ghapi.WorkflowRunListOptions
	jobCalls []jobCall
}

type jobCall struct {
	runID int64
	opts  ghapi.PageOptions
}

type fakeObserver struct {
	repositories []string
	requests     []apiRequest
	successes    []history.SyncState
	errors       []string
}

type apiRequest struct {
	repository string
	status     int
}

func (o *fakeObserver) ObserveHistoryRepository(repository string) {
	o.repositories = append(o.repositories, repository)
}

func (o *fakeObserver) ObserveHistoryAPIRequest(repository string, status int) {
	o.requests = append(o.requests, apiRequest{repository: repository, status: status})
}

func (o *fakeObserver) ObserveHistorySyncSuccess(_ string, state history.SyncState) {
	o.successes = append(o.successes, state)
}

func (o *fakeObserver) ObserveHistorySyncError(repository string) {
	o.errors = append(o.errors, repository)
}

func (f *fakeAPI) ListRepositoryWorkflowRuns(ctx context.Context, opts ghapi.WorkflowRunListOptions) ([]ghapi.WorkflowRun, ghapi.ResponseMetadata, error) {
	f.runCalls = append(f.runCalls, opts)
	if f.listRuns == nil {
		return nil, ghapi.ResponseMetadata{}, nil
	}
	return f.listRuns(ctx, opts)
}

func (f *fakeAPI) ListWorkflowJobs(ctx context.Context, runID int64, opts ghapi.PageOptions) ([]ghapi.WorkflowJob, ghapi.ResponseMetadata, error) {
	f.jobCalls = append(f.jobCalls, jobCall{runID: runID, opts: opts})
	if f.listJobs == nil {
		return nil, ghapi.ResponseMetadata{}, nil
	}
	return f.listJobs(ctx, runID, opts)
}

type fakeStore struct {
	runs     map[string]history.WorkflowRun
	jobs     map[string]history.WorkflowJob
	sessions []history.RunnerSession
	states   map[string]history.SyncState
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		runs: map[string]history.WorkflowRun{}, jobs: map[string]history.WorkflowJob{},
		states: map[string]history.SyncState{},
	}
}

func (s *fakeStore) UpsertWorkflowRun(_ context.Context, run history.WorkflowRun) error {
	s.runs[fmt.Sprintf("%s:%d", run.Repository, run.ID)] = run
	return nil
}

func (s *fakeStore) UpsertWorkflowJob(_ context.Context, job history.WorkflowJob) error {
	s.jobs[fmt.Sprintf("%s:%d", job.Repository, job.ID)] = job
	return nil
}

func (s *fakeStore) ListRunnerSessions(_ context.Context, opts history.ListOptions) ([]history.RunnerSession, error) {
	var filtered []history.RunnerSession
	for _, session := range s.sessions {
		if opts.Repository == "" || session.Repository == opts.Repository {
			filtered = append(filtered, session)
		}
	}
	start := opts.Offset
	if start > len(filtered) {
		start = len(filtered)
	}
	end := start + opts.Limit
	if end > len(filtered) {
		end = len(filtered)
	}
	return append([]history.RunnerSession(nil), filtered[start:end]...), nil
}

func (s *fakeStore) SetSyncState(_ context.Context, state history.SyncState) error {
	s.states[state.Key] = state
	return nil
}

func (s *fakeStore) SyncState(_ context.Context, key string) (history.SyncState, error) {
	state, ok := s.states[key]
	if !ok {
		return history.SyncState{}, history.ErrNotFound
	}
	return state, nil
}

func TestPaginationUsesHundredPerPageForRunsAndAllAttemptJobs(t *testing.T) {
	store := newFakeStore()
	store.sessions = []history.RunnerSession{{Repository: "o/r", WorkflowJobID: 501}}
	api := &fakeAPI{}
	api.listRuns = func(_ context.Context, opts ghapi.WorkflowRunListOptions) ([]ghapi.WorkflowRun, ghapi.ResponseMetadata, error) {
		switch opts.Page {
		case 1:
			runs := make([]ghapi.WorkflowRun, perPage)
			for i := range runs {
				runs[i] = workflowRun(int64(i + 1))
			}
			return runs, ghapi.ResponseMetadata{NextPage: 2}, nil
		case 2:
			return []ghapi.WorkflowRun{workflowRun(501)}, ghapi.ResponseMetadata{}, nil
		default:
			t.Fatalf("unexpected runs page %d", opts.Page)
			return nil, ghapi.ResponseMetadata{}, nil
		}
	}
	api.listJobs = func(_ context.Context, runID int64, opts ghapi.PageOptions) ([]ghapi.WorkflowJob, ghapi.ResponseMetadata, error) {
		if runID != 501 {
			return nil, ghapi.ResponseMetadata{}, nil
		}
		if opts.Page == 1 {
			jobs := make([]ghapi.WorkflowJob, perPage)
			for i := range jobs {
				jobs[i] = workflowJob(int64(1000+i), runID, "other")
			}
			return jobs, ghapi.ResponseMetadata{NextPage: 2}, nil
		}
		return []ghapi.WorkflowJob{workflowJob(501, runID, "runner")}, ghapi.ResponseMetadata{}, nil
	}

	syncer := newTestSyncer(t, store, api, Options{})
	report, err := syncer.Sync(t.Context())
	if err != nil || report.Degraded {
		t.Fatalf("Sync = %+v, %v", report, err)
	}
	if len(api.runCalls) != 2 || api.runCalls[0].PerPage != 100 || api.runCalls[1].Page != 2 {
		t.Fatalf("run calls = %+v", api.runCalls)
	}
	if len(api.jobCalls) != 102 {
		t.Fatalf("job calls = %d, want 102", len(api.jobCalls))
	}
	lastTwo := api.jobCalls[len(api.jobCalls)-2:]
	if lastTwo[0].opts.PerPage != 100 || lastTwo[0].opts.Page != 1 || lastTwo[1].opts.Page != 2 {
		t.Fatalf("job pagination = %+v", lastTwo)
	}
	if len(store.jobs) != 1 {
		t.Fatalf("stored jobs = %d, want 1", len(store.jobs))
	}
}

func TestCreatedShardRecursivelySplitsAtAPIResultCap(t *testing.T) {
	store := newFakeStore()
	start := testNow.Add(-time.Hour)
	api := &fakeAPI{}
	api.listRuns = func(_ context.Context, opts ghapi.WorkflowRunListOptions) ([]ghapi.WorkflowRun, ghapi.ResponseMetadata, error) {
		if opts.CreatedFrom.Equal(start) && opts.CreatedTo.Equal(testNow) {
			runs := make([]ghapi.WorkflowRun, perPage)
			next := opts.Page + 1
			if opts.Page == 10 {
				next = 0
			}
			return runs, ghapi.ResponseMetadata{NextPage: next}, nil
		}
		return nil, ghapi.ResponseMetadata{}, nil
	}
	syncer := newTestSyncer(t, store, api, Options{
		Backfill: true, StartTime: start, ShardDuration: 2 * time.Hour,
	})
	report, err := syncer.Sync(t.Context())
	if err != nil || report.Degraded {
		t.Fatalf("Sync = %+v, %v", report, err)
	}
	if len(api.runCalls) != 12 {
		t.Fatalf("run calls = %d, want 10 capped pages plus 2 child shards", len(api.runCalls))
	}
	leftTo, rightFrom, ok := splitRange(start, testNow)
	if !ok {
		t.Fatal("expected splittable range")
	}
	if got := api.runCalls[10]; !got.CreatedFrom.Equal(start) || !got.CreatedTo.Equal(leftTo) {
		t.Fatalf("left shard = %s..%s", got.CreatedFrom, got.CreatedTo)
	}
	if got := api.runCalls[11]; !got.CreatedFrom.Equal(rightFrom) || !got.CreatedTo.Equal(testNow) {
		t.Fatalf("right shard = %s..%s", got.CreatedFrom, got.CreatedTo)
	}
}

func TestBackfillResumeStartsAtFirstUncommittedShard(t *testing.T) {
	store := newFakeStore()
	start := testNow.Add(-2 * time.Hour)
	failTo := testNow.Add(-time.Hour)
	failing := true
	api := &fakeAPI{listRuns: func(_ context.Context, opts ghapi.WorkflowRunListOptions) ([]ghapi.WorkflowRun, ghapi.ResponseMetadata, error) {
		if failing && opts.CreatedTo.Equal(failTo) {
			return nil, ghapi.ResponseMetadata{}, errors.New("temporary failure")
		}
		return nil, ghapi.ResponseMetadata{}, nil
	}}
	syncer := newTestSyncer(t, store, api, Options{
		Backfill: true, StartTime: start, ShardDuration: time.Hour,
	})
	first, err := syncer.Sync(t.Context())
	if err != nil || !first.Degraded {
		t.Fatalf("first Sync = %+v, %v", first, err)
	}
	var cursor syncCursor
	if err := decodeState(store, "o/r", &cursor); err != nil {
		t.Fatal(err)
	}
	if !cursor.Next.Equal(failTo) {
		t.Fatalf("resume next = %s, want %s", cursor.Next, failTo)
	}

	failing = false
	api.runCalls = nil
	second, err := syncer.Sync(t.Context())
	if err != nil || second.Degraded {
		t.Fatalf("second Sync = %+v, %v", second, err)
	}
	if len(api.runCalls) == 0 || !api.runCalls[0].CreatedTo.Equal(failTo) {
		t.Fatalf("resumed calls = %+v", api.runCalls)
	}
}

func TestExactAndLegacyAttributionAndReruns(t *testing.T) {
	store := newFakeStore()
	store.sessions = []history.RunnerSession{
		{ID: "session-1", Repository: "o/r", WorkflowJobID: 11, RunnerName: "session-runner"},
	}
	api := &fakeAPI{
		listRuns: func(context.Context, ghapi.WorkflowRunListOptions) ([]ghapi.WorkflowRun, ghapi.ResponseMetadata, error) {
			return []ghapi.WorkflowRun{workflowRun(1)}, ghapi.ResponseMetadata{}, nil
		},
		listJobs: func(context.Context, int64, ghapi.PageOptions) ([]ghapi.WorkflowJob, ghapi.ResponseMetadata, error) {
			jobs := []ghapi.WorkflowJob{
				workflowJob(11, 1, "different-name"),
				workflowJob(12, 1, "session-runner"),
				workflowJob(13, 1, "legacy-v1-abc"),
				workflowJob(14, 1, "unrelated"),
			}
			jobs[1].RunAttempt = 2
			return jobs, ghapi.ResponseMetadata{}, nil
		},
	}
	syncer := newTestSyncer(t, store, api, Options{LegacyPrefixes: []string{"legacy-v1-"}})
	report, err := syncer.Sync(t.Context())
	if err != nil || report.Degraded {
		t.Fatalf("Sync = %+v, %v", report, err)
	}
	item := report.Repositories[0]
	if item.ExactJobs != 2 || item.InferredJobs != 1 || item.Jobs != 3 {
		t.Fatalf("report = %+v", item)
	}
	if len(store.jobs) != 3 {
		t.Fatalf("stored jobs = %+v", store.jobs)
	}
	if _, ok := store.jobs["o/r:14"]; ok {
		t.Fatal("stored unrelated job")
	}
	if got := store.jobs["o/r:11"]; got.AttributionSource != "exact_job_id" ||
		got.AttributionConfidence != 100 || got.LocalSessionID == "" {
		t.Fatalf("job-ID attribution = %+v", got)
	}
	if got := store.jobs["o/r:12"]; got.AttributionSource != "exact_runner_name" ||
		got.AttributionConfidence != 100 || got.RunAttempt != 2 {
		t.Fatalf("runner-name attribution = %+v", got)
	}
	if got := store.jobs["o/r:13"]; got.AttributionSource != "inferred_legacy_prefix" ||
		got.AttributionConfidence != 50 {
		t.Fatalf("legacy attribution = %+v", got)
	}
}

func TestExpiredRun404IsTerminalAndDoesNotDegrade(t *testing.T) {
	store := newFakeStore()
	api := &fakeAPI{
		listRuns: func(context.Context, ghapi.WorkflowRunListOptions) ([]ghapi.WorkflowRun, ghapi.ResponseMetadata, error) {
			return []ghapi.WorkflowRun{workflowRun(1)}, ghapi.ResponseMetadata{}, nil
		},
		listJobs: func(context.Context, int64, ghapi.PageOptions) ([]ghapi.WorkflowJob, ghapi.ResponseMetadata, error) {
			return nil, ghapi.ResponseMetadata{StatusCode: http.StatusNotFound}, errors.New("gone")
		},
	}
	report, err := newTestSyncer(t, store, api, Options{}).Sync(t.Context())
	if err != nil || report.Degraded || report.Repositories[0].ExpiredRuns != 1 {
		t.Fatalf("Sync = %+v, %v", report, err)
	}
}

func TestRateLimitResetWaitsAndRetries(t *testing.T) {
	store := newFakeStore()
	now := testNow
	calls := 0
	api := &fakeAPI{listRuns: func(context.Context, ghapi.WorkflowRunListOptions) ([]ghapi.WorkflowRun, ghapi.ResponseMetadata, error) {
		calls++
		if calls == 1 {
			return nil, ghapi.ResponseMetadata{
				StatusCode: http.StatusTooManyRequests,
				RateLimit:  ghapi.RateLimit{Remaining: 0, Reset: now.Add(2 * time.Second)},
			}, errors.New("rate limited")
		}
		return nil, ghapi.ResponseMetadata{}, nil
	}}
	var sleeps []time.Duration
	syncer := newTestSyncer(t, store, api, Options{
		Now: func() time.Time { return now },
		Sleep: func(ctx context.Context, delay time.Duration) error {
			sleeps = append(sleeps, delay)
			now = now.Add(delay)
			return ctx.Err()
		},
	})
	report, err := syncer.Sync(t.Context())
	if err != nil || report.Degraded {
		t.Fatalf("Sync = %+v, %v", report, err)
	}
	if calls != 2 || !reflect.DeepEqual(sleeps, []time.Duration{2 * time.Second}) {
		t.Fatalf("calls=%d sleeps=%v", calls, sleeps)
	}
}

func TestRateLimitWaitHonorsContextCancellation(t *testing.T) {
	store := newFakeStore()
	api := &fakeAPI{listRuns: func(context.Context, ghapi.WorkflowRunListOptions) ([]ghapi.WorkflowRun, ghapi.ResponseMetadata, error) {
		return nil, ghapi.ResponseMetadata{
			StatusCode: http.StatusTooManyRequests,
			RateLimit:  ghapi.RateLimit{Remaining: 0, Reset: testNow.Add(time.Hour)},
		}, errors.New("rate limited")
	}}
	ctx, cancel := context.WithCancel(context.Background())
	syncer := newTestSyncer(t, store, api, Options{
		Sleep: func(context.Context, time.Duration) error {
			cancel()
			return context.Canceled
		},
	})
	_, err := syncer.Sync(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Sync error = %v, want context cancellation", err)
	}
}

func TestOverlapReconciliationIsIdempotent(t *testing.T) {
	store := newFakeStore()
	store.sessions = []history.RunnerSession{{Repository: "o/r", RunnerName: "runner"}}
	api := &fakeAPI{
		listRuns: func(context.Context, ghapi.WorkflowRunListOptions) ([]ghapi.WorkflowRun, ghapi.ResponseMetadata, error) {
			return []ghapi.WorkflowRun{workflowRun(1)}, ghapi.ResponseMetadata{}, nil
		},
		listJobs: func(context.Context, int64, ghapi.PageOptions) ([]ghapi.WorkflowJob, ghapi.ResponseMetadata, error) {
			return []ghapi.WorkflowJob{workflowJob(10, 1, "runner")}, ghapi.ResponseMetadata{}, nil
		},
	}

	syncer := newTestSyncer(t, store, api, Options{Overlap: time.Hour})
	for i := 0; i < 2; i++ {
		report, err := syncer.Sync(t.Context())
		if err != nil || report.Degraded {
			t.Fatalf("Sync %d = %+v, %v", i, report, err)
		}
	}
	if len(store.runs) != 1 || len(store.jobs) != 1 {
		t.Fatalf("idempotent sizes: runs=%d jobs=%d", len(store.runs), len(store.jobs))
	}
	if len(api.runCalls) != 2 {
		t.Fatalf("reconciliation calls = %d", len(api.runCalls))
	}
	for _, call := range api.runCalls {
		if !call.CreatedFrom.Equal(testNow.Add(-time.Hour)) || !call.CreatedTo.Equal(testNow) {
			t.Fatalf("overlap range = %s..%s", call.CreatedFrom, call.CreatedTo)
		}
	}
}

func TestObserverReceivesAPIAndRepositorySyncOutcomes(t *testing.T) {
	store := newFakeStore()
	observer := &fakeObserver{}
	api := &fakeAPI{
		listRuns: func(context.Context, ghapi.WorkflowRunListOptions) ([]ghapi.WorkflowRun, ghapi.ResponseMetadata, error) {
			return nil, ghapi.ResponseMetadata{StatusCode: http.StatusOK}, nil
		},
	}
	syncer := newTestSyncer(t, store, api, Options{Observer: observer})
	report, err := syncer.Sync(t.Context())
	if err != nil || report.Degraded {
		t.Fatalf("successful Sync = %+v, %v", report, err)
	}
	if !reflect.DeepEqual(observer.repositories, []string{"o/r"}) {
		t.Fatalf("repositories = %v", observer.repositories)
	}
	if !reflect.DeepEqual(observer.requests, []apiRequest{{repository: "o/r", status: http.StatusOK}}) {
		t.Fatalf("requests = %+v", observer.requests)
	}
	if len(observer.successes) != 1 || !observer.successes[0].BackfillComplete ||
		observer.successes[0].LastSuccessAt == nil {
		t.Fatalf("successes = %+v", observer.successes)
	}

	api.listRuns = func(context.Context, ghapi.WorkflowRunListOptions) ([]ghapi.WorkflowRun, ghapi.ResponseMetadata, error) {
		return nil, ghapi.ResponseMetadata{}, errors.New("network failure")
	}
	report, err = syncer.Sync(t.Context())
	if err != nil || !report.Degraded {
		t.Fatalf("failed Sync = %+v, %v", report, err)
	}
	if !reflect.DeepEqual(observer.errors, []string{"o/r"}) {
		t.Fatalf("errors = %v", observer.errors)
	}
	if got := observer.requests[len(observer.requests)-1]; got.status != 0 {
		t.Fatalf("failed request = %+v", got)
	}
}

func newTestSyncer(t *testing.T, store *fakeStore, api *fakeAPI, opts Options) *Syncer {
	t.Helper()
	if opts.Now == nil {
		opts.Now = func() time.Time { return testNow }
	}
	syncer, err := New(store, []Repository{{Name: "o/r", API: api}}, opts)
	if err != nil {
		t.Fatal(err)
	}
	return syncer
}

func workflowRun(id int64) ghapi.WorkflowRun {
	return ghapi.WorkflowRun{
		ID: id, Name: "CI", DisplayTitle: "CI run", Status: "completed",
		Conclusion: "success", CreatedAt: testNow.Add(-time.Minute),
		UpdatedAt: testNow, RunStartedAt: testNow.Add(-30 * time.Second),
	}
}

func workflowJob(id, runID int64, runner string) ghapi.WorkflowJob {
	return ghapi.WorkflowJob{
		ID: id, RunID: runID, RunAttempt: 1, Name: "test",
		Status: "completed", Conclusion: "success", RunnerName: runner,
		CreatedAt: testNow.Add(-time.Minute), StartedAt: testNow.Add(-30 * time.Second),
		CompletedAt: testNow,
	}
}

func decodeState(store *fakeStore, repository string, cursor *syncCursor) error {
	state, ok := store.states[cursorKey(repository)]
	if !ok {
		return errors.New("cursor was not saved")
	}
	return jsonUnmarshal([]byte(state.Cursor), cursor)
}

var jsonUnmarshal = func(data []byte, value any) error {
	return json.Unmarshal(data, value)
}
