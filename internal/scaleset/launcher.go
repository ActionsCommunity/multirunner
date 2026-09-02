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
	// Ownership is attached to backend resources for restart reconciliation.
	Ownership backend.RunnerOwnership
	// OnStart and OnStop report runner lifecycle events.
	OnStart func()
	OnStop  func(exitCode int, err error)
	// Logger reports recoverable launch and cleanup failures.
	Logger *slog.Logger
	// cleanupTimeout bounds each kill and deregistration operation.
	cleanupTimeout time.Duration
	// launchTimeout bounds JIT generation and backend launch together.
	launchTimeout time.Duration
	// reconcileInterval controls periodic reconciliation.
	reconcileInterval time.Duration
}

// Launcher implements the scale set listener's handler interface by translating
// a desired runner count into backend launches.
//
// A Launcher is safe for concurrent use.
type Launcher struct {
	ctx  context.Context
	jit  jitGenerator
	be   backend.Backend
	opts Options

	mu                   sync.Mutex
	removeMu             sync.Mutex
	running              map[string]*runnerState
	pendingRegistrations map[int64]string
	seq                  int
}

type runnerState struct {
	handle              backend.RunnerHandle
	runnerID            int64
	busy                bool
	occupiesCapacity    bool
	cleanupStarted      bool
	needsDeregistration bool
	registrationRemoved bool
	cleanupMu           sync.Mutex
	terminated          bool
	cleaned             bool
	stopReported        bool
	exitCode            int
	waitErr             error
}

