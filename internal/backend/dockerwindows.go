package backend

import (
	"fmt"
	"runtime"
	"strings"

	"github.com/docker/docker/api/types/container"
)

// NewDockerWindows creates a backend bound to a Windows Docker daemon (a
// standalone dockerd.exe in Windows-container mode, typically on a custom named
// pipe such as npipe:////./pipe/docker_engine_windows). On a local Windows
// controller, isolation "" or "auto" picks process on Windows Server and hyperv
// on client editions. Remote and non-Windows controllers must select "process"
// or "hyperv" explicitly because their local registry cannot describe the
// daemon host.
func NewDockerWindows(host, isolation string) (Backend, error) {
	if isolation == "" || isolation == "auto" {
		if !isLocalWindowsPipe(host) {
			return nil, fmt.Errorf("docker windows isolation=auto requires a verified-local npipe host; set isolation to process or hyperv for %q", host)
		}
		isolation = autoIsolation()
	}
	return newDockerBackend("docker-windows", host, container.Isolation(isolation))
}

func isLocalWindowsPipe(host string) bool {
	return runtime.GOOS == "windows" && strings.HasPrefix(strings.ToLower(host), "npipe://")
}
