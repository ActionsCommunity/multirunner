//go:build linux || darwin

package main

import (
	"github.com/kardianos/service"
)

func captureServiceOutput(bool, service.Logger) (func(), error) {
	// systemd and launchd own these handles, so crash output survives the process.
	return func() {}, nil
}
