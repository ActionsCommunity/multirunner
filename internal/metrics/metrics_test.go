package metrics

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GerardSmit/multirunner/internal/history"
	"github.com/GerardSmit/multirunner/internal/runner"
)

func TestHealthIsDegradedWhenRequiredSessionIsUnavailable(t *testing.T) {
	metrics := New()
	metrics.SetRequiredSessionAvailable("linux", false)

	response := httptest.NewRecorder()
	metrics.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/health", nil))
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("health status = %d, want %d", response.Code, http.StatusServiceUnavailable)
	}

	metrics.SetRequiredSessionAvailable("linux", true)
	response = httptest.NewRecorder()
	metrics.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/health", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("recovered health status = %d, want %d", response.Code, http.StatusOK)
	}
}

func TestHealthStateIsConcurrencySafe(t *testing.T) {
	metrics := New()
	handler := metrics.Handler()
	var wg sync.WaitGroup
	for worker := range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for iteration := range 100 {
				name := "linux"
				if worker%2 != 0 {
					name = "windows"
				}
				metrics.SetRequiredSessionAvailable(name, iteration%2 == 0)
				response := httptest.NewRecorder()
				handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/health", nil))
				if response.Code != http.StatusOK && response.Code != http.StatusServiceUnavailable {
					t.Errorf("unexpected health status %d", response.Code)
					return
				}
			}
		}()
	}
	wg.Wait()

	metrics.SetRequiredSessionAvailable("linux", true)
	metrics.SetRequiredSessionAvailable("windows", false)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/health", nil))
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("final health status = %d, want %d", response.Code, http.StatusServiceUnavailable)
	}
}

func TestOperationalJournalLossIsVisibleInMetricsAndHealth(t *testing.T) {
	metrics := New()
	metrics.ObserveOperationalJournalRejected("queue_full")
	metrics.ObserveOperationalJournalAppendError()

	response := httptest.NewRecorder()
	metrics.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/health", nil))
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("health status = %d, want %d", response.Code, http.StatusServiceUnavailable)
	}
	body := scrapeMetrics(t, metrics)
	assertMetric(t, body, `multirunner_operational_journal_rejections_total{reason="queue_full"} 1`)
	assertMetric(t, body, `multirunner_operational_journal_append_errors_total 1`)
	assertMetric(t, body, `multirunner_operational_journal_healthy 0`)

	metrics.SetOperationalJournalAvailable(true)
	response = httptest.NewRecorder()
	metrics.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/health", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("recovered health status = %d, want %d", response.Code, http.StatusOK)
	}
}

func TestLifecycleObserverPreservesMetricsAndIgnoresUnusableLaunches(t *testing.T) {
	metrics := New()
	hooks := metrics.Hooks()
	if hooks.Observer == nil || hooks.OnStart == nil || hooks.OnStop == nil {
		t.Fatal("Hooks did not expose both structured and compatibility adapters")
	}

	hooks.Observer.ObserveRunnerLifecycle(t.Context(), runner.LifecycleEvent{
		Type: runner.LifecyclePlanned, LocalSessionID: "failed", Pool: "linux",
	})
	hooks.Observer.ObserveRunnerLifecycle(t.Context(), runner.LifecycleEvent{
		Type: runner.LifecycleFailed, LocalSessionID: "failed", Pool: "linux", Error: "jit config failed",
	})
	body := scrapeMetrics(t, metrics)
	assertMetric(t, body, `multirunner_jobs_total{pool="linux",result="error"} 1`)
	assertMetric(t, body, `multirunner_reprovision_errors_total{pool="linux"} 1`)
	if strings.Contains(body, `multirunner_runners_active{pool="linux"} -1`) {
		t.Errorf("unusable launch decremented active runners:\n%s", body)
	}

	hooks.Observer.ObserveRunnerLifecycle(t.Context(), runner.LifecycleEvent{
		Type: runner.LifecycleLaunched, LocalSessionID: "success", Pool: "linux",
	})
	body = scrapeMetrics(t, metrics)
	assertMetric(t, body, `multirunner_runners_active{pool="linux"} 1`)

	hooks.Observer.ObserveRunnerLifecycle(t.Context(), runner.LifecycleEvent{
		Type: runner.LifecycleStopped, LocalSessionID: "success", Pool: "linux", ExitCode: 17,
	})
	body = scrapeMetrics(t, metrics)
	assertMetric(t, body, `multirunner_runners_active{pool="linux"} 0`)
	assertMetric(t, body, `multirunner_jobs_total{pool="linux",result="success"} 1`)
	assertMetric(t, body, `multirunner_reprovision_errors_total{pool="linux"} 1`)
}

