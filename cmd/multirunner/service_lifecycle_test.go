package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kardianos/service"

	"github.com/GerardSmit/multirunner/internal/servicehost"
)

type recordingServiceLogger struct {
	mu       sync.Mutex
	errors   []string
	warnings []string
	infos    []string
}

func (l *recordingServiceLogger) Error(values ...interface{}) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.errors = append(l.errors, strings.TrimSpace(fmt.Sprint(values...)))
	return nil
}

func (l *recordingServiceLogger) Warning(values ...interface{}) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.warnings = append(l.warnings, strings.TrimSpace(fmt.Sprint(values...)))
	return nil
}

func (l *recordingServiceLogger) Info(values ...interface{}) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.infos = append(l.infos, strings.TrimSpace(fmt.Sprint(values...)))
	return nil
}

func (l *recordingServiceLogger) Errorf(format string, values ...interface{}) error {
	return l.Error(fmt.Sprintf(format, values...))
}

func (l *recordingServiceLogger) Warningf(format string, values ...interface{}) error {
	return l.Warning(fmt.Sprintf(format, values...))
}

func (l *recordingServiceLogger) Infof(format string, values ...interface{}) error {
	return l.Info(fmt.Sprintf(format, values...))
}

func TestProgramFatalReturnTerminatesWithFailure(t *testing.T) {
	logger := &recordingServiceLogger{}
	exitCode := make(chan int, 1)
	p := &program{
		run:         func(context.Context) error { return errors.New("worker failed") },
		terminate:   func(code int, _ string) { exitCode <- code },
		logger:      logger,
		stopTimeout: time.Second,
	}
	if err := p.Start(nil); err != nil {
		t.Fatal(err)
	}
	select {
	case code := <-exitCode:
		if code != servicehost.FailureExitCode {
			t.Errorf("exit code = %d, want %d", code, servicehost.FailureExitCode)
		}
	case <-time.After(time.Second):
		t.Fatal("fatal return did not terminate the process")
	}
	if len(logger.errors) != 1 || !strings.Contains(logger.errors[0], "worker failed") {
		t.Fatalf("fatal log = %v", logger.errors)
	}
}

func TestProgramRecoversParentSupervisorPanic(t *testing.T) {
	type termination struct {
		code    int
		summary string
		err     error
	}

	configPath := filepath.Join(t.TempDir(), "config.yaml")
	logger := &recordingServiceLogger{}
	terminations := make(chan termination, 2)
	panicValue := strings.Join([]string{
		"token=" + serviceSupervisorGitHubToken,
		"JIT_CONFIG=" + serviceSupervisorJIT,
		"http://cache:3000/_mr/" + serviceSupervisorCacheToken + "/results",
		"-----BEGIN PRIVATE KEY-----",
		serviceSupervisorPrivateValue,
		"-----END PRIVATE KEY-----",
		"control=\x00\x01\x1b[31m",
		strings.Repeat("diagnostic", maxServiceLogTailBytes),
	}, "\n")
	p := &program{
		cfgPath: configPath,
		run: func(context.Context) error {
			panic(panicValue)
		},
		terminate: func(code int, summary string) {
			recordedCode, err := managedServiceExitCode(configPath, false, time.Now(), summary)
			if recordedCode != code && err == nil {
				err = fmt.Errorf("recorded exit code = %d, want %d", recordedCode, code)
			}
			terminations <- termination{code: code, summary: summary, err: err}
		},
		logger: logger,
	}
	if err := p.Start(nil); err != nil {
		t.Fatal(err)
	}

	var result termination
	select {
	case result = <-terminations:
	case <-time.After(3 * time.Second):
		t.Fatal("parent panic did not invoke the terminator")
	}
	if result.err != nil {
		t.Fatal(result.err)
	}
	if result.code != servicehost.FailureExitCode {
		t.Fatalf("exit code = %d, want %d", result.code, servicehost.FailureExitCode)
	}

	p.mu.Lock()
	done := p.done
	p.mu.Unlock()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("parent panic recovery deadlocked")
	}
	select {
	case duplicate := <-terminations:
		t.Fatalf("terminator invoked more than once: %+v", duplicate)
	default:
	}

	if !strings.Contains(result.summary, "orchestrator panicked") ||
		!strings.Contains(result.summary, "stack:") ||
		!strings.Contains(result.summary, "service_lifecycle") {
		t.Fatalf("panic diagnostic is incomplete: %q", result.summary)
	}
	assertServiceOutputRedacted(t, result.summary)
	for _, forbidden := range []string{"PRIVATE KEY", "\x00", "\x01", "\x1b"} {
		if strings.Contains(result.summary, forbidden) {
			t.Fatalf("panic diagnostic retained %q: %q", forbidden, result.summary)
		}
	}

	status, err := servicehost.ReadFailureStatus(configPath, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if status.Count != 1 || len(status.LastError) > maxServiceLogTailBytes {
		t.Fatalf("failure status = %+v", status)
	}
	if !strings.Contains(status.LastError, "panic: token=<redacted>") ||
		!strings.Contains(status.LastError, "stack:") {
		t.Fatalf("durable failure diagnostic is incomplete: %q", status.LastError)
	}
	assertServiceOutputRedacted(t, status.LastError)
	state, err := os.ReadFile(filepath.Join(filepath.Dir(configPath), ".multirunner-service-state.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(state) > 4096 {
		t.Fatalf("failure state size = %d", len(state))
	}
	if len(logger.errors) != 1 {
		t.Fatalf("panic log count = %d, want 1", len(logger.errors))
	}
	assertServiceOutputRedacted(t, string(state))
	assertServiceOutputRedacted(t, logger.errors[0])
}

func TestProgramIntentionalStopDoesNotTerminate(t *testing.T) {
	started := make(chan struct{})
	exited := make(chan int, 1)
	p := &program{
		run: func(ctx context.Context) error {
			close(started)
			<-ctx.Done()
			return ctx.Err()
		},
		terminate:   func(code int, _ string) { exited <- code },
		stopTimeout: time.Second,
	}
	if err := p.Start(nil); err != nil {
		t.Fatal(err)
	}
	<-started
	if err := p.Stop(nil); err != nil {
		t.Fatal(err)
	}
	select {
	case code := <-exited:
		t.Fatalf("intentional stop terminated with %d", code)
	default:
	}
}

func TestProgramRejectsSecondStart(t *testing.T) {
	release := make(chan struct{})
	p := &program{run: func(context.Context) error {
		<-release
		return nil
	}}
	if err := p.Start(nil); err != nil {
		t.Fatal(err)
	}
	if err := p.Start(nil); err == nil {
		t.Fatal("second Start succeeded")
	}
	close(release)
}

func TestProgramStopIsBounded(t *testing.T) {
	release := make(chan struct{})
	logger := &recordingServiceLogger{}
	p := &program{
		run: func(context.Context) error {
			<-release
			return nil
		},
		logger:      logger,
		stopTimeout: 20 * time.Millisecond,
	}
	if err := p.Start(nil); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if err := p.Stop(nil); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed > 250*time.Millisecond {
		t.Fatalf("Stop took %s, want a bounded return", elapsed)
	}
	close(release)
	if len(logger.warnings) != 1 || !strings.Contains(logger.warnings[0], "timed out") {
		t.Fatalf("timeout warning = %v", logger.warnings)
	}
}

func TestClassifyServiceHealth(t *testing.T) {
	tests := []struct {
		name      string
		status    service.Status
		err       error
		unhealthy bool
		report    bool
	}{
		{name: "running", status: service.StatusRunning, report: true},
		{name: "stopped", status: service.StatusStopped, unhealthy: true, report: true},
		{name: "crash loop", status: service.StatusUnknown, err: errors.New("service in failed state"), unhealthy: true, report: true},
		{name: "not installed", status: service.StatusUnknown, err: service.ErrNotInstalled},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, unhealthy, report := classifyServiceHealth(test.status, test.err)
			if unhealthy != test.unhealthy || report != test.report {
				t.Fatalf("classify = unhealthy %v, report %v", unhealthy, report)
			}
		})
	}
}

