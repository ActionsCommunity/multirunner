package main

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"sync"
	"time"

	"github.com/kardianos/service"

	"github.com/GerardSmit/multirunner/internal/servicehost"
)

type serviceRunner func(context.Context) error
type processTerminator func(int, string)

type managedServiceError struct {
	configPath  string
	interactive bool
	err         error
}

func (e *managedServiceError) Error() string {
	return e.err.Error()
}

func (e *managedServiceError) Unwrap() error {
	return e.err
}

func managedServiceExitCode(configPath string, interactive bool, now time.Time, summary string) (int, error) {
	if interactive {
		return servicehost.FailureExitCode, nil
	}
	err := servicehost.RecordFailure(configPath, now, sanitizeLogText(summary))
	if err != nil {
		return servicehost.FailureExitCode, err
	}
	return servicehost.FailureExitCode, nil
}

// program coordinates the orchestrator lifecycle while the platform service
// adapter owns process controls.
type program struct {
	cfgPath        string
	installDeps    bool
	interactive    bool
	run            serviceRunner
	terminate      processTerminator
	terminateClean processTerminator
	logger         service.Logger
	stopTimeout    time.Duration

	mu       sync.Mutex
	cancel   context.CancelFunc
	done     chan struct{}
	stopping bool
}

func (p *program) Start(service.Service) error {
	p.mu.Lock()
	if p.done != nil {
		p.mu.Unlock()
		return fmt.Errorf("orchestrator already started")
	}
	ctx, cancel := context.WithCancel(context.Background())
	p.cancel = cancel
	p.done = make(chan struct{})
	done := p.done
	p.stopping = false
	p.mu.Unlock()

	go p.runOrchestrator(ctx, done)
	return nil
}

func (p *program) runOrchestrator(ctx context.Context, done chan struct{}) {
	defer close(done)
	defer func() {
		if recovered := recover(); recovered != nil {
			const separator = "\nstack:\n"
			panicValue := truncateLogSummary(sanitizeLogText(fmt.Sprint(recovered)), maxServiceLogTailBytes/4)
			stackBudget := maxServiceLogTailBytes - len("panic: ") - len(panicValue) - len(separator)
			stack := truncateLogSummary(sanitizeLogText(string(debug.Stack())), stackBudget)
			p.fail(ctx, "orchestrator panicked", errors.New("panic: "+panicValue+separator+stack))
		}
	}()

	run := p.run
	if run == nil {
		run = func(ctx context.Context) error {
			return superviseServiceWorker(ctx, p.cfgPath, p.interactive, p.installDeps, p.logger)
		}
	}
	if err := run(ctx); err != nil {
		var exhausted *recoveryExhaustedError
		if errors.As(err, &exhausted) {
			p.stopCrashLoop(exhausted.Error())
			return
		}
		p.fail(ctx, "orchestrator stopped with a fatal error", err)
	}
}

func (p *program) stopCrashLoop(summary string) {
	p.mu.Lock()
	if p.stopping {
		p.mu.Unlock()
		return
	}
	p.stopping = true
	terminate := p.terminateClean
	logger := p.logger
	p.mu.Unlock()

	summary = sanitizeLogText(summary)
	if logger != nil {
		_ = logger.Error(summary)
	}
	if terminate != nil {
		terminate(0, summary)
		return
	}
	terminateServiceProcess(0)
}

func (p *program) fail(ctx context.Context, message string, err error) {
	p.mu.Lock()
	if p.stopping || ctx.Err() != nil {
		p.mu.Unlock()
		return
	}
	p.stopping = true
	terminate := p.terminate
	logger := p.logger
	p.mu.Unlock()

	summary := message + ": " + sanitizeLogText(err.Error())
	if logger != nil {
		_ = logger.Error(summary)
	}
	if terminate != nil {
		terminate(servicehost.FailureExitCode, summary)
	}
}

func (p *program) Stop(service.Service) error {
	p.mu.Lock()
	p.stopping = true
	cancel := p.cancel
	done := p.done
	timeout := p.stopTimeout
	p.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	if p.cfgPath != "" {
		if err := servicehost.ClearFailures(p.cfgPath); err != nil && p.logger != nil {
			_ = p.logger.Warning("clear service failure state: " + sanitizeLogText(err.Error()))
		}
	}
	if done == nil {
		return nil
	}
	if timeout <= 0 {
		timeout = servicehost.StopTimeout
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
		if p.logger != nil {
			_ = p.logger.Warning("orchestrator stop timed out")
		}
	}
	return nil
}

func classifyServiceHealth(status service.Status, err error) (message string, unhealthy, report bool) {
	if errors.Is(err, service.ErrNotInstalled) {
		return "", false, false
	}
	if err != nil {
		return "DEGRADED: " + sanitizeLogText(err.Error()), true, true
	}
	switch status {
	case service.StatusRunning:
		return "running", false, true
	case service.StatusStopped:
		return "STOPPED", true, true
	default:
		return "DEGRADED: service state is unknown", true, true
	}
}
