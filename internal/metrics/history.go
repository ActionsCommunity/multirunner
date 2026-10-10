package metrics

import (
	"context"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/GerardSmit/multirunner/internal/history"
)

const historyCollectTimeout = 2 * time.Second

type historySummarySource interface {
	Summary(context.Context, string) (history.Summary, error)
}

type historyCollector struct {
	source  historySummarySource
	records *prometheus.Desc
	pending *prometheus.Desc
}

func newHistoryCollector(source historySummarySource) *historyCollector {
	return &historyCollector{
		source: source,
		records: prometheus.NewDesc(
			"multirunner_history_records",
			"Persisted history records by kind.",
			[]string{"kind"},
			nil,
		),
		pending: prometheus.NewDesc(
			"multirunner_history_pending_sessions",
			"Runner sessions without a terminal lifecycle event.",
			nil,
			nil,
		),
	}
}

func (c *historyCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.records
	ch <- c.pending
}

func (c *historyCollector) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), historyCollectTimeout)
	defer cancel()
	summary, err := c.source.Summary(ctx, "")
	if err != nil {
		return
	}
	for kind, count := range map[string]int64{
		"runner_sessions":    summary.RunnerSessions,
		"workflow_runs":      summary.WorkflowRuns,
		"workflow_jobs":      summary.WorkflowJobs,
		"workflow_steps":     summary.WorkflowSteps,
		"webhook_deliveries": summary.WebhookDeliveries,
	} {
		ch <- prometheus.MustNewConstMetric(c.records, prometheus.GaugeValue, float64(count), kind)
	}
	ch <- prometheus.MustNewConstMetric(c.pending, prometheus.GaugeValue, float64(summary.PendingSessions))
}

// EnableHistory registers metrics backed by the durable history store.
func (m *Metrics) EnableHistory(source historySummarySource) {
	m.historyAPICalls = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "multirunner_history_api_requests_total",
		Help: "GitHub history API requests by repository and HTTP status.",
	}, []string{"repo", "status"})
	m.historySyncErrors = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "multirunner_history_sync_errors_total",
		Help: "History synchronization failures by repository.",
	}, []string{"repo"})
	m.historyLastSuccess = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "multirunner_history_last_success_timestamp_seconds",
		Help: "Unix timestamp of the last successful history synchronization checkpoint.",
	}, []string{"repo"})
	m.historyBackfillComplete = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "multirunner_history_backfill_complete",
		Help: "Whether the available GitHub history backfill is complete.",
	}, []string{"repo"})
	m.reg.MustRegister(
		newHistoryCollector(source),
		m.historyAPICalls,
		m.historySyncErrors,
		m.historyLastSuccess,
		m.historyBackfillComplete,
	)
}

// ObserveHistoryRepository initializes repository-scoped history gauges.
func (m *Metrics) ObserveHistoryRepository(repository string) {
	m.historyLastSuccess.WithLabelValues(repository).Set(0)
	m.historyBackfillComplete.WithLabelValues(repository).Set(0)
}

// ObserveHistoryAPIRequest records one GitHub history API response.
func (m *Metrics) ObserveHistoryAPIRequest(repository string, status int) {
	label := "error"
	if status > 0 {
		label = strconv.Itoa(status)
	}
	m.historyAPICalls.WithLabelValues(repository, label).Inc()
}

// ObserveHistorySyncSuccess records a durable synchronization checkpoint.
func (m *Metrics) ObserveHistorySyncSuccess(repository string, state history.SyncState) {
	if state.LastSuccessAt != nil {
		m.historyLastSuccess.WithLabelValues(repository).Set(float64(state.LastSuccessAt.Unix()))
	}
	if state.BackfillComplete {
		m.historyBackfillComplete.WithLabelValues(repository).Set(1)
	} else {
		m.historyBackfillComplete.WithLabelValues(repository).Set(0)
	}
}

// ObserveHistorySyncError records a failed repository synchronization pass.
func (m *Metrics) ObserveHistorySyncError(repository string) {
	m.historySyncErrors.WithLabelValues(repository).Inc()
}