func TestManagedServiceExitCodeKeepsForegroundFailureNonzero(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	now := time.Date(2026, time.September, 1, 12, 0, 0, 0, time.UTC)
	for attempt := 0; attempt < servicehost.CrashLoopFailureCount+1; attempt++ {
		code, err := managedServiceExitCode(configPath, true, now, "fatal")
		if err != nil {
			t.Fatal(err)
		}
		if code != servicehost.FailureExitCode {
			t.Fatalf("foreground exit code = %d, want %d", code, servicehost.FailureExitCode)
		}
	}
	count, err := servicehost.FailureCount(configPath, now)
	if err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("foreground failure count = %d, want 0", count)
	}
}

func TestManagedServiceExitCodeRecordsManagedFailures(t *testing.T) {
	now := time.Date(2026, time.September, 1, 12, 0, 0, 0, time.UTC)
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	for attempt := 0; attempt < servicehost.CrashLoopFailureCount; attempt++ {
		code, err := managedServiceExitCode(configPath, false, now.Add(time.Duration(attempt)*time.Second), "fatal")
		if err != nil {
			t.Fatal(err)
		}
		if code != servicehost.FailureExitCode {
			t.Fatalf("managed exit code = %d, want %d", code, servicehost.FailureExitCode)
		}
	}
}

func TestManagedServiceExitCodeStaysNonzeroWhenAccountingFails(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "missing", "config.yaml")
	code, err := managedServiceExitCode(configPath, false, time.Now(), "fatal")
	if err == nil {
		t.Fatal("accounting failure returned nil")
	}
	if code != servicehost.FailureExitCode {
		t.Fatalf("fallback exit code = %d, want %d", code, servicehost.FailureExitCode)
	}
}

func TestManagedServiceErrorUnwrapsCause(t *testing.T) {
	cause := errors.New("bootstrap failed")
	err := &managedServiceError{err: cause}
	if err.Error() != cause.Error() || !errors.Is(err, cause) {
		t.Fatalf("managed error did not preserve cause: %v", err)
	}
}

func TestProgramStopsCrashLoopWithCleanExit(t *testing.T) {
	logger := &recordingServiceLogger{}
	cleanExit := make(chan int, 1)
	failureExit := make(chan int, 1)
	p := &program{
		run:            func(context.Context) error { return &recoveryExhaustedError{count: servicehost.CrashLoopFailureCount} },
		terminate:      func(code int, _ string) { failureExit <- code },
		terminateClean: func(code int, _ string) { cleanExit <- code },
		logger:         logger,
	}
	if err := p.Start(nil); err != nil {
		t.Fatal(err)
	}
	select {
	case code := <-cleanExit:
		if code != 0 {
			t.Fatalf("crash-loop exit code = %d, want 0", code)
		}
	case <-time.After(time.Second):
		t.Fatal("crash-loop suppression did not terminate")
	}
	select {
	case code := <-failureExit:
		t.Fatalf("crash-loop suppression used failure exit %d", code)
	default:
	}
	if len(logger.errors) != 1 || !strings.Contains(logger.errors[0], "recovery stopped") {
		t.Fatalf("crash-loop log = %v", logger.errors)
	}
}
