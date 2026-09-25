package backend

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"os/exec"
	"sort"
	"strconv"
	"strings"
)

// containerdBackend runs Windows containers via containerd + the runhcs shim,
// driven through the nerdctl CLI. This is the supported Windows-container path:
// the standalone Moby dockerd's bundled hcsshim cannot create the Hyper-V utility
// VM on current Windows builds, and process isolation is Server-only. nerdctl is
// used rather than the raw containerd Go client because it also wires up CNI
// networking and isolation, which the runner container needs to reach GitHub.
type containerdBackend struct {
	nerdctl   string // path to nerdctl.exe
	address   string // containerd pipe, e.g. \\.\pipe\containerd-multirunner
	namespace string
	isolation string // "process" | "hyperv"
}

// NewContainerdWindows builds a Windows-container backend on containerd/runhcs.
// isolation "" or "auto" picks process on Windows Server, hyperv on client.
func NewContainerdWindows(nerdctlPath, address, namespace, isolation string) (Backend, error) {
	if nerdctlPath == "" {
		p, err := exec.LookPath("nerdctl.exe")
		if err != nil {
			return nil, fmt.Errorf("nerdctl not found (set containerd.nerdctl or install it): %w", err)
		}
		nerdctlPath = p
	}
	if address == "" {
		address = `\\.\pipe\containerd-containerd` // containerd's default Windows pipe
	}
	if namespace == "" {
		namespace = "multirunner"
	}
	if isolation == "" || isolation == "auto" {
		isolation = autoIsolation()
	}
	return &containerdBackend{nerdctl: nerdctlPath, address: address, namespace: namespace, isolation: isolation}, nil
}

func (b *containerdBackend) Name() string { return "containerd-windows" }

// run executes nerdctl with the configured address+namespace and returns stdout.
func (b *containerdBackend) run(ctx context.Context, args ...string) (string, error) {
	full := append([]string{"--address", b.address, "--namespace", b.namespace}, args...)
	cmd := exec.CommandContext(ctx, b.nerdctl, full...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return out.String(), fmt.Errorf("nerdctl %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(errb.String()))
	}
	return out.String(), nil
}

func (b *containerdBackend) Ping(ctx context.Context) error {
	if _, err := b.run(ctx, "version", "--format", "{{.Server.Version}}"); err != nil {
		return fmt.Errorf("ping containerd: %w", err)
	}
	return nil
}

func (b *containerdBackend) OSType(ctx context.Context) (string, error) {
	// containerd with the runhcs shim only runs Windows containers.
	return "windows", nil
}

func (b *containerdBackend) EnsureImage(ctx context.Context, imageRef string) error {
	if _, err := b.run(ctx, "image", "inspect", imageRef); err == nil {
		return nil
	}
	if _, err := b.run(ctx, "pull", imageRef); err != nil {
		return fmt.Errorf("pull %s: %w", imageRef, err)
	}
	return nil
}

func (b *containerdBackend) Launch(ctx context.Context, req LaunchRequest) (RunnerHandle, error) {
	if err := req.validateContainerSettings(); err != nil {
		return nil, fmt.Errorf("container %s settings: %w", req.Name, err)
	}
	if req.Container.MemorySwapBytes != 0 {
		return nil, fmt.Errorf("container %s settings: memory swap is unsupported by nerdctl on Windows", req.Name)
	}
	if len(req.Container.DNS) != 0 {
		return nil, fmt.Errorf("container %s settings: DNS is unsupported by nerdctl on Windows", req.Name)
	}

	// nerdctl can't --add-host on Windows, and host.docker.internal doesn't
	// resolve on the nat network — so point the cache URL at the container
	// network's host gateway (which is the host running the cache server).
	env := req.Env
	if CacheHost(req.Env) != "" {
		if gw := b.hostGatewayIP(ctx); gw != "" {
			env = RewriteCacheHost(req.Env, gw)
		}
	}

	args := b.launchArgs(req, env)
	out, err := b.run(ctx, args...)
	if err != nil {
		// `nerdctl run -d` can create or start the container before its process
		// is cancelled or its response is lost. The deterministic name remains
		// a valid cleanup handle even when no container ID was printed.
		return &containerdHandle{b: b, name: req.Name, id: req.Name},
			fmt.Errorf("run container %s: %w", req.Name, err)
	}
	id := strings.TrimSpace(out)
	if id == "" {
		id = req.Name
	}
	return &containerdHandle{b: b, name: req.Name, id: id}, nil
}

func (b *containerdBackend) launchArgs(req LaunchRequest, env map[string]string) []string {
	args := []string{"run", "-d", "--name", req.Name, "--isolation", b.isolation}
	if req.Container.CPUCount != 0 {
		args = append(args, "--cpus", strconv.FormatInt(req.Container.CPUCount, 10))
	}
	if req.Container.MemoryBytes != 0 {
		args = append(args, "--memory", strconv.FormatInt(req.Container.MemoryBytes, 10))
	}

	args = append(args, "-e", "JIT_CONFIG="+req.EncodedJITConfig)
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		args = append(args, "-e", k+"="+env[k])
	}

	for _, m := range req.Mounts {
		v := m.Source + ":" + m.Target
		if m.ReadOnly {
			v += ":ro"
		}
		args = append(args, "-v", v)
	}
	args = append(args,
		"--label", "multirunner=true",
		"--label", "multirunner.name="+req.Name,
		req.Image,
	)
	return args
}

