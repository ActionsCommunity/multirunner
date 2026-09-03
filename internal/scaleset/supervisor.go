package scaleset

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/actions/scaleset"
)

const (
	supervisorInitialBackoff = 2 * time.Second
	supervisorMaxBackoff     = time.Minute
	supervisorHealthyAfter   = time.Minute
)

// SupervisedSession describes one independently restarted scale-set session.
type SupervisedSession struct {
	Name          string
	Run           func(context.Context, func()) error
	OnStateChange func(SessionAvailability)
}

// SessionAvailability is the externally observable state of a required session.
type SessionAvailability uint8

const (
	SessionBackingOff SessionAvailability = iota
	SessionAvailable
	SessionPermanentlyUnavailable
)

type supervisorConfig struct {
	initialBackoff time.Duration
	maxBackoff     time.Duration
	healthyAfter   time.Duration
	now            func() time.Time
	sleep          func(context.Context, time.Duration) bool
}

func defaultSupervisorConfig() supervisorConfig {
	return supervisorConfig{
		initialBackoff: supervisorInitialBackoff,
		maxBackoff:     supervisorMaxBackoff,
		healthyAfter:   supervisorHealthyAfter,
		now:            time.Now,
		sleep:          sleepWithContext,
	}
}

// RunSupervised runs every session independently. Recoverable failures restart
// only their session. Permanent failures stop that session without canceling
// healthy siblings and surface when all sessions have stopped.
func RunSupervised(ctx context.Context, sessions []SupervisedSession, logger *slog.Logger) error {
	return runSupervised(ctx, sessions, defaultSupervisorConfig(), logger)
}

func runSupervised(ctx context.Context, sessions []SupervisedSession, cfg supervisorConfig, logger *slog.Logger) error {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	for _, session := range sessions {
		if session.Name == "" {
			return errors.New("scale set session name is required")
		}
		if session.Run == nil {
			return fmt.Errorf("scale set %q session function is required", session.Name)
		}
	}
	errs := make([]error, len(sessions))
	var wg sync.WaitGroup
	for i, session := range sessions {
		i, session := i, session
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = superviseSession(ctx, session, cfg, logger.With("scaleSet", session.Name))
			if errs[i] != nil {
				logger.Error("scale set session stopped",
					slog.String("scaleSet", session.Name),
					slog.Any("error", errs[i]))
			}
		}()
	}
	wg.Wait()
	return errors.Join(errs...)
}

func superviseSession(ctx context.Context, session SupervisedSession, cfg supervisorConfig, logger *slog.Logger) error {
	failures := 0
	reportSessionState(session, SessionBackingOff)
	for ctx.Err() == nil {
		started := cfg.now()
		var availableOnce sync.Once
		err := session.Run(ctx, func() {
			availableOnce.Do(func() {
				reportSessionState(session, SessionAvailable)
			})
		})
		if isPermanentSessionError(err) {
			reportSessionState(session, SessionPermanentlyUnavailable)
			return fmt.Errorf("scale set %q failed permanently: %w", session.Name, err)
		}
		if ctx.Err() != nil {
			return nil
		}
		if err == nil {
			err = errors.New("session stopped unexpectedly")
		}
		if cfg.now().Sub(started) >= cfg.healthyAfter {
			failures = 0
		}
		failures++
		delay := supervisorBackoff(failures, cfg.initialBackoff, cfg.maxBackoff)
		reportSessionState(session, SessionBackingOff)
		logger.Error("scale set session failed; retrying",
			slog.Int("consecutiveFailures", failures),
			slog.Duration("retryAfter", delay),
			slog.Any("error", err))
		if !cfg.sleep(ctx, delay) {
			return nil
		}
	}
	return nil
}

func reportSessionState(session SupervisedSession, state SessionAvailability) {
	if session.OnStateChange != nil {
		session.OnStateChange(state)
	}
}

func supervisorBackoff(failures int, initial, maximum time.Duration) time.Duration {
	delay := initial
	for n := 1; n < failures && delay < maximum; n++ {
		if delay > maximum/2 {
			return maximum
		}
		delay *= 2
	}
	if delay > maximum {
		return maximum
	}
	return delay
}

func sleepWithContext(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func isPermanentSessionError(err error) bool {
	if err == nil {
		return false
	}
	var permanent permanentSessionError
	if errors.As(err, &permanent) {
		return true
	}
	if errors.Is(err, scaleset.ErrInvalidGitHubConfigURL) {
		return true
	}
	message := err.Error()
	for _, status := range []string{"400", "401", "403", "422"} {
		if isHTTPStatusMessage(message, status) {
			return true
		}
	}
	return false
}

func isHTTPStatus(err error, status string) bool {
	return err != nil && isHTTPStatusMessage(err.Error(), status)
}

func isHTTPStatusMessage(message, status string) bool {
	return strings.Contains(message, `status="`+status+` `) ||
		strings.Contains(message, "status code: "+status) ||
		strings.Contains(message, "status code "+status)
}
