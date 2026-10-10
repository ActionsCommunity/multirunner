package consoleapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/GerardSmit/multirunner/internal/alerts"
	"github.com/GerardSmit/multirunner/internal/consoleauth"
	"github.com/GerardSmit/multirunner/internal/history"
	"github.com/GerardSmit/multirunner/internal/runtimecontrol"
)

type contractResources struct{}

func (contractResources) ListWorkflowRuns(
	context.Context, history.ListOptions,
) ([]history.WorkflowRun, error) {
	return []history.WorkflowRun{
		{Repository: "actionscommunity/multirunner", WorkflowName: "CI"},
		{Repository: "actionscommunity/multirunner", WorkflowName: "Release"},
	}, nil
}

func (contractResources) ListWorkflowJobs(
	context.Context, history.ListOptions,
) ([]history.WorkflowJob, error) {
	return []history.WorkflowJob{{Labels: []string{"self-hosted", "linux"}}}, nil
}

func (contractResources) ListRunnerSessions(
	context.Context, history.ListOptions,
) ([]history.RunnerSession, error) {
	return []history.RunnerSession{{ID: "session-1", PoolName: "linux"}}, nil
}

func (contractResources) ListAlertAnnotations(
	context.Context, string, int, int,
) ([]alerts.Annotation, error) {
	return []alerts.Annotation{{ID: "annotation-1", AlertID: "alert-1"}}, nil
}

func (contractResources) ListAuditRecords(
	context.Context, int, int,
) ([]history.AuditRecord, error) {
	return []history.AuditRecord{{ID: "audit-1", Source: "command"}}, nil
}

func TestContractSchemaDefinesEveryRequiredResourceGroup(t *testing.T) {
	var schema struct {
		Definitions map[string]json.RawMessage `json:"$defs"`
		Groups      []string                   `json:"x-resource-groups"`
		Routes      []struct {
			Method string `json:"method"`
			Path   string `json:"path"`
		} `json:"x-routes"`
		SSE         struct {
			HostLimit    int `json:"host_stream_limit"`
			SessionLimit int `json:"session_stream_limit"`
			Lifetime     int `json:"maximum_lifetime_seconds"`
		} `json:"x-sse"`
	}
	if err := json.Unmarshal(ContractSchema(), &schema); err != nil {
		t.Fatal(err)
	}
	if len(schema.Groups) != 26 {
		t.Fatalf("resource groups = %d, want 26", len(schema.Groups))
	}
	for _, definition := range []string{
		"error", "collection", "fieldViolation", "system", "session",
		"event", "commandRequest", "commandPlan", "command",
	} {
		if len(schema.Definitions[definition]) == 0 {
			t.Errorf("missing schema definition %q", definition)
		}
	}
	routes := make(map[string]struct{}, len(schema.Routes))
	for _, route := range schema.Routes {
		routes[route.Method+" "+route.Path] = struct{}{}
	}
	for _, route := range []string{
		"GET /api/v1/session",
		"GET /api/v1/restores",
		"POST /api/v1/exports",
		"GET /api/v1/exports/{id}",
		"GET /api/v1/support-bundles/{id}",
		"GET /api/v1/jobs/{id}/log",
	} {
		if _, ok := routes[route]; !ok {
			t.Errorf("contract is missing registered route %q", route)
		}
	}
	if schema.SSE.HostLimit != DefaultHostStreamLimit ||
		schema.SSE.SessionLimit != DefaultSessionStreamLimit ||
		schema.SSE.Lifetime != int(DefaultStreamMaximumLifetime/time.Second) {
		t.Fatalf("SSE contract = %+v", schema.SSE)
	}
}

func TestRequiredCompatibilityResourceRoutesUseTypedCollections(t *testing.T) {
	auth := newTestAuthenticator(t)
	handler := New(Options{
		Auth: auth, LegacyAPI: http.NotFoundHandler(), UI: http.NotFoundHandler(),
		HostID: "host-1", HostEpoch: "epoch-1", Resources: contractResources{},
	})
	cookie := pairSession(t, handler, auth)
	for _, route := range []string{
		"/api/v1/hosts", "/api/v1/pools", "/api/v1/sessions",
		"/api/v1/repositories", "/api/v1/workflows", "/api/v1/annotations",
		"/api/v1/tags", "/api/v1/audit",
	} {
		response := authenticatedRequest(t, handler, cookie, route)
		if response.Code != http.StatusOK {
			t.Errorf("%s status = %d body=%s", route, response.Code, response.Body.String())
			continue
		}
		for _, field := range []string{
			`"items"`, `"applied_filters"`, `"cursor"`, `"next_cursor"`, `"count"`,
		} {
			if !strings.Contains(response.Body.String(), field) {
				t.Errorf("%s missing %s: %s", route, field, response.Body.String())
			}
		}
	}
}

func TestVersionedMethodAndNotFoundErrorsAreJSON(t *testing.T) {
	auth := newTestAuthenticator(t)
	handler := New(Options{
		Auth: auth, LegacyAPI: http.NotFoundHandler(), UI: http.NotFoundHandler(),
	})
	cookie := pairSession(t, handler, auth)
	for _, test := range []struct {
		method string
		path   string
		status int
		code   string
	}{
		{http.MethodPost, "/api/v1/system", http.StatusMethodNotAllowed, "method_not_allowed"},
		{http.MethodGet, "/api/v1/not-a-route", http.StatusNotFound, "not_found"},
	} {
		request := httptest.NewRequest(test.method, test.path, nil)
		addPairedSession(request, cookie)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != test.status ||
			!strings.Contains(response.Header().Get("Content-Type"), "application/json") ||
			!strings.Contains(response.Body.String(), test.code) ||
			!strings.Contains(response.Body.String(), `"field_violations":[]`) ||
			!strings.Contains(response.Body.String(), `"retry":null`) {
			t.Errorf("%s %s response = %d %v %s",
				test.method, test.path, response.Code, response.Header(), response.Body.String())
		}
	}
}

func TestControlValidationDistinguishesInvalidFromUnsupported(t *testing.T) {
	invalid := httptest.NewRecorder()
	writeControlValidationError(invalid, errors.New("invalid backup purpose"))
	if invalid.Code != http.StatusBadRequest ||
		!strings.Contains(invalid.Body.String(), `"validation_failed"`) {
		t.Fatalf("invalid response = %d %s", invalid.Code, invalid.Body.String())
	}
	unsupported := httptest.NewRecorder()
	writeControlValidationError(unsupported, runtimecontrol.UnsupportedCapabilityReason(
		"backup.create", "disabled by operator configuration",
	))
	if unsupported.Code != http.StatusConflict ||
		!strings.Contains(unsupported.Body.String(), `"capability_unsupported"`) ||
		!strings.Contains(unsupported.Body.String(), "disabled by operator configuration") {
		t.Fatalf("unsupported response = %d %s", unsupported.Code, unsupported.Body.String())
	}
}

func TestUpdateInspectionWithoutConfigurationIsTyped(t *testing.T) {
	response := httptest.NewRecorder()
	updateInspectionHandler(nil).ServeHTTP(
		response, httptest.NewRequest(http.MethodGet, "/api/v1/updates/check", nil),
	)
	if response.Code != http.StatusOK ||
		!strings.Contains(response.Body.String(), `"apply_allowed":false`) ||
		!strings.Contains(response.Body.String(), `"reason":"update metadata URL is not configured"`) {
		t.Fatalf("response = %d %s", response.Code, response.Body.String())
	}
}

func newTestAuthenticator(t *testing.T) *consoleauth.Authenticator {
	t.Helper()
	auth, err := consoleauth.New(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	return auth
}
