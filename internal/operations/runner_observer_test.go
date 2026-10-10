package operations

import (
	"testing"
	"time"

	"github.com/GerardSmit/multirunner/internal/runner"
)

type capturingAppender struct {
	inputs []EventInput
}

func (a *capturingAppender) TryAppend(input EventInput) bool {
	a.inputs = append(a.inputs, input)
	return true
}

func TestRunnerLifecycleObserverMapsAuthoritativeEvent(t *testing.T) {
	appender := &capturingAppender{}
	observer := NewRunnerLifecycleObserver(appender)
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	observer.ObserveRunnerLifecycle(t.Context(), runner.LifecycleEvent{
		Type: runner.LifecycleLaunched, LocalSessionID: "session-1",
		Pool: "linux", Target: "o/r", Repository: "o/r", RunnerName: "mr-1",
		QueuedRunID: 10, QueuedRunAttempt: 2, QueuedJobID: 20,
		GitHubRegistrationID: 30, BackendInstanceID: "container-1",
		Timestamp: now, ExitCode: -1,
	})
	if len(appender.inputs) != 1 {
		t.Fatalf("events = %d, want 1", len(appender.inputs))
	}
	got := appender.inputs[0]
	if got.Type != "runner.launched" || got.EntityID != "session-1" ||
		got.CorrelationID != "session-1" || got.Timestamp != now ||
		got.ActorKind != ActorSystem {
		t.Fatalf("mapped event = %+v", got)
	}
	if string(got.Payload) == "" {
		t.Fatal("mapped payload is empty")
	}
}

func TestRunnerLifecycleObserverIgnoresMissingSession(t *testing.T) {
	appender := &capturingAppender{}
	NewRunnerLifecycleObserver(appender).ObserveRunnerLifecycle(t.Context(), runner.LifecycleEvent{
		Type: runner.LifecyclePlanned,
	})
	if len(appender.inputs) != 0 {
		t.Fatalf("events = %d, want 0", len(appender.inputs))
	}
}
