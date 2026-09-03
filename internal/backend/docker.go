package backend

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	imagetypes "github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/client"
)

// dockerBackend drives a single Docker daemon (Linux or Windows). The OS-specific
// constructors set host, name, and isolation.
type dockerBackend struct {
	cli       *client.Client
	name      string
	isolation container.Isolation // empty for Linux; "process"/"hyperv" for Windows
	autoPull  bool
}

var _ OwnedRunnerStore = (*dockerBackend)(nil)

func newDockerBackend(name, host string, isolation container.Isolation) (*dockerBackend, error) {
	cli, err := client.NewClientWithOpts(
		client.WithHost(host),
		client.WithAPIVersionNegotiation(),
	)
	if err != nil {
		return nil, fmt.Errorf("docker client (%s): %w", host, err)
	}
	return &dockerBackend{cli: cli, name: name, isolation: isolation, autoPull: true}, nil
}

func (b *dockerBackend) Name() string { return b.name }

func (b *dockerBackend) Ping(ctx context.Context) error {
	if _, err := b.cli.Ping(ctx); err != nil {
		return fmt.Errorf("ping %s: %w", b.name, err)
	}
	return nil
}

func (b *dockerBackend) OSType(ctx context.Context) (string, error) {
	info, err := b.cli.Info(ctx)
	if err != nil {
		return "", fmt.Errorf("daemon info %s: %w", b.name, err)
	}
	return info.OSType, nil
}

func (b *dockerBackend) EnsureImage(ctx context.Context, imageRef string) error {
	if _, err := b.cli.ImageInspect(ctx, imageRef); err == nil {
		return nil
	}
	if !b.autoPull {
		return fmt.Errorf("image %s not present and auto-pull disabled", imageRef)
	}
	rc, err := b.cli.ImagePull(ctx, imageRef, imagetypes.PullOptions{})
	if err != nil {
		return fmt.Errorf("pull %s: %w", imageRef, err)
	}
	defer rc.Close()
	// Drain the pull progress stream so the pull completes.
	if _, err := io.Copy(io.Discard, rc); err != nil {
		return fmt.Errorf("pull %s (drain): %w", imageRef, err)
	}
	return nil
}

func (b *dockerBackend) Launch(ctx context.Context, req LaunchRequest) (RunnerHandle, error) {
	if err := req.Ownership.Validate(); err != nil {
		return nil, fmt.Errorf("docker: %w", err)
	}
	if !req.Ownership.IsZero() && req.Ownership.RunnerID == 0 {
		return nil, fmt.Errorf("docker: runner ID is required for owned launches")
	}
	env := make([]string, 0, len(req.Env)+1)
	env = append(env, "JIT_CONFIG="+req.EncodedJITConfig)
	keys := make([]string, 0, len(req.Env))
	for k := range req.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		env = append(env, k+"="+req.Env[k])
	}

	cfg := &container.Config{
		Image: req.Image,
		Env:   env,
		Labels: map[string]string{
			labelManaged: "true",
			labelName:    req.Name,
			labelIndex:   fmt.Sprintf("%d", req.Index),
		},
	}
	for key, value := range ownershipLabels(req.Ownership) {
		cfg.Labels[key] = value
	}

	host := &container.HostConfig{
		// Scale-set cleanup removes the stopped container only after GitHub
		// deregistration succeeds. Pool launchers still remove it through Kill.
		AutoRemove: req.Ownership.IsZero(),
		Mounts:     toDockerMounts(req.Mounts),
	}
	if b.isolation != "" {
		host.Isolation = b.isolation
	}
	// Make the cache server's hostname resolve to the host (host.docker.internal
	// is not auto-added on plain Linux Docker), so the self-hosted cache works.
	if ch := CacheHost(req.Env); ch != "" {
		host.ExtraHosts = append(host.ExtraHosts, ch+":host-gateway")
	}

	handle := &dockerHandle{
		cli:      b.cli,
		id:       req.Name,
		preserve: !req.Ownership.IsZero(),
	}
	created, err := b.cli.ContainerCreate(ctx, cfg, host, nil, nil, req.Name)
	if err != nil {
		// A lost create response can still leave a container behind. Docker APIs
		// accept the deterministic name anywhere an ID is accepted, so return a
		// cleanup handle even when the daemon did not return the created ID.
		return handle, fmt.Errorf("create container %s: %w", req.Name, err)
	}
	handle.id = created.ID
	if err := b.cli.ContainerStart(ctx, created.ID, container.StartOptions{}); err != nil {
		// The daemon may have started the container before the response was lost.
		// Return its handle so the lifecycle owner can terminate it with a
		// detached cleanup context before deleting the GitHub registration.
		return handle, fmt.Errorf("start container %s: %w", req.Name, err)
	}
	return handle, nil
}

