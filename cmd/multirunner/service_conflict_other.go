//go:build !windows

package main

import (
	"path/filepath"
)

// runningServiceConfig is not inspected outside Windows; service managers there
// do not expose the installed command line through a stable API.
func runningServiceConfig() string { return "" }

func sameConfigPath(a, b string) bool {
	return filepath.Clean(a) == filepath.Clean(b)
}
