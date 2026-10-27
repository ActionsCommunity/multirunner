package runner

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestTerminalObservationSurvivesCancellation(t *testing.T) {
	gh, _ := jitServer(t, http.StatusNoContent)
	ctx, cancel := context.WithCancel(context.WithValue(t.Context(), struct{}{}, "retained"))
	var terminal LifecycleEvent
	observer := LifecycleObserverFunc(func(observed context.Context, event LifecycleEvent) {
		if event.Type != LifecycleFailed && event.Type != LifecycleStopped {
			return
		}
		terminal = event
		if observed.Err() != nil || observed.Value(struct{}{}) != "retained" {
			t.Errorf("terminal context lost validity or values: %v", observed.Err())
		}
		deadline, ok := observed.Deadline()
		if !ok || time.Until(deadline) > cleanupTimeout {
			t.Errorf("terminal observation is not bounded: %v, %v", deadline, ok)
		}
	})
	_, err := RunOnce(ctx, gh, stubBackend{handle: &stubHandle{onWait: cancel}},
		Spec{Name: "cancelled", LocalSessionID: "cancelled", Observer: observer}, discardLogger())
	if !errors.Is(err, context.Canceled) || terminal.Type != LifecycleFailed {
		t.Fatalf("RunOnce = %v, terminal = %+v", err, terminal)
	}
}

func TestIntentionalStopPreservesCleanupAndWaitFailures(t *testing.T) {
	for _, tc := range []struct {
		name       string
		cause      error
		killErr    error
		waitErr    error
		deleteCode int
		wantError  string
		conclusion string
	}{
		{name: "terminate", cause: ErrTerminated, deleteCode: http.StatusNoContent, conclusion: "terminated"},
		{name: "shutdown", cause: ErrShutdown, deleteCode: http.StatusNoContent, conclusion: "shutdown"},
		{name: "kill failure", cause: ErrTerminated, killErr: errors.New("kill failed"), deleteCode: http.StatusNoContent, wantError: "kill failed"},
		{name: "deregistration failure", cause: ErrTerminated, deleteCode: http.StatusInternalServerError, wantError: "deregister runner"},
		{name: "wait failure", cause: ErrTerminated, waitErr: errors.New("transport failed"), deleteCode: http.StatusNoContent, wantError: "transport failed"},
		{name: "joined wait failure", cause: ErrTerminated, waitErr: errors.Join(context.Canceled, errors.New("real failure")), deleteCode: http.StatusNoContent, wantError: "real failure"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gh, _ := jitServer(t, tc.deleteCode)
			ctx, cancel := context.WithCancelCause(t.Context())
			recorder := &lifecycleRecorder{}
			code, err := RunOnce(ctx, gh, stubBackend{handle: &stubHandle{
				code: 137, waitErr: tc.waitErr, killErr: tc.killErr,
				onWait: func() { cancel(tc.cause) },
			}}, Spec{Name: "intentional", Observer: recorder}, discardLogger())
			terminal := recorder.events[len(recorder.events)-1]
			if tc.wantError == "" {
				if err != nil || code != 0 || terminal.Type != LifecycleStopped || terminal.Conclusion != tc.conclusion {
					t.Fatalf("RunOnce = %d, %v; terminal = %+v", code, err, terminal)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.wantError) || terminal.Type != LifecycleFailed {
				t.Fatalf("RunOnce = %v; terminal = %+v; want %q", err, terminal, tc.wantError)
			}
		})
	}
}
