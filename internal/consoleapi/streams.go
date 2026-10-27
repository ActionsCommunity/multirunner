package consoleapi

import (
	"sync"
	"time"
)

const (
	DefaultHostStreamLimit       = 16
	DefaultSessionStreamLimit    = 2
	DefaultStreamMaximumLifetime = 30 * time.Minute
)

type StreamMetrics interface {
	ObserveConsoleStreamOpened()
	ObserveConsoleStreamClosed()
	ObserveConsoleStreamRejected(reason string)
}

type streamAdmission struct {
	mu              sync.Mutex
	hostLimit       int
	sessionLimit    int
	active          int
	activeBySession map[string]int
	metrics         StreamMetrics
}

func newStreamAdmission(hostLimit, sessionLimit int, metrics StreamMetrics) *streamAdmission {
	if hostLimit <= 0 {
		hostLimit = DefaultHostStreamLimit
	}
	if sessionLimit <= 0 {
		sessionLimit = DefaultSessionStreamLimit
	}
	return &streamAdmission{
		hostLimit: hostLimit, sessionLimit: sessionLimit,
		activeBySession: make(map[string]int), metrics: metrics,
	}
}

func (a *streamAdmission) acquire(sessionID string) (func(), string, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	reason := ""
	switch {
	case a.active >= a.hostLimit:
		reason = "host_limit"
	case a.activeBySession[sessionID] >= a.sessionLimit:
		reason = "session_limit"
	}
	if reason != "" {
		if a.metrics != nil {
			a.metrics.ObserveConsoleStreamRejected(reason)
		}
		return nil, reason, false
	}
	a.active++
	a.activeBySession[sessionID]++
	if a.metrics != nil {
		a.metrics.ObserveConsoleStreamOpened()
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			a.mu.Lock()
			a.active--
			a.activeBySession[sessionID]--
			if a.activeBySession[sessionID] == 0 {
				delete(a.activeBySession, sessionID)
			}
			a.mu.Unlock()
			if a.metrics != nil {
				a.metrics.ObserveConsoleStreamClosed()
			}
		})
	}, "", true
}
