package historyui

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/GerardSmit/multirunner/internal/history"
)

type fakeReader struct {
	summary  history.Summary
	runs     []history.WorkflowRun
	jobs     []history.WorkflowJob
	sessions []history.RunnerSession
	states   map[string]history.SyncState
	err      error
	runCalls []history.ListOptions
}

func (f *fakeReader) Summary(context.Context, string) (history.Summary, error) {
	return f.summary, f.err
}

func (f *fakeReader) ListWorkflowRuns(_ context.Context, options history.ListOptions) ([]history.WorkflowRun, error) {
	f.runCalls = append(f.runCalls, options)
	if f.err != nil {
		return nil, f.err
	}
	values := slices.Clone(f.runs)
	values = slices.DeleteFunc(values, func(value history.WorkflowRun) bool {
		return options.Repository != "" && value.Repository != options.Repository ||
			options.Status != "" && value.Status != options.Status
	})
	return page(values, options), nil
}

func (f *fakeReader) ListWorkflowJobs(_ context.Context, options history.ListOptions) ([]history.WorkflowJob, error) {
	if f.err != nil {
		return nil, f.err
	}
	values := slices.Clone(f.jobs)
	values = slices.DeleteFunc(values, func(value history.WorkflowJob) bool {
		return options.Repository != "" && value.Repository != options.Repository ||
			options.Status != "" && value.Status != options.Status
	})
	return page(values, options), nil
}

func (f *fakeReader) ListRunnerSessions(_ context.Context, options history.ListOptions) ([]history.RunnerSession, error) {
	if f.err != nil {
		return nil, f.err
	}
	values := slices.Clone(f.sessions)
	values = slices.DeleteFunc(values, func(value history.RunnerSession) bool {
		return options.Repository != "" && value.Repository != options.Repository ||
			options.Status != "" && value.Status != options.Status
	})
	return page(values, options), nil
}

func (f *fakeReader) SyncState(_ context.Context, key string) (history.SyncState, error) {
	if f.err != nil {
		return history.SyncState{}, f.err
	}
	state, ok := f.states[key]
	if !ok {
		return history.SyncState{}, history.ErrNotFound
	}
	return state, nil
}

func page[T any](values []T, options history.ListOptions) []T {
	start := min(options.Offset, len(values))
	limit := options.Limit
	if limit <= 0 {
		limit = 100
	}
	end := min(start+limit, len(values))
	return values[start:end]
}

func newTestHandler(t *testing.T, reader Reader) http.Handler {
	t.Helper()
	handler, err := New(reader)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return handler
}

func TestOverviewRendersAndEscapesHistory(t *testing.T) {
	now := time.Date(2026, 10, 8, 1, 0, 0, 0, time.UTC)
	reader := &fakeReader{
		summary: history.Summary{WorkflowRuns: 1, WorkflowJobs: 2},
		runs: []history.WorkflowRun{{
			ID: 42, Repository: "acme/widgets", WorkflowName: `<script>alert("x")</script>`,
			Status: "completed", Conclusion: "success", CreatedAt: now,
		}},
	}
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	response := httptest.NewRecorder()
	newTestHandler(t, reader).ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d", response.Code)
	}
	body := response.Body.String()
	if !strings.Contains(body, "Workflow runs") || !strings.Contains(body, "/runs/acme/widgets/42") {
		t.Fatalf("overview missing history: %s", body)
	}
	if strings.Contains(body, `<script>alert("x")</script>`) || !strings.Contains(body, "&lt;script&gt;") {
		t.Fatalf("workflow name was not escaped: %s", body)
	}
}

func TestRunsValidatesFiltersAndPaginates(t *testing.T) {
	reader := &fakeReader{runs: []history.WorkflowRun{
		{ID: 3, Repository: "acme/widgets", Status: "completed"},
		{ID: 2, Repository: "acme/widgets", Status: "completed"},
		{ID: 1, Repository: "acme/widgets", Status: "completed"},
	}}
	handler := newTestHandler(t, reader)

	request := httptest.NewRequest(http.MethodGet, "/runs?repository=acme%2Fwidgets&status=completed&page=2&page_size=1", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	body := response.Body.String()
	if !strings.Contains(body, "Run 2") || strings.Contains(body, "Run 3") ||
		!strings.Contains(body, "Previous") || !strings.Contains(body, "Next") {
		t.Fatalf("unexpected page: %s", body)
	}
	last := reader.runCalls[len(reader.runCalls)-1]
	if last.Repository != "acme/widgets" || last.Status != "completed" || last.Offset != 1 || last.Limit != 2 {
		t.Fatalf("list options = %+v", last)
	}

	for _, target := range []string{
		"/runs?repository=not-a-repository",
		"/runs?status=success",
		"/runs?page=0",
		"/runs?page_size=101",
	} {
		response = httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, target, nil))
		if response.Code != http.StatusBadRequest {
			t.Errorf("%s status = %d, want 400", target, response.Code)
		}
	}
}

