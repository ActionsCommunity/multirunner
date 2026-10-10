package main

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeSetupConfig(cfgPath string, _ *bufio.Reader, _ io.Writer) error {
	return os.WriteFile(cfgPath, []byte("github: {}\n"), 0o600)
}

func failSetupConnect(t *testing.T) setupConnector {
	return func(string, *bufio.Reader, io.Writer) error {
		t.Fatal("connect must not run")
		return nil
	}
}

func TestEnsureConfigPassesThroughExistingConfig(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(cfgPath, []byte("x: 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	abs, start, err := ensureConfig(cfgPath, false, true, strings.NewReader(""), &out, failSetupConnect(t))
	if err != nil || !start || abs != cfgPath || out.Len() != 0 {
		t.Fatalf("ensureConfig = %q %v %v, output %q", abs, start, err, out.String())
	}
}

func TestEnsureConfigMissingWithoutTerminalExplainsNextStep(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	for _, dryRun := range []bool{false, true} {
		interactive := dryRun
		_, start, err := ensureConfig(cfgPath, dryRun, interactive, strings.NewReader("y\n"), io.Discard, failSetupConnect(t))
		if err == nil || start {
			t.Fatalf("dryRun=%v: expected error, got start=%v", dryRun, start)
		}
		if !strings.Contains(err.Error(), "multirunner connect --config "+cfgPath) {
			t.Fatalf("error lacks next step: %v", err)
		}
	}
}

func TestEnsureConfigGuidedSetupStartsWithDefaultPath(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	var out bytes.Buffer
	var connected string
	connect := func(path string, in *bufio.Reader, w io.Writer) error {
		connected = path
		return writeSetupConfig(path, in, w)
	}
	abs, start, err := ensureConfig(cfgPath, false, true, strings.NewReader("\n\n\n"), &out, connect)
	if err != nil || !start || abs != cfgPath || connected != cfgPath {
		t.Fatalf("ensureConfig = %q %v %v (connected %q)", abs, start, err, connected)
	}
	for _, want := range []string{"No multirunner config found", "Set up multirunner now?", "Start multirunner now?"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("output lacks %q:\n%s", want, out.String())
		}
	}
}

func TestEnsureConfigGuidedSetupHonorsChosenPathAndDecline(t *testing.T) {
	dir := t.TempDir()
	chosen := filepath.Join(dir, "nested", "multirunner.yaml")
	input := "yes\n" + chosen + "\nn\n"
	abs, start, err := ensureConfig(filepath.Join(dir, "config.yaml"), false, true, strings.NewReader(input), io.Discard, writeSetupConfig)
	if err != nil || start || abs != "" {
		t.Fatalf("ensureConfig = %q %v %v", abs, start, err)
	}
	if _, err := os.Stat(chosen); err != nil {
		t.Fatalf("config not written at chosen path: %v", err)
	}
}

func TestEnsureConfigDeclinedSetupDoesNotConnect(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	_, start, err := ensureConfig(cfgPath, false, true, strings.NewReader("n\n"), io.Discard, failSetupConnect(t))
	if err == nil || start || !strings.Contains(err.Error(), "no config file") {
		t.Fatalf("ensureConfig start=%v err=%v", start, err)
	}
}

func TestEnsureConfigReportsConnectFailure(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	want := errors.New("authorization denied")
	connect := func(string, *bufio.Reader, io.Writer) error { return want }
	_, start, err := ensureConfig(cfgPath, false, true, strings.NewReader("\n\n"), io.Discard, connect)
	if !errors.Is(err, want) || start {
		t.Fatalf("ensureConfig start=%v err=%v", start, err)
	}
}
