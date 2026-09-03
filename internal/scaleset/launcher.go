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
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/actions/scaleset"

	"github.com/GerardSmit/multirunner/internal/backend"
)

// jitGenerator is the part of *scaleset.Client this package needs. Narrowing it
// keeps the launcher testable without a live GitHub session.
type jitGenerator interface {
	GenerateJitRunnerConfig(
		ctx context.Context,
		setting *scaleset.RunnerScaleSetJitRunnerSetting,
		scaleSetID int,
	) (*scaleset.RunnerScaleSetJitRunnerConfig, error)
	RemoveRunner(ctx context.Context, runnerID int64) error
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
	// OnStart and OnStop report runner lifecycle events.
	OnStart func()
	OnStop  func(exitCode int, err error)
	// Logger records launches and runner exits. A runner that starts and dies
	// immediately - a deprecated runner build, a bad image, a rejected JIT
	// config - is otherwise invisible: the listener keeps polling, GitHub keeps
	// the job queued, and nothing on the host says why.
	Logger *slog.Logger
	// cleanupTimeout bounds each kill and deregistration operation.
	cleanupTimeout time.Duration
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
	running map[string]runnerState
	seq     int
	// desired is the count GitHub last asked for. A runner that exits frees a
	// slot straight away, but the listener only re-asks on its next message, so
	// without this the freed slot idles until then.
	desired int
	// stopped marks that Shutdown has begun. Refilling past that point would
	// start runners the shutdown is trying to tear down, and they would outlive
	// it.
	stopped bool
}

type runnerState struct {
	handle   backend.RunnerHandle
	runnerID int64
}

