// Package backend abstracts the launching of one ephemeral runner instance.
// Each backend manages a specific container daemon (WSL2 Linux or standalone
// Windows dockerd); a future VM backend can implement the same interface.
package backend

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/url"
	"strconv"
	"time"
)

const backendCleanupTimeout = 10 * time.Second

const (
	labelManaged    = "multirunner"
	labelName       = "multirunner.name"
	labelIndex      = "multirunner.index"
	labelInstance   = "multirunner.instance"
	labelTarget     = "multirunner.target"
	labelPool       = "multirunner.pool"
	labelScaleSetID = "multirunner.scaleset-id"
	labelRunnerID   = "multirunner.runner-id"
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
	// Ownership identifies a scale-set runner strongly enough for a restarted
	// multirunner instance to reclaim only its own backend resources.
	Ownership RunnerOwnership
}

// RunnerOwnership is persisted as backend metadata for restart reconciliation.
// An empty Instance means the launch is not eligible for reconciliation.
type RunnerOwnership struct {
	Instance   string
	Target     string
	Pool       string
	ScaleSetID int
	RunnerID   int64
}

// IsZero reports whether a request belongs to a provisioning mode that does not
// use restart reconciliation.
func (o RunnerOwnership) IsZero() bool {
	return o == (RunnerOwnership{})
}

// Validate rejects partial ownership, which would preserve a resource without
// leaving enough metadata to reclaim it safely.
func (o RunnerOwnership) Validate() error {
	if o.IsZero() {
		return nil
	}
	if o.Instance == "" || o.Target == "" || o.Pool == "" ||
		o.ScaleSetID == 0 {
		return fmt.Errorf("complete runner ownership is required")
	}
	return nil
}

// OwnedRunner identifies one backend resource proven to match an ownership
// boundary.
type OwnedRunner struct {
	ResourceID string
	Name       string
	RunnerID   int64
}

// OwnedRunnerStore is an optional backend capability used to find and remove
// scale-set resources left behind by a previous process.
type OwnedRunnerStore interface {
	ListOwnedRunners(ctx context.Context, ownership RunnerOwnership) ([]OwnedRunner, error)
	RemoveOwnedRunner(ctx context.Context, resourceID string) error
}

// OwnedRunnerStoreFor returns the optional reconciliation capability without
// exposing backend implementations to lifecycle coordinators.
func OwnedRunnerStoreFor(be Backend) OwnedRunnerStore {
	store, _ := be.(OwnedRunnerStore)
	return store
}

func ownershipLabels(ownership RunnerOwnership) map[string]string {
	if ownership.IsZero() {
		return nil
	}
	return map[string]string{
		labelManaged:    "true",
		labelInstance:   ownership.Instance,
		labelTarget:     ownership.Target,
		labelPool:       ownership.Pool,
		labelScaleSetID: strconv.Itoa(ownership.ScaleSetID),
		labelRunnerID:   strconv.FormatInt(ownership.RunnerID, 10),
	}
}

func reconciliationLabels(ownership RunnerOwnership) map[string]string {
	labels := ownershipLabels(ownership)
	delete(labels, labelScaleSetID)
	delete(labels, labelRunnerID)
	return labels
}

func ownedRunner(resourceID string, labels map[string]string) (OwnedRunner, bool) {
	runnerID, err := strconv.ParseInt(labels[labelRunnerID], 10, 64)
	if err != nil || runnerID == 0 {
		return OwnedRunner{}, false
	}
	return OwnedRunner{
		ResourceID: resourceID,
		Name:       labels[labelName],
		RunnerID:   runnerID,
	}, true
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