// New returns a Launcher that provisions onto be.
func New(ctx context.Context, jit jitGenerator, be backend.Backend, opts Options) *Launcher {
	if ctx == nil {
		ctx = context.Background()
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	if opts.Ownership.ScaleSetID == 0 {
		opts.Ownership.ScaleSetID = opts.ScaleSetID
	}
	return &Launcher{
		ctx:                  ctx,
		jit:                  jit,
		be:                   be,
		opts:                 opts,
		running:              make(map[string]*runnerState),
		pendingRegistrations: make(map[int64]string),
	}
}

// allowedLocked reports how many runners may be started to reach want, given
// how many are already running and the configured cap. Callers hold l.mu.
func (l *Launcher) allowedLocked(want int) int {
	have := l.runningLocked()
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
// Runners are ephemeral: each exits after one job, then convergent cleanup
// removes its registration and backend record. A runner still up may be mid-job.
func (l *Launcher) HandleDesiredRunnerCount(ctx context.Context, count int) (int, error) {
	if err := l.retryPendingRegistrations(ctx); err != nil {
		if isPermanentSessionError(err) {
			return l.Running(), err
		}
		l.opts.Logger.Error("unused registration cleanup retry failed; listener remains active",
			slog.Any("error", err))
	}
	if err := l.retryPendingCleanup(ctx); err != nil {
		l.mu.Lock()
		running := l.runningLocked()
		l.mu.Unlock()
		if isPermanentSessionError(err) {
			return running, err
		}
		l.opts.Logger.Error("runner cleanup retry failed; listener remains active",
			slog.Int("running", running),
			slog.Any("error", err))
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	for n := l.allowedLocked(count); n > 0; n-- {
		if err := l.launchLocked(); err != nil {
			if isPermanentSessionError(err) {
				return l.runningLocked(), err
			}
			// actions/scaleset acknowledges the message before this callback.
			// Keep the listener alive so its next statistics update can retry.
			l.opts.Logger.Error("runner launch failed; listener remains active",
				slog.Int("running", l.runningLocked()),
				slog.Any("error", err))
			return l.runningLocked(), nil
		}
	}
	return l.runningLocked(), nil
}

// launchLocked generates a JIT config and starts one runner. Callers hold l.mu.
func (l *Launcher) launchLocked() (err error) {
	if l.opts.OnStart != nil {
		l.opts.OnStart()
	}
	defer func() {
		if err != nil && l.opts.OnStop != nil {
			l.opts.OnStop(-1, err)
		}
	}()

	l.seq++
	id, err := shortID()
	if err != nil {
		return fmt.Errorf("scaleset: generate runner name: %w", err)
	}
	name := fmt.Sprintf("mr-scaleset-%d-%s", l.opts.ScaleSetID, id)

	launchCtx, cancel := context.WithTimeout(l.ctx, l.launchDuration())
	defer cancel()
	jit, err := l.jit.GenerateJitRunnerConfig(
		launchCtx,
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
	if jit.Runner == nil || jit.Runner.ID == 0 {
		return fmt.Errorf("scaleset: generate JIT config for %s returned no runner identity", name)
	}
	runnerID := int64(jit.Runner.ID)
	ownership := l.opts.Ownership
	ownership.RunnerID = runnerID

	// This is the whole integration point. The scale set listener hands back
	// the same base64 blob generate-jitconfig returns, which is exactly what
	// LaunchRequest already carries, so no backend needs to change.
	handle, err := l.be.Launch(launchCtx, backend.LaunchRequest{
		Name:             name,
		Image:            l.opts.Image,
		EncodedJITConfig: jit.EncodedJITConfig,
		WorkFolder:       l.opts.WorkFolder,
		Labels:           l.opts.Labels,
		Env:              l.opts.Env,
		Mounts:           l.opts.Mounts,
		Index:            l.seq,
		Ownership:        ownership,
	})
	if err != nil {
		launchErr := fmt.Errorf("scaleset: launch %s: %w", name, err)
		if handle != nil {
			// A non-nil handle means the backend cannot prove the resource is
			// absent. Count it until detached cleanup confirms termination, then
			// converge registration and backend deletion in the normal order.
			state := &runnerState{
				handle:           handle,
				runnerID:         runnerID,
				occupiesCapacity: true,
				stopReported:     true,
			}
			l.running[name] = state
			go func() {
				if cleanupErr := l.finishRunner(name, state, -1, nil, true); cleanupErr != nil {
					l.opts.Logger.Error("partial runner launch cleanup failed",
						slog.String("runner", name),
						slog.Any("error", cleanupErr))
				}
			}()
			return launchErr
		}
		if removeErr := l.removeRunner(context.WithoutCancel(l.ctx), int64(jit.Runner.ID)); removeErr != nil {
			l.pendingRegistrations[int64(jit.Runner.ID)] = name
			return errors.Join(launchErr, fmt.Errorf("scaleset: remove unused runner %s: %w", name, removeErr))
		}
		return launchErr
	}

	state := &runnerState{handle: handle, runnerID: runnerID, occupiesCapacity: true}
	l.running[name] = state
	go l.awaitExit(name, state)
	return nil
}

func (l *Launcher) launchDuration() time.Duration {
	if l.opts.launchTimeout > 0 {
		return l.opts.launchTimeout
	}
	return 2 * time.Minute
}

// awaitExit frees the slot once the runner finishes. Each runner is ephemeral,
// so exactly one exit is expected per launch.
func (l *Launcher) awaitExit(name string, state *runnerState) {
	code, err := state.handle.Wait(context.Background())
	l.finishRunner(name, state, code, err, err != nil)
}

// HandleJobStarted records that an assignment became a running job. The runner
// was already started in response to the desired count, so there is nothing to
// provision here.
func (l *Launcher) HandleJobStarted(_ context.Context, job *scaleset.JobStarted) error {
	if job == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	for name, state := range l.running {
		if (job.RunnerID != 0 && state.runnerID == int64(job.RunnerID)) ||
			(job.RunnerName != "" && name == job.RunnerName) {
			state.busy = true
			break
		}
	}
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
	return l.runningLocked()
}

func (l *Launcher) runningLocked() int {
	running := 0
	for _, state := range l.running {
		if state.occupiesCapacity {
			running++
		}
	}
	return running
}

// Shutdown terminates runners still owned by this listener and removes their
// registrations. Each runner is cleaned up concurrently, and each kill and
// deregistration receives its own bounded context.
func (l *Launcher) Shutdown(ctx context.Context) error {
	l.mu.Lock()
	runners := make(map[string]*runnerState, len(l.running))
	for name, state := range l.running {
		runners[name] = state
	}
	l.mu.Unlock()

	errCh := make(chan error, len(runners))
	var wg sync.WaitGroup
	for name, state := range runners {
		wg.Add(1)
		go func(name string, state *runnerState) {
			defer wg.Done()
			if err := l.finishRunner(name, state, -1, nil, true); err != nil {
				errCh <- err
			}
		}(name, state)
	}

	wg.Wait()
	close(errCh)

	var errs []error
	for err := range errCh {
		errs = append(errs, err)
	}
	if err := l.retryPendingRegistrations(ctx); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// Reconcile removes registrations for backend resources carrying the complete
// ownership boundary, then removes the resources. This order leaves a
// discoverable retry record when GitHub cleanup fails.
func (l *Launcher) Reconcile(ctx context.Context) (int, error) {
	reconcileCtx, cancel := context.WithTimeout(ctx, l.cleanupDuration())
	defer cancel()
	return l.reconcile(reconcileCtx)
}

func (l *Launcher) reconcile(ctx context.Context) (int, error) {
	store := backend.OwnedRunnerStoreFor(l.be)
	if store == nil {
		return 0, fmt.Errorf("backend %q does not support scale-set ownership reconciliation", l.be.Name())
	}

	owned, err := store.ListOwnedRunners(ctx, l.opts.Ownership)
	if err != nil {
		return 0, fmt.Errorf("list owned runners: %w", err)
	}

	l.mu.Lock()
	activeResources := make(map[string]struct{}, len(l.running))
	for _, state := range l.running {
		activeResources[state.handle.ID()] = struct{}{}
	}
	l.mu.Unlock()

	reconciled := 0
	var errs []error
	for _, runner := range owned {
		if _, active := activeResources[runner.ResourceID]; active {
			continue
		}
		if err := l.removeRunner(ctx, runner.RunnerID); err != nil {
			errs = append(errs, fmt.Errorf("remove registration for %s: %w", runner.Name, err))
			continue
		}
		removeErr := store.RemoveOwnedRunner(ctx, runner.ResourceID)
		if removeErr != nil {
			errs = append(errs, fmt.Errorf("remove backend runner %s: %w", runner.Name, removeErr))
			continue
		}
		reconciled++
	}
	return reconciled, errors.Join(errs...)
}

func shortID() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}