func TestLegacyMetricsHooksRetainExistingBehavior(t *testing.T) {
	metrics := New()
	hooks := metrics.Hooks()
	hooks.OnStart("windows")
	body := scrapeMetrics(t, metrics)
	assertMetric(t, body, `multirunner_runners_active{pool="windows"} 1`)

	hooks.OnStop("windows", -1, io.ErrUnexpectedEOF)
	body = scrapeMetrics(t, metrics)
	assertMetric(t, body, `multirunner_runners_active{pool="windows"} 0`)
	assertMetric(t, body, `multirunner_jobs_total{pool="windows",result="error"} 1`)
	assertMetric(t, body, `multirunner_reprovision_errors_total{pool="windows"} 1`)
}

type historySummaryStub struct {
	summary history.Summary
}

func (s historySummaryStub) Summary(context.Context, string) (history.Summary, error) {
	return s.summary, nil
}

func TestHistoryMetricsExposeStoreAndSynchronizationState(t *testing.T) {
	metrics := New()
	metrics.EnableHistory(historySummaryStub{summary: history.Summary{
		RunnerSessions: 3, PendingSessions: 1, WorkflowRuns: 2,
		WorkflowJobs: 4, WorkflowSteps: 9, WebhookDeliveries: 5,
	}})
	metrics.ObserveHistoryRepository("o/r")
	metrics.ObserveHistoryAPIRequest("o/r", http.StatusOK)
	metrics.ObserveHistoryAPIRequest("o/r", 0)
	metrics.ObserveHistorySyncError("o/r")
	success := time.Unix(1_799_999_999, 0).UTC()
	metrics.ObserveHistorySyncSuccess("o/r", history.SyncState{
		LastSuccessAt: &success, BackfillComplete: true,
	})

	body := scrapeMetrics(t, metrics)
	for _, want := range []string{
		`multirunner_history_records{kind="runner_sessions"} 3`,
		`multirunner_history_records{kind="workflow_runs"} 2`,
		`multirunner_history_records{kind="workflow_jobs"} 4`,
		`multirunner_history_records{kind="workflow_steps"} 9`,
		`multirunner_history_records{kind="webhook_deliveries"} 5`,
		`multirunner_history_pending_sessions 1`,
		`multirunner_history_api_requests_total{repo="o/r",status="200"} 1`,
		`multirunner_history_api_requests_total{repo="o/r",status="error"} 1`,
		`multirunner_history_sync_errors_total{repo="o/r"} 1`,
		`multirunner_history_last_success_timestamp_seconds{repo="o/r"} 1.799999999e+09`,
		`multirunner_history_backfill_complete{repo="o/r"} 1`,
	} {
		assertMetric(t, body, want)
	}
}

func TestConsoleSSEMetricsExposeActiveAndRejectedStreams(t *testing.T) {
	metrics := New()
	metrics.ObserveConsoleStreamOpened()
	metrics.ObserveConsoleStreamRejected("host_limit")

	body := scrapeMetrics(t, metrics)
	assertMetric(t, body, `multirunner_console_sse_active 1`)
	assertMetric(t, body,
		`multirunner_console_sse_rejected_total{reason="host_limit"} 1`)

	metrics.ObserveConsoleStreamClosed()
	body = scrapeMetrics(t, metrics)
	assertMetric(t, body, `multirunner_console_sse_active 0`)
}

func scrapeMetrics(t *testing.T, metrics *Metrics) string {
	t.Helper()
	response := httptest.NewRecorder()
	metrics.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("metrics status = %d, want %d", response.Code, http.StatusOK)
	}
	body, err := io.ReadAll(response.Result().Body)
	if err != nil {
		t.Fatalf("read metrics: %v", err)
	}
	return string(body)
}

func assertMetric(t *testing.T, body, want string) {
	t.Helper()
	if !strings.Contains(body, want) {
		t.Errorf("metrics missing %q:\n%s", want, body)
	}
}
