package metrics

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
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
