// Package pool maintains N ephemeral runner slots per OS pool, re-provisioning
// each slot when its runner finishes its single job (the process-exit model).
package pool

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"github.com/GerardSmit/multirunner/internal/backend"
	"github.com/GerardSmit/multirunner/internal/config"
	"github.com/GerardSmit/multirunner/internal/github"
)

// Pool keeps a pool's slots filled with ephemeral runners.
type Pool struct {
	l       *Launcher
	size    int
	maxFail int
	logger  *slog.Logger
}

// New builds a Pool.
func New(cfg config.Pool, image string, be backend.Backend, gh github.ClientProvider, env map[string]string, mounts []backend.Mount, logger *slog.Logger) *Pool {
	return NewWithHooks(cfg, image, be, gh, env, mounts, logger, Hooks{})
}

// NewWithHooks builds a Pool with lifecycle hooks (metrics).
func NewWithHooks(cfg config.Pool, image string, be backend.Backend, gh github.ClientProvider, env map[string]string, mounts []backend.Mount, logger *slog.Logger, hooks Hooks) *Pool {
	return NewPool(NewLauncher(cfg, image, be, gh, env, mounts, logger, hooks), logger)
}

// NewPool wraps an existing Launcher (shared with the autoscaler).
func NewPool(l *Launcher, logger *slog.Logger) *Pool {
	return &Pool{
		l:       l,
		size:    l.cfg.Size,
		maxFail: l.cfg.MaxConsecutiveFailures,
		logger:  logger.With("pool", l.Name()),
	}
}

// Run ensures the image is present, then keeps all slots filled until ctx ends.
func (p *Pool) Run(ctx context.Context) error {
	if err := p.l.EnsureImage(ctx); err != nil {
		return err
	}
	var wg sync.WaitGroup
	for i := 0; i < p.size; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			p.runSlot(ctx, index)
		}(i)
	}
	wg.Wait()
	return nil
}

// runSlot keeps one ephemeral runner alive in a loop, re-provisioning on exit.
func (p *Pool) runSlot(ctx context.Context, index int) {
	failures := 0
	for ctx.Err() == nil {
		_, err := p.l.RunOne(ctx)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			failures++
			delay := backoff(failures)
			p.logger.Error("slot failed; backing off",
				"index", index, "consecutive_failures", failures, "delay", delay.String(), "err", err)
			if failures >= p.maxFail {
				p.logger.Error("slot hit max consecutive failures; still retrying",
					"index", index, "max", p.maxFail)
			}
			if !sleep(ctx, delay) {
				return
			}
			continue
		}
		failures = 0
		// Clean exit: immediately loop to provision a fresh ephemeral runner.
	}
}

// Manager runs multiple pools concurrently.
type Manager struct {
	pools  []*Pool
	logger *slog.Logger
}

// NewManager builds a Manager.
func NewManager(pools []*Pool, logger *slog.Logger) *Manager {
	return &Manager{pools: pools, logger: logger}
}

// Run starts all pools and blocks until ctx is cancelled.
func (m *Manager) Run(ctx context.Context) error {
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	var wg sync.WaitGroup
	errCh := make(chan error, len(m.pools))
	for _, p := range m.pools {
		wg.Add(1)
		go func(pl *Pool) {
			defer wg.Done()
			if err := pl.Run(runCtx); err != nil && runCtx.Err() == nil {
				m.logger.Error("pool stopped", "pool", pl.l.Name(), "err", err)
				select {
				case errCh <- fmt.Errorf("pool %s: %w", pl.l.Name(), err):
					cancel()
				default:
				}
			}
		}(p)
	}
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case err := <-errCh:
		<-done
		return err
	case <-ctx.Done():
		cancel()
		<-done
		return nil
	case <-done:
		select {
		case err := <-errCh:
			return err
		default:
		}
		return nil
	}
}
