//go:build linux

package main

import (
	"github.com/kardianos/service"
)

func captureServiceOutput(bool, service.Logger) (func(), error) {
	// systemd owns these handles, so crash output survives the process.
	return func() {}, nil
}
