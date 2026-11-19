package main

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/GerardSmit/multirunner/internal/config"
	"github.com/GerardSmit/multirunner/internal/consoleauth"
	"github.com/GerardSmit/multirunner/internal/metrics"
	"github.com/GerardSmit/multirunner/internal/runtimecontrol"
)

func TestDegradedReadOnlyHandlerDisablesMutations(t *testing.T) {
	var forwarded bool
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		forwarded = true
		w.WriteHeader(http.StatusNoContent)
	})
	handler := degradedReadOnlyHandler(next, degradedConsoleState{
		Listen: "127.0.0.1:8081", Reason: "command_initialization_failed",
	})

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/system", nil))
	if response.Code != http.StatusOK ||
		!strings.Contains(response.Body.String(), `"status":"degraded"`) ||
		!strings.Contains(response.Body.String(), `"mutations_enabled":false`) {
		t.Fatalf("system response = %d %s", response.Code, response.Body.String())
	}

	response = httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/v1/commands", nil))
	if response.Code != http.StatusServiceUnavailable ||
		!strings.Contains(response.Body.String(), `"code":"degraded_read_only"`) {
		t.Fatalf("mutation response = %d %s", response.Code, response.Body.String())
	}
	if forwarded {
		t.Fatal("mutation reached the underlying console handler")
	}

	response = httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/configuration", nil))
	if response.Code != http.StatusNoContent || !forwarded {
		t.Fatalf("read response = %d, forwarded=%v", response.Code, forwarded)
	}
}

func TestOperationsConsoleCapabilitiesDefaultFailClosed(t *testing.T) {
	registry := runtimecontrol.NewRegistry()
	configureConsoleCapabilityGates(
		registry, config.OperationsConsoleCapabilities{},
	)
	for _, capability := range registry.Capabilities() {
		if capability.Supported {
			t.Fatalf("capability enabled by default: %+v", capability)
		}
		if !strings.Contains(
			capability.Reason,
			"history.operations_console.capabilities",
		) {
			t.Fatalf("capability lacks configuration reason: %+v", capability)
		}
	}
}

func TestSetupHistoryFailureKeepsAuthoritativeRuntimeAvailable(t *testing.T) {
	root := t.TempDir()
	databasePath := filepath.Join(root, "history.db")
	if err := os.Mkdir(databasePath, 0o700); err != nil {
		t.Fatal(err)
	}

	configPath := filepath.Join(root, "config.yaml")
	if err := os.WriteFile(configPath, []byte("history:\n  enabled: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{}
	cfg.History.Enabled = true
	cfg.History.DatabasePath = databasePath
	cfg.History.Listen = "127.0.0.1:0"
	observability := metrics.New()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx, cancel := context.WithCancel(context.Background())

	runtime, err := setupHistory(ctx, configPath, cfg, nil, observability, logger)
	if err != nil {
		cancel()
		t.Fatalf("setupHistory returned a runner-fatal error: %v", err)
	}
	if runtime != nil {
		cancel()
		t.Fatal("persistent history runtime unexpectedly initialized")
	}
	response := httptest.NewRecorder()
	observability.Handler().ServeHTTP(
		response, httptest.NewRequest(http.MethodGet, "/health", nil),
	)
	if response.Code != http.StatusServiceUnavailable {
		cancel()
		t.Fatalf("health status = %d, want degraded", response.Code)
	}
	cancel()
	time.Sleep(20 * time.Millisecond)
}

func TestDegradedConsoleStateRemainsAuthenticated(t *testing.T) {
	authenticator, err := consoleauth.New(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	handler := authenticator.Handler(degradedConsoleSecurityHeaders(
		degradedReadOnlyHandler(http.NotFoundHandler(), degradedConsoleState{
			Listen: "127.0.0.1:8081", Reason: "console_persistence_unavailable",
		}),
	))

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/system", nil))
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous system status = %d, want 401", response.Code)
	}

	token, err := authenticator.PairingToken()
	if err != nil {
		t.Fatal(err)
	}
	response = httptest.NewRecorder()
	pairRequest := httptest.NewRequest(
		http.MethodPost,
		"http://127.0.0.1:8081/auth/pair",
		strings.NewReader(`{"token":`+strconv.Quote(token)+`}`),
	)
	pairRequest.Header.Set("Content-Type", "application/json")
	pairRequest.Header.Set("Origin", "http://127.0.0.1:8081")
	pairRequest.Header.Set("Sec-Fetch-Site", "same-origin")
	handler.ServeHTTP(
		response,
		pairRequest,
	)
	if response.Code != http.StatusNoContent {
		t.Fatalf("pairing status = %d, want 204", response.Code)
	}
	cookies := response.Result().Cookies()
	if len(cookies) != 2 {
		t.Fatalf("pairing cookies = %d, want 2", len(cookies))
	}
	var sessionCookie *http.Cookie
	proof := ""
	for _, cookie := range cookies {
		if cookie.HttpOnly {
			sessionCookie = cookie
		} else {
			proof = cookie.Value
		}
	}
	if sessionCookie == nil || proof == "" {
		t.Fatalf("unexpected pairing cookies: %#v", cookies)
	}

	request := httptest.NewRequest(http.MethodGet, "/api/v1/system", nil)
	request.AddCookie(sessionCookie)
	request.Header.Set(consoleauth.ProofHeader, proof)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK ||
		!strings.Contains(response.Body.String(), `"status":"degraded"`) {
		t.Fatalf("paired system response = %d %s", response.Code, response.Body.String())
	}
}
