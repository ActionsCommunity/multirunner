package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/GerardSmit/multirunner/internal/config"
	"github.com/GerardSmit/multirunner/internal/servicehost"
)

const (
	serviceSupervisorHelperEnv    = "MULTIRUNNER_TEST_SERVICE_HELPER"
	serviceSupervisorCompleteEnv  = "MULTIRUNNER_TEST_SERVICE_COMPLETE"
	serviceSupervisorSecret       = "configured-literal-secret"
	serviceSupervisorEnvSecret    = "environment-literal-secret"
	serviceSupervisorGitHubToken  = "github_pat_ABCDEFGHIJKLMNOPQRSTUVWXYZ123456"
	serviceSupervisorJIT          = "BASE64-JIT-SECRET"
	serviceSupervisorCacheToken   = "generated-cache-secret"
	serviceSupervisorPrivateValue = "PRIVATE-BODY"
)

func TestServiceSupervisorHelper(t *testing.T) {
	mode := os.Getenv(serviceSupervisorHelperEnv)
	if mode == "" {
		return
	}
	switch mode {
	case "clean":
		fmt.Fprintln(os.Stdout, "worker ready")
		_, _ = io.Copy(io.Discard, os.Stdin)
		if err := os.WriteFile(os.Getenv(serviceSupervisorCompleteEnv), []byte("stopped"), 0o600); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	case "stubborn", "descendant":
		fmt.Fprintln(os.Stdout, "worker ready")
		for {
			time.Sleep(time.Hour)
		}
	case "exit":
		fmt.Fprintln(os.Stdout, "worker exited cleanly")
	case "panic":
		descendant := exec.Command(os.Args[0], "-test.run=^TestServiceSupervisorHelper$")
		descendant.Env = helperEnvironment("descendant", "")
		descendant.Stdout = os.Stdout
		descendant.Stderr = os.Stderr
		if err := descendant.Start(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Fprintln(os.Stderr, panicFixture())
		go func() {
			panic(panicFixture())
		}()
		for {
			time.Sleep(time.Hour)
		}
	default:
		os.Exit(2)
	}
}

func TestSuperviseProcessRejectsInvalidExecutables(t *testing.T) {
	logger := &recordingServiceLogger{}
	if err := superviseProcess(t.Context(), serviceProcessSpec{}, logger); err == nil {
		t.Fatal("empty executable path succeeded")
	}
	spec := serviceProcessSpec{
		path:         filepath.Join(t.TempDir(), "missing-executable"),
		env:          os.Environ(),
		stopTimeout:  time.Second,
		drainTimeout: time.Second,
	}
	if err := superviseProcess(t.Context(), spec, logger); err == nil || !strings.Contains(err.Error(), "start service worker") {
		t.Fatalf("missing executable error = %v", err)
	}
}

func TestSupervisorTreatsUnexpectedCleanWorkerExitAsFailure(t *testing.T) {
	logger := &recordingServiceLogger{}
	err := superviseProcess(t.Context(), helperProcessSpec("exit", ""), logger)
	var processErr *supervisedProcessError
	if !errors.As(err, &processErr) {
		t.Fatalf("clean worker exit error = %T %v", err, err)
	}
	if processErr.reason != "service_worker_exit exit_code=0 unexpected=true" {
		t.Fatalf("clean worker exit reason = %q", processErr.reason)
	}
	if !strings.Contains(processErr.tail, "worker exited cleanly") {
		t.Fatalf("clean worker diagnostic tail = %q", processErr.tail)
	}
}

func TestSupervisorCapturesArbitraryGoroutinePanicAndReapsTree(t *testing.T) {
	logger := &recordingServiceLogger{}
	spec := helperProcessSpec("panic", "")
	start := time.Now()
	err := superviseProcess(t.Context(), spec, logger)
	if time.Since(start) > 5*time.Second {
		t.Fatal("supervisor did not bound worker tree shutdown")
	}
	var processErr *supervisedProcessError
	if !errors.As(err, &processErr) {
		t.Fatalf("superviseProcess error = %T %v", err, err)
	}
	if !strings.Contains(processErr.reason, "service_worker_exit") {
		t.Fatalf("structured exit reason = %q", processErr.reason)
	}
	assertServiceOutputRedacted(t, strings.Join(append(logger.infos, err.Error()), "\n"))
	if len(processErr.tail) > maxServiceLogTailBytes {
		t.Fatalf("diagnostic tail length = %d", len(processErr.tail))
	}
	if !strings.Contains(processErr.tail, "panic:") {
		t.Fatalf("diagnostic tail lost panic summary: %q", processErr.tail)
	}
}

func TestSupervisorIntentionalStopIsCleanAndWaitsForWorker(t *testing.T) {
	complete := filepath.Join(t.TempDir(), "complete")
	logger := &recordingServiceLogger{}
	ctx, cancel := context.WithCancel(t.Context())
	result := make(chan error, 1)
	go func() {
		result <- superviseProcess(ctx, helperProcessSpec("clean", complete), logger)
	}()
	waitForHelperReady(t, logger)
	cancel()
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("intentional stop = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("intentional stop did not reap worker")
	}
	if content, err := os.ReadFile(complete); err != nil || string(content) != "stopped" {
		t.Fatalf("worker completion content=%q err=%v", content, err)
	}
}

func TestSupervisorBoundsUncooperativeWorkerStop(t *testing.T) {
	logger := &recordingServiceLogger{}
	ctx, cancel := context.WithCancel(t.Context())
	spec := helperProcessSpec("stubborn", "")
	spec.stopTimeout = 50 * time.Millisecond
	result := make(chan error, 1)
	go func() {
		result <- superviseProcess(ctx, spec, logger)
	}()
	waitForHelperReady(t, logger)
	start := time.Now()
	cancel()
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("bounded stop = %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("uncooperative worker was not reaped")
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("uncooperative worker stop exceeded its bound")
	}
}

func TestProgramPersistsSanitizedWorkerFailure(t *testing.T) {
	logger := &recordingServiceLogger{}
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	exitCode := make(chan int, 1)
	p := &program{
		cfgPath: configPath,
		run: func(ctx context.Context) error {
			return superviseProcess(ctx, helperProcessSpec("panic", ""), logger)
		},
		terminate: func(code int, summary string) {
			recordedCode, err := managedServiceExitCode(configPath, false, time.Now(), summary)
			if err != nil {
				t.Error(err)
			}
			if recordedCode != code {
				t.Errorf("recorded exit code = %d, want %d", recordedCode, code)
			}
			exitCode <- code
		},
		logger: logger,
	}
	if err := p.Start(nil); err != nil {
		t.Fatal(err)
	}
	select {
	case code := <-exitCode:
		if code != servicehost.FailureExitCode {
			t.Fatalf("service exit code = %d", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("worker failure did not terminate supervisor")
	}
	status, err := servicehost.ReadFailureStatus(configPath, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if status.Count != 1 || !strings.Contains(status.LastError, "service_worker_exit") {
		t.Fatalf("failure status = %+v", status)
	}
	assertServiceOutputRedacted(t, status.LastError)
	state, err := os.ReadFile(filepath.Join(filepath.Dir(configPath), ".multirunner-service-state.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(state) > 4096 {
		t.Fatalf("failure state size = %d", len(state))
	}
	assertServiceOutputRedacted(t, string(state))
}

func TestSuperviseServiceWorkerStopsAtCrashLoopBudgetBeforeConfigLoad(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	now := time.Now()
	for attempt := 0; attempt < servicehost.CrashLoopFailureCount; attempt++ {
		if err := servicehost.RecordFailure(configPath, now.Add(time.Duration(attempt)*time.Millisecond), "fatal"); err != nil {
			t.Fatal(err)
		}
	}
	err := superviseServiceWorker(t.Context(), configPath, false, false, &recordingServiceLogger{})
	var exhausted *recoveryExhaustedError
	if !errors.As(err, &exhausted) {
		t.Fatalf("crash-loop admission = %T %v", err, err)
	}
}

func TestWithoutServiceWorkerEnvironmentPreventsRecursiveMarkers(t *testing.T) {
	environment := []string{
		"PATH=value",
		serviceWorkerMarkerEnv + "=old",
		serviceWorkerConfigEnv + "=old",
		serviceWorkerInstallDepsEnv + "=true",
		serviceWorkerInteractiveEnv + "=true",
	}
	filtered := withoutServiceWorkerEnvironment(environment)
	if len(filtered) != 1 || filtered[0] != "PATH=value" {
		t.Fatalf("filtered environment = %v", filtered)
	}
}

func TestNewServiceWorkerSpecBuildsPrivateWorkerContract(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	configData := `github:
  scope: repo
  owner: example
  repo: project
auth:
  pat: configured-pat-value
pools:
  - name: linux
    os: linux
    size: 1
    docker:
      host: unix:///var/run/docker.sock
`
	if err := os.WriteFile(configPath, []byte(configData), 0o600); err != nil {
		t.Fatal(err)
	}
	spec, err := newServiceWorkerSpec(configPath, true, true)
	if err != nil {
		t.Fatal(err)
	}
	environment := strings.Join(spec.env, "\n")
	for _, want := range []string{
		serviceWorkerMarkerEnv + "=" + serviceWorkerMarkerValue,
		serviceWorkerConfigEnv + "=" + configPath,
		serviceWorkerInstallDepsEnv + "=true",
		serviceWorkerInteractiveEnv + "=true",
	} {
		if !strings.Contains(environment, want) {
			t.Errorf("worker environment missing %q", want)
		}
	}
	if spec.path == "" || spec.stopTimeout != serviceWorkerStopTimeout || spec.drainTimeout != serviceOutputDrainTimeout {
		t.Fatalf("worker spec = %+v", spec)
	}
	if !containsString(spec.secrets, "configured-pat-value") {
		t.Fatal("worker spec does not redact its configured PAT")
	}
}

func TestNewServiceWorkerSpecRejectsInvalidConfig(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(configPath, []byte("invalid: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := newServiceWorkerSpec(configPath, false, false); err == nil {
		t.Fatal("invalid worker config succeeded")
	}
}

func TestServiceRedactionSecretsIncludesConfiguredEnvironmentAndPrivateKeyValues(t *testing.T) {
	privateKeyPath := filepath.Join(t.TempDir(), "app.pem")
	privateKeyBody := "private-key-value-with-sufficient-length"
	privateKey := "-----BEGIN PRIVATE KEY-----\n" + privateKeyBody + "\n-----END PRIVATE KEY-----"
	if err := os.WriteFile(privateKeyPath, []byte(privateKey), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MULTIRUNNER_TEST_API_TOKEN", serviceSupervisorEnvSecret)
	cfg := &config.Config{
		Auth:    config.Auth{PAT: serviceSupervisorSecret, PrivateKeyPath: privateKeyPath},
		Webhook: config.Webhook{Secret: "webhook-secret-value"},
		Cache:   config.Cache{AccessToken: "cache-secret-value"},
	}
	secrets := serviceRedactionSecrets(cfg)
	for _, want := range []string{
		serviceSupervisorSecret,
		serviceSupervisorEnvSecret,
		"webhook-secret-value",
		"cache-secret-value",
		privateKey,
		privateKeyBody,
	} {
		found := false
		for _, secret := range secrets {
			if secret == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("redaction secrets missing configured value of length %d", len(want))
		}
	}
	for i := 1; i < len(secrets); i++ {
		if len(secrets[i]) > len(secrets[i-1]) {
			t.Fatal("redaction secrets are not ordered longest first")
		}
	}
}

func helperProcessSpec(mode, completePath string) serviceProcessSpec {
	return serviceProcessSpec{
		path:         os.Args[0],
		args:         []string{"-test.run=^TestServiceSupervisorHelper$"},
		env:          helperEnvironment(mode, completePath),
		secrets:      []string{serviceSupervisorSecret, serviceSupervisorEnvSecret},
		stopTimeout:  time.Second,
		drainTimeout: time.Second,
	}
}

func helperEnvironment(mode, completePath string) []string {
	env := withoutServiceWorkerEnvironment(os.Environ())
	env = append(env,
		serviceSupervisorHelperEnv+"="+mode,
		serviceSupervisorCompleteEnv+"="+completePath,
		"TEST_API_TOKEN="+serviceSupervisorEnvSecret,
	)
	return env
}

func panicFixture() string {
	return strings.Join([]string{
		"token=" + serviceSupervisorGitHubToken,
		"JIT_CONFIG=" + serviceSupervisorJIT,
		"http://cache:3000/_mr/" + serviceSupervisorCacheToken + "/results",
		"unexpected " + serviceSupervisorSecret,
		"environment " + serviceSupervisorEnvSecret,
		"-----BEGIN PRIVATE KEY-----",
		serviceSupervisorPrivateValue,
		"-----END PRIVATE KEY-----",
	}, "\n")
}

func assertServiceOutputRedacted(t *testing.T, output string) {
	t.Helper()
	for _, secret := range []string{
		serviceSupervisorGitHubToken,
		serviceSupervisorJIT,
		serviceSupervisorCacheToken,
		serviceSupervisorSecret,
		serviceSupervisorEnvSecret,
		serviceSupervisorPrivateValue,
	} {
		if strings.Contains(output, secret) {
			t.Fatalf("service output leaked %q: %q", secret, output)
		}
	}
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func waitForHelperReady(t *testing.T, logger *recordingServiceLogger) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		logger.mu.Lock()
		ready := strings.Contains(strings.Join(logger.infos, "\n"), "worker ready")
		logger.mu.Unlock()
		if ready {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("helper did not report ready")
}
