package backend

import (
	"context"
	"fmt"
	"io"
	"sort"

	"github.com/docker/docker/api/types/container"
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
	if _, _, err := b.cli.ImageInspectWithRaw(ctx, imageRef); err == nil {
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
	cfg, host, err := b.launchConfigs(req)
	if err != nil {
		return nil, err
	}

	created, err := b.cli.ContainerCreate(ctx, cfg, host, nil, nil, req.Name)
	if err != nil {
		// A lost create response can still leave a container behind. Docker APIs
		// accept the deterministic name anywhere an ID is accepted, so return a
		// cleanup handle even when the daemon did not return the created ID.
		return &dockerHandle{cli: b.cli, id: req.Name}, fmt.Errorf("create container %s: %w", req.Name, err)
	}
	handle := &dockerHandle{cli: b.cli, id: created.ID}
	if err := b.cli.ContainerStart(ctx, created.ID, container.StartOptions{}); err != nil {
		// The daemon may have started the container before the response was lost.
		// Return its handle so the lifecycle owner can terminate it with a
		// detached cleanup context before deleting the GitHub registration.
		return handle, fmt.Errorf("start container %s: %w", req.Name, err)
	}
	return handle, nil
}

func (b *dockerBackend) launchConfigs(req LaunchRequest) (*container.Config, *container.HostConfig, error) {
	if err := req.validateContainerSettings(); err != nil {
		return nil, nil, fmt.Errorf("container %s settings: %w", req.Name, err)
	}
	isWindows := b.name == "docker-windows"
	if isWindows && req.MemorySwapBytes != 0 {
		return nil, nil, fmt.Errorf("container %s settings: memory swap is unsupported by Docker on Windows", req.Name)
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
			"multirunner":       "true",
			"multirunner.name":  req.Name,
			"multirunner.index": fmt.Sprintf("%d", req.Index),
		},
	}

	host := &container.HostConfig{
		AutoRemove: true,
		Mounts:     toDockerMounts(req.Mounts),
		DNS:        append([]string(nil), req.DNS...),
	}
	host.Memory = req.MemoryBytes
	host.MemorySwap = req.MemorySwapBytes
	if isWindows {
		host.CPUCount = req.CPUCount
	} else {
		host.NanoCPUs = req.CPUCount * nanoCPUsPerCPU
	}
	if b.isolation != "" {
		host.Isolation = b.isolation
	}
	// Make the cache server's hostname resolve to the host (host.docker.internal
	// is not auto-added on plain Linux Docker), so the self-hosted cache works.
	if ch := CacheHost(req.Env); ch != "" {
		host.ExtraHosts = append(host.ExtraHosts, ch+":host-gateway")
	}
	return cfg, host, nil
}

func (b *dockerBackend) Close() error { return b.cli.Close() }

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
	cli *client.Client
	id  string
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
	// Best-effort: stop, then force-remove (AutoRemove may already handle removal).
	_ = h.cli.ContainerStop(ctx, h.id, container.StopOptions{})
	err := h.cli.ContainerRemove(ctx, h.id, container.RemoveOptions{Force: true})
	if client.IsErrNotFound(err) {
		return nil
	}
	return err
}
