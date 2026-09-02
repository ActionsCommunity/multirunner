package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/kardianos/service"

	"github.com/GerardSmit/multirunner/internal/config"
	"github.com/GerardSmit/multirunner/internal/servicehost"
)

const (
	serviceWorkerMarkerEnv      = "MULTIRUNNER_INTERNAL_SERVICE_WORKER"
	serviceWorkerMarkerValue    = "supervised-v1"
	serviceWorkerConfigEnv      = "MULTIRUNNER_INTERNAL_SERVICE_CONFIG"
	serviceWorkerInstallDepsEnv = "MULTIRUNNER_INTERNAL_SERVICE_INSTALL_DEPS"
	serviceWorkerInteractiveEnv = "MULTIRUNNER_INTERNAL_SERVICE_INTERACTIVE"
	serviceWorkerStopTimeout    = 8 * time.Second
	serviceOutputDrainTimeout   = time.Second
)

type serviceProcessSpec struct {
	path         string
	args         []string
	env          []string
	secrets      []string
	stopTimeout  time.Duration
	drainTimeout time.Duration
}

type supervisedProcessGroup interface {
	Kill() error
	Close() error
}

type supervisedProcessError struct {
	reason string
	tail   string
}

func (e *supervisedProcessError) Error() string {
	if e.tail == "" {
		return e.reason
	}
	return e.reason + "\noutput_tail:\n" + e.tail
}

type recoveryExhaustedError struct {
	count int
}

func (e *recoveryExhaustedError) Error() string {
	return fmt.Sprintf("service recovery stopped after %d failures within %s", e.count, servicehost.FailureWindow)
}

func superviseServiceWorker(ctx context.Context, configPath string, interactive, installDeps bool, logger service.Logger) error {
	if !interactive {
		count, err := servicehost.FailureCount(configPath, time.Now())
		if err != nil {
			return fmt.Errorf("read service recovery state: %w", err)
		}
		if count >= servicehost.CrashLoopFailureCount {
			return &recoveryExhaustedError{count: count}
		}
	}
	spec, err := newServiceWorkerSpec(configPath, interactive, installDeps)
	if err != nil {
		return err
	}
	return superviseProcess(ctx, spec, logger)
}

func newServiceWorkerSpec(configPath string, interactive, installDeps bool) (serviceProcessSpec, error) {
	executable, err := os.Executable()
	if err != nil {
		return serviceProcessSpec{}, fmt.Errorf("locate service executable: %w", err)
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		return serviceProcessSpec{}, err
	}
	env := withoutServiceWorkerEnvironment(os.Environ())
	env = append(env,
		serviceWorkerMarkerEnv+"="+serviceWorkerMarkerValue,
		serviceWorkerConfigEnv+"="+configPath,
		serviceWorkerInstallDepsEnv+"="+strconv.FormatBool(installDeps),
		serviceWorkerInteractiveEnv+"="+strconv.FormatBool(interactive),
	)
	return serviceProcessSpec{
		path:         executable,
		env:          env,
		secrets:      serviceRedactionSecrets(cfg),
		stopTimeout:  serviceWorkerStopTimeout,
		drainTimeout: serviceOutputDrainTimeout,
	}, nil
}

