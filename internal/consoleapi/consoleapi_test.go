package consoleapi

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GerardSmit/multirunner/internal/alerts"
	"github.com/GerardSmit/multirunner/internal/backup"
	"github.com/GerardSmit/multirunner/internal/consoleauth"
	"github.com/GerardSmit/multirunner/internal/control"
	"github.com/GerardSmit/multirunner/internal/history"
	"github.com/GerardSmit/multirunner/internal/operations"
	"github.com/GerardSmit/multirunner/internal/pool"
	"github.com/GerardSmit/multirunner/internal/restore"
	"github.com/GerardSmit/multirunner/internal/runtimecontrol"
	"github.com/GerardSmit/multirunner/internal/searchexport"
	"github.com/GerardSmit/multirunner/internal/supportbundle"
	"github.com/GerardSmit/multirunner/internal/transientlog"
)

func TestHandlerRequiresPairingAndServesVersionedAPI(t *testing.T) {
	auth, err := consoleauth.New(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	handler := New(Options{
		Auth:      auth,
		LegacyAPI: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("legacy")) }),
		UI:        http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ui")) }),
		Listen:    "127.0.0.1:9092",
		Database:  "history.db",
		StartedAt: time.Unix(1000, 0),
	})

	unauthenticated := httptest.NewRecorder()
	handler.ServeHTTP(unauthenticated, httptest.NewRequest(http.MethodGet, "/api/v1/system", nil))
	if unauthenticated.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated request returned %d", unauthenticated.Code)
	}

	session := pairSession(t, handler, auth)

	cookieOnly := httptest.NewRequest(http.MethodGet, "/api/v1/system", nil)
	cookieOnly.AddCookie(session.Cookie)
	cookieOnlyResponse := httptest.NewRecorder()
	handler.ServeHTTP(cookieOnlyResponse, cookieOnly)
	if cookieOnlyResponse.Code != http.StatusUnauthorized {
		t.Fatalf("cookie-only API request returned %d", cookieOnlyResponse.Code)
	}
	legacyCookieOnly := httptest.NewRequest(http.MethodGet, "/api/legacy", nil)
	legacyCookieOnly.AddCookie(session.Cookie)
	legacyCookieOnlyResponse := httptest.NewRecorder()
	handler.ServeHTTP(legacyCookieOnlyResponse, legacyCookieOnly)
	if legacyCookieOnlyResponse.Code != http.StatusUnauthorized {
		t.Fatalf("cookie-only legacy API request returned %d", legacyCookieOnlyResponse.Code)
	}
	staticRequest := httptest.NewRequest(http.MethodGet, "/", nil)
	staticRequest.AddCookie(session.Cookie)
	staticResponse := httptest.NewRecorder()
	handler.ServeHTTP(staticResponse, staticRequest)
	if staticResponse.Code != http.StatusOK || staticResponse.Body.String() != "ui" {
		t.Fatalf("cookie-authenticated static request = %d %q", staticResponse.Code, staticResponse.Body.String())
	}

	request := httptest.NewRequest(http.MethodGet, "/api/v1/system", nil)
	request.AddCookie(session.Cookie)
	request.Header.Set(consoleauth.ProofHeader, session.Proof)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"api_version":"v1"`) {
		t.Fatalf("unexpected system response: %d %s", response.Code, response.Body.String())
	}
	if response.Header().Get("Permissions-Policy") == "" {
		t.Fatal("permissions policy is missing")
	}
}

type fileBundleReader struct {
	path string
}

type fileBackupReader struct {
	path     string
	metadata backup.Metadata
}

type memoryRestoreReader struct {
	items []restore.Metadata
}

func (r memoryRestoreReader) List(context.Context, int) ([]restore.Metadata, error) {
	return r.items, nil
}

func (r fileBackupReader) List(context.Context, int) ([]backup.Metadata, error) {
	return []backup.Metadata{r.metadata}, nil
}

func (r fileBackupReader) Open(
	context.Context, string,
) (backup.Metadata, *os.File, error) {
	file, err := os.Open(r.path)
	return r.metadata, file, err
}

type memorySearch struct {
	options history.SearchOptions
}

type memoryRunInspection struct {
	options []history.ListOptions
}

type memoryJobLogs struct {
	actorID       string
	correlationID string
	jobID         int64
}

type memoryAnalytics struct {
	options history.AnalyticsOptions
}

func (m *memoryAnalytics) Analytics(
	_ context.Context, options history.AnalyticsOptions,
) (history.AnalyticsReport, error) {
	m.options = options
	return history.AnalyticsReport{
		GroupBy: options.GroupBy, Since: options.Since, Until: options.Until,
		Rows: []history.AnalyticsRow{{
			Key: "o/r", Repository: "o/r", TotalJobs: 10,
			SuccessfulJobs: 9, SuccessRate: 0.9,
		}},
	}, nil
}

type blockingAnalytics struct{}

func (blockingAnalytics) Analytics(
	ctx context.Context, _ history.AnalyticsOptions,
) (history.AnalyticsReport, error) {
	<-ctx.Done()
	return history.AnalyticsReport{}, ctx.Err()
}

func TestVersionedAnalyticsValidatesWindowAndGrouping(t *testing.T) {
	auth, err := consoleauth.New(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	analytics := &memoryAnalytics{}
	handler := New(Options{
		Auth: auth, Listen: "127.0.0.1:9092", Analytics: analytics,
		LegacyAPI: http.NotFoundHandler(), UI: http.NotFoundHandler(),
	})
	request := httptest.NewRequest(
		http.MethodGet,
		"/api/v1/analytics?group_by=workflow&since=2026-10-01T00:00:00Z&until=2026-10-09T00:00:00Z",
		nil,
	)
	addPairedSession(request, pairSession(t, handler, auth))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK ||
		!strings.Contains(response.Body.String(), `"success_rate":0.9`) {
		t.Fatalf("analytics = %d %s", response.Code, response.Body.String())
	}
	if analytics.options.GroupBy != "workflow" ||
		analytics.options.Since.Day() != 1 || analytics.options.Until.Day() != 9 {
		t.Fatalf("analytics options = %+v", analytics.options)
	}
}

func TestAnalyticsRouteReturnsTypedTimeout(t *testing.T) {
	original := analyticsRouteTimeout
	analyticsRouteTimeout = 10 * time.Millisecond
	t.Cleanup(func() { analyticsRouteTimeout = original })
	request := httptest.NewRequest(http.MethodGet, "/api/v1/analytics", nil)
	response := httptest.NewRecorder()
	analyticsHandler(blockingAnalytics{}).ServeHTTP(response, request)
	if response.Code != http.StatusServiceUnavailable ||
		!strings.Contains(response.Body.String(), `"code":"analytics_timeout"`) {
		t.Fatalf("response = %d %s", response.Code, response.Body.String())
	}
}

func (m *memoryJobLogs) Fetch(
	_ context.Context, actorID, correlationID string, jobID int64,
) (transientlog.Result, error) {
	m.actorID, m.correlationID, m.jobID = actorID, correlationID, jobID
	return transientlog.Result{
		JobID: jobID, Repository: "actionscommunity/multirunner",
		Content: []byte("masked log\n"),
	}, nil
}

func TestTransientJobLogIsAuthenticatedAuditedAndNeverCached(t *testing.T) {
	auth, err := consoleauth.New(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	logs := &memoryJobLogs{}
	handler := New(Options{
		Auth: auth, Listen: "127.0.0.1:9092", Logs: logs,
		LegacyAPI: http.NotFoundHandler(), UI: http.NotFoundHandler(),
	})
	request := httptest.NewRequest(http.MethodGet, "/api/v1/jobs/84/log", nil)
	request.Header.Set("X-Correlation-ID", "correlation-1")
	addPairedSession(request, pairSession(t, handler, auth))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || response.Body.String() != "masked log\n" {
		t.Fatalf("job log = %d %q", response.Code, response.Body.String())
	}
	if response.Header().Get("Cache-Control") != "no-store" ||
		response.Header().Get("Content-Type") != "text/plain; charset=utf-8" ||
		response.Header().Get("X-Log-Bytes") != "11" {
		t.Fatalf("job log headers = %v", response.Header())
	}
	if logs.actorID == "" || logs.correlationID == "" || logs.jobID != 84 {
		t.Fatalf("job log request = %+v", logs)
	}
}

func (m *memoryRunInspection) WorkflowRun(
	_ context.Context, repository string, runID int64,
) (history.WorkflowRun, error) {
	return history.WorkflowRun{
		ID: runID, Repository: repository, WorkflowName: "CI",
		DisplayTitle: "Inspect this run", Status: "completed",
		Conclusion: "failure", CreatedAt: time.Unix(1000, 0),
	}, nil
}

func (m *memoryRunInspection) ListWorkflowJobs(
	_ context.Context, options history.ListOptions,
) ([]history.WorkflowJob, error) {
	m.options = append(m.options, options)
	return []history.WorkflowJob{{
		ID: 84, RunID: options.RunID, Repository: options.Repository,
		Name: "integration", Status: "completed", Conclusion: "failure",
		CreatedAt: time.Unix(1000, 0),
		Steps:     []history.WorkflowStep{{Number: 1, Name: "Test"}},
	}}, nil
}

func (m *memoryRunInspection) ListRunnerSessions(
	_ context.Context, options history.ListOptions,
) ([]history.RunnerSession, error) {
	m.options = append(m.options, options)
	return []history.RunnerSession{{
		ID: "session-1", Repository: options.Repository,
		WorkflowRunID: options.RunID, Status: "stopped",
		StartedAt: time.Unix(1000, 0), PlannedAt: time.Unix(999, 0),
	}}, nil
}

func TestVersionedRunInspectionCorrelatesJobsAndRunners(t *testing.T) {
	auth, err := consoleauth.New(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	runs := &memoryRunInspection{}
	handler := New(Options{
		Auth: auth, Listen: "127.0.0.1:9092", Runs: runs,
		LegacyAPI: http.NotFoundHandler(), UI: http.NotFoundHandler(),
	})
	request := httptest.NewRequest(
		http.MethodGet,
		"/api/v1/runs/42?repository=actionscommunity%2Fmultirunner",
		nil,
	)
	addPairedSession(request, pairSession(t, handler, auth))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK ||
		!strings.Contains(response.Body.String(), `"display_title":"Inspect this run"`) ||
		!strings.Contains(response.Body.String(), `"local_session_id":""`) ||
		!strings.Contains(response.Body.String(), `"id":"session-1"`) {
		t.Fatalf("run inspection = %d %s", response.Code, response.Body.String())
	}
	if len(runs.options) != 2 || runs.options[0].RunID != 42 ||
		runs.options[1].Repository != "actionscommunity/multirunner" {
		t.Fatalf("run inspection options = %+v", runs.options)
	}
}

func (s *memorySearch) Search(
	_ context.Context, options history.SearchOptions,
) ([]history.SearchResult, error) {
	s.options = options
	return []history.SearchResult{{
		EntityType: "run", EntityKey: "o/r:42", Repository: "o/r",
		Title: "Release verification", Route: "/runs?repository=o%2Fr&run_id=42",
	}}, nil
}

func TestVersionedSearchIsAuthenticatedAndBounded(t *testing.T) {
	auth, err := consoleauth.New(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	search := &memorySearch{}
	handler := New(Options{
		Auth: auth, Listen: "127.0.0.1:9092", Search: search,
		LegacyAPI: http.NotFoundHandler(), UI: http.NotFoundHandler(),
	})
	request := httptest.NewRequest(
		http.MethodGet,
		"/api/v1/search?q=release&type=run&repository=o%2Fr&limit=12",
		nil,
	)
	addPairedSession(request, pairSession(t, handler, auth))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK ||
		!strings.Contains(response.Body.String(), `"title":"Release verification"`) {
		t.Fatalf("search = %d %s", response.Code, response.Body.String())
	}
	assertCollectionEnvelope(t, response.Body.Bytes())
	if search.options.Query != "release" || search.options.EntityType != "run" ||
		search.options.Repository != "o/r" || search.options.Limit != 13 {
		t.Fatalf("search options = %+v", search.options)
	}

	invalid := httptest.NewRequest(http.MethodGet, "/api/v1/search?q=ok&limit=101", nil)
	addPairedSession(invalid, pairSession(t, handler, auth))
	invalidResponse := httptest.NewRecorder()
	handler.ServeHTTP(invalidResponse, invalid)
	if invalidResponse.Code != http.StatusBadRequest {
		t.Fatalf("invalid search = %d %s", invalidResponse.Code, invalidResponse.Body.String())
	}
}

func (r fileBundleReader) Open(
	_ context.Context, id string,
) (supportbundle.Metadata, *os.File, error) {
	if id != "bundle" {
		return supportbundle.Metadata{}, nil, supportbundle.ErrNotFound
	}
	file, err := os.Open(r.path)
	return supportbundle.Metadata{
		ID: id, FileName: "support.zip", SHA256: "abc123",
		CreatedAt: time.Unix(1000, 0),
	}, file, err
}

func TestSupportBundleDownloadIsAuthenticatedAndNeverCached(t *testing.T) {
	auth, err := consoleauth.New(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "support.zip")
	if err := os.WriteFile(path, []byte("zip-data"), 0o600); err != nil {
		t.Fatal(err)
	}
	handler := New(Options{
		Auth: auth, Listen: "127.0.0.1:9092",
		LegacyAPI: http.NotFoundHandler(), UI: http.NotFoundHandler(),
		SupportBundles: fileBundleReader{path: path},
	})
	paired := pairSession(t, handler, auth)
	cookieOnly := httptest.NewRequest(http.MethodGet, "/api/v1/support-bundles/bundle", nil)
	cookieOnly.AddCookie(paired.Cookie)
	cookieOnlyResponse := httptest.NewRecorder()
	handler.ServeHTTP(cookieOnlyResponse, cookieOnly)
	if cookieOnlyResponse.Code != http.StatusUnauthorized {
		t.Fatalf("cookie-only download = %d", cookieOnlyResponse.Code)
	}
	request := httptest.NewRequest(http.MethodGet, "/api/v1/support-bundles/bundle", nil)
	addPairedSession(request, paired)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || response.Body.String() != "zip-data" {
		t.Fatalf("download = %d %q", response.Code, response.Body.String())
	}
	if response.Header().Get("Cache-Control") != "no-store" ||
		response.Header().Get("Content-Type") != "application/zip" ||
		!strings.Contains(response.Header().Get("Content-Disposition"), "support.zip") ||
		response.Header().Get("X-Content-SHA256") != "abc123" {
		t.Fatalf("download headers = %v", response.Header())
	}
}

func TestBackupListAndDownloadAreAuthenticatedAndNeverCached(t *testing.T) {
	auth, err := consoleauth.New(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(t.TempDir(), "backup.db")
	if err := os.WriteFile(path, []byte("sqlite-backup"), 0o600); err != nil {
		t.Fatal(err)
	}
	metadata := backup.Metadata{
		ID: "backup-1", State: backup.StateSucceeded, FileName: "backup-1.db",
		SHA256: "abc123", CompletedAt: time.Unix(1000, 0),
		DownloadURL: "/api/v1/backups/backup-1/download",
	}
	handler := New(Options{
		Auth: auth, Listen: "127.0.0.1:9092",
		LegacyAPI: http.NotFoundHandler(), UI: http.NotFoundHandler(),
		Backups: fileBackupReader{path: path, metadata: metadata},
	})
	cookie := pairSession(t, handler, auth)
	list := authenticatedRequest(t, handler, cookie, "/api/v1/backups")
	if list.Code != http.StatusOK || !strings.Contains(list.Body.String(), `"id":"backup-1"`) {
		t.Fatalf("backup list = %d %s", list.Code, list.Body.String())
	}
	assertCollectionEnvelope(t, list.Body.Bytes())
	request := httptest.NewRequest(http.MethodGet, metadata.DownloadURL, nil)
	addPairedSession(request, cookie)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || response.Body.String() != "sqlite-backup" {
		t.Fatalf("backup download = %d %q", response.Code, response.Body.String())
	}
	if response.Header().Get("Cache-Control") != "no-store" ||
		response.Header().Get("Content-Type") != "application/vnd.sqlite3" ||
		!strings.Contains(response.Header().Get("Content-Disposition"), "backup-1.db") ||
		response.Header().Get("X-Content-SHA256") != "abc123" {
		t.Fatalf("backup download headers = %v", response.Header())
	}
}

func TestRestoreListIsAuthenticated(t *testing.T) {
	auth, err := consoleauth.New(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	handler := New(Options{
		Auth: auth, Listen: "127.0.0.1:9092",
		LegacyAPI: http.NotFoundHandler(), UI: http.NotFoundHandler(),
		Restores: memoryRestoreReader{items: []restore.Metadata{{
			ID: "restore-1", BackupID: "backup-1", State: restore.StateStaged,
		}}},
	})
	unauthenticated := httptest.NewRecorder()
	handler.ServeHTTP(
		unauthenticated,
		httptest.NewRequest(http.MethodGet, "/api/v1/restores", nil),
	)
	if unauthenticated.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated restore list = %d", unauthenticated.Code)
	}
	cookie := pairSession(t, handler, auth)
	response := authenticatedRequest(t, handler, cookie, "/api/v1/restores")
	if response.Code != http.StatusOK ||
		!strings.Contains(response.Body.String(), `"id":"restore-1"`) {
		t.Fatalf("restore list = %d %s", response.Code, response.Body.String())
	}
	assertCollectionEnvelope(t, response.Body.Bytes())
}

func TestSavedViewsAndSearchExportsUseMutationAndDownloadBoundaries(t *testing.T) {
	auth, err := consoleauth.New(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}

	store := openConsoleStore(t)
	exporter, err := searchexport.New(searchexport.Options{
		Root: filepath.Join(t.TempDir(), "exports"), Source: store, Store: store,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertWorkflowRun(t.Context(), history.WorkflowRun{
		Repository: "o/r", ID: 42, Name: "Release", WorkflowName: "Release",
		DisplayTitle: "Release verification", Status: "completed", Conclusion: "success",
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	handler := New(Options{
		Auth: auth, Listen: "127.0.0.1:9092", SavedViews: store, Exports: exporter,
		LegacyAPI: http.NotFoundHandler(), UI: http.NotFoundHandler(),
	})
	cookie := pairSession(t, handler, auth)
	sessionResponse := authenticatedRequest(t, handler, cookie, "/api/v1/session")
	var session consoleauth.Session
	if err := json.Unmarshal(sessionResponse.Body.Bytes(), &session); err != nil {
		t.Fatal(err)
	}
	create := mutationRequest(
		handler, cookie, session.CSRFToken, http.MethodPost, "/api/v1/saved-views",
		"saved-1", []byte(`{"name":"Release failures","query":"release","entity_type":"run"}`),
	)
	if create.Code != http.StatusCreated || create.Header().Get("ETag") != `"1"` {
		t.Fatalf("create saved view = %d %s", create.Code, create.Body.String())
	}
	var saved history.SavedView
	if err := json.Unmarshal(create.Body.Bytes(), &saved); err != nil {
		t.Fatal(err)
	}
	list := authenticatedRequest(t, handler, cookie, "/api/v1/saved-views")
	if list.Code != http.StatusOK || !strings.Contains(list.Body.String(), `"Release failures"`) {
		t.Fatalf("list saved views = %d %s", list.Code, list.Body.String())
	}
	assertCollectionEnvelope(t, list.Body.Bytes())
	export := mutationRequest(
		handler, cookie, session.CSRFToken, http.MethodPost, "/api/v1/exports",
		"export-1", []byte(`{"query":"release","entity_type":"run"}`),
	)
	if export.Code != http.StatusCreated {
		t.Fatalf("create export = %d %s", export.Code, export.Body.String())
	}
	var metadata searchexport.Metadata
	if err := json.Unmarshal(export.Body.Bytes(), &metadata); err != nil {
		t.Fatal(err)
	}
	download := authenticatedRequest(t, handler, cookie, metadata.DownloadURL)
	if download.Code != http.StatusOK ||
		download.Header().Get("Cache-Control") != "no-store" ||
		download.Header().Get("Content-Type") != "application/x-ndjson" ||
		!strings.Contains(download.Body.String(), `"entity_type":"run"`) {
		t.Fatalf("download export = %d %v %s", download.Code, download.Header(), download.Body.String())
	}
	deleteRequest := newMutationRequest(
		cookie, session.CSRFToken, http.MethodDelete,
		"/api/v1/saved-views/"+saved.ID, "delete-1", nil,
	)
	deleteRequest.Header.Set("If-Match", `"1"`)
	deleteResponse := httptest.NewRecorder()
	handler.ServeHTTP(deleteResponse, deleteRequest)
	if deleteResponse.Code != http.StatusNoContent {
		t.Fatalf("delete saved view = %d %s", deleteResponse.Code, deleteResponse.Body.String())
	}
}

func TestAlertAPIListsAndMutatesVersionedIncidents(t *testing.T) {
	auth, err := consoleauth.New(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	store := openConsoleStore(t)
	rules, err := store.AlertRulesForEvent(t.Context(), "runner.failed")
	if err != nil || len(rules) != 1 {
		t.Fatalf("runner.failed rules = %+v err=%v", rules, err)
	}
	instance, err := store.ObserveAlert(t.Context(), alerts.Observation{
		Rule: rules[0], DedupKey: "session-api", Summary: rules[0].Name,
		Details: json.RawMessage(`{"pool":"linux"}`), SourceEventID: "event-api",
		ObservedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	handler := New(Options{
		Auth: auth, Listen: "127.0.0.1:9092", Alerts: store,
		LegacyAPI: http.NotFoundHandler(), UI: http.NotFoundHandler(),
	})
	cookie := pairSession(t, handler, auth)
	sessionResponse := authenticatedRequest(t, handler, cookie, "/api/v1/session")
	var session consoleauth.Session
	if err := json.Unmarshal(sessionResponse.Body.Bytes(), &session); err != nil {
		t.Fatal(err)
	}

	list := authenticatedRequest(t, handler, cookie, "/api/v1/alerts?state=open")
	if list.Code != http.StatusOK || !strings.Contains(list.Body.String(), instance.ID) {
		t.Fatalf("list alerts = %d %s", list.Code, list.Body.String())
	}
	assertCollectionEnvelope(t, list.Body.Bytes())
	incidents := authenticatedRequest(t, handler, cookie, "/api/v1/incidents?state=open")
	if incidents.Code != http.StatusOK || !strings.Contains(incidents.Body.String(), instance.ID) {
		t.Fatalf("list incidents = %d %s", incidents.Code, incidents.Body.String())
	}
	read := authenticatedRequest(t, handler, cookie, "/api/v1/alerts/"+instance.ID)
	if read.Code != http.StatusOK || read.Header().Get("ETag") != `"1"` {
		t.Fatalf("read alert = %d %v %s", read.Code, read.Header(), read.Body.String())
	}

	ackRequest := newMutationRequest(
		cookie, session.CSRFToken, http.MethodPost,
		"/api/v1/alerts/"+instance.ID+"/acknowledge", "ack-api",
		[]byte(`{"reason":"investigating"}`),
	)
	ackRequest.Header.Set("If-Match", `"1"`)
	ack := httptest.NewRecorder()
	handler.ServeHTTP(ack, ackRequest)
	if ack.Code != http.StatusOK || ack.Header().Get("ETag") != `"2"` {
		t.Fatalf("acknowledge alert = %d %v %s", ack.Code, ack.Header(), ack.Body.String())
	}
	retryRequest := newMutationRequest(
		cookie, session.CSRFToken, http.MethodPost,
		"/api/v1/alerts/"+instance.ID+"/acknowledge", "ack-api",
		[]byte(`{"reason":"investigating"}`),
	)
	retryRequest.Header.Set("If-Match", `"1"`)
	retry := httptest.NewRecorder()
	handler.ServeHTTP(retry, retryRequest)
	if retry.Code != http.StatusOK || retry.Header().Get("ETag") != `"2"` {
		t.Fatalf("retry acknowledgement = %d %v %s", retry.Code, retry.Header(), retry.Body.String())
	}

	until := time.Now().UTC().Add(time.Hour).Format(time.RFC3339)
	silenceRequest := newMutationRequest(
		cookie, session.CSRFToken, http.MethodPost,
		"/api/v1/alerts/"+instance.ID+"/silence", "silence-api",
		[]byte(`{"until":"`+until+`","reason":"planned maintenance"}`),
	)
	silenceRequest.Header.Set("If-Match", `"2"`)
	silence := httptest.NewRecorder()
	handler.ServeHTTP(silence, silenceRequest)
	if silence.Code != http.StatusOK || silence.Header().Get("ETag") != `"3"` {
		t.Fatalf("silence alert = %d %v %s", silence.Code, silence.Header(), silence.Body.String())
	}

	annotation := mutationRequest(
		handler, cookie, session.CSRFToken, http.MethodPost,
		"/api/v1/alerts/"+instance.ID+"/annotations", "annotation-api",
		[]byte(`{"body":"Maintenance is in progress."}`),
	)
	if annotation.Code != http.StatusCreated {
		t.Fatalf("annotate alert = %d %s", annotation.Code, annotation.Body.String())
	}

	resolveRequest := newMutationRequest(
		cookie, session.CSRFToken, http.MethodPost,
		"/api/v1/alerts/"+instance.ID+"/resolve", "resolve-api",
		[]byte(`{"reason":"recovered"}`),
	)
	resolveRequest.Header.Set("If-Match", `"3"`)
	resolve := httptest.NewRecorder()
	handler.ServeHTTP(resolve, resolveRequest)
	if resolve.Code != http.StatusOK || resolve.Header().Get("ETag") != `"4"` ||
		!strings.Contains(resolve.Body.String(), `"state":"resolved"`) {
		t.Fatalf("resolve alert = %d %v %s", resolve.Code, resolve.Header(), resolve.Body.String())
	}
}

func openConsoleStore(t *testing.T) *history.Store {
	t.Helper()
	store, err := history.Open(t.Context(), filepath.Join(t.TempDir(), "history.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func mutationRequest(
	handler http.Handler, session pairedSession, csrf, method, route, key string, body []byte,
) *httptest.ResponseRecorder {
	request := newMutationRequest(session, csrf, method, route, key, body)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func newMutationRequest(
	session pairedSession, csrf, method, route, key string, body []byte,
) *http.Request {
	request := httptest.NewRequest(method, route, bytes.NewReader(body))
	request.Host = "127.0.0.1:9092"
	request.Header.Set("Origin", "http://127.0.0.1:9092")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", key)
	request.Header.Set("X-CSRF-Token", csrf)
	addPairedSession(request, session)
	return request
}

type memoryEvents struct {
	mu      sync.Mutex
	events  []operations.Event
	runners []operations.RunnerState
}

func (s *memoryEvents) AppendOperationalEvent(_ context.Context, epoch string, input operations.EventInput) (operations.Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sequence := int64(len(s.events) + 1)
	event := operations.Event{
		ID: operations.EventID(epoch, sequence), SchemaVersion: operations.CurrentEventSchemaVersion,
		HostID: "host", HostEpoch: epoch, Sequence: sequence,
		Type: input.Type, EntityType: input.EntityType, EntityID: input.EntityID,
		Timestamp: time.Now().UTC(), ActorKind: input.ActorKind, Payload: json.RawMessage(`{}`),
	}
	s.events = append(s.events, event)
	return event, nil
}

func (s *memoryEvents) ListOperationalEvents(_ context.Context, query operations.EventQuery) ([]operations.Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	limit := query.Limit
	if limit <= 0 {
		limit = 100
	}
	var result []operations.Event
	for _, event := range s.events {
		if event.HostEpoch == query.HostEpoch && event.Sequence > query.AfterSequence {
			result = append(result, event)
			if len(result) == limit {
				break
			}
		}
	}
	return result, nil
}

func (s *memoryEvents) OperationalEventBounds(_ context.Context, epoch string) (int64, int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var minimum, maximum int64
	for _, event := range s.events {
		if event.HostEpoch != epoch {
			continue
		}
		if minimum == 0 || event.Sequence < minimum {
			minimum = event.Sequence
		}
		if event.Sequence > maximum {
			maximum = event.Sequence
		}
	}
	return minimum, maximum, nil
}

func (s *memoryEvents) OperationalSnapshot(_ context.Context, epoch string) (operations.Snapshot, error) {
	_, maximum, _ := s.OperationalEventBounds(context.Background(), epoch)
	return operations.Snapshot{
		HostEpoch: epoch, LastSequence: maximum, GeneratedAt: time.Now().UTC(),
		Runners: append([]operations.RunnerState(nil), s.runners...),
	}, nil
}

func (s *memoryEvents) OperationalSnapshotAt(_ context.Context, epoch string, maximum int64) (operations.Snapshot, error) {
	return operations.Snapshot{
		HostEpoch: epoch, LastSequence: maximum, GeneratedAt: time.Now().UTC(),
		Runners: append([]operations.RunnerState(nil), s.runners...),
	}, nil
}

type pairedSession struct {
	Cookie *http.Cookie
	Proof  string
}

func pairSession(t *testing.T, handler http.Handler, auth *consoleauth.Authenticator) pairedSession {
	t.Helper()
	token, err := auth.PairingToken()
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	request := httptest.NewRequest(
		http.MethodPost,
		"http://127.0.0.1:9092/auth/pair",
		strings.NewReader(`{"token":`+strconv.Quote(token)+`}`),
	)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", "http://127.0.0.1:9092")
	request.Header.Set("Sec-Fetch-Site", "same-origin")
	handler.ServeHTTP(response, request)
	var cookie *http.Cookie
	proof := ""
	for _, candidate := range response.Result().Cookies() {
		if candidate.HttpOnly {
			cookie = candidate
		} else {
			proof = candidate.Value
		}
	}
	if cookie == nil || proof == "" {
		t.Fatalf("unexpected pairing cookies: %#v", response.Result().Cookies())
	}
	return pairedSession{Cookie: cookie, Proof: proof}
}

func addPairedSession(request *http.Request, session pairedSession) {
	request.AddCookie(session.Cookie)
	request.Header.Set(consoleauth.ProofHeader, session.Proof)
}

func TestEventStreamReplaysThenDeliversLiveEvents(t *testing.T) {
	auth, err := consoleauth.New(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	store := &memoryEvents{}
	journal, err := operations.NewJournal(t.Context(), store, "epoch", operations.JournalOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(journal.Close)
	first, err := journal.Append(t.Context(), operations.EventInput{
		Type: "runner.planned", EntityType: "runner_session", EntityID: "one",
		ActorKind: operations.ActorSystem,
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := journal.Append(t.Context(), operations.EventInput{
		Type: "runner.launched", EntityType: "runner_session", EntityID: "one",
		ActorKind: operations.ActorSystem,
	})
	if err != nil {
		t.Fatal(err)
	}
	handler := New(Options{
		Auth: auth, LegacyAPI: http.NotFoundHandler(),
		UI: http.NotFoundHandler(), HostEpoch: "epoch", Events: store,
		LiveEvents: journal, Heartbeat: time.Hour,
	})
	paired := pairSession(t, handler, auth)
	cookieOnly := httptest.NewRequest(http.MethodGet, "/api/v1/events", nil)
	cookieOnly.AddCookie(paired.Cookie)
	cookieOnlyResponse := httptest.NewRecorder()
	handler.ServeHTTP(cookieOnlyResponse, cookieOnly)
	if cookieOnlyResponse.Code != http.StatusUnauthorized {
		t.Fatalf("cookie-only event stream = %d", cookieOnlyResponse.Code)
	}
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/api/v1/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	addPairedSession(request, paired)
	request.Header.Set("Last-Event-ID", first.ID)
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK ||
		response.Header.Get("Content-Type") != "text/event-stream" ||
		response.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("stream response = %d headers=%v", response.StatusCode, response.Header)
	}
	reader := bufio.NewReader(response.Body)
	if id := readSSEEventID(t, reader); id != second.ID {
		t.Fatalf("replayed event ID = %q, want %q", id, second.ID)
	}
	third, err := journal.Append(t.Context(), operations.EventInput{
		Type: "runner.stopped", EntityType: "runner_session", EntityID: "one",
		ActorKind: operations.ActorSystem,
	})
	if err != nil {
		t.Fatal(err)
	}
	if id := readSSEEventID(t, reader); id != third.ID {
		t.Fatalf("live event ID = %q, want %q", id, third.ID)
	}
}

func readSSEEventID(t *testing.T, reader *bufio.Reader) string {
	t.Helper()
	deadline := time.After(2 * time.Second)
	lines := make(chan string, 1)
	go func() {
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				lines <- ""
				return
			}
			if strings.HasPrefix(line, "id: ") {
				lines <- strings.TrimSpace(strings.TrimPrefix(line, "id: "))
				return
			}
		}
	}()
	select {
	case id := <-lines:
		if id == "" {
			t.Fatal("stream ended before event ID")
		}
		return id
	case <-deadline:
		t.Fatal("timed out waiting for SSE event ID")
		return ""
	}
}

type boundedEvents struct {
	minimum int64
	maximum int64
}

func (s boundedEvents) ListOperationalEvents(context.Context, operations.EventQuery) ([]operations.Event, error) {
	return nil, nil
}

func (s boundedEvents) OperationalEventBounds(context.Context, string) (int64, int64, error) {
	return s.minimum, s.maximum, nil
}

func (s boundedEvents) OperationalSnapshot(context.Context, string) (operations.Snapshot, error) {
	return operations.Snapshot{HostEpoch: "current", LastSequence: s.maximum}, nil
}

func (s boundedEvents) OperationalSnapshotAt(context.Context, string, int64) (operations.Snapshot, error) {
	return operations.Snapshot{HostEpoch: "current", LastSequence: s.maximum}, nil
}

func TestEventStreamRejectsWrongEpochAndExpiredCursor(t *testing.T) {
	auth, err := consoleauth.New(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	handler := New(Options{
		Auth: auth, LegacyAPI: http.NotFoundHandler(), UI: http.NotFoundHandler(),
		HostEpoch: "current", Events: boundedEvents{minimum: 10, maximum: 20},
	})
	cookie := pairSession(t, handler, auth)
	tests := []struct {
		name   string
		cursor string
		code   string
	}{
		{name: "wrong epoch", cursor: "old:12", code: "event_cursor_invalid"},
		{name: "expired", cursor: "current:3", code: "event_cursor_expired"},
		{name: "newer", cursor: "current:21", code: "event_cursor_invalid"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, "/api/v1/events", nil)
			addPairedSession(request, cookie)
			request.Header.Set("Last-Event-ID", test.cursor)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != http.StatusConflict ||
				!strings.Contains(response.Body.String(), strconv.Quote(test.code)) {
				t.Fatalf("response = %d %s", response.Code, response.Body.String())
			}
		})
	}
}

func TestOperationalQueriesUseStableCursorAndSnakeCaseJSON(t *testing.T) {
	auth, err := consoleauth.New(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	store := &memoryEvents{
		events: []operations.Event{
			{ID: "epoch:1", HostEpoch: "epoch", Sequence: 1, Type: "runner.planned", EntityType: "runner_session", EntityID: "one"},
			{ID: "epoch:2", HostEpoch: "epoch", Sequence: 2, Type: "runner.launched", EntityType: "runner_session", EntityID: "one"},
			{ID: "epoch:3", HostEpoch: "epoch", Sequence: 3, Type: "runner.planned", EntityType: "runner_session", EntityID: "two"},
		},
		runners: []operations.RunnerState{
			{ID: "one", HostEpoch: "epoch", Sequence: 2, Status: "launched"},
			{ID: "two", HostEpoch: "epoch", Sequence: 3, Status: "planned"},
		},
	}
	handler := New(Options{
		Auth: auth, LegacyAPI: http.NotFoundHandler(), UI: http.NotFoundHandler(),
		HostEpoch: "epoch", Events: store,
	})
	cookie := pairSession(t, handler, auth)

	first := authenticatedRequest(t, handler, cookie, "/api/v1/events/query?limit=1&entity_type=runner_session")
	if first.Code != http.StatusOK {
		t.Fatalf("first event query = %d %s", first.Code, first.Body.String())
	}
	var page collectionResponse[operations.Event]
	if err := json.Unmarshal(first.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if page.Count != 1 || page.Items[0].ID != "epoch:1" || page.NextCursor == "" ||
		!strings.Contains(first.Body.String(), `"host_epoch":"epoch"`) ||
		strings.Contains(first.Body.String(), `"HostEpoch"`) {
		t.Fatalf("unexpected first event page: %s", first.Body.String())
	}

	store.mu.Lock()
	store.events = append(store.events, operations.Event{
		ID: "epoch:4", HostEpoch: "epoch", Sequence: 4, Type: "runner.stopped",
		EntityType: "runner_session", EntityID: "one",
	})
	store.mu.Unlock()
	second := authenticatedRequest(t, handler, cookie,
		"/api/v1/events/query?limit=1&entity_type=runner_session&cursor="+page.NextCursor)
	if second.Code != http.StatusOK {
		t.Fatalf("second event query = %d %s", second.Code, second.Body.String())
	}
	if err := json.Unmarshal(second.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if page.Items[0].ID != "epoch:2" || page.MaxSequence != 3 {
		t.Fatalf("stable event page = %+v", page)
	}

	runners := authenticatedRequest(t, handler, cookie, "/api/v1/runners?limit=1")
	if runners.Code != http.StatusOK || !strings.Contains(runners.Body.String(), `"id":"one"`) {
		t.Fatalf("runner query = %d %s", runners.Code, runners.Body.String())
	}
	if runners.Header().Get("X-Correlation-ID") == "" {
		t.Fatal("correlation ID header is missing")
	}
}

func authenticatedRequest(t *testing.T, handler http.Handler, session pairedSession, target string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, target, nil)
	addPairedSession(request, session)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func assertCollectionEnvelope(t *testing.T, data []byte) {
	t.Helper()
	var value map[string]json.RawMessage
	if err := json.Unmarshal(data, &value); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"items", "applied_filters", "cursor", "next_cursor", "count"} {
		if _, ok := value[field]; !ok {
			t.Errorf("collection missing %q: %s", field, data)
		}
	}
}

type commandTestPool struct {
	name     string
	paused   bool
	sessions []string
}

func (p *commandTestPool) Name() string             { return p.name }
func (p *commandTestPool) Pause()                   { p.paused = true }
func (p *commandTestPool) Resume()                  { p.paused = false }
func (p *commandTestPool) Paused() bool             { return p.paused }
func (p *commandTestPool) ActiveSessions() []string { return append([]string(nil), p.sessions...) }
func (p *commandTestPool) Drain(context.Context) error {
	p.paused = true
	return nil
}
func (p *commandTestPool) Terminate(_ context.Context, id string) error {
	for index, session := range p.sessions {
		if session == id {
			p.sessions = append(p.sessions[:index], p.sessions[index+1:]...)
			return nil
		}
	}
	return pool.ErrRunnerSessionNotFound
}

func TestCommandAPIEnforcesMutationBoundaryAndIdempotency(t *testing.T) {
	auth, err := consoleauth.New(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	store, err := history.Open(t.Context(), filepath.Join(t.TempDir(), "history.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	controls := runtimecontrol.NewRegistry()
	controls.SetPools([]runtimecontrol.PoolController{&commandTestPool{name: "linux"}})
	handler := New(Options{
		Auth: auth, LegacyAPI: http.NotFoundHandler(), UI: http.NotFoundHandler(),
		Listen: "127.0.0.1:9092", HostID: "host", Commands: store, Controls: controls,
	})
	cookie := pairSession(t, handler, auth)
	sessionResponse := authenticatedRequest(t, handler, cookie, "/api/v1/session")
	var session consoleauth.Session
	if err := json.Unmarshal(sessionResponse.Body.Bytes(), &session); err != nil {
		t.Fatal(err)
	}
	if session.ActorID == "" || session.CSRFToken == "" {
		t.Fatalf("session response = %s", sessionResponse.Body.String())
	}

	body := []byte(`{"type":"pool.pause","target_type":"pool","target_id":"linux","parameters":{},"reason":"maintenance"}`)
	forbidden := commandAPIRequest(t, handler, cookie, session.CSRFToken, "key-1", body)
	forbidden.Header().Set("unused", "unused")
	if forbidden.Code != http.StatusAccepted {
		t.Fatalf("valid command = %d %s", forbidden.Code, forbidden.Body.String())
	}
	var created control.Command
	if err := json.Unmarshal(forbidden.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.State != control.StateQueued || created.ActorID != session.ActorID {
		t.Fatalf("created command = %+v", created)
	}

	duplicate := commandAPIRequest(t, handler, cookie, session.CSRFToken, "key-1", body)
	if duplicate.Code != http.StatusOK {
		t.Fatalf("duplicate command = %d %s", duplicate.Code, duplicate.Body.String())
	}
	var replayed control.Command
	if err := json.Unmarshal(duplicate.Body.Bytes(), &replayed); err != nil {
		t.Fatal(err)
	}
	if replayed.ID != created.ID {
		t.Fatalf("duplicate ID = %q, want %q", replayed.ID, created.ID)
	}

	mismatch := []byte(`{"type":"pool.pause","target_type":"pool","target_id":"linux","parameters":{"different":true},"reason":"maintenance"}`)
	conflict := commandAPIRequest(t, handler, cookie, session.CSRFToken, "key-1", mismatch)
	if conflict.Code != http.StatusConflict ||
		!strings.Contains(conflict.Body.String(), `"idempotency_conflict"`) {
		t.Fatalf("idempotency conflict = %d %s", conflict.Code, conflict.Body.String())
	}

	read := authenticatedRequest(t, handler, cookie, "/api/v1/commands/"+created.ID)
	if read.Code != http.StatusOK || !strings.Contains(read.Body.String(), `"state":"queued"`) {
		t.Fatalf("command read = %d %s", read.Code, read.Body.String())
	}
}

func TestCommandAPIRejectsOriginCSRFConfirmationAndUnknownFields(t *testing.T) {
	auth, err := consoleauth.New(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	store, err := history.Open(t.Context(), filepath.Join(t.TempDir(), "history.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	controls := runtimecontrol.NewRegistry()
	controls.SetPools([]runtimecontrol.PoolController{&commandTestPool{name: "linux"}})
	handler := New(Options{
		Auth: auth, LegacyAPI: http.NotFoundHandler(), UI: http.NotFoundHandler(),
		Listen: "127.0.0.1:9092", HostID: "host", Commands: store, Controls: controls,
	})
	cookie := pairSession(t, handler, auth)
	sessionResponse := authenticatedRequest(t, handler, cookie, "/api/v1/session")
	var session consoleauth.Session
	if err := json.Unmarshal(sessionResponse.Body.Bytes(), &session); err != nil {
		t.Fatal(err)
	}

	body := []byte(`{"type":"pool.drain","target_type":"pool","target_id":"linux","parameters":{},"reason":"maintenance","confirmation":"wrong"}`)
	request := httptest.NewRequest(http.MethodPost, "/api/v1/commands", bytes.NewReader(body))
	request.Host = "127.0.0.1:9092"
	request.Header.Set("Origin", "http://evil.invalid")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", "key-origin")
	request.Header.Set("X-CSRF-Token", session.CSRFToken)
	addPairedSession(request, cookie)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("cross-origin command = %d %s", response.Code, response.Body.String())
	}

	wrongConfirmation := commandAPIRequest(t, handler, cookie, session.CSRFToken, "key-confirm", body)
	if wrongConfirmation.Code != http.StatusBadRequest {
		t.Fatalf("wrong confirmation = %d %s", wrongConfirmation.Code, wrongConfirmation.Body.String())
	}
	unknownField := []byte(`{"type":"pool.pause","target_type":"pool","target_id":"linux","parameters":{},"reason":"","extra":true}`)
	unknown := commandAPIRequest(t, handler, cookie, session.CSRFToken, "key-extra", unknownField)
	if unknown.Code != http.StatusBadRequest {
		t.Fatalf("unknown field = %d %s", unknown.Code, unknown.Body.String())
	}
}

func TestSensitiveCommandRequiresRecentPairing(t *testing.T) {
	current := time.Unix(1000, 0)
	auth, err := consoleauth.NewWithClock(make([]byte, 32), func() time.Time { return current })
	if err != nil {
		t.Fatal(err)
	}
	store, err := history.Open(t.Context(), filepath.Join(t.TempDir(), "history.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	controls := runtimecontrol.NewRegistry()
	controls.SetPools([]runtimecontrol.PoolController{&commandTestPool{name: "linux"}})
	handler := New(Options{
		Auth: auth, LegacyAPI: http.NotFoundHandler(), UI: http.NotFoundHandler(),
		Listen: "127.0.0.1:9092", HostID: "host", Commands: store, Controls: controls,
	})
	paired := pairSession(t, handler, auth)
	sessionResponse := authenticatedRequest(t, handler, paired, "/api/v1/session")
	var session consoleauth.Session
	if err := json.Unmarshal(sessionResponse.Body.Bytes(), &session); err != nil {
		t.Fatal(err)
	}
	current = current.Add(11 * time.Minute)
	sensitive := []byte(`{"type":"pool.drain","target_type":"pool","target_id":"linux","parameters":{},"reason":"maintenance","confirmation":"drain linux"}`)
	response := commandAPIRequest(t, handler, paired, session.CSRFToken, "stale-sensitive", sensitive)
	if response.Code != http.StatusForbidden ||
		!strings.Contains(response.Body.String(), `"code":"step_up_required"`) ||
		!strings.Contains(response.Body.String(), `"pairing_required":true`) {
		t.Fatalf("stale sensitive command = %d %s", response.Code, response.Body.String())
	}

	ordinary := []byte(`{"type":"pool.pause","target_type":"pool","target_id":"linux","parameters":{},"reason":"maintenance"}`)
	response = commandAPIRequest(t, handler, paired, session.CSRFToken, "stale-ordinary", ordinary)
	if response.Code != http.StatusAccepted {
		t.Fatalf("stale ordinary command = %d %s", response.Code, response.Body.String())
	}
}

func TestCommandPreviewEnforcesSecurityValidationAndCapabilities(t *testing.T) {
	current := time.Unix(1000, 0)
	auth, err := consoleauth.NewWithClock(make([]byte, 32), func() time.Time { return current })
	if err != nil {
		t.Fatal(err)
	}
	store, err := history.Open(t.Context(), filepath.Join(t.TempDir(), "history.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	controls := runtimecontrol.NewRegistry()
	controls.SetPools([]runtimecontrol.PoolController{&commandTestPool{name: "linux"}})
	handler := New(Options{
		Auth: auth, LegacyAPI: http.NotFoundHandler(), UI: http.NotFoundHandler(),
		Listen: "127.0.0.1:9092", HostID: "host", Commands: store, Controls: controls,
	})
	paired := pairSession(t, handler, auth)
	sessionResponse := authenticatedRequest(t, handler, paired, "/api/v1/session")
	var session consoleauth.Session
	if err := json.Unmarshal(sessionResponse.Body.Bytes(), &session); err != nil {
		t.Fatal(err)
	}

	valid := []byte(`{"type":"pool.pause","target_type":"pool","target_id":"linux","parameters":{}}`)
	response := commandPreviewRequest(t, handler, paired, session.CSRFToken, valid)
	if response.Code != http.StatusOK ||
		!strings.Contains(response.Body.String(), `"conflict_domain":"pool:linux"`) {
		t.Fatalf("valid preview = %d %s", response.Code, response.Body.String())
	}

	response = commandPreviewRequest(t, handler, paired, "wrong-csrf", valid)
	if response.Code != http.StatusForbidden ||
		!strings.Contains(response.Body.String(), `"code":"csrf_failed"`) {
		t.Fatalf("CSRF preview = %d %s", response.Code, response.Body.String())
	}

	invalid := []byte(`{"type":"pool.pause","target_type":"runner","target_id":"linux","parameters":{}}`)
	response = commandPreviewRequest(t, handler, paired, session.CSRFToken, invalid)
	if response.Code != http.StatusBadRequest ||
		!strings.Contains(response.Body.String(), `"code":"validation_failed"`) {
		t.Fatalf("invalid preview = %d %s", response.Code, response.Body.String())
	}

	current = current.Add(11 * time.Minute)
	sensitive := []byte(`{"type":"pool.drain","target_type":"pool","target_id":"linux","parameters":{}}`)
	response = commandPreviewRequest(t, handler, paired, session.CSRFToken, sensitive)
	if response.Code != http.StatusForbidden ||
		!strings.Contains(response.Body.String(), `"code":"step_up_required"`) {
		t.Fatalf("stale preview = %d %s", response.Code, response.Body.String())
	}

	current = current.Add(time.Second)
	fresh := pairSession(t, handler, auth)
	freshSessionResponse := authenticatedRequest(t, handler, fresh, "/api/v1/session")
	if err := json.Unmarshal(freshSessionResponse.Body.Bytes(), &session); err != nil {
		t.Fatal(err)
	}
	unsupported := []byte(`{"type":"history.sync","target_type":"system","target_id":"history","parameters":{}}`)
	response = commandPreviewRequest(t, handler, fresh, session.CSRFToken, unsupported)
	if response.Code != http.StatusConflict ||
		!strings.Contains(response.Body.String(), `"code":"capability_unsupported"`) {
		t.Fatalf("unsupported preview = %d %s", response.Code, response.Body.String())
	}
}

func commandPreviewRequest(
	t *testing.T, handler http.Handler, session pairedSession, csrf string, body []byte,
) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(
		http.MethodPost, "/api/v1/commands/preview", bytes.NewReader(body),
	)
	request.Host = "127.0.0.1:9092"
	request.Header.Set("Origin", "http://127.0.0.1:9092")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-CSRF-Token", csrf)
	addPairedSession(request, session)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func commandAPIRequest(
	t *testing.T, handler http.Handler, session pairedSession, csrf, key string, body []byte,
) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "/api/v1/commands", bytes.NewReader(body))
	request.Host = "127.0.0.1:9092"
	request.RemoteAddr = "127.0.0.1:50000"
	request.Header.Set("Origin", "http://127.0.0.1:9092")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", key)
	request.Header.Set("X-CSRF-Token", csrf)
	addPairedSession(request, session)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}