func (b *dockerBackend) Close() error { return b.cli.Close() }

func (b *dockerBackend) ListOwnedRunners(ctx context.Context, ownership RunnerOwnership) ([]OwnedRunner, error) {
	if err := ownership.Validate(); err != nil || ownership.IsZero() {
		return nil, fmt.Errorf("docker: complete runner ownership is required")
	}
	labels := reconciliationLabels(ownership)
	args := filters.NewArgs()
	for key, value := range labels {
		args.Add("label", key+"="+value)
	}
	containers, err := b.cli.ContainerList(ctx, container.ListOptions{All: true, Filters: args})
	if err != nil {
		return nil, fmt.Errorf("list owned containers: %w", err)
	}
	owned := make([]OwnedRunner, 0, len(containers))
	for _, candidate := range containers {
		if !labelsMatch(candidate.Labels, labels) {
			continue
		}
		if runner, ok := ownedRunner(candidate.ID, candidate.Labels); ok {
			owned = append(owned, runner)
		}
	}
	return owned, nil
}

func (b *dockerBackend) RemoveOwnedRunner(ctx context.Context, resourceID string) error {
	err := b.cli.ContainerRemove(ctx, resourceID, container.RemoveOptions{Force: true})
	if cerrdefs.IsNotFound(err) {
		return nil
	}
	return err
}

func labelsMatch(actual, expected map[string]string) bool {
	for key, value := range expected {
		if actual[key] != value {
			return false
		}
	}
	return true
}

func toDockerMounts(ms []Mount) []mount.Mount {
	if len(ms) == 0 {
		return nil
	}
	out := make([]mount.Mount, 0, len(ms))
	for _, m := range ms {
		typ := mount.TypeBind
		if m.Volume {
			typ = mount.TypeVolume
		}
		out = append(out, mount.Mount{
			Type:     typ,
			Source:   m.Source,
			Target:   m.Target,
			ReadOnly: m.ReadOnly,
		})
	}
	return out
}

// dockerHandle is a running container.
type dockerHandle struct {
	cli      *client.Client
	id       string
	preserve bool
}

func (h *dockerHandle) ID() string { return h.id }

func (h *dockerHandle) Wait(ctx context.Context) (int, error) {
	statusCh, errCh := h.cli.ContainerWait(ctx, h.id, container.WaitConditionNotRunning)
	select {
	case err := <-errCh:
		return -1, err
	case st := <-statusCh:
		if st.Error != nil {
			return int(st.StatusCode), fmt.Errorf("container wait error: %s", st.Error.Message)
		}
		return int(st.StatusCode), nil
	case <-ctx.Done():
		return -1, ctx.Err()
	}
}

func (h *dockerHandle) Logs(ctx context.Context) (io.ReadCloser, error) {
	return h.cli.ContainerLogs(ctx, h.id, container.LogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Follow:     true,
	})
}

func (h *dockerHandle) Kill(ctx context.Context) error {
	stopErr := h.cli.ContainerStop(ctx, h.id, container.StopOptions{})
	if cerrdefs.IsNotFound(stopErr) {
		stopErr = nil
	}
	if h.preserve {
		return stopErr
	}
	removeErr := h.cli.ContainerRemove(ctx, h.id, container.RemoveOptions{Force: true})
	if cerrdefs.IsNotFound(removeErr) {
		removeErr = nil
	}
	return errors.Join(stopErr, removeErr)
}
