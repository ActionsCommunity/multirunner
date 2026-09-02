package scaleset

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"
)

func TestSupervisorBackoffIsBounded(t *testing.T) {
	if got := supervisorBackoff(1, time.Second, 8*time.Second); got != time.Second {
		t.Fatalf("first delay = %s, want 1s", got)
	}
	if got := supervisorBackoff(4, time.Second, 8*time.Second); got != 8*time.Second {
		t.Fatalf("fourth delay = %s, want 8s", got)
	}
	if got := supervisorBackoff(100, time.Second, 8*time.Second); got != 8*time.Second {
		t.Fatalf("bounded delay = %s, want 8s", got)
	}
}

func TestSupervisorResetsBackoffAfterHealthyRun(t *testing.T) {
	base := time.Unix(100, 0)
	times := []time.Time{
		base,
		base.Add(time.Second),
		base.Add(2 * time.Second),
		base.Add(2*time.Second + time.Minute),
	}
	var nowIndex int
	var delays []time.Duration
	cfg := supervisorConfig{
		initialBackoff: time.Second,
		maxBackoff:     8 * time.Second,
		healthyAfter:   time.Minute,
		now: func() time.Time {
			value := times[nowIndex]
			nowIndex++
			return value
		},
		sleep: func(_ context.Context, delay time.Duration) bool {
			delays = append(delays, delay)
			return len(delays) < 2
		},
	}
	err := superviseSession(t.Context(), SupervisedSession{
		Name: "linux",
		Run:  func(context.Context) error { return errors.New("temporary") },
	}, cfg, testLogger())
	if err != nil {
		t.Fatalf("superviseSession: %v", err)
	}
	if len(delays) != 2 || delays[0] != time.Second || delays[1] != time.Second {
		t.Fatalf("delays = %v, want [1s 1s]", delays)
	}
}

func TestSupervisorCancellationInterruptsBackoff(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	startedSleep := make(chan struct{})
	cfg := defaultSupervisorConfig()
	cfg.sleep = func(ctx context.Context, _ time.Duration) bool {
		close(startedSleep)
		<-ctx.Done()
		return false
	}

	done := make(chan error, 1)
	go func() {
		done <- superviseSession(ctx, SupervisedSession{
			Name: "linux",
			Run:  func(context.Context) error { return errors.New("temporary") },
		}, cfg, testLogger())
	}()
	<-startedSleep
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("superviseSession: %v", err)
	}
}

func TestRunSupervisedReturnsForCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	err := RunSupervised(ctx, []SupervisedSession{{
		Name: "linux",
		Run:  func(context.Context) error { t.Fatal("session ran after cancellation"); return nil },
	}}, nil)
	if err != nil {
		t.Fatalf("RunSupervised: %v", err)
	}
}

func TestRunSupervisedRejectsInvalidSession(t *testing.T) {
	if err := RunSupervised(t.Context(), []SupervisedSession{{Name: "linux"}}, nil); err == nil {
		t.Fatal("RunSupervised accepted a nil session function")
	}
}

func TestSleepWithContextReturnsOnCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if sleepWithContext(ctx, time.Hour) {
		t.Fatal("sleepWithContext reported a completed delay after cancellation")
	}
}

func TestRunSupervisedIsolatesTransientSiblingFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	var failedRuns atomic.Int32
	healthyStarted := make(chan struct{})
	retried := make(chan struct{})
	cfg := defaultSupervisorConfig()
	cfg.sleep = func(context.Context, time.Duration) bool { return true }
	sessions := []SupervisedSession{
		{
			Name: "failing",
			Run: func(ctx context.Context) error {
				if failedRuns.Add(1) == 1 {
					return errors.New("network reset")
				}
				close(retried)
				<-ctx.Done()
				return ctx.Err()
			},
		},
		{
			Name: "healthy",
			Run: func(ctx context.Context) error {
				close(healthyStarted)
				<-ctx.Done()
				return ctx.Err()
			},
		},
	}
	done := make(chan error, 1)
	go func() { done <- runSupervised(ctx, sessions, cfg, testLogger()) }()
	<-healthyStarted
	<-retried
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("runSupervised: %v", err)
	}
}

func TestRunSupervisedSurfacesPermanentFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	siblingCanceled := make(chan struct{})
	healthyStarted := make(chan struct{})
	permanentReturned := make(chan struct{})
	sessions := []SupervisedSession{
		{
			Name: "unauthorized",
			Run: func(context.Context) error {
				close(permanentReturned)
				return errors.New(`request failed(status="401 Unauthorized")`)
			},
		},
		{
			Name: "healthy",
			Run: func(ctx context.Context) error {
				close(healthyStarted)
				<-ctx.Done()
				close(siblingCanceled)
				return ctx.Err()
			},
		},
	}
	done := make(chan error, 1)
	go func() {
		done <- runSupervised(ctx, sessions, defaultSupervisorConfig(),
			slog.New(slog.NewTextHandler(io.Discard, nil)))
	}()
	<-healthyStarted
	<-permanentReturned
	select {
	case <-siblingCanceled:
		t.Fatal("permanent failure canceled healthy sibling")
	default:
	}
	cancel()
	err := <-done
	if err == nil {
		t.Fatal("runSupervised returned nil for permanent authentication failure")
	}
	<-siblingCanceled
}

func TestHTTPNotFoundIsRecoverable(t *testing.T) {
	var runs atomic.Int32
	cfg := defaultSupervisorConfig()
	cfg.sleep = func(context.Context, time.Duration) bool { return runs.Load() < 2 }

	err := superviseSession(t.Context(), SupervisedSession{
		Name: "recreated",
		Run: func(context.Context) error {
			runs.Add(1)
			return errors.New(`request failed(status="404 Not Found")`)
		},
	}, cfg, testLogger())
	if err != nil {
		t.Fatalf("404 stopped session permanently: %v", err)
	}
	if runs.Load() != 2 {
		t.Fatalf("session runs = %d, want retry after 404", runs.Load())
	}
}

func TestAuthenticationAndConfigurationErrorsRemainPermanent(t *testing.T) {
	for _, err := range []error{
		errors.New(`request failed(status="400 Bad Request")`),
		errors.New(`request failed(status="401 Unauthorized")`),
		errors.New(`request failed(status="403 Forbidden")`),
		errors.New(`request failed(status="422 Unprocessable Entity")`),
	} {
		if !isPermanentSessionError(err) {
			t.Errorf("error %q was not permanent", err)
		}
	}
}
