package main

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/GerardSmit/multirunner/internal/config"
	"github.com/GerardSmit/multirunner/internal/metrics"
	scalesetmode "github.com/GerardSmit/multirunner/internal/scaleset"
)

func TestScaleSetHealthTracksEveryRequiredSession(t *testing.T) {
	m := metrics.New()
	report := newScaleSetHealthReporter(m, []config.Pool{
		{Name: "linux"},
		{Name: "windows"},
	})

	assertHealthStatus(t, m, http.StatusServiceUnavailable)

	report("linux", scalesetmode.SessionAvailable)
	assertHealthStatus(t, m, http.StatusServiceUnavailable)

	report("windows", scalesetmode.SessionAvailable)
	assertHealthStatus(t, m, http.StatusOK)

	report("linux", scalesetmode.SessionBackingOff)
	assertHealthStatus(t, m, http.StatusServiceUnavailable)

	report("linux", scalesetmode.SessionAvailable)
	assertHealthStatus(t, m, http.StatusOK)

	report("linux", scalesetmode.SessionPermanentlyUnavailable)
	assertHealthStatus(t, m, http.StatusServiceUnavailable)
}

func TestScaleSetHealthReportsConcurrentSessionsIndependently(t *testing.T) {
	m := metrics.New()
	report := newScaleSetHealthReporter(m, []config.Pool{
		{Name: "linux"},
		{Name: "windows"},
	})

	var wg sync.WaitGroup
	for _, sessionKey := range []string{"linux", "windows"} {
		sessionKey := sessionKey
		wg.Add(1)
		go func() {
			defer wg.Done()
			report(sessionKey, scalesetmode.SessionAvailable)
		}()
	}
	wg.Wait()
	assertHealthStatus(t, m, http.StatusOK)

	report("linux", scalesetmode.SessionBackingOff)
	assertHealthStatus(t, m, http.StatusServiceUnavailable)
	report("windows", scalesetmode.SessionAvailable)
	assertHealthStatus(t, m, http.StatusServiceUnavailable)
}

func TestNonScaleSetHealthHasNoRequiredSessions(t *testing.T) {
	assertHealthStatus(t, metrics.New(), http.StatusOK)
}

func assertHealthStatus(t *testing.T, m *metrics.Metrics, want int) {
	t.Helper()
	response := httptest.NewRecorder()
	m.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/health", nil))
	if response.Code != want {
		t.Fatalf("health status = %d, want %d", response.Code, want)
	}
}
