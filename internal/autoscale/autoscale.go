// Package autoscale scales ephemeral runners on demand: it launches runners when
// jobs are queued (via webhook events and/or API polling), up to each pool's max.
// Polling is outbound-only, so it works behind NAT where webhooks cannot reach.
package autoscale

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/GerardSmit/multirunner/internal/config"
	"github.com/GerardSmit/multirunner/internal/github"
	"github.com/GerardSmit/multirunner/internal/pool"
)

// Scaler launches runners on demand across a set of pool launchers.
type Scaler struct {
	states    []*state
	gh        github.ClientProvider
	scope     config.Scope
	pollEvery time.Duration
	logger    *slog.Logger
	baseCtx   context.Context // long-lived launch context (set in Run)
}

type state struct {
	l   *pool.Launcher
	sem chan struct{} // capacity = launcher max
}

// New builds a Scaler. pollSec <= 0 disables API polling (webhook-only).
func New(launchers []*pool.Launcher, gh github.ClientProvider, scope config.Scope, pollSec int, logger *slog.Logger) *Scaler {
	states := make([]*state, len(launchers))
	for i, l := range launchers {
		states[i] = &state{l: l, sem: make(chan struct{}, l.Max())}
	}
	every := time.Duration(pollSec) * time.Second
	return &Scaler{states: states, gh: gh, scope: scope, pollEvery: every,
		logger: logger.With("component", "autoscale"), baseCtx: context.Background()}
}

// Run ensures images are present, starts the poller (if enabled), and blocks
// until ctx is cancelled.
func (s *Scaler) Run(ctx context.Context) error {
	s.baseCtx = ctx // launched runners use this long-lived context, not a request ctx
	for _, st := range s.states {
		if err := st.l.EnsureImage(ctx); err != nil {
			return err
		}
	}
	s.logger.Info("autoscaler running", "pools", len(s.states), "poll", s.pollEvery.String())
	if s.pollEvery > 0 {
		s.reconcile() // initial top-up
		go s.pollLoop(ctx)
	}
	<-ctx.Done()
	return nil
}

// OnQueued launches one runner for the first matching pool with spare capacity,
// registered to repo ("owner/repo") so the runner can actually serve the job
// that triggered it. An empty or unmanaged repo falls back to rotation.
// Launches use the scaler's long-lived context (NOT the caller's), so a webhook
// handler returning does not cancel the runner.
func (s *Scaler) OnQueued(repo string, labels []string) {
	s.launchFor(s.gh.ClientFor(repo), labels)
}

// launchFor launches one runner on client for the first matching pool with spare
// capacity.
func (s *Scaler) launchFor(client *github.Client, labels []string) {
	for _, st := range s.states {
		if labelsMatch(st.l.Labels(), labels) {
			if s.tryLaunch(st, client) {
				return
			}
		}
	}
	s.logger.Debug("queued job: no matching pool with spare capacity", "labels", labels)
}

func (s *Scaler) tryLaunch(st *state, client *github.Client) bool {
	select {
	case st.sem <- struct{}{}:
		target := "rotation"
		if client != nil {
			target = client.Target()
		}
		s.logger.Info("scaling up", "pool", st.l.Name(), "target", target)
		go func() {
			defer func() { <-st.sem }()
			if _, err := st.l.RunOneOn(s.baseCtx, client); err != nil && s.baseCtx.Err() == nil {
				s.logger.Error("runner failed", "pool", st.l.Name(), "err", err)
			}
		}()
		return true
	default:
		return false
	}
}

func (s *Scaler) pollLoop(ctx context.Context) {
	t := time.NewTicker(s.pollEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.reconcile()
		}
	}
}

// reconcile queries queued work (repo/repos scope) and tops up runners to capacity.
// Each job carries the client for the repo that queued it, so the runner is
// registered there rather than wherever rotation happens to point.
func (s *Scaler) reconcile() {
	if s.scope != config.ScopeRepo && s.scope != config.ScopeRepos {
		return // org/enterprise: rely on webhook (no cheap queued-jobs endpoint)
	}
	jobs, err := s.gh.QueuedJobs(s.baseCtx)
	if err != nil {
		s.logger.Warn("poll queued jobs failed", "err", err)
		return
	}
	for _, job := range jobs {
		s.launchFor(job.Client, job.Labels)
	}
}

// labelsMatch reports whether a pool with poolLabels can serve a job requesting
// jobLabels (the pool must carry every requested label).
//
// Matching is case-insensitive because GitHub treats runner labels that way: a
// job requesting the standard `[self-hosted, Windows, X64]` is served by a runner
// registered as `windows`/`x64`. Comparing case-sensitively here made autoscale
// silently match nothing for any workflow using GitHub's default label casing.
func labelsMatch(poolLabels, jobLabels []string) bool {
	for _, jl := range jobLabels {
		found := false
		for _, pl := range poolLabels {
			if strings.EqualFold(pl, jl) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}
