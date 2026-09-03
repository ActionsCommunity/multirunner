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
  #
  # Everything below is optional except name, os and docker.host. Provisioning
  # defaults to scaleset for repo and org scopes and pool for scope=repos;
  # backend defaults to docker, and qemu needs no docker.host at all. Cache,
  # git cache, metrics and webhook tuning are covered in config.example.yaml.
  - name: linux-pool
    # linux | windows
    os: linux
    # Idle runners, or the cap on concurrent runners when autoscaling.
    size: 2
    # What a workflow's runs-on has to match.
    labels: [self-hosted, linux, x64]
    # linux: minimal, native-build, node, dotnet, rust, go
    # windows: minimal, node, dotnet, buildtools
    # Or set image: instead, to pin one of your own.
    image_tier: minimal
    docker:
      # Other common endpoints:
      #   tcp://127.0.0.1:2375             WSL2 Docker Engine over TCP
      #   npipe:////./pipe/docker_engine   Docker Desktop / Podman on Windows
      #   unix:///var/run/docker.sock      Linux host daemon
      host: %q
      # Mounts the host Docker socket into the job: trusted workflows only.
      enable_dind: false
    tool_cache:
      # shared-volume persists hostedtoolcache between runners; off disables it.
      mode: shared-volume
`, dockerHost)
}
