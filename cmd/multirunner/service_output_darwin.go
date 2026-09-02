//go:build darwin

package main

import (
	"os"

	"github.com/kardianos/service"

	"github.com/GerardSmit/multirunner/internal/servicehost"
)

func captureServiceOutput(interactive bool, _ service.Logger) (func(), error) {
	if interactive {
		return func() {}, nil
	}
	cleanup, err := servicehost.RegisterLaunchdProcess(os.Getpid())
	if err != nil {
		return nil, err
	}
	return func() {
		_ = cleanup()
	}, nil
}
