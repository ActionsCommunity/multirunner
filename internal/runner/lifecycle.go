package runner

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"regexp"
	"strings"
	"time"
)

// LifecycleEventType identifies a runner provisioning milestone.
type LifecycleEventType string

const (
	LifecyclePlanned    LifecycleEventType = "planned"
	LifecycleRegistered LifecycleEventType = "registered"
	LifecycleLaunched   LifecycleEventType = "launched"
	LifecycleStopped    LifecycleEventType = "stopped"
	LifecycleFailed     LifecycleEventType = "failed"
)

// LifecycleEvent is a point-in-time snapshot of one local runner session.
// ExitCode is -1 until a backend wait returned a process exit code.
type LifecycleEvent struct {
	Type                 LifecycleEventType
	LocalSessionID       string
	Pool                 string
	Target               string
	Repository           string
	RunnerName           string
	QueuedRunID          int64
	QueuedRunAttempt     int
	QueuedJobID          int64
	GitHubRegistrationID int64
	BackendInstanceID    string
	Timestamp            time.Time
	ExitCode             int
	Error                string // Sanitized for durable observation.
	Conclusion           string // Optional intentional terminal outcome.
}

var (
	ErrTerminated = errors.New("runner intentionally terminated")
	ErrShutdown   = errors.New("runner stopped for host shutdown")
)

func intentionalStop(ctx context.Context) string {
	switch {
	case errors.Is(context.Cause(ctx), ErrTerminated):
		return "terminated"
	case errors.Is(context.Cause(ctx), ErrShutdown):
		return "shutdown"
	default:
		return ""
	}
}

// cancellationOnly rejects joined cleanup failures even when one cause is cancellation.
func cancellationOnly(err error) bool {
	for err != nil {
		if err == context.Canceled || err == context.DeadlineExceeded {
			return true
		}
		err = errors.Unwrap(err)
	}
	return false
}

// LifecycleObserver receives ordered lifecycle events. Implementations must be
// safe for concurrent calls from different runner sessions.
type LifecycleObserver interface {
	ObserveRunnerLifecycle(context.Context, LifecycleEvent)
}

// LifecycleObserverFunc adapts a function to LifecycleObserver.
type LifecycleObserverFunc func(context.Context, LifecycleEvent)

// ObserveRunnerLifecycle implements LifecycleObserver.
func (f LifecycleObserverFunc) ObserveRunnerLifecycle(
	ctx context.Context, event LifecycleEvent,
) {
	f(ctx, event)
}

// NewLocalSessionID returns an opaque identifier for one local provisioning
// attempt. It is generated before GitHub registration or backend launch.
func NewLocalSessionID() string {
	var id [16]byte
	_, _ = rand.Read(id[:])
	id[6] = (id[6] & 0x0f) | 0x40
	id[8] = (id[8] & 0x3f) | 0x80
	return hex.EncodeToString(id[:])
}

var (
	urlSecretPattern = regexp.MustCompile(`(?i)(https?://[^?\s]+)\?[^\s]+`)
	secretPattern    = regexp.MustCompile(`(?i)\b(token|authorization|password|secret|pat)\s*[:=]\s*[^\s,;]+`)
)

func sanitizeLifecycleError(err error) string {
	if err == nil {
		return ""
	}

	message := strings.Join(strings.Fields(err.Error()), " ")
	message = urlSecretPattern.ReplaceAllString(message, "$1?[redacted]")
	message = secretPattern.ReplaceAllString(message, "$1=[redacted]")
	const maxLength = 512
	if len(message) > maxLength {
		message = message[:maxLength] + "..."
	}
	return message
}

// SanitizeLifecycleError bounds and redacts errors before durable observation.
func SanitizeLifecycleError(err error) string { return sanitizeLifecycleError(err) }
