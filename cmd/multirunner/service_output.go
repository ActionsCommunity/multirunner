package main

import "github.com/kardianos/service"

// The supervisor owns worker stdout and stderr. The service process itself
// must not redirect descriptors because raw crash text has not been sanitized.
func captureServiceOutput(bool, service.Logger) (func(), error) {
	return func() {}, nil
}
