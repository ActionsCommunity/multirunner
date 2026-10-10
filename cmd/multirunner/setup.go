package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/kardianos/service"
)

// setupConnector runs the interactive connect flow that creates a config.
type setupConnector func(cfgPath string, in *bufio.Reader, out io.Writer) error

// missingConfigError explains how to create a config when none exists and no
// terminal is available to ask.
func missingConfigError(cfgPath string) error {
	return fmt.Errorf("no config file at %s.\n"+
		"Create one with:  multirunner connect --config %s\n"+
		"or pass --config <path> to use an existing config", cfgPath, cfgPath)
}

// ensureConfig returns the config path the orchestrator should use and whether
// it should start. An existing (or unreadable) config passes through so Load
// reports its own errors. A missing config on a terminal offers guided setup
// through connect; off a terminal, or for a dry run, it fails with next steps.
func ensureConfig(cfgPath string, dryRun, interactive bool, in io.Reader, out io.Writer, connect setupConnector) (string, bool, error) {
	abs, err := filepath.Abs(cfgPath)
	if err != nil {
		return "", false, err
	}
	if _, err := os.Stat(abs); !errors.Is(err, os.ErrNotExist) {
		return abs, true, nil
	}
	if dryRun || !interactive {
		return "", false, missingConfigError(abs)
	}

	reader := bufio.NewReader(in)
	p := &prompt{r: reader, out: out}
	fmt.Fprintf(out, "No multirunner config found at %s.\n", abs)
	if !p.yesNo("Set up multirunner now?", true) {
		return "", false, missingConfigError(abs)
	}
	if answer := p.line(fmt.Sprintf("Config file [%s]: ", abs)); answer != "" {
		if abs, err = filepath.Abs(answer); err != nil {
			return "", false, err
		}
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o700); err != nil {
		return "", false, fmt.Errorf("create config directory: %w", err)
	}
	if err := connect(abs, reader, out); err != nil {
		return "", false, err
	}
	if _, err := os.Stat(abs); err != nil {
		return "", false, fmt.Errorf("setup did not create %s: %w", abs, err)
	}
	fmt.Fprintln(out)
	if !p.yesNo("Start multirunner now?", true) {
		return "", false, nil
	}
	return abs, true, nil
}

// connectForSetup runs the same interactive flow as `multirunner connect` with
// no flags: choose the authentication model, authorize, and pick the target.
func connectForSetup(cfgPath string, in *bufio.Reader, out io.Writer) error {
	st := newSteps(out, 3)
	if wantOwnApp(in, st, true) {
		return connectCmd(cfgPath, connectFlags{name: "multirunner"}, in, out, true, st)
	}
	return connectDeviceCmd(cfgPath, connectFlags{}, nil, in, out, true, st)
}

// runWithSetup starts the orchestrator, first offering guided setup when the
// config file does not exist.
func runWithSetup(out io.Writer, cfgPath string, installDeps, dryRun bool) error {
	// Under a service manager, keep the managed failure accounting in runService.
	if !dryRun && !service.Interactive() {
		return runService(cfgPath, installDeps)
	}
	interactive := isTerminal(os.Stdin) && isTerminal(os.Stdout)
	abs, start, err := ensureConfig(cfgPath, dryRun, interactive, os.Stdin, out, connectForSetup)
	if err != nil || !start {
		return err
	}
	if dryRun {
		return planRun(out, abs, installDeps)
	}
	return runService(abs, installDeps)
}
