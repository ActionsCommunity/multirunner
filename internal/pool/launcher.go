package pool

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/GerardSmit/multirunner/internal/backend"
	"github.com/GerardSmit/multirunner/internal/config"
	"github.com/GerardSmit/multirunner/internal/github"
	"github.com/GerardSmit/multirunner/internal/runner"
)

const (
	backoffBase = 2 * time.Second
	backoffMax  = 60 * time.Second
)

// Hooks observes a runner's lifecycle. OnStart and OnStop are retained for
// compatibility; Observer receives the structured event stream.
type Hooks struct {
	OnStart  func(pool string)
	OnStop   func(pool string, exitCode int, err error)
	Observer runner.LifecycleObserver
}

// QueueIDs identifies the queued GitHub work that caused a demand-driven
// launch. Zero values mean the corresponding ID is not known.
type QueueIDs struct {
	RunID int64
	JobID int64
}

// Launcher launches one ephemeral runner for a pool. It is the shared unit used
// by both the always-on pool model and the webhook autoscaler.
type Launcher struct {
	cfg          config.Pool
	image        string
	be           backend.Backend
	gh           github.ClientProvider
	env          map[string]string
	mounts       []backend.Mount
	container    backend.ContainerSettings
	logger       *slog.Logger
	hooks        Hooks
	controlMu    sync.Mutex
	paused       bool
	provisioning int
	active       map[string]*activeRunner
	changed      chan struct{}
}

type activeRunner struct {
	cancel context.CancelFunc
	done   chan struct{}
}

var ErrRunnerSessionNotFound = errors.New("runner session is not active")

// NewLauncher builds a Launcher.
func NewLauncher(cfg config.Pool, image string, be backend.Backend, gh github.ClientProvider, env map[string]string, mounts []backend.Mount, logger *slog.Logger, hooks Hooks) *Launcher {
	return &Launcher{
		cfg: cfg, image: image, be: be, gh: gh,
		env: env, mounts: mounts,
		container: backend.ContainerSettings{
			CPUCount:        int64(cfg.Container.CPUs),
			MemoryBytes:     cfg.Container.MemoryBytes(),
			MemorySwapBytes: cfg.Container.MemorySwapBytes(),
			DNS:             append([]string(nil), cfg.Container.DNS...),
		},
		logger: logger.With("pool", cfg.Name), hooks: hooks,
		active: make(map[string]*activeRunner), changed: make(chan struct{}),
	}
}

// Name is the pool name.
func (l *Launcher) Name() string { return l.cfg.Name }

// Max is the pool's max concurrent runners.
func (l *Launcher) Max() int { return l.cfg.Size }

// Labels are the runner labels for this pool.
func (l *Launcher) Labels() []string { return l.cfg.Labels }

// Allows reports whether this pool may register a runner for job.
func (l *Launcher) Allows(job github.QueuedJob) bool {
	target := job.Repository
	if target == "" && job.Client != nil {
		target = job.Client.Target()
	}
	return job.Client != nil && l.cfg.CanServeJob(
		target, job.WorkflowPath, job.Event, job.Actor, job.Ref,
	)
}

// RequiresWorkflowMetadata reports whether this pool authorizes jobs using
// fields that are available only from the workflow-run record.
func (l *Launcher) RequiresWorkflowMetadata() bool { return len(l.cfg.Workflows) != 0 }

// EnsureImage makes sure the runner image is present.
func (l *Launcher) EnsureImage(ctx context.Context) error {
	l.logger.Info("ensuring runner image", "image", l.image)
	if err := l.be.EnsureImage(ctx, l.image); err != nil {
		return fmt.Errorf("pool %s: %w", l.cfg.Name, err)
	}
	return nil
}

// RunOne provisions a fresh JIT runner for warm-pool slot zero and blocks
// until it finishes its one job. This is the warm-capacity path: there is no
// queued job to place against. When a specific repo has queued work, call
// RunOneOn instead.
func (l *Launcher) RunOne(ctx context.Context) (int, error) {
	return l.RunOneForSlot(ctx, 0)
}

// RunOneForSlot provisions a fresh runner for a stable warm-pool slot.
func (l *Launcher) RunOneForSlot(ctx context.Context, slot int) (int, error) {
	return l.RunOneOn(ctx, l.gh.ClientForSlot(slot))
}

// RunOneOn provisions a fresh JIT runner registered to client's repo and blocks
// until it finishes its one job. A repo-scoped runner binds to exactly one repo,
// so demand-driven launches must pass the client for the repo that queued the
// job; otherwise the runner idles on a repo that has no work while the job that
// triggered the launch stays queued. A nil client is rejected.
func (l *Launcher) RunOneOn(ctx context.Context, client *github.Client) (int, error) {
	return l.RunJob(ctx, github.QueuedJob{Client: client})
}

// RunJob provisions a fresh JIT runner for an authorized queued job.
func (l *Launcher) RunJob(ctx context.Context, job github.QueuedJob) (int, error) {
	return l.RunJobWithQueue(ctx, job, QueueIDs{RunID: job.RunID, JobID: job.JobID})
}

