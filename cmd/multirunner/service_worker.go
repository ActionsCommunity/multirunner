package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
)

type serviceWorkerOptions struct {
	configPath  string
	interactive bool
	installDeps bool
}

type workerOrchestrator func(context.Context, string, bool, bool, io.Writer) error

func init() {
	if os.Getenv(serviceWorkerMarkerEnv) != serviceWorkerMarkerValue {
		return
	}
	os.Exit(runServiceWorkerFromEnvironment())
}

func runServiceWorkerFromEnvironment() int {
	options, err := serviceWorkerOptionsFromEnvironment()
	if err != nil {
		fmt.Fprintln(os.Stderr, "multirunner service worker: "+err.Error())
		return 1
	}
	clearServiceWorkerEnvironment()
	return runServiceWorker(options, os.Stdin, os.Stdout, os.Stderr, runOrchestrator)
}

func serviceWorkerOptionsFromEnvironment() (serviceWorkerOptions, error) {
	configPath := os.Getenv(serviceWorkerConfigEnv)
	if configPath == "" {
		return serviceWorkerOptions{}, errors.New("missing internal config path")
	}
	installDeps, err := strconv.ParseBool(os.Getenv(serviceWorkerInstallDepsEnv))
	if err != nil {
		return serviceWorkerOptions{}, errors.New("invalid internal install-deps value")
	}
	interactive, err := strconv.ParseBool(os.Getenv(serviceWorkerInteractiveEnv))
	if err != nil {
		return serviceWorkerOptions{}, errors.New("invalid internal interactive value")
	}
	return serviceWorkerOptions{
		configPath:  configPath,
		interactive: interactive,
		installDeps: installDeps,
	}, nil
}

func clearServiceWorkerEnvironment() {
	for _, name := range []string{
		serviceWorkerMarkerEnv,
		serviceWorkerConfigEnv,
		serviceWorkerInstallDepsEnv,
		serviceWorkerInteractiveEnv,
	} {
		_ = os.Unsetenv(name)
	}
}

func runServiceWorker(options serviceWorkerOptions, control io.Reader, output, errorOutput io.Writer, run workerOrchestrator) int {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		_, _ = io.Copy(io.Discard, control)
		cancel()
	}()
	if err := run(ctx, options.configPath, options.interactive, options.installDeps, output); err != nil {
		fmt.Fprintln(errorOutput, sanitizeLogText(err.Error()))
		return 1
	}
	return 0
}
