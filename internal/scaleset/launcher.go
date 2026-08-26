// Package scaleset provisions runners from a GitHub Actions runner scale set.
//
// The other provisioning modes decide when to launch a runner themselves: pool
// keeps a fixed number idle, and autoscale reacts to workflow_job events it
// polls for or receives by webhook. This mode does neither. It holds a
// long-poll session open through github.com/actions/scaleset and lets GitHub
// report how many runners the scale set should have, which is the same
// mechanism actions-runner-controller uses.
//
// Only the source of the decision changes. Runners are still ephemeral, still
// carry a JIT config, and are still started through backend.Backend, so every
// existing backend works here unchanged.
package scaleset

import (
	"context"
	"fmt"
	"sync"

	"github.com/actions/scaleset"

	"github.com/GerardSmit/multirunner/internal/backend"
)

// jitGenerator is the part of *scaleset.Client this package needs. Narrowing it
// to one method keeps the launcher testable without a live GitHub session.
type jitGenerator interface {
	GenerateJitRunnerConfig(
		ctx context.Context,
		setting *scaleset.RunnerScaleSetJitRunnerSetting,
		scaleSetID int,
	) (*scaleset.RunnerScaleSetJitRunnerConfig, error)
}

// Options configures a Launcher.
type Options struct {
	// ScaleSetID identifies the scale set this launcher provisions for.
	ScaleSetID int
	// Image is the runner container image to launch.
	Image string
	// WorkFolder is the runner work directory inside the container.
	WorkFolder string
	// Labels are informational; they are already baked into the JIT config.
	Labels []string
	// Env is injected into every runner (cache redirect, tool cache, ...).
	Env map[string]string
	// Mounts are tool-cache volumes, git mirror, and similar.
	Mounts []backend.Mount
	// MaxRunners caps concurrent runners regardless of what GitHub asks for.
	// Zero means the host will honour any requested count.
	MaxRunners int
}

// Launcher implements the scale set listener's handler interface by translating
// a desired runner count into backend launches.
//
// A Launcher is safe for concurrent use.
type Launcher struct {
	jit  jitGenerator
	be   backend.Backend
	opts Options

	mu      sync.Mutex
	running map[string]struct{}
	seq     int
}

// New returns a Launcher that provisions onto be.
func New(jit jitGenerator, be backend.Backend, opts Options) *Launcher {
	return &Launcher{
		jit:     jit,
		be:      be,
		opts:    opts,
		running: make(map[string]struct{}),
	}
}

// allowedLocked reports how many runners may be started to reach want, given
// how many are already running and the configured cap. Callers hold l.mu.
func (l *Launcher) allowedLocked(want int) int {
	have := len(l.running)
	if want <= have {
		return 0
	}
	start := want - have
	if l.opts.MaxRunners > 0 {
		free := l.opts.MaxRunners - have
		if free <= 0 {
			return 0
		}
		if start > free {
			start = free
		}
	}
	return start
}

// HandleDesiredRunnerCount starts runners until the desired count is met and
// reports how many this host is actually serving. Returning a smaller number
// tells GitHub the host is at capacity rather than silently dropping work.
//
// Runners are ephemeral: each exits after one job and removes itself, so
// nothing is torn down here. A runner that is still up may be mid-job.
func (l *Launcher) HandleDesiredRunnerCount(ctx context.Context, count int) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	for n := l.allowedLocked(count); n > 0; n-- {
		if err := l.launchLocked(ctx); err != nil {
			// Report what actually started. A partial launch is still progress,
			// and the listener asks again on the next assignment.
			return len(l.running), err
		}
	}
	return len(l.running), nil
}

// launchLocked generates a JIT config and starts one runner. Callers hold l.mu.
func (l *Launcher) launchLocked(ctx context.Context) error {
	l.seq++
	name := fmt.Sprintf("mr-scaleset-%d-%d", l.opts.ScaleSetID, l.seq)

	jit, err := l.jit.GenerateJitRunnerConfig(
		ctx,
		&scaleset.RunnerScaleSetJitRunnerSetting{Name: name},
		l.opts.ScaleSetID,
	)
	if err != nil {
		return fmt.Errorf("scaleset: generate JIT config for %s: %w", name, err)
	}

	// This is the whole integration point. The scale set listener hands back
	// the same base64 blob generate-jitconfig returns, which is exactly what
	// LaunchRequest already carries, so no backend needs to change.
	handle, err := l.be.Launch(ctx, backend.LaunchRequest{
		Name:             name,
		Image:            l.opts.Image,
		EncodedJITConfig: jit.EncodedJITConfig,
		WorkFolder:       l.opts.WorkFolder,
		Labels:           l.opts.Labels,
		Env:              l.opts.Env,
		Mounts:           l.opts.Mounts,
		Index:            l.seq,
	})
	if err != nil {
		return fmt.Errorf("scaleset: launch %s: %w", name, err)
	}

	l.running[name] = struct{}{}
	go l.awaitExit(name, handle)
	return nil
}

// awaitExit frees the slot once the runner finishes. Each runner is ephemeral,
// so exactly one exit is expected per launch.
func (l *Launcher) awaitExit(name string, h backend.RunnerHandle) {
	_, _ = h.Wait(context.Background())

	l.mu.Lock()
	delete(l.running, name)
	l.mu.Unlock()
}

// HandleJobStarted records that an assignment became a running job. The runner
// was already started in response to the desired count, so there is nothing to
// provision here.
func (l *Launcher) HandleJobStarted(_ context.Context, _ *scaleset.JobStarted) error {
	return nil
}

// HandleJobCompleted is a no-op. The runner's own exit frees its slot, which
// covers the job-failed and runner-crashed cases too.
func (l *Launcher) HandleJobCompleted(_ context.Context, _ *scaleset.JobCompleted) error {
	return nil
}

// Running reports how many runners are currently alive.
func (l *Launcher) Running() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.running)
}
