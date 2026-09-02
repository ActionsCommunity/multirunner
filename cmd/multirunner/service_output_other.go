//go:build !windows && !linux && !darwin

package main

import "github.com/kardianos/service"

func captureServiceOutput(bool, service.Logger) (func(), error) {
	// systemd and launchd attach the process streams to their native log sinks.
	return func() {}, nil
}
