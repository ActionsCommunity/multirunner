package consoleui

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHandlerServesIndexAndSPAFallback(t *testing.T) {
	handler, err := New()
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{"/", "/runs/123"} {
		request := httptest.NewRequest(http.MethodGet, target, nil)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("%s returned %d", target, response.Code)
		}
		if !strings.Contains(response.Body.String(), "Multirunner Operations Console") {
			t.Fatalf("%s did not serve the console index", target)
		}
		if response.Header().Get("Cache-Control") != "no-cache" {
			t.Fatalf("%s did not disable index caching", target)
		}
	}
}

func TestHandlerDoesNotFallbackForAPIOrMissingAssets(t *testing.T) {
	handler, err := New()
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{"/api/v1/system", "/assets/missing.js"} {
		request := httptest.NewRequest(http.MethodGet, target, nil)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusNotFound {
			t.Fatalf("%s returned %d, want 404", target, response.Code)
		}
	}
}

func TestHandlerSecurityAndMethodPolicy(t *testing.T) {
	handler, err := New()
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST returned %d", response.Code)
	}
	if response.Header().Get("Content-Security-Policy") == "" {
		t.Fatal("content security policy is missing")
	}
}
