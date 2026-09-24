// Package metrics exposes Prometheus metrics + a health endpoint and provides
// pool lifecycle hooks that update them.
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
)

// Metrics holds the registry and instruments.
type Metrics struct {
	reg    *prometheus.Registry
	active *prometheus.GaugeVec
	jobs   *prometheus.CounterVec
	reprov *prometheus.CounterVec

	healthMu         sync.RWMutex
	requiredSessions map[string]bool
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
	reg.MustRegister(active, jobs, reprov)
	return &Metrics{
		reg: reg, active: active, jobs: jobs, reprov: reprov,
		requiredSessions: make(map[string]bool),
	}
}

// Hooks returns pool lifecycle hooks that update the metrics.
func (m *Metrics) Hooks() pool.Hooks {
	return pool.Hooks{
		OnStart: func(p string) { m.active.WithLabelValues(p).Inc() },
		OnStop: func(p string, code int, err error) {
			m.active.WithLabelValues(p).Dec()
			result := "success"
			if err != nil {
				result = "error"
				m.reprov.WithLabelValues(p).Inc()
			}
			m.jobs.WithLabelValues(p, result).Inc()
		},
	}
}

// SetRequiredSessionAvailable records whether a required scale-set session is
// currently able to serve work.
func (m *Metrics) SetRequiredSessionAvailable(name string, available bool) {
	m.healthMu.Lock()
	defer m.healthMu.Unlock()
	m.requiredSessions[name] = available
}

func (m *Metrics) healthy() bool {
	m.healthMu.RLock()
	defer m.healthMu.RUnlock()
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