// New returns a Launcher that provisions onto be.
func New(jit jitGenerator, be backend.Backend, opts Options) *Launcher {
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	return &Launcher{
		jit:     jit,
		be:      be,
		opts:    opts,
		running: make(map[string]runnerState),
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

	if l.stopped {
		return len(l.running), nil
	}
	l.desired = count
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
func (l *Launcher) launchLocked(ctx context.Context) (err error) {
	if l.opts.OnStart != nil {
		l.opts.OnStart()
	}
	defer func() {
		if err != nil && l.opts.OnStop != nil {
			l.opts.OnStop(-1, err)
		}
	}()

	l.seq++
	name := fmt.Sprintf("mr-scaleset-%d-%s", l.opts.ScaleSetID, shortID())

	jit, err := l.jit.GenerateJitRunnerConfig(
		ctx,
		&scaleset.RunnerScaleSetJitRunnerSetting{
			Name:       name,
			WorkFolder: l.opts.WorkFolder,
		},
		l.opts.ScaleSetID,
	)
	if err != nil {
		return fmt.Errorf("scaleset: generate JIT config for %s: %w", name, err)
	}
	if jit == nil {
		return fmt.Errorf("scaleset: generate JIT config for %s returned nil", name)
	}

	// This is the whole integration point. The scale set listener hands back
	// the same base64 blob generate-jitconfig returns, which is exactly what
	// LaunchRequest already carries, so no backend needs to change.
	l.opts.Logger.Info("starting runner",
		slog.String("runner", name),
		slog.String("image", l.opts.Image),
		slog.Int("running", len(l.running)),
	)

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
		launchErr := fmt.Errorf("scaleset: launch %s: %w", name, err)
		if jit.Runner != nil && jit.Runner.ID != 0 {
			removeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
			defer cancel()
			if removeErr := l.jit.RemoveRunner(removeCtx, int64(jit.Runner.ID)); removeErr != nil {
				return errors.Join(launchErr, fmt.Errorf("scaleset: remove unused runner %s: %w", name, removeErr))
			}
		}
		l.opts.Logger.Error("start runner failed", slog.String("runner", name), slog.Any("error", launchErr))
		return launchErr
	}

	var runnerID int64
	if jit.Runner != nil {
		runnerID = int64(jit.Runner.ID)
	}
	l.running[name] = runnerState{handle: handle, runnerID: runnerID}
	go l.awaitExit(name, handle)
	return nil
}

// awaitExit frees the slot once the runner finishes. Each runner is ephemeral,
// so exactly one exit is expected per launch.
func (l *Launcher) awaitExit(name string, h backend.RunnerHandle) {
	started := time.Now()
	code, err := h.Wait(context.Background())
	lived := time.Since(started)

	// A runner that fails to start is the failure mode that reads as "nothing
	// happens": GitHub holds the job, the listener re-launches on the next poll,
	// and the loop repeats. The runner's exit code is the signal - entrypoint.sh
	// waits on run.sh and propagates it - because elapsed time is not: an
	// ephemeral runner can legitimately finish a small job in seconds.
	switch {
	case err != nil:
		l.opts.Logger.Error("runner exited with an error",
			slog.String("runner", name), slog.Duration("lived", lived), slog.Any("error", err))
	case code != 0:
		l.opts.Logger.Error("runner exited without completing a job",
			slog.String("runner", name), slog.Int("exitCode", code), slog.Duration("lived", lived),
			slog.String("hint", "the job stays queued and another runner starts on the next poll; inspect the container logs for "+name+" - a runner that dies in seconds usually means the image's runner build was rejected by GitHub"))
	default:
		l.opts.Logger.Info("runner finished", slog.String("runner", name), slog.Duration("lived", lived))
	}

	l.mu.Lock()
	delete(l.running, name)
	l.mu.Unlock()
	if l.opts.OnStop != nil {
		l.opts.OnStop(code, err)
	}
	l.refill()
}

// refill starts runners for work GitHub has already asked for but that this host
// had no free slot to serve. Two runners finishing moments apart otherwise leave
// one slot filled and one idle until the listener's next message, which halves
// throughput for the rest of a queue.
//
// It never exceeds the count GitHub last asked for, so a drained queue starts
// nothing: desired is refreshed on every message, including job completions.
func (l *Launcher) refill() {
	ctx, cancel := context.WithTimeout(context.Background(), refillTimeout)
	defer cancel()

	l.mu.Lock()
	defer l.mu.Unlock()
	if l.stopped {
		return
	}
	for n := l.allowedLocked(l.desired); n > 0; n-- {
		if err := l.launchLocked(ctx); err != nil {
			l.opts.Logger.Error("refill after a runner exit failed", slog.Any("error", err))
			return
		}
	}
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

// Shutdown terminates runners still owned by this listener and removes their
// registrations. Each runner is cleaned up concurrently, and each kill and
// deregistration receives its own bounded context.
func (l *Launcher) Shutdown(ctx context.Context) error {
	l.mu.Lock()
	l.stopped = true
	runners := make(map[string]runnerState, len(l.running))
	for name, state := range l.running {
		runners[name] = state
	}
	l.mu.Unlock()

	timeout := l.opts.cleanupTimeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}

	errCh := make(chan error, len(runners)*2)
	var removeMu sync.Mutex
	var wg sync.WaitGroup
	for name, state := range runners {
		wg.Add(1)
		go func(name string, state runnerState) {
			defer wg.Done()

			killCtx, cancelKill := context.WithTimeout(context.WithoutCancel(ctx), timeout)
			if err := state.handle.Kill(killCtx); err != nil {
				errCh <- fmt.Errorf("kill %s: %w", name, err)
			}
			cancelKill()

			if state.runnerID != 0 {
				removeMu.Lock()
				removeCtx, cancelRemove := context.WithTimeout(context.WithoutCancel(ctx), timeout)
				if err := l.jit.RemoveRunner(removeCtx, state.runnerID); err != nil {
					errCh <- fmt.Errorf("remove runner %s: %w", name, err)
				}
				cancelRemove()
				removeMu.Unlock()
			}
		}(name, state)
	}

	wg.Wait()
	close(errCh)

	var errs []error
	for err := range errCh {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// refillTimeout bounds the launches triggered by a runner exit. It is generous
// because it covers a JIT config round trip plus a container start.
const refillTimeout = 2 * time.Minute

func shortID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
