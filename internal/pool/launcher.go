package pool

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
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

// Hooks observe a runner's lifecycle (used for metrics). Either may be nil.
type Hooks struct {
	OnStart func(pool string)
	OnStop  func(pool string, exitCode int, err error)
}

// Launcher launches one ephemeral runner for a pool. It is the shared unit used
// by both the always-on pool model and the webhook autoscaler.
type Launcher struct {
	cfg    config.Pool
	image  string
	be     backend.Backend
	gh     github.ClientProvider
	env    map[string]string
	mounts []backend.Mount
	logger *slog.Logger
	hooks  Hooks
}

// NewLauncher builds a Launcher.
func NewLauncher(cfg config.Pool, image string, be backend.Backend, gh github.ClientProvider, env map[string]string, mounts []backend.Mount, logger *slog.Logger, hooks Hooks) *Launcher {
	return &Launcher{
		cfg: cfg, image: image, be: be, gh: gh,
		env: env, mounts: mounts, logger: logger.With("pool", cfg.Name), hooks: hooks,
	}
}

// Name is the pool name.
func (l *Launcher) Name() string { return l.cfg.Name }

// Max is the pool's max concurrent runners.
func (l *Launcher) Max() int { return l.cfg.Size }

// Labels are the runner labels for this pool.
func (l *Launcher) Labels() []string { return l.cfg.Labels }

// EnsureImage makes sure the runner image is present.
func (l *Launcher) EnsureImage(ctx context.Context) error {
	l.logger.Info("ensuring runner image", "image", l.image)
	if err := l.be.EnsureImage(ctx, l.image); err != nil {
		return fmt.Errorf("pool %s: %w", l.cfg.Name, err)
	}
	return nil
}

// RunOne provisions a fresh JIT runner for the next repo in rotation and blocks
// until it finishes its one job. This is the warm-capacity path: there is no
// queued job to place against, so rotation is the best available choice. When a
// specific repo has queued work, call RunOneOn instead.
func (l *Launcher) RunOne(ctx context.Context) (int, error) {
	return l.RunOneOn(ctx, l.gh.NextClient())
}

// RunOneOn provisions a fresh JIT runner registered to client's repo and blocks
// until it finishes its one job. A repo-scoped runner binds to exactly one repo,
// so demand-driven launches must pass the client for the repo that queued the
// job; otherwise the runner idles on a repo that has no work while the job that
// triggered the launch stays queued. A nil client falls back to rotation.
func (l *Launcher) RunOneOn(ctx context.Context, client *github.Client) (int, error) {
	// Resolve before the OnStart hook so an unusable pool cannot leak an
	// unmatched start into the metrics.
	if client == nil {
		client = l.gh.NextClient()
	}
	if client == nil {
		return 0, fmt.Errorf("pool %s: no github client available to register a runner", l.cfg.Name)
	}
	if l.hooks.OnStart != nil {
		l.hooks.OnStart(l.cfg.Name)
	}
	spec := runner.Spec{
		Name:          l.runnerName(),
		Image:         l.image,
		RunnerGroupID: l.cfg.RunnerGroupID,
		Labels:        l.cfg.Labels,
		WorkFolder:    l.cfg.WorkFolder,
		Env:           l.env,
		Mounts:        l.mounts,
	}
	code, err := runner.RunOnce(ctx, client, l.be, spec, l.logger.With("target", client.Target()))
	if l.hooks.OnStop != nil {
		l.hooks.OnStop(l.cfg.Name, code, err)
	}
	return code, err
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
