package backend

import "github.com/docker/docker/api/types/container"

// NewDockerWindows creates a backend bound to a Windows Docker daemon (a
// standalone dockerd.exe in Windows-container mode, typically on a custom named
// pipe such as npipe:////./pipe/docker_engine_windows). isolation "" or "auto"
// picks process on Windows Server and hyperv on client editions; "process" or
// "hyperv" force the mode. Mirrors NewContainerdWindows so both Windows
// backends resolve isolation identically.
func NewDockerWindows(host, isolation string) (Backend, error) {
	if isolation == "" || isolation == "auto" {
		isolation = autoIsolation()
	}
	return newDockerBackend("docker-windows", host, container.Isolation(isolation))
}
