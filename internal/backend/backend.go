// Package backend abstracts the launching of one ephemeral runner instance.
// Each backend manages a specific container daemon (WSL2 Linux or standalone
// Windows dockerd); a future VM backend can implement the same interface.
package backend

import (
	"context"
	"io"
	"net"
	"net/url"
)

// Mount describes a bind or named-volume mount into a runner container.
type Mount struct {
	Source   string // host path (bind) or volume name (volume)
	Target   string // path inside the container
	ReadOnly bool
	Volume   bool // true => named volume, false => host bind mount
}

// LaunchRequest carries everything needed to start one ephemeral runner slot.
type LaunchRequest struct {
	// Name uniquely identifies this runner instance for this provisioning cycle.
	Name string
	// Image is the runner container image to launch.
	Image string
	// EncodedJITConfig is the base64 blob from GitHub's generate-jitconfig.
	EncodedJITConfig string
	// WorkFolder is the runner work directory inside the container.
	WorkFolder string
	// Labels are the runner labels (informational; already baked into the JIT config).
	Labels []string
	// Env is injected into the container (JIT_CONFIG, cache redirect, tool cache, ...).
	Env map[string]string
	// Mounts are tool-cache volumes, docker socket (DinD), git mirror, etc.
	Mounts []Mount
	// Index is the slot number within the pool (0-based), for naming/logging.
	Index int
}

// CacheHost returns the hostname in the cache URL (ACTIONS_RESULTS_URL) that must
// resolve to the host running the cache server, or "" if there is no cache
// redirect or the host is already a bare IP (no mapping needed). Container
// backends map it to the host gateway; the VM backend rewrites it to the SLIRP
// host alias so the self-hosted cache is reachable from inside the runner.
func CacheHost(env map[string]string) string {
	u := env["ACTIONS_RESULTS_URL"]
	if u == "" {
		return ""
	}
	p, err := url.Parse(u)
	if err != nil {
		return ""
	}
	h := p.Hostname()
	if net.ParseIP(h) != nil {
		return ""
	}
	return h
}

// RewriteCacheHost returns a copy of env with the cache URLs' hostname replaced
// by host (preserving the port). Used by backends where the configured cache
// hostname won't resolve from inside the runner (the VM rewrites it to the SLIRP
// alias; containerd rewrites it to the container network's host gateway, since
// nerdctl can't add hosts on Windows).
func RewriteCacheHost(env map[string]string, host string) map[string]string {
	out := make(map[string]string, len(env))
	for k, v := range env {
		out[k] = v
	}
	for _, key := range []string{"ACTIONS_RESULTS_URL", "ACTIONS_CACHE_URL", "MR_GIT_BUNDLE_URL"} {
		u, ok := out[key]
		if !ok {
			continue
		}
		p, err := url.Parse(u)
		if err != nil || p.Hostname() == "" || net.ParseIP(p.Hostname()) != nil {
			continue
		}
		h := host
		if port := p.Port(); port != "" {
			h += ":" + port
		}
		p.Host = h
		out[key] = p.String()
	}
	return out
}

// RunnerHandle represents one running ephemeral runner instance.
type RunnerHandle interface {
	// Wait blocks until the runner exits and returns its exit code.
	Wait(ctx context.Context) (exitCode int, err error)
	// Logs returns a reader for the runner's combined stdout/stderr.
	Logs(ctx context.Context) (io.ReadCloser, error)
	// Kill forcibly terminates the runner (used during shutdown).
	Kill(ctx context.Context) error
	// ID returns the backend-specific identifier (container ID) for logging.
	ID() string
}

// Backend creates and manages ephemeral runner instances on one daemon.
type Backend interface {
	// Name is a human-readable identifier, e.g. "docker-linux".
	Name() string
	// Ping verifies the backend is reachable.
	Ping(ctx context.Context) error
	// OSType returns the daemon's container OS ("linux" or "windows").
	OSType(ctx context.Context) (string, error)
	// EnsureImage makes sure the runner image is present (pull if missing).
	EnsureImage(ctx context.Context, imageRef string) error
	// Launch starts one ephemeral runner and returns immediately with a handle.
	// If it cannot prove that a failed launch left no live instance, it returns
	// both a cleanup-capable handle and an error. Callers must terminate that
	// handle before releasing the runner registration.
	Launch(ctx context.Context, req LaunchRequest) (RunnerHandle, error)
	// Close releases backend resources (daemon client connections).
	Close() error
}
