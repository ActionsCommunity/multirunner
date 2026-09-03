package backend

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
	nerdctl    string // path to nerdctl.exe
	address    string // containerd pipe, e.g. \\.\pipe\containerd-multirunner
	namespace  string
	isolation  string // "process" | "hyperv"
	runCommand func(context.Context, ...string) (string, error)
}

var _ OwnedRunnerStore = (*containerdBackend)(nil)

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
	if b.runCommand != nil {
		return b.runCommand(ctx, args...)
	}
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
	if err := req.Ownership.Validate(); err != nil {
		return nil, fmt.Errorf("containerd: %w", err)
	}
	if !req.Ownership.IsZero() && req.Ownership.RunnerID == 0 {
		return nil, fmt.Errorf("containerd: runner ID is required for owned launches")
	}
	args := []string{"run", "-d", "--name", req.Name, "--isolation", b.isolation}

	// nerdctl can't --add-host on Windows, and host.docker.internal doesn't
	// resolve on the nat network — so point the cache URL at the container
	// network's host gateway (which is the host running the cache server).
	env := req.Env
	if CacheHost(req.Env) != "" {
		if gw := b.hostGatewayIP(ctx); gw != "" {
			env = RewriteCacheHost(req.Env, gw)
		}
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
		"--label", labelManaged+"=true",
		"--label", labelName+"="+req.Name,
		req.Image,
	)
	if labels := ownershipLabels(req.Ownership); len(labels) > 0 {
		keys := make([]string, 0, len(labels))
		for key := range labels {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		imageIndex := len(args) - 1
		ownershipArgs := make([]string, 0, len(keys)*2)
		for _, key := range keys {
			ownershipArgs = append(ownershipArgs, "--label", key+"="+labels[key])
		}
		args = append(args[:imageIndex], append(ownershipArgs, args[imageIndex:]...)...)
	}

	out, err := b.run(ctx, args...)
	if err != nil {
		// `nerdctl run -d` can create/start the container before its process is
		// cancelled or its response is lost. The deterministic name remains a
		// valid cleanup handle even when no container ID was printed.
		expected := ownershipLabels(req.Ownership)
		if expected == nil {
			expected = make(map[string]string)
		}
		expected[labelManaged] = "true"
		expected[labelName] = req.Name
		return &containerdHandle{
				b: b, name: req.Name, id: req.Name,
				preserve:           !req.Ownership.IsZero(),
				failedLaunchLabels: expected,
			},
			fmt.Errorf("run container %s: %w", req.Name, err)
	}
	id := strings.TrimSpace(out)
	if id == "" {
		id = req.Name
	}
	return &containerdHandle{
		b: b, name: req.Name, id: id,
		preserve: !req.Ownership.IsZero(),
	}, nil
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

func (b *containerdBackend) ListOwnedRunners(ctx context.Context, ownership RunnerOwnership) ([]OwnedRunner, error) {
	if err := ownership.Validate(); err != nil || ownership.IsZero() {
		return nil, fmt.Errorf("containerd: complete runner ownership is required")
	}
	labels := reconciliationLabels(ownership)
	args := []string{"ps", "-a"}
	keys := make([]string, 0, len(labels))
	for key := range labels {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		args = append(args, "--filter", "label="+key+"="+labels[key])
	}
	args = append(args, "--format", "{{.ID}}")
	out, err := b.run(ctx, args...)
	if err != nil {
		return nil, fmt.Errorf("list owned containers: %w", err)
	}

	ids := strings.Fields(out)
	owned := make([]OwnedRunner, 0, len(ids))
	for _, id := range ids {
		raw, err := b.run(ctx, "inspect", "--format", "{{json .Config.Labels}}", id)
		if err != nil {
			return nil, fmt.Errorf("inspect owned container %s: %w", id, err)
		}
		var actual map[string]string
		if err := json.Unmarshal([]byte(strings.TrimSpace(raw)), &actual); err != nil {
			return nil, fmt.Errorf("decode labels for owned container %s: %w", id, err)
		}
		if !labelsMatch(actual, labels) {
			continue
		}
		if runner, ok := ownedRunner(id, actual); ok {
			owned = append(owned, runner)
		}
	}
	return owned, nil
}

func (b *containerdBackend) RemoveOwnedRunner(ctx context.Context, resourceID string) error {
	_, err := b.run(ctx, "rm", "-f", resourceID)
	if isMissingContainerError(err) {
		return nil
	}
	return err
}

func isMissingContainerError(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "no such")
}

// containerdHandle is one running container, addressed by its name.
type containerdHandle struct {
	b                  *containerdBackend
	name               string
	id                 string
	preserve           bool
	failedLaunchLabels map[string]string
}

func (h *containerdHandle) ID() string { return h.id }

func (h *containerdHandle) Wait(ctx context.Context) (int, error) {
	// nerdctl wait blocks until the container exits and prints its exit code.
	out, err := h.b.run(ctx, "wait", h.name)
	if !h.preserve {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), backendCleanupTimeout)
		_, cleanupErr := h.b.run(cleanupCtx, "rm", "-f", h.name)
		cancel()
		if !isMissingContainerError(cleanupErr) {
			err = errors.Join(err, cleanupErr)
		}
	}
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
	if h.failedLaunchLabels != nil {
		raw, err := h.b.run(ctx, "inspect", "--format", "{{json .Config.Labels}}", h.name)
		if isMissingContainerError(err) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("inspect failed container: %w", err)
		}
		var actual map[string]string
		if err := json.Unmarshal([]byte(strings.TrimSpace(raw)), &actual); err != nil {
			return fmt.Errorf("decode failed container labels: %w", err)
		}
		if !labelsMatch(actual, h.failedLaunchLabels) {
			return fmt.Errorf("refusing to remove container with mismatched ownership")
		}
	}
	command := "rm"
	args := []string{"-f", h.name}
	if h.preserve {
		command = "stop"
		args = []string{h.name}
	}
	_, err := h.b.run(ctx, append([]string{command}, args...)...)
	if isMissingContainerError(err) {
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
