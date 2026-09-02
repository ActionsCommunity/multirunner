package scaleset

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/GerardSmit/multirunner/internal/backend"
	upstream "github.com/actions/scaleset"
)

func (l *Launcher) finishRunner(name string, state *runnerState, code int, waitErr error, terminate bool) error {
	state.cleanupMu.Lock()
	defer state.cleanupMu.Unlock()
	if state.cleaned {
		return nil
	}

	l.mu.Lock()
	busy := state.busy
	if !state.cleanupStarted {
		state.cleanupStarted = true
		state.exitCode = code
		state.waitErr = waitErr
		state.needsDeregistration = terminate || !busy
	} else if !terminate {
		state.exitCode = code
		state.waitErr = waitErr
	}
	l.mu.Unlock()

	if !state.terminated {
		if terminate {
			killCtx, cancel := context.WithTimeout(context.WithoutCancel(context.Background()), l.cleanupDuration())
			err := state.handle.Kill(killCtx)
			cancel()
			if err != nil {
				return fmt.Errorf("kill %s: %w", name, err)
			}
		}
		state.terminated = true
		l.mu.Lock()
		state.occupiesCapacity = false
		reportStop := !state.stopReported
		state.stopReported = true
		l.mu.Unlock()
		if reportStop && l.opts.OnStop != nil {
			l.opts.OnStop(state.exitCode, state.waitErr)
		}
	}

	if state.runnerID != 0 && state.needsDeregistration {
		if err := l.removeRunner(context.Background(), state.runnerID); err != nil {
			return fmt.Errorf("remove runner %s: %w", name, err)
		}
		state.runnerID = 0
	}

	store := backend.OwnedRunnerStoreFor(l.be)
	if store == nil {
		return fmt.Errorf("backend %q does not support scale-set ownership cleanup", l.be.Name())
	}
	removeCtx, cancel := context.WithTimeout(context.Background(), l.cleanupDuration())
	err := store.RemoveOwnedRunner(removeCtx, state.handle.ID())
	cancel()
	if err != nil {
		return fmt.Errorf("remove backend runner %s: %w", name, err)
	}

	state.cleaned = true
	l.mu.Lock()
	if current := l.running[name]; current == state {
		delete(l.running, name)
	}
	l.mu.Unlock()

	return state.waitErr
}

func (l *Launcher) retryPendingCleanup(ctx context.Context) error {
	l.mu.Lock()
	pending := make(map[string]*runnerState)
	for name, state := range l.running {
		if state.cleanupStarted {
			pending[name] = state
		}
	}
	l.mu.Unlock()

	var errs []error
	for name, state := range pending {
		if err := l.finishRunner(name, state, state.exitCode, state.waitErr, true); err != nil {
			errs = append(errs, err)
		}
		if ctx.Err() != nil {
			break
		}
	}
	return errors.Join(errs...)
}

func (l *Launcher) retryPendingRegistrations(ctx context.Context) error {
	l.mu.Lock()
	pending := make(map[int64]string, len(l.pendingRegistrations))
	for runnerID, name := range l.pendingRegistrations {
		pending[runnerID] = name
	}
	l.mu.Unlock()

	var errs []error
	for runnerID, name := range pending {
		if err := l.removeRunner(ctx, runnerID); err != nil {
			errs = append(errs, fmt.Errorf("remove unused runner %s: %w", name, err))
			continue
		}
		l.mu.Lock()
		delete(l.pendingRegistrations, runnerID)
		l.mu.Unlock()
	}
	return errors.Join(errs...)
}

func (l *Launcher) removeRunner(ctx context.Context, runnerID int64) error {
	if runnerID == 0 {
		return nil
	}
	l.removeMu.Lock()
	defer l.removeMu.Unlock()
	removeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), l.cleanupDuration())
	defer cancel()
	err := l.jit.RemoveRunner(removeCtx, runnerID)
	if errors.Is(err, upstream.RunnerNotFoundError) || isHTTPStatus(err, "404") {
		return nil
	}
	return err
}

func (l *Launcher) cleanupDuration() time.Duration {
	if l.opts.cleanupTimeout > 0 {
		return l.opts.cleanupTimeout
	}
	return 10 * time.Second
}