// RunJobWithQueue provisions a fresh JIT runner and attaches any known queued
// run/job IDs to its lifecycle events.
func (l *Launcher) RunJobWithQueue(ctx context.Context, job github.QueuedJob, queue QueueIDs) (int, error) {
	client := job.Client
	// Resolve before the OnStart hook so an unusable pool cannot leak an
	// unmatched start into the metrics.
	if client == nil {
		return 0, fmt.Errorf("pool %s: no github client available to register a runner", l.cfg.Name)
	}
	if !l.Allows(job) {
		target := job.Repository
		if target == "" {
			target = client.Target()
		}
		return 0, fmt.Errorf(
			"pool %s: job from repository %s workflow %q event %q actor %q is not allowed",
			l.cfg.Name, target, job.WorkflowPath, job.Event, job.Actor,
		)
	}
	if err := l.reserveProvisioning(ctx); err != nil {
		return 0, err
	}
	reserved := true
	defer func() {
		if !reserved {
			return
		}
		l.controlMu.Lock()
		l.provisioning--
		l.notifyControlChangeLocked()
		l.controlMu.Unlock()
	}()
	if l.hooks.Observer == nil && l.hooks.OnStart != nil {
		l.hooks.OnStart(l.cfg.Name)
	}
	sessionID := runner.NewLocalSessionID()
	runContext, cancel := context.WithCancel(ctx)
	active := &activeRunner{cancel: cancel, done: make(chan struct{})}
	l.controlMu.Lock()
	l.active[sessionID] = active
	l.provisioning--
	reserved = false
	l.notifyControlChangeLocked()
	l.controlMu.Unlock()
	defer func() {
		cancel()
		l.controlMu.Lock()
		delete(l.active, sessionID)
		close(active.done)
		l.notifyControlChangeLocked()
		l.controlMu.Unlock()
	}()
	runnerName := l.runnerName()
	spec := runner.Spec{
		Name:             runnerName,
		LocalSessionID:   sessionID,
		Pool:             l.cfg.Name,
		Target:           client.Target(),
		Repository:       job.Repository,
		QueuedRunID:      queue.RunID,
		QueuedRunAttempt: job.RunAttempt,
		QueuedJobID:      queue.JobID,
		Observer:         l.hooks.Observer,
		Image:            l.image,
		RunnerGroupID:    l.cfg.RunnerGroupID,
		Labels:           l.cfg.Labels,
		WorkFolder:       l.cfg.WorkFolder,
		Env:              l.env,
		Mounts:           l.mounts,
		Container:        l.container,
	}
	code, err := runner.RunOnce(runContext, client, l.be, spec, l.logger.With("target", client.Target()))
	if l.hooks.Observer == nil && l.hooks.OnStop != nil {
		l.hooks.OnStop(l.cfg.Name, code, err)
	}
	return code, err
}

// Pause prevents new runners from being provisioned while allowing active
// sessions to finish.
func (l *Launcher) Pause() {
	l.controlMu.Lock()
	defer l.controlMu.Unlock()
	if !l.paused {
		l.paused = true
		l.notifyControlChangeLocked()
	}
}

// Resume allows new runners to be provisioned.
func (l *Launcher) Resume() {
	l.controlMu.Lock()
	defer l.controlMu.Unlock()
	if l.paused {
		l.paused = false
		l.notifyControlChangeLocked()
	}
}

// Paused reports whether new provisioning is paused.
func (l *Launcher) Paused() bool {
	l.controlMu.Lock()
	defer l.controlMu.Unlock()
	return l.paused
}

// ActiveSessions returns the stable local IDs of active runner sessions.
func (l *Launcher) ActiveSessions() []string {
	l.controlMu.Lock()
	defer l.controlMu.Unlock()
	sessions := make([]string, 0, len(l.active))
	for id := range l.active {
		sessions = append(sessions, id)
	}
	sort.Strings(sessions)
	return sessions
}

// Drain pauses provisioning and waits for active runner sessions to finish.
func (l *Launcher) Drain(ctx context.Context) error {
	l.controlMu.Lock()
	if !l.paused {
		l.paused = true
		l.notifyControlChangeLocked()
	}
	l.controlMu.Unlock()
	for {
		l.controlMu.Lock()
		if l.provisioning == 0 && len(l.active) == 0 {
			l.controlMu.Unlock()
			return nil
		}
		changed := l.changed
		l.controlMu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-changed:
		}
	}
}

// Terminate cancels one active runner through its existing cleanup path and
// waits until that session is no longer owned by the launcher.
func (l *Launcher) Terminate(ctx context.Context, sessionID string) error {
	l.controlMu.Lock()
	active, ok := l.active[sessionID]
	if ok {
		active.cancel()
	}
	l.controlMu.Unlock()
	if !ok {
		return ErrRunnerSessionNotFound
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-active.done:
		return nil
	}
}

func (l *Launcher) reserveProvisioning(ctx context.Context) error {
	for {
		l.controlMu.Lock()
		if !l.paused {
			l.provisioning++
			l.notifyControlChangeLocked()
			l.controlMu.Unlock()
			return nil
		}
		changed := l.changed
		l.controlMu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-changed:
		}
	}
}

func (l *Launcher) waitUntilProvisioningAllowed(ctx context.Context) error {
	if err := l.reserveProvisioning(ctx); err != nil {
		return err
	}
	l.controlMu.Lock()
	l.provisioning--
	l.notifyControlChangeLocked()
	l.controlMu.Unlock()
	return nil
}

func (l *Launcher) notifyControlChangeLocked() {
	close(l.changed)
	l.changed = make(chan struct{})
}

func (l *Launcher) runnerName() string {
	return fmt.Sprintf("%s-%s-%s", l.cfg.NamePrefix, l.cfg.OS, shortID())
}

// backoff returns an exponential delay capped at backoffMax.
func backoff(failures int) time.Duration {
	d := backoffBase
	for i := 1; i < failures; i++ {
		d *= 2
		if d >= backoffMax {
			return backoffMax
		}
	}
	if d > backoffMax {
		return backoffMax
	}
	return d
}

// sleep waits for d or until ctx is cancelled; returns false if cancelled.
func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

func shortID() string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
