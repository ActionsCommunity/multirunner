package scaleset

import (
	upstream "github.com/actions/scaleset"
	"testing"
)

func TestExitBeforeCompletionDoesNotReplaceLastJob(t *testing.T) {
	be := &fakeBackend{}
	l := New(t.Context(), &fakeJIT{}, be, Options{ScaleSetID: 1, MaxRunners: 1})
	defer l.Shutdown(t.Context())
	if _, err := l.HandleDesiredRunnerCount(t.Context(), 1); err != nil {
		t.Fatal(err)
	}
	name := be.requests()[0].Name
	be.finish(0)
	waitFor(t, func() bool { l.mu.Lock(); defer l.mu.Unlock(); return len(l.running) == 0 })
	// A heartbeat repeats the old count before the completion message arrives.
	if _, err := l.HandleDesiredRunnerCount(t.Context(), 1); err != nil {
		t.Fatal(err)
	}
	if err := l.HandleJobCompleted(t.Context(), &upstream.JobCompleted{RunnerName: name, RunnerID: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := l.HandleDesiredRunnerCount(t.Context(), 0); err != nil {
		t.Fatal(err)
	}
	if len(be.requests()) != 1 || l.Running() != 0 {
		t.Fatalf("extra runners: launches=%d running=%d", len(be.requests()), l.Running())
	}
}

func TestCompletionStatisticsBeforeExitKeepsRemainingDemand(t *testing.T) {
	be := &fakeBackend{}
	l := New(t.Context(), &fakeJIT{}, be, Options{ScaleSetID: 1, MaxRunners: 1})
	defer l.Shutdown(t.Context())
	if _, err := l.HandleDesiredRunnerCount(t.Context(), 2); err != nil {
		t.Fatal(err)
	}
	name := be.requests()[0].Name
	if err := l.HandleJobCompleted(t.Context(), &upstream.JobCompleted{RunnerName: name}); err != nil {
		t.Fatal(err)
	}
	if _, err := l.HandleDesiredRunnerCount(t.Context(), 1); err != nil {
		t.Fatal(err)
	}
	be.finish(0)
	waitFor(t, func() bool { return len(be.requests()) == 2 })
	if l.Running() != 1 {
		t.Fatal("remaining job did not receive a replacement")
	}
}

func TestCompletionBeforeStatisticsDoesNotRefillStaleDemand(t *testing.T) {
	be := &fakeBackend{}
	l := New(t.Context(), &fakeJIT{}, be, Options{ScaleSetID: 1, MaxRunners: 1})
	defer l.Shutdown(t.Context())
	if _, err := l.HandleDesiredRunnerCount(t.Context(), 1); err != nil {
		t.Fatal(err)
	}
	if err := l.HandleJobCompleted(t.Context(), &upstream.JobCompleted{RunnerName: be.requests()[0].Name}); err != nil {
		t.Fatal(err)
	}
	be.finish(0)
	waitFor(t, func() bool { l.mu.Lock(); defer l.mu.Unlock(); return len(l.running) == 0 })
	if len(be.requests()) != 1 {
		t.Fatal("refilled before completion statistics")
	}
	if _, err := l.HandleDesiredRunnerCount(t.Context(), 0); err != nil {
		t.Fatal(err)
	}
	if len(be.requests()) != 1 {
		t.Fatal("refilled after drained queue")
	}
}
