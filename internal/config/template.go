package config

import (
	"fmt"
	"runtime"
)

// ExamplePoolYAML is the smallest pool that validates. It is shown in the error
// a config without pools produces, where a short, paste-ready answer beats a
// tour of the schema.
const ExamplePoolYAML = `pools:
  - name: linux-pool
    os: linux
    size: 2
    labels: [self-hosted, linux, x64]
    docker:
      host: "tcp://127.0.0.1:2375"
`

// FallbackDockerHost is the endpoint written when nothing on this host answered
// a probe: the named pipe Docker Desktop and Podman expose on Windows, the
// daemon socket everywhere else. It is a placeholder to edit, not a claim -
// `multirunner doctor` reports whether it is reachable.
func FallbackDockerHost() string {
	if runtime.GOOS == "windows" {
		return `npipe:////./pipe/docker_engine`
	}
	return "unix:///var/run/docker.sock"
}

// PoolsYAML is the pool connect writes into a config that has none. It is a
// live, valid pool rather than a commented template: a config that loads is a
// better starting point than one the reader has to assemble. dockerHost is the
// endpoint discovery actually found on this machine; an empty value falls back
// to the platform default for the reader to correct.
func PoolsYAML(dockerHost string) string {
	if dockerHost == "" {
		dockerHost = FallbackDockerHost()
	}
	return fmt.Sprintf(`
pools:
  # A first pool, ready to run. `+"`multirunner doctor`"+` checks it against this host.
  - name: linux-pool
    os: linux                         # linux | windows
    size: 2                           # idle runners, or max capacity when autoscaling
    labels: [self-hosted, linux, x64]
    image_tier: minimal               # linux: minimal, native-build, node, dotnet
    #                                 # windows: minimal, node, dotnet, buildtools
    # image: "ghcr.io/my-org/custom-runner:latest"   # or pin your own image
    docker:
      # Other common endpoints:
      #   tcp://127.0.0.1:2375             WSL2 Docker Engine over TCP
      #   npipe:////./pipe/docker_engine   Docker Desktop / Podman on Windows
      #   unix:///var/run/docker.sock      Linux host daemon
      host: %q
      enable_dind: false              # mounts the host Docker socket: trusted jobs only
    tool_cache:
      mode: shared-volume             # shared-volume | off (persists hostedtoolcache)

# Optional, all defaulted:
#   provisioning: scaleset | pool | autoscale
#     Who triggers runners. Unset means scaleset for repo and org scopes, pool
#     for scope=repos. scaleset lets GitHub report the desired count over an
#     outbound long-poll; pool keeps size runners idle; autoscale launches on
#     demand from queued jobs and/or workflow_job webhooks (needs a public URL).
#   pools[].backend: docker | containerd | qemu
#     qemu runs a Windows VM and needs no docker.host.
#   pools[].scale_set / runner_group
#     scaleset only; scale_set defaults to the pool name.
#
# Cache, git cache, metrics and webhook tuning are documented in
# config.example.yaml in the multirunner repository.
`, dockerHost)
}