func TestRunDetailShowsLinksAndAttributionBadges(t *testing.T) {
	now := time.Date(2026, 10, 8, 1, 0, 0, 0, time.UTC)
	reader := &fakeReader{
		runs: []history.WorkflowRun{{
			ID: 42, Repository: "acme/widgets", WorkflowName: "CI", Status: "completed",
			Conclusion: "success", HTMLURL: "https://github.com/acme/widgets/actions/runs/42",
			CreatedAt: now,
		}},
		jobs: []history.WorkflowJob{
			{ID: 100, RunID: 42, Repository: "acme/widgets", Name: "exact job", RunnerGroup: "default"},
			{ID: 101, RunID: 42, Repository: "acme/widgets", Name: "inferred job", RunnerGroup: "hosted"},
		},
		sessions: []history.RunnerSession{
			{ID: "session", Repository: "acme/widgets", WorkflowJobID: 100, PoolName: "linux-x64"},
		},
	}
	response := httptest.NewRecorder()
	newTestHandler(t, reader).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/runs/acme/widgets/42", nil))

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	body := response.Body.String()
	for _, expected := range []string{"Open on GitHub", "linux-x64", "exact", "hosted", "inferred"} {
		if !strings.Contains(body, expected) {
			t.Errorf("detail missing %q: %s", expected, body)
		}
	}
}

func TestJSONAPIsUseDefaultsAndReturnEmptyArrays(t *testing.T) {
	reader := &fakeReader{
		summary: history.Summary{
			CancelledJobs: 2, AverageDurationSeconds: 92,
		},
		states: map[string]history.SyncState{
			"acme/widgets": {Key: "acme/widgets", Cursor: "cursor"},
		},
	}
	handler := newTestHandler(t, reader)

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/summary", nil))
	if response.Code != http.StatusOK ||
		!strings.Contains(response.Body.String(), `"cancelled_jobs":2`) ||
		!strings.Contains(response.Body.String(), `"average_duration_seconds":92`) {
		t.Fatalf("summary response = %d %s", response.Code, response.Body.String())
	}

	response = httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/runs", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d", response.Code)
	}
	var payload struct {
		Runs       []history.WorkflowRun `json:"runs"`
		Pagination pagination            `json:"pagination"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Runs == nil || payload.Pagination.Page != 1 || payload.Pagination.PageSize != DefaultPageSize {
		t.Fatalf("payload = %+v", payload)
	}

	response = httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/sync-state?key=acme%2Fwidgets", nil))
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"cursor":"cursor"`) {
		t.Fatalf("sync response = %d %s", response.Code, response.Body.String())
	}

	response = httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/jobs?run_id=42", nil))
	if response.Code != http.StatusBadRequest {
		t.Fatalf("jobs without repository status = %d", response.Code)
	}
}

func TestEmptyDegradedAndSecurityHeaders(t *testing.T) {
	handler := newTestHandler(t, &fakeReader{})
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/", nil))
	if !strings.Contains(response.Body.String(), "No workflow history") {
		t.Fatalf("empty state missing: %s", response.Body.String())
	}
	assertSecurityHeaders(t, response.Header())

	handler = newTestHandler(t, &fakeReader{err: errors.New("database offline")})
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/", nil))
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "temporarily unavailable") {
		t.Fatalf("degraded response = %d %s", response.Code, response.Body.String())
	}

	response = httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/assets/app.js", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("asset status = %d", response.Code)
	}
	assertSecurityHeaders(t, response.Header())
}

func assertSecurityHeaders(t *testing.T, header http.Header) {
	t.Helper()
	for _, name := range []string{
		"Content-Security-Policy", "Permissions-Policy", "Referrer-Policy",
		"X-Content-Type-Options", "X-Frame-Options", "Cache-Control",
	} {
		if header.Get(name) == "" {
			t.Errorf("missing security header %s", name)
		}
	}
	if strings.Contains(header.Get("Content-Security-Policy"), "'unsafe-inline'") {
		t.Errorf("CSP allows inline content: %s", header.Get("Content-Security-Policy"))
	}
}
