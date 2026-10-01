//go:build windows

package backend

import (
	"os"
	"sort"
	"strings"
)

// candidateDockerHosts lists the endpoints to probe on Windows. The named pipes
// are enumerated rather than guessed, because which one exists is exactly what
// distinguishes Docker Desktop from Podman from a Windows-containers daemon —
// the thing the operator would otherwise have to look up.
func candidateDockerHosts() []string {
	hosts := make([]string, 0, 6)
	for _, name := range dockerPipeNames() {
		hosts = append(hosts, `npipe:////./pipe/`+name)
	}
	// A TCP daemon exposes nothing in the pipe namespace, so it is always tried.
	return append(hosts, "tcp://127.0.0.1:2375")
}

// dockerPipeNames returns the container-daemon pipes present in the pipe
// namespace, likeliest first. Reading the namespace can fail on a locked-down
// host; a fixed list then keeps discovery useful.
func dockerPipeNames() []string {
	entries, err := os.ReadDir(`\\.\pipe\`)
	if err != nil {
		return []string{"docker_engine", "docker_engine_windows", "podman-machine-default"}
	}

	var names []string
	for _, e := range entries {
		name := e.Name()
		lower := strings.ToLower(name)
		if strings.Contains(lower, "docker") || strings.Contains(lower, "podman") {
			names = append(names, name)
		}
	}
	sort.Slice(names, func(i, j int) bool {
		if pi, pj := pipeRank(names[i]), pipeRank(names[j]); pi != pj {
			return pi < pj
		}
		return names[i] < names[j]
	})
	return names
}

// pipeRank orders the enumerated pipes so the general-purpose engine endpoint is
// probed before OS-specific or machine-specific ones.
func pipeRank(name string) int {
	switch strings.ToLower(name) {
	case "docker_engine":
		return 0
	case "docker_engine_windows":
		return 1
	case "podman-machine-default":
		return 2
	default:
		return 3
	}
}