func superviseProcess(ctx context.Context, spec serviceProcessSpec, logger service.Logger) error {
	if spec.path == "" {
		return errors.New("service worker path is empty")
	}
	stopTimeout := spec.stopTimeout
	if stopTimeout <= 0 {
		stopTimeout = serviceWorkerStopTimeout
	}
	drainTimeout := spec.drainTimeout
	if drainTimeout <= 0 {
		drainTimeout = serviceOutputDrainTimeout
	}

	reader, writer, err := os.Pipe()
	if err != nil {
		return fmt.Errorf("create service output pipe: %w", err)
	}
	defer reader.Close()

	// #nosec G204 -- the executable is os.Executable and arguments are passed without a shell.
	cmd := exec.Command(spec.path, spec.args...)
	cmd.Env = spec.env
	cmd.Stdout = writer
	cmd.Stderr = writer
	stdin, err := cmd.StdinPipe()
	if err != nil {
		_ = writer.Close()
		return fmt.Errorf("create service control pipe: %w", err)
	}
	prepareSupervisedProcess(cmd)
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		_ = writer.Close()
		return fmt.Errorf("start service worker: %w", err)
	}
	group, err := attachSupervisedProcess(cmd)
	if err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		_ = stdin.Close()
		_ = writer.Close()
		return fmt.Errorf("contain service worker: %w", err)
	}
	defer group.Close()
	_ = writer.Close()

	tail := &sanitizedLogTail{}
	drainDone := make(chan struct{})
	go func() {
		defer close(drainDone)
		copyServiceOutputWithSecrets(reader, logger, tail, spec.secrets...)
	}()

	waitDone := make(chan error, 1)
	go func() {
		waitDone <- cmd.Wait()
	}()

	var waitErr error
	intentionalStop := false
	select {
	case waitErr = <-waitDone:
	case <-ctx.Done():
		select {
		case waitErr = <-waitDone:
		default:
			intentionalStop = true
			_ = stdin.Close()
			waitErr = stopSupervisedProcess(cmd, group, waitDone, stopTimeout)
		}
	}
	_ = stdin.Close()

	// Descendants can inherit the worker's output descriptor. Terminate the
	// process tree after the worker exits so output draining cannot hang.
	_ = group.Kill()
	waitForServiceOutput(reader, drainDone, drainTimeout)

	if intentionalStop {
		return nil
	}
	if waitErr == nil {
		return &supervisedProcessError{
			reason: "service_worker_exit exit_code=0 unexpected=true",
			tail:   tail.String(),
		}
	}
	return &supervisedProcessError{
		reason: describeSupervisedProcessExit(cmd.ProcessState, waitErr),
		tail:   tail.String(),
	}
}

func stopSupervisedProcess(cmd *exec.Cmd, group supervisedProcessGroup, waitDone <-chan error, timeout time.Duration) error {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case err := <-waitDone:
		return err
	case <-timer.C:
		if err := group.Kill(); err != nil {
			_ = cmd.Process.Kill()
		}
		waitTimer := time.NewTimer(serviceOutputDrainTimeout)
		defer waitTimer.Stop()
		select {
		case err := <-waitDone:
			return err
		case <-waitTimer.C:
			return errors.New("service worker did not exit after forced termination")
		}
	}
}

func waitForServiceOutput(reader io.Closer, drainDone <-chan struct{}, timeout time.Duration) {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-drainDone:
	case <-timer.C:
		_ = reader.Close()
		select {
		case <-drainDone:
		case <-time.After(time.Second):
		}
	}
}

func withoutServiceWorkerEnvironment(environment []string) []string {
	filtered := make([]string, 0, len(environment))
	for _, entry := range environment {
		name, _, _ := strings.Cut(entry, "=")
		switch strings.ToUpper(name) {
		case serviceWorkerMarkerEnv, serviceWorkerConfigEnv, serviceWorkerInstallDepsEnv, serviceWorkerInteractiveEnv:
			continue
		default:
			filtered = append(filtered, entry)
		}
	}
	return filtered
}

func serviceRedactionSecrets(cfg *config.Config) []string {
	secrets := []string{cfg.Auth.PAT, cfg.Webhook.Secret, cfg.Cache.AccessToken}
	if cfg.Auth.PrivateKeyPath != "" {
		if privateKey, err := os.ReadFile(cfg.Auth.PrivateKeyPath); err == nil {
			secrets = append(secrets, string(privateKey))
			for line := range strings.Lines(string(privateKey)) {
				line = strings.TrimSpace(line)
				if len(line) >= 16 && !strings.Contains(line, "PRIVATE KEY-----") {
					secrets = append(secrets, line)
				}
			}
		}
	}
	for _, entry := range os.Environ() {
		name, value, found := strings.Cut(entry, "=")
		if found && len(value) >= 4 && isSensitiveEnvironmentName(name) {
			secrets = append(secrets, value)
		}
	}
	sort.Slice(secrets, func(i, j int) bool {
		return len(secrets[i]) > len(secrets[j])
	})
	filtered := secrets[:0]
	seen := make(map[string]struct{}, len(secrets))
	for _, secret := range secrets {
		if secret == "" {
			continue
		}
		if _, exists := seen[secret]; exists {
			continue
		}
		seen[secret] = struct{}{}
		filtered = append(filtered, secret)
	}
	return filtered
}

func isSensitiveEnvironmentName(name string) bool {
	upper := strings.ToUpper(name)
	for _, part := range []string{"TOKEN", "SECRET", "PASSWORD", "CREDENTIAL", "JIT_CONFIG", "AUTHORIZATION", "API_KEY", "ACCESS_KEY"} {
		if strings.Contains(upper, part) {
			return true
		}
	}
	return false
}
