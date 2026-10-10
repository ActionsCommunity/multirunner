// Package metrics exposes Prometheus metrics + a health endpoint and adapts
// structured runner lifecycle events into the existing metric series.
package metrics

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/GerardSmit/multirunner/internal/pool"
	"github.com/GerardSmit/multirunner/internal/runner"
)

// Metrics holds the registry and instruments.
type Metrics struct {
	reg    *prometheus.Registry
	active *prometheus.GaugeVec
	jobs   *prometheus.CounterVec
	reprov *prometheus.CounterVec

	operationalJournalRejections *prometheus.CounterVec
	operationalJournalErrors     prometheus.Counter
	operationalJournalHealthy    prometheus.Gauge
	consoleSSEActive             prometheus.Gauge
	consoleSSERejected           *prometheus.CounterVec

	historyAPICalls         *prometheus.CounterVec
	historySyncErrors       *prometheus.CounterVec
	historyLastSuccess      *prometheus.GaugeVec
	historyBackfillComplete *prometheus.GaugeVec

	lifecycleMu sync.Mutex
	launched    map[string]bool

	healthMu         sync.RWMutex
	requiredSessions map[string]bool
	journalAvailable bool
}

// New builds the metrics set.
func New() *Metrics {
	reg := prometheus.NewRegistry()
	active := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "multirunner_runners_active", Help: "Currently running ephemeral runners.",
	}, []string{"pool"})
	jobs := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "multirunner_jobs_total", Help: "Ephemeral runners that completed (one job each).",
	}, []string{"pool", "result"})
	reprov := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "multirunner_reprovision_errors_total", Help: "Runner launch/JIT errors.",
	}, []string{"pool"})
	journalRejections := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "multirunner_operational_journal_rejections_total",
		Help: "Best-effort operational events rejected before durable append.",
	}, []string{"reason"})
	journalErrors := prometheus.NewCounter(prometheus.CounterOpts{
		Name: "multirunner_operational_journal_append_errors_total",
		Help: "Operational journal durable append failures.",
	})
	journalHealthy := prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "multirunner_operational_journal_healthy",
		Help: "Whether the operational journal has initialized without detected event loss.",
	})
	journalHealthy.Set(1)
	sseActive := prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "multirunner_console_sse_active",
		Help: "Currently active authenticated Operations Console SSE streams.",
	})
	sseRejected := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "multirunner_console_sse_rejected_total",
		Help: "Operations Console SSE streams rejected by admission control.",
	}, []string{"reason"})
	reg.MustRegister(
		active, jobs, reprov, journalRejections, journalErrors, journalHealthy,
		sseActive, sseRejected,
	)
	return &Metrics{
		reg: reg, active: active, jobs: jobs, reprov: reprov,
		operationalJournalRejections: journalRejections,
		operationalJournalErrors:     journalErrors,
		operationalJournalHealthy:    journalHealthy,
		consoleSSEActive:             sseActive,
		consoleSSERejected:           sseRejected,
		launched:                     make(map[string]bool),
		requiredSessions:             make(map[string]bool),
		journalAvailable:             true,
	}
}

func (m *Metrics) ObserveConsoleStreamOpened() {
	m.consoleSSEActive.Inc()
}

func (m *Metrics) ObserveConsoleStreamClosed() {
	m.consoleSSEActive.Dec()
}

func (m *Metrics) ObserveConsoleStreamRejected(reason string) {
	m.consoleSSERejected.WithLabelValues(reason).Inc()
}

// Hooks returns a compatibility wrapper carrying the structured metrics
// observer. Existing composition can keep passing pool.Hooks unchanged.
func (m *Metrics) Hooks() pool.Hooks {
	return pool.Hooks{
		Observer: m,
		OnStart: func(poolName string) {
			m.active.WithLabelValues(poolName).Inc()
		},
		OnStop: func(poolName string, _ int, err error) {
			m.active.WithLabelValues(poolName).Dec()
			result := "success"
			if err != nil {
				result = "error"
				m.reprov.WithLabelValues(poolName).Inc()
			}
			m.jobs.WithLabelValues(poolName, result).Inc()
		},
	}
}

// ObserveRunnerLifecycle implements runner.LifecycleObserver while preserving
// the existing metric names and terminal-result semantics.
func (m *Metrics) ObserveRunnerLifecycle(
	_ context.Context, event runner.LifecycleEvent,
) {
	m.lifecycleMu.Lock()
	defer m.lifecycleMu.Unlock()

	switch event.Type {
	case runner.LifecycleLaunched:
		if !m.launched[event.LocalSessionID] {
			m.launched[event.LocalSessionID] = true
			m.active.WithLabelValues(event.Pool).Inc()
		}
	case runner.LifecycleStopped, runner.LifecycleFailed:
		if m.launched[event.LocalSessionID] {
			delete(m.launched, event.LocalSessionID)
			m.active.WithLabelValues(event.Pool).Dec()
		}
		result := "success"
		if event.Type == runner.LifecycleFailed {
			result = "error"
			m.reprov.WithLabelValues(event.Pool).Inc()
		}
		m.jobs.WithLabelValues(event.Pool, result).Inc()
	}
}

// SetRequiredSessionAvailable records whether a required scale-set session is
// currently able to serve work.
func (m *Metrics) SetRequiredSessionAvailable(name string, available bool) {
	m.healthMu.Lock()
	defer m.healthMu.Unlock()
	m.requiredSessions[name] = available
}

// SetOperationalJournalAvailable exposes console persistence degradation
// without making runner provisioning depend on the console.
func (m *Metrics) SetOperationalJournalAvailable(available bool) {
	m.healthMu.Lock()
	m.journalAvailable = available
	m.healthMu.Unlock()
	if available {
		m.operationalJournalHealthy.Set(1)
	} else {
		m.operationalJournalHealthy.Set(0)
	}
}

// ObserveOperationalJournalRejected records an event that never reached the
// durable journal.
func (m *Metrics) ObserveOperationalJournalRejected(reason string) {
	m.operationalJournalRejections.WithLabelValues(reason).Inc()
	m.SetOperationalJournalAvailable(false)
}

// ObserveOperationalJournalAppendError records a failed durable append.
func (m *Metrics) ObserveOperationalJournalAppendError() {
	m.operationalJournalErrors.Inc()
	m.SetOperationalJournalAvailable(false)
}

func (m *Metrics) healthy() bool {
	m.healthMu.RLock()
	defer m.healthMu.RUnlock()
	if !m.journalAvailable {
		return false
	}
	for _, available := range m.requiredSessions {
		if !available {
			return false
		}
	}
	return true
}

// Handler returns the metrics and health HTTP surface.
func (m *Metrics) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(m.reg, promhttp.HandlerOpts{}))
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		if !m.healthy() {
			http.Error(w, "degraded", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok")
	})
	return mux
}

// Serve runs the /metrics + /health endpoints until ctx is cancelled.
func (m *Metrics) Serve(ctx context.Context, listen string, logger *slog.Logger) error {
	srv := &http.Server{Addr: listen, Handler: m.Handler()}
	go func() {
		logger.Info("metrics listening", "addr", listen)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("metrics server stopped", "err", err)
		}
	}()
	<-ctx.Done()
	shutCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	return srv.Shutdown(shutCtx)
}