// hostGatewayIP returns the host's IPv4 on the "vEthernet (nat)" interface — the
// gateway of the default nat container network, i.e. the host's address as seen
// from inside a Windows container (nerdctl's network inspect leaves IPAM empty
// for HNS-managed networks, so we read the interface directly).
func (b *containerdBackend) hostGatewayIP(ctx context.Context) string {
	ifs, err := net.Interfaces()
	if err != nil {
		return ""
	}
	for _, ifc := range ifs {
		if !strings.Contains(strings.ToLower(ifc.Name), "(nat)") {
			continue
		}
		addrs, _ := ifc.Addrs()
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok && ipn.IP.To4() != nil {
				return ipn.IP.String()
			}
		}
	}
	return ""
}

func (b *containerdBackend) Close() error { return nil }

// containerdHandle is one running container, addressed by its name.
type containerdHandle struct {
	b    *containerdBackend
	name string
	id   string
}

func (h *containerdHandle) ID() string { return h.id }

func (h *containerdHandle) Wait(ctx context.Context) (int, error) {
	// nerdctl wait blocks until the container exits and prints its exit code.
	out, err := h.b.run(ctx, "wait", h.name)
	// Clean up the stopped container regardless (no --rm so wait can read the code).
	_, _ = h.b.run(context.WithoutCancel(ctx), "rm", "-f", h.name)
	if err != nil {
		return -1, err
	}
	code, convErr := strconv.Atoi(strings.TrimSpace(out))
	if convErr != nil {
		return -1, fmt.Errorf("parse exit code %q: %w", strings.TrimSpace(out), convErr)
	}
	return code, nil
}

func (h *containerdHandle) Logs(ctx context.Context) (io.ReadCloser, error) {
	cmd := exec.CommandContext(ctx, h.b.nerdctl,
		"--address", h.b.address, "--namespace", h.b.namespace, "logs", "-f", h.name)
	pr, pw := io.Pipe()
	cmd.Stdout, cmd.Stderr = pw, pw
	if err := cmd.Start(); err != nil {
		_ = pw.Close()
		return nil, err
	}
	go func() { pw.CloseWithError(cmd.Wait()) }()
	return &cmdReadCloser{ReadCloser: pr, cmd: cmd}, nil
}

func (h *containerdHandle) Kill(ctx context.Context) error {
	_, err := h.b.run(ctx, "rm", "-f", h.name)
	if err != nil && strings.Contains(err.Error(), "no such") {
		return nil
	}
	return err
}

// cmdReadCloser ties a log stream's lifetime to the nerdctl process.
type cmdReadCloser struct {
	io.ReadCloser
	cmd *exec.Cmd
}

func (c *cmdReadCloser) Close() error {
	err := c.ReadCloser.Close()
	_ = c.cmd.Process.Kill()
	_ = c.cmd.Wait()
	return err
}
