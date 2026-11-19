package consoleapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/GerardSmit/multirunner/internal/consoleauth"
	"github.com/GerardSmit/multirunner/internal/control"
	"github.com/GerardSmit/multirunner/internal/history"
	"github.com/GerardSmit/multirunner/internal/operations"
	"github.com/GerardSmit/multirunner/internal/runtimecontrol"
)

func TestCommandRecoveryEnforcesOperatorBoundaryAndStaleState(t *testing.T) {
	current := time.Now()
	auth, err := consoleauth.NewWithClock(make([]byte, 32), func() time.Time { return current })
	if err != nil {
		t.Fatal(err)
	}
	store, err := history.Open(t.Context(), filepath.Join(t.TempDir(), "history.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	command, _, err := store.CreateCommand(t.Context(), control.Request{
		IdempotencyKey: "uncertain", Type: runtimecontrol.CommandPoolPause, HostID: "host",
		TargetType: "pool", TargetID: "linux", ConflictDomain: "pool:linux",
		ActorKind: operations.ActorOperator, ActorID: "operator",
	})
	if err != nil {
		t.Fatal(err)
	}
	command, err = store.ClaimNextCommand(t.Context(), "worker", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	command, err = store.TransitionCommand(t.Context(), command.ID, control.Transition{
		ExpectedState: control.StateClaimed, To: control.StateUnknownOutcome,
		ActorKind: operations.ActorSystem, ActorID: "worker", FencingToken: command.FencingToken,
	})
	if err != nil {
		t.Fatal(err)
	}
	handler := New(Options{Auth: auth, HostID: "host", Commands: store,
		Controls: runtimecontrol.NewRegistry(), Listen: "127.0.0.1:9092",
		LegacyAPI: http.NotFoundHandler(), UI: http.NotFoundHandler()})
	cookie := pairSession(t, handler, auth)
	var session consoleauth.Session
	if err := json.Unmarshal(authenticatedRequest(t, handler, cookie, "/api/v1/session").Body.Bytes(), &session); err != nil {
		t.Fatal(err)
	}
	route := "/api/v1/commands/" + command.ID + "/reconcile"
	body := []byte(fmt.Sprintf(`{"reason":"check current runtime evidence","confirmation":"reconcile %s","fencing_token":%d}`,
		command.ID, command.FencingToken))
	for _, test := range []struct {
		name   string
		csrf   string
		origin string
		body   []byte
		status int
	}{
		{"csrf", "wrong", "http://127.0.0.1:9092", body, http.StatusForbidden},
		{"origin", session.CSRFToken, "http://attacker.example", body, http.StatusForbidden},
		{"confirmation", session.CSRFToken, "http://127.0.0.1:9092",
			[]byte(`{"reason":"check","confirmation":"wrong","fencing_token":1}`), http.StatusBadRequest},
		{"unknown field", session.CSRFToken, "http://127.0.0.1:9092",
			[]byte(`{"reason":"check","confirmation":"wrong","fencing_token":1,"release":true}`), http.StatusBadRequest},
		{"trailing value", session.CSRFToken, "http://127.0.0.1:9092", append(append([]byte(nil), body...), []byte(` {}`)...), http.StatusBadRequest},
		{"stale fence", session.CSRFToken, "http://127.0.0.1:9092",
			[]byte(fmt.Sprintf(`{"reason":"check","confirmation":"reconcile %s","fencing_token":99}`, command.ID)), http.StatusConflict},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := newMutationRequest(cookie, test.csrf, http.MethodPost, route, "", test.body)
			request.Header.Set("Origin", test.origin)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != test.status {
				t.Fatalf("recovery = %d %s", response.Code, response.Body.String())
			}
		})
	}
	current = current.Add(11 * time.Minute)
	stale := mutationRequest(handler, cookie, session.CSRFToken, http.MethodPost, route, "", body)
	if stale.Code != http.StatusForbidden {
		t.Fatalf("stale pairing = %d %s", stale.Code, stale.Body.String())
	}
	cookie = pairSession(t, handler, auth)
	if err := json.Unmarshal(authenticatedRequest(t, handler, cookie, "/api/v1/session").Body.Bytes(), &session); err != nil {
		t.Fatal(err)
	}
	response := mutationRequest(handler, cookie, session.CSRFToken, http.MethodPost, route, "", body)
	if response.Code != http.StatusAccepted {
		t.Fatalf("valid recovery = %d %s", response.Code, response.Body.String())
	}
	resumed, err := store.Command(t.Context(), command.ID)
	if err != nil || resumed.State != control.StateReconciling ||
		resumed.FencingToken <= command.FencingToken || !resumed.State.BlocksConflictDomain() {
		t.Fatalf("recovery did not preserve the uncertainty fence: %+v, %v", resumed, err)
	}
	response = mutationRequest(handler, cookie, session.CSRFToken, http.MethodPost, route, "", body)
	if response.Code != http.StatusConflict {
		t.Fatalf("replayed recovery = %d %s", response.Code, response.Body.String())
	}
}

func TestDrainPlanKeepsCancellationAndTerminationAvailable(t *testing.T) {
	drain, err := planCommand(commandRequest{Type: runtimecontrol.CommandPoolDrain, TargetType: "pool", TargetID: "linux"})
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{runtimecontrol.CommandPoolResume, runtimecontrol.CommandPoolCancelDrain,
		runtimecontrol.CommandRunnerTerminate, runtimecontrol.CommandRunnerRecycle} {
		input := commandRequest{Type: kind, TargetType: "pool", TargetID: "linux"}
		if kind == runtimecontrol.CommandRunnerTerminate || kind == runtimecontrol.CommandRunnerRecycle {
			input.TargetType, input.TargetID = "runner", "warm-idle"
			input.Parameters = json.RawMessage(`{"pool":"linux"}`)
		}
		plan, err := planCommand(input)
		if err != nil || plan.ConflictDomain == drain.ConflictDomain {
			t.Fatalf("%s shares drain lock: %+v, %v", kind, plan, err)
		}
	}
}
