package consoleapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/GerardSmit/multirunner/internal/update"
)

type fakeUpdateInspector struct {
	inspection update.Inspection
	err        error
}

type fakeUpdateReader struct{}

func (fakeUpdateReader) ListUpdates(context.Context, int) ([]update.Metadata, error) {
	return []update.Metadata{{ID: "update-1", Version: "v1.2.0"}}, nil
}

func (i fakeUpdateInspector) Inspect(context.Context) (update.Inspection, error) {
	return i.inspection, i.err
}

func TestUpdateInspectionHandlerReportsTrustGate(t *testing.T) {
	handler := updateInspectionHandler(fakeUpdateInspector{
		inspection: update.Inspection{
			CheckedAt:       time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC),
			TrustConfigured: false, MetadataConsistent: true, Compatible: true,
			Version: "v1.2.0", ApplyAllowed: false,
			Reason: "metadata is untrusted",
		},
	})
	request := httptest.NewRequest(http.MethodGet, "/api/v1/updates/check", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	var inspection update.Inspection
	if err := json.NewDecoder(response.Body).Decode(&inspection); err != nil {
		t.Fatal(err)
	}
	if inspection.ApplyAllowed || inspection.TrustConfigured ||
		inspection.Version != "v1.2.0" {
		t.Fatalf("inspection = %+v", inspection)
	}
}

func TestPlanUpdateCommandRequiresGuardedSystemTarget(t *testing.T) {
	plan, err := planCommand(commandRequest{
		Type: "update.stage", TargetType: "system", TargetID: "updates",
		Parameters: json.RawMessage(`{"version":"v1.2.0"}`),
	})
	if err != nil {
		t.Fatal(err)
	}

	if plan.ConflictDomain != "host-maintenance" ||
		plan.ConfirmationPhrase != "stage trusted update v1.2.0" ||
		!plan.ReasonRequired {
		t.Fatalf("plan = %+v", plan)
	}
	if _, err := planCommand(commandRequest{
		Type: "update.stage", TargetType: "system", TargetID: "updates",
		Parameters: json.RawMessage(`{"version":"latest"}`),
	}); err == nil {
		t.Fatal("non-semantic update version accepted")
	}
}

func TestUpdateListUsesCollectionContract(t *testing.T) {
	response := httptest.NewRecorder()
	updateListHandler(fakeUpdateReader{}).ServeHTTP(
		response, httptest.NewRequest(http.MethodGet, "/api/v1/updates", nil),
	)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", response.Code, response.Body.String())
	}
	assertCollectionEnvelope(t, response.Body.Bytes())
}
