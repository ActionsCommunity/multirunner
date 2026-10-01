//go:build !windows

package backend

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

// candidateDockerHosts lists the endpoints to probe on Linux and macOS. Which
// one exists is what distinguishes a root daemon from rootless Docker, Podman,
// Docker Desktop, Colima and Rancher Desktop — the lookup an operator would
// otherwise have to do by hand.
func candidateDockerHosts() []string {
	var hosts []string
	add := func(path string) {
		if path != "" {
			hosts = append(hosts, "unix://"+path)
		}
	}

	add("/var/run/docker.sock")

	// Rootless Docker and Podman both live under the user's runtime directory.
	runtimeDir := os.Getenv("XDG_RUNTIME_DIR")
	if runtimeDir == "" {
		runtimeDir = fmt.Sprintf("/run/user/%d", os.Getuid())
	}
	add(filepath.Join(runtimeDir, "docker.sock"))
	add(filepath.Join(runtimeDir, "podman", "podman.sock"))
	add("/var/run/podman/podman.sock")

	// The desktop distributions put their socket under the user's home.
	if home, err := os.UserHomeDir(); err == nil {
		add(filepath.Join(home, ".docker", "run", "docker.sock"))
		add(filepath.Join(home, ".docker", "desktop", "docker.sock"))
		add(filepath.Join(home, ".rd", "docker.sock"))
		add(filepath.Join(home, ".colima", "default", "docker.sock"))
		if runtime.GOOS == "darwin" {
			add(filepath.Join(home, ".local", "share", "containers", "podman", "machine", "podman.sock"))
		}
	}

	return append(hosts, "tcp://127.0.0.1:2375")
}
