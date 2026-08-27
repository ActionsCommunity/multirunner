// Package runner runs the lifecycle of a single ephemeral runner instance:
// fetch a fresh JIT config, launch it on a backend, stream its logs, and wait
// for it to finish its one job.
package runner

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/docker/docker/pkg/stdcopy"

	"github.com/GerardSmit/multirunner/internal/backend"
	"github.com/GerardSmit/multirunner/internal/github"
)

// Spec describes one runner to launch.
type Spec struct {
	Name          string
	Image         string
	RunnerGroupID int64
	Labels        []string
	WorkFolder    string
	Env           map[string]string
	Mounts        []backend.Mount
	Index         int
}

// RunOnce provisions a fresh JIT config, launches the runner on the backend,
// streams its logs, and blocks until it exits (after its single job). The
// returned exit code is the runner process exit code.
func RunOnce(ctx context.Context, gh *github.Client, be backend.Backend, spec Spec, logger *slog.Logger) (int, error) {
	jit, err := gh.GenerateJITConfig(ctx, github.JITConfigRequest{
		Name:          spec.Name,
		RunnerGroupID: spec.RunnerGroupID,
		Labels:        spec.Labels,
		WorkFolder:    spec.WorkFolder,
	})
	if err != nil {
		return -1, fmt.Errorf("jit config: %w", err)
	}

	handle, err := be.Launch(ctx, backend.LaunchRequest{
		Name:             spec.Name,
		Image:            spec.Image,
		EncodedJITConfig: jit.EncodedJITConfig,
		WorkFolder:       spec.WorkFolder,
		Labels:           spec.Labels,
		Env:              spec.Env,
		Mounts:           spec.Mounts,
		Index:            spec.Index,
	})
	if err != nil {
		return -1, fmt.Errorf("launch: %w", err)
	}

	logger.Info("runner launched", "name", spec.Name, "container", short(handle.ID()), "runner_id", jit.Runner.ID)

	logCtx, cancelLogs := context.WithCancel(ctx)
	go streamLogs(logCtx, handle, logger.With("runner", spec.Name))

	code, waitErr := handle.Wait(ctx)
	cancelLogs()

	// GitHub removes an ephemeral runner once it finishes its single job, but a
	// runner that exits without taking one (host reboot, bad image, a kill on
	// shutdown) leaves its registration behind permanently. Those pile up as
	// offline runners on the repo and count against its runner limit. The
	// container has exited by the time Wait returns, so cleanup is safe here
	// regardless of why it exited, and in the common case GitHub has already
	// done it for us.
	defer deregister(ctx, gh, jit.Runner.ID, spec.Name, logger)

	if ctx.Err() != nil {
		// Shutdown: terminate the in-flight runner with a detached, bounded
		// context (the job ctx is already cancelled).
		detached, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
		defer cancel()
		_ = handle.Kill(detached)
		return code, ctx.Err()
	}
	if waitErr != nil {
		return code, fmt.Errorf("wait: %w", waitErr)
	}
	logger.Info("runner exited", "name", spec.Name, "exit_code", code)
	return code, nil
}

// cleanupTimeout bounds the detached cleanup calls made once the job context is
// already cancelled.
const cleanupTimeout = 10 * time.Second

// deregister removes a runner registration best-effort. A registration that is
// already gone is the expected outcome for a runner that completed its job, so
// that case is not worth a warning.
func deregister(ctx context.Context, gh *github.Client, runnerID int64, name string, logger *slog.Logger) {
	if runnerID == 0 {
		return
	}
	detached, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
	defer cancel()

	err := gh.DeleteRunner(detached, runnerID)
	switch {
	case err == nil:
		logger.Debug("reclaimed unused runner registration", "name", name, "runner_id", runnerID)
	case errors.Is(err, github.ErrRunnerNotFound):
	default:
		logger.Warn("deregister runner failed", "name", name, "runner_id", runnerID, "err", err)
	}
}

func streamLogs(ctx context.Context, handle backend.RunnerHandle, logger *slog.Logger) {
	rc, err := handle.Logs(ctx)
	if err != nil {
		logger.Debug("logs unavailable", "err", err)
		return
	}
	if rc == nil {
		return // backend provides no log stream (e.g. VM backend)
	}
	defer rc.Close()

	pr, pw := io.Pipe()
	go func() {
		// Container logs (no TTY) are multiplexed; demux stdout+stderr into pw.
		_, _ = stdcopy.StdCopy(pw, pw, rc)
		_ = pw.Close()
	}()

	sc := bufio.NewScanner(pr)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		logger.Debug("job", "line", sc.Text())
	}
}

func short(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}
