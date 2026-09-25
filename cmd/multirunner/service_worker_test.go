package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestWorkerMarkerDispatchesBeforeCommandConstruction(t *testing.T) {
	command := exec.Command(os.Args[0], "-test.run=^TestServiceSupervisorHelper$")
	command.Env = append(withoutServiceWorkerEnvironment(os.Environ()),
		serviceWorkerMarkerEnv+"="+serviceWorkerMarkerValue,
		serviceWorkerConfigEnv+"="+filepath.Join(t.TempDir(), "missing.yaml"),
		serviceWorkerInstallDepsEnv+"=false",
		serviceWorkerInteractiveEnv+"=false",
		serviceSupervisorHelperEnv+"=stubborn",
	)
	output, err := command.CombinedOutput()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
		t.Fatalf("worker entry exit=%v output=%q", err, output)
	}
	if strings.Contains(string(output), "worker ready") {
		t.Fatalf("test command ran before worker dispatch: %q", output)
	}
}

func TestServiceWorkerOptionsValidatePrivateEnvironment(t *testing.T) {
	t.Setenv(serviceWorkerConfigEnv, "")
	if _, err := serviceWorkerOptionsFromEnvironment(); err == nil {
		t.Fatal("missing config path succeeded")
	}
	t.Setenv(serviceWorkerConfigEnv, "config.yaml")
	t.Setenv(serviceWorkerInstallDepsEnv, "invalid")
	if _, err := serviceWorkerOptionsFromEnvironment(); err == nil {
		t.Fatal("invalid install-deps value succeeded")
	}
	t.Setenv(serviceWorkerInstallDepsEnv, "true")
	t.Setenv(serviceWorkerInteractiveEnv, "invalid")
	if _, err := serviceWorkerOptionsFromEnvironment(); err == nil {
		t.Fatal("invalid interactive value succeeded")
	}
	t.Setenv(serviceWorkerInteractiveEnv, "false")
	options, err := serviceWorkerOptionsFromEnvironment()
	if err != nil {
		t.Fatal(err)
	}
	if options.configPath != "config.yaml" || options.interactive || !options.installDeps {
		t.Fatalf("worker options = %+v", options)
	}
}

func TestClearServiceWorkerEnvironmentPreventsMarkerInheritance(t *testing.T) {
	for _, name := range []string{
		serviceWorkerMarkerEnv,
		serviceWorkerConfigEnv,
		serviceWorkerInstallDepsEnv,
		serviceWorkerInteractiveEnv,
	} {
		t.Setenv(name, "value")
	}
	clearServiceWorkerEnvironment()
	for _, name := range []string{
		serviceWorkerMarkerEnv,
		serviceWorkerConfigEnv,
		serviceWorkerInstallDepsEnv,
		serviceWorkerInteractiveEnv,
	} {
		if _, exists := os.LookupEnv(name); exists {
			t.Errorf("internal worker environment %s is still set", name)
		}
	}
}

func TestRunServiceWorkerFromEnvironmentRejectsMissingConfig(t *testing.T) {
	t.Setenv(serviceWorkerConfigEnv, "")
	if code := runServiceWorkerFromEnvironment(); code != 1 {
		t.Fatalf("missing worker config exit code = %d", code)
	}
}

func TestRunServiceWorkerReturnsCleanlyAfterControlEOF(t *testing.T) {
	var output bytes.Buffer
	code := runServiceWorker(
		serviceWorkerOptions{configPath: "config.yaml", interactive: true, installDeps: true},
		strings.NewReader(""),
		&output,
		&output,
		func(ctx context.Context, configPath string, interactive, installDeps bool, _ io.Writer) error {
			<-ctx.Done()
			if configPath != "config.yaml" || !interactive || !installDeps {
				t.Errorf("worker arguments = %q %v %v", configPath, interactive, installDeps)
			}
			return nil
		},
	)
	if code != 0 || output.Len() != 0 {
		t.Fatalf("clean worker code=%d output=%q", code, output.String())
	}
}

func TestRunServiceWorkerSanitizesFatalErrors(t *testing.T) {
	var output bytes.Buffer
	code := runServiceWorker(
		serviceWorkerOptions{configPath: "config.yaml"},
		strings.NewReader(""),
		io.Discard,
		&output,
		func(context.Context, string, bool, bool, io.Writer) error {
			return errors.New("token=" + serviceSupervisorGitHubToken)
		},
	)
	if code != 1 {
		t.Fatalf("fatal worker code = %d", code)
	}
	assertServiceOutputRedacted(t, output.String())
	if !strings.Contains(output.String(), "token=<redacted>") {
		t.Fatalf("fatal worker output = %q", output.String())
	}
}
