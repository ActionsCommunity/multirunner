package backend

import (
	"context"
	"os"
	"sync"
	"time"

	"github.com/docker/docker/client"
)

// DockerEndpoint is a container daemon that answered a ping, and the container
// OS it runs. OSType is what decides whether a pool can use it: a Windows daemon
// cannot run a linux pool however reachable it is.
type DockerEndpoint struct {
	Host   string
	OSType string
}

// discoverTimeout bounds each candidate probe. Discovery runs on a path that is
// already reporting a failure, so it must add a moment, not a stall.
const discoverTimeout = 2 * time.Second

// DiscoverDockerHosts probes the endpoints a Docker-compatible daemon commonly
// listens on and returns the ones that answer. It exists so an unreachable
// docker.host can be reported with the endpoints that do work on this machine,
// rather than leaving the reader to guess which of Docker Desktop, Podman, WSL2
// or rootless they have.
//
// It never returns an error: an endpoint that does not answer is simply absent.
func DiscoverDockerHosts(ctx context.Context) []DockerEndpoint {
	// DOCKER_HOST is an explicit statement about where the daemon is, so it
	// outranks anything found by looking around.
	candidates := candidateDockerHosts()
	if env := os.Getenv("DOCKER_HOST"); env != "" {
		candidates = append([]string{env}, candidates...)
	}
	candidates = dedupe(candidates)

	found := make([]DockerEndpoint, len(candidates))
	var wg sync.WaitGroup
	for i, host := range candidates {
		wg.Add(1)
		go func(i int, host string) {
			defer wg.Done()
			if ep, ok := probeDockerHost(ctx, host); ok {
				found[i] = ep
			}
		}(i, host)
	}
	wg.Wait()

	// Preserve candidate order, which lists the likeliest endpoints first.
	out := make([]DockerEndpoint, 0, len(found))
	for _, ep := range found {
		if ep.Host != "" {
			out = append(out, ep)
		}
	}
	return out
}

func probeDockerHost(ctx context.Context, host string) (DockerEndpoint, bool) {
	ctx, cancel := context.WithTimeout(ctx, discoverTimeout)
	defer cancel()

	cli, err := client.NewClientWithOpts(client.WithHost(host), client.WithAPIVersionNegotiation())
	if err != nil {
		return DockerEndpoint{}, false
	}
	defer cli.Close()

	ping, err := cli.Ping(ctx)
	if err != nil {
		return DockerEndpoint{}, false
	}
	// Ping carries OSType on daemons that report it; Info is the fallback and
	// costs an extra round trip only when needed.
	osType := ping.OSType
	if osType == "" {
		info, err := cli.Info(ctx)
		if err != nil {
			return DockerEndpoint{}, false
		}
		osType = info.OSType
	}
	return DockerEndpoint{Host: host, OSType: osType}, true
}

// dedupe keeps the first occurrence of each host, so an endpoint named both by
// DOCKER_HOST and by the candidate list is probed once.
func dedupe(hosts []string) []string {
	seen := make(map[string]bool, len(hosts))
	out := hosts[:0:0]
	for _, h := range hosts {
		if seen[h] {
			continue
		}
		seen[h] = true
		out = append(out, h)
	}
	return out
}

// PickDockerHost returns the reachable endpoint that runs osType containers, or
// "" when none does. It is what turns discovery into a value a config can hold.
func PickDockerHost(ctx context.Context, osType string) string {
	for _, ep := range DiscoverDockerHosts(ctx) {
		if ep.OSType == osType {
			return ep.Host
		}
	}
	return ""
}
