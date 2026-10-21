// Package autoscale scales ephemeral runners on demand: it launches runners when
// jobs are queued (via webhook events and/or API polling), up to each pool's max.
// Polling is outbound-only, so it works behind NAT where webhooks cannot reach.
package autoscale

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/GerardSmit/multirunner/internal/config"
	"github.com/GerardSmit/multirunner/internal/github"
	"github.com/GerardSmit/multirunner/internal/pool"
	"github.com/GerardSmit/multirunner/internal/webhook"
)

const (
	defaultMetadataWorkers = 4
	metadataQueuePerWorker = 32
	maxPendingJobs         = 4096
)

// Scaler launches runners on demand across a set of pool launchers.
type Scaler struct {
	states          []*state
	gh              github.ClientProvider
	scope           config.Scope
	pollEvery       time.Duration
	logger          *slog.Logger
	ctxMu           sync.RWMutex
	baseCtx         context.Context
	metadataQueue   chan metadataRequest
	metadataWorkers int
	metadataOnce    sync.Once
	metadataWG      sync.WaitGroup
	resolveMetadata func(context.Context, *github.Client, github.QueuedJob) (github.QueuedJob, error)
	demandMu        sync.Mutex
	pending         map[string]github.QueuedJob
	retryAfter      map[string]time.Time
	inflight        map[string]struct{}
	recent          map[string]struct{}
	recentOrder     []string
	wake            chan struct{}
	demandWG        sync.WaitGroup
	runnerWG        sync.WaitGroup
}

type state struct {
	l   *pool.Launcher
	sem chan struct{} // capacity = launcher max
}

type metadataRequest struct {
	client *github.Client
	job    github.QueuedJob
}

var _ webhook.QueuedObserver = (*Scaler)(nil)

// New builds a Scaler. pollSec <= 0 disables API polling (webhook-only).
func New(launchers []*pool.Launcher, gh github.ClientProvider, scope config.Scope, pollSec int, logger *slog.Logger) *Scaler {
	states := make([]*state, len(launchers))
	workers := 0
	for i, l := range launchers {
		states[i] = &state{l: l, sem: make(chan struct{}, l.Max())}
		workers += l.Max()
	}
	if workers <= 0 || workers > defaultMetadataWorkers {
		workers = defaultMetadataWorkers
	}
	every := time.Duration(pollSec) * time.Second
	scaler := &Scaler{
		states: states, gh: gh, scope: scope, pollEvery: every,
		logger: logger.With("component", "autoscale"), baseCtx: context.Background(),
		metadataWorkers: workers,
		metadataQueue:   make(chan metadataRequest, workers*metadataQueuePerWorker),
		pending:         make(map[string]github.QueuedJob),
		retryAfter:      make(map[string]time.Time),
		inflight:        make(map[string]struct{}),
		recent:          make(map[string]struct{}),
		wake:            make(chan struct{}, 1),
	}
	scaler.resolveMetadata = func(
		ctx context.Context, client *github.Client, job github.QueuedJob,
	) (github.QueuedJob, error) {
		return client.ResolveQueuedJob(ctx, job.Repository, job.RunID, job.Labels, job)
	}
	return scaler
}

// Run ensures images are present, starts the poller (if enabled), and blocks
// until ctx is cancelled.
func (s *Scaler) Run(ctx context.Context) error {
	s.setContext(ctx)
	defer func() {
		s.demandMu.Lock()
		clear(s.pending)
		clear(s.retryAfter)
		s.demandMu.Unlock()
	}()
	for _, st := range s.states {
		if err := st.l.EnsureImage(ctx); err != nil {
			return err
		}
	}
	s.startMetadataWorkers(ctx)
	s.demandWG.Add(1)
	go func() {
		defer s.demandWG.Done()
		s.demandLoop(ctx)
	}()
	s.logger.Info("autoscaler running", "pools", len(s.states), "poll", s.pollEvery.String())
	if s.pollEvery > 0 {
		s.reconcile() // initial top-up
		s.demandWG.Add(1)
		go func() {
			defer s.demandWG.Done()
			s.pollLoop(ctx)
		}()
	}
	<-ctx.Done()
	s.metadataWG.Wait()
	s.demandWG.Wait()
	s.runnerWG.Wait()
	return nil
}

// OnQueued retains demand for the first matching pool with spare capacity,
// registered to repo ("owner/repo") so the runner can actually serve the job
// that triggered it. Empty or unmanaged repositories are ignored.
// Launches use the scaler's long-lived context (NOT the caller's), so a webhook
// handler returning does not cancel the runner.
func (s *Scaler) OnQueued(event webhook.WorkflowJobEvent) {
	client := s.gh.ClientFor(event.Repository)
	if client == nil {
		s.logger.Warn("ignoring queued job for unmanaged repository", "repo", event.Repository)
		return
	}
	job := github.QueuedJob{
		Client:        client,
		Repository:    event.Repository,
		Labels:        append([]string(nil), event.WorkflowJob.Labels...),
		RunID:         event.WorkflowJob.RunID,
		RunAttempt:    event.WorkflowJob.RunAttempt,
		JobID:         event.WorkflowJob.ID,
		JobName:       event.WorkflowJob.Name,
		JobHTMLURL:    event.WorkflowJob.HTMLURL,
		JobStatus:     event.WorkflowJob.Status,
		JobConclusion: event.WorkflowJob.Conclusion,
		WorkflowName:  event.WorkflowJob.WorkflowName,
		HeadBranch:    event.WorkflowJob.HeadBranch,
		HeadSHA:       event.WorkflowJob.HeadSHA,
		Ref:           event.WorkflowJob.HeadBranch,
	}
	requiresMetadata := false
	for _, st := range s.states {
		if labelsMatch(st.l.Labels(), job.Labels) && st.l.RequiresWorkflowMetadata() {
			requiresMetadata = true
			break
		}
	}
	if !requiresMetadata {
		s.launchFor(job)
		return
	}
	request := metadataRequest{client: client, job: job}
	select {
	case <-s.context().Done():
		return
	case s.metadataQueue <- request:
	default:
		s.logger.Warn("deferring queued job because workflow metadata queue is full",
			"repo", job.Repository, "run_id", job.RunID,
			"queue_capacity", cap(s.metadataQueue))
		s.launchFor(job)
	}
}

func (s *Scaler) startMetadataWorkers(ctx context.Context) {
	s.metadataOnce.Do(func() {
		for range s.metadataWorkers {
			s.metadataWG.Add(1)
			go func() {
				defer s.metadataWG.Done()
				s.metadataWorker(ctx)
			}()
		}
	})
}

func (s *Scaler) metadataWorker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case request := <-s.metadataQueue:
			resolved, err := s.resolveMetadata(ctx, request.client, request.job)
			if err != nil {
				if ctx.Err() == nil {
					s.logger.Warn("deferring queued job whose workflow metadata could not be resolved",
						"repo", request.job.Repository, "run_id", request.job.RunID, "err", err)
					if !errors.Is(err, github.ErrJobInactive) {
						s.launchFor(request.job)
					}
				}
				continue
			}
			s.launchFor(resolved)
		}
	}
}

func (s *Scaler) setContext(ctx context.Context) {
	s.ctxMu.Lock()
	s.baseCtx = ctx
	s.ctxMu.Unlock()
}

func (s *Scaler) context() context.Context {
	s.ctxMu.RLock()
	ctx := s.baseCtx
	s.ctxMu.RUnlock()
	if ctx == nil {
		return context.Background()
	}
	return ctx
}

// launchFor retains authorized demand until a matching pool has spare capacity.
func (s *Scaler) launchFor(job github.QueuedJob) {
	eligible := false
	for _, st := range s.states {
		if (st.l.Allows(job) || st.l.RequiresWorkflowMetadata()) && labelsMatch(st.l.Labels(), job.Labels) {
			eligible = true
			break
		}
	}
	if !eligible || s.context().Err() != nil {
		return
	}
	key := demandKey(job)
	s.demandMu.Lock()
	defer s.demandMu.Unlock()
	if _, ok := s.pending[key]; ok {
		return
	}
	if _, ok := s.inflight[key]; ok {
		return
	}
	if _, ok := s.recent[key]; ok {
		return
	}
	if len(s.pending) >= maxPendingJobs {
		s.logger.Warn("queued job demand queue is full", "repo", job.Repository,
			"job_id", job.JobID, "queue_capacity", maxPendingJobs)
		return
	}
	s.pending[key] = job
	s.signalDemand()
}

func (s *Scaler) tryLaunch(st *state, job github.QueuedJob) bool {
	if s.context().Err() != nil || st.l.Paused() {
		return false
	}
	select {
	case st.sem <- struct{}{}:
		target := job.Client.Target()
		s.logger.Info("scaling up", "pool", st.l.Name(), "target", target)
		ctx := s.context()
		key := demandKey(job)
		s.demandMu.Lock()
		if _, ok := s.pending[key]; !ok {
			s.demandMu.Unlock()
			<-st.sem
			return false
		}
		delete(s.pending, key)
		delete(s.retryAfter, key)
		s.inflight[key] = struct{}{}
		s.demandMu.Unlock()
		s.runnerWG.Add(1)
		go func() {
			defer s.runnerWG.Done()
			code, err := st.l.RunJobIfUnpaused(ctx, job)
			s.demandMu.Lock()
			delete(s.inflight, key)
			if ctx.Err() == nil && (err != nil || code != 0) {
				// Retain failed/paused demand without occupying a runner slot;
				// failures are paced and revalidated before another attempt.
				if _, finished := s.recent[key]; !finished {
					if len(s.pending) < maxPendingJobs {
						s.pending[key] = job
						if !errors.Is(err, pool.ErrProvisioningPaused) {
							s.retryAfter[key] = time.Now().Add(5 * time.Second)
						}
					} else {
						s.logger.Warn("queued job demand queue is full during retry",
							"repo", job.Repository, "job_id", job.JobID, "queue_capacity", maxPendingJobs)
					}
				}
			} else {
				s.rememberDemandLocked(key)
			}
			s.demandMu.Unlock()
			<-st.sem
			s.signalDemand()
			if err != nil && !errors.Is(err, pool.ErrProvisioningPaused) && ctx.Err() == nil {
				s.logger.Error("runner failed", "pool", st.l.Name(), "err", err)
			}
		}()
		return true
	default:
		return false
	}
}

func demandKey(job github.QueuedJob) string {
	if job.JobID > 0 {
		return fmt.Sprintf("%s/job/%d", strings.ToLower(job.Repository), job.JobID)
	}
	return fmt.Sprintf("%s/%d/%d/%d", strings.ToLower(job.Repository), job.RunID, job.RunAttempt, job.JobID)
}

func (s *Scaler) signalDemand() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *Scaler) rememberDemandLocked(key string) {
	if _, exists := s.recent[key]; exists {
		return
	}
	s.recent[key] = struct{}{}
	s.recentOrder = append(s.recentOrder, key)
	if len(s.recentOrder) > maxPendingJobs {
		delete(s.recent, s.recentOrder[0])
		s.recentOrder = s.recentOrder[1:]
	}
}

// OnWorkflowJob can be registered as a webhook observer to invalidate delayed
// demand as soon as GitHub reports that another runner took or finished it.
func (s *Scaler) OnWorkflowJob(_ context.Context, event webhook.WorkflowJobEvent) {
	if event.Action != "completed" && event.Action != "in_progress" {
		return
	}
	key := demandKey(github.QueuedJob{Repository: event.Repository,
		RunID: event.WorkflowJob.RunID, RunAttempt: event.WorkflowJob.RunAttempt, JobID: event.WorkflowJob.ID})
	s.demandMu.Lock()
	delete(s.pending, key)
	delete(s.retryAfter, key)
	s.rememberDemandLocked(key)
	s.demandMu.Unlock()
	s.signalDemand()
}

func (s *Scaler) demandLoop(ctx context.Context) {
	// Resume is a launcher-local control; the bounded dispatcher checks it even
	// in webhook-only mode where no subsequent GitHub delivery is guaranteed.
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.wake:
		case <-ticker.C:
		}
		s.dispatchDemand(ctx)
	}
}

func (s *Scaler) dispatchDemand(ctx context.Context) {
	s.demandMu.Lock()
	keys := make([]string, 0, len(s.pending))
	for key := range s.pending {
		keys = append(keys, key)
	}
	s.demandMu.Unlock()
	sort.Strings(keys)
	for _, key := range keys {
		if ctx.Err() != nil {
			return
		}
		s.demandMu.Lock()
		job, exists := s.pending[key]
		retryAfter := s.retryAfter[key]
		s.demandMu.Unlock()
		if !exists || time.Now().Before(retryAfter) {
			continue
		}
		available := false
		for _, st := range s.states {
			if !st.l.Paused() && len(st.sem) < cap(st.sem) &&
				(st.l.Allows(job) || st.l.RequiresWorkflowMetadata()) && labelsMatch(st.l.Labels(), job.Labels) {
				available = true
				break
			}
		}
		if available {
			resolveCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
			resolved, err := s.revalidateDemand(resolveCtx, job)
			cancel()
			if err != nil {
				if ctx.Err() == nil {
					s.logger.Warn("queued demand revalidation failed",
						"repo", job.Repository, "job_id", job.JobID, "err", err)
				}
				s.demandMu.Lock()
				if errors.Is(err, errDemandInactive) || errors.Is(err, github.ErrJobInactive) {
					delete(s.pending, key)
					delete(s.retryAfter, key)
					s.rememberDemandLocked(key)
				} else if _, pending := s.pending[key]; pending {
					s.retryAfter[key] = time.Now().Add(5 * time.Second)
				}
				s.demandMu.Unlock()
				continue
			}
			authorized := false
			for _, st := range s.states {
				if st.l.Allows(resolved) && labelsMatch(st.l.Labels(), resolved.Labels) {
					authorized = true
					if s.tryLaunch(st, resolved) {
						break
					}
				}
			}
			if !authorized {
				s.demandMu.Lock()
				delete(s.pending, key)
				delete(s.retryAfter, key)
				s.rememberDemandLocked(key)
				s.demandMu.Unlock()
			}
		}
	}
}

var errDemandInactive = errors.New("job is no longer queued")

func (s *Scaler) revalidateDemand(ctx context.Context, job github.QueuedJob) (github.QueuedJob, error) {
	resolved, err := s.resolveMetadata(ctx, job.Client, job)
	if err != nil {
		return github.QueuedJob{}, err
	}
	if resolved.JobStatus != "" && resolved.JobStatus != "queued" {
		return github.QueuedJob{}, fmt.Errorf("%w: %d (%s)", errDemandInactive, job.JobID, resolved.JobStatus)
	}
	if resolved.Status != "" && resolved.Status != "queued" && resolved.Status != "in_progress" {
		return github.QueuedJob{}, fmt.Errorf("%w: run %d (%s)", errDemandInactive, job.RunID, resolved.Status)
	}
	if job.JobID > 0 {
		current, err := job.Client.QueuedJobState(ctx, job)
		if err != nil {
			return github.QueuedJob{}, err
		}
		if current.Status != "queued" {
			return github.QueuedJob{}, fmt.Errorf("%w: %d (%s)", errDemandInactive, job.JobID, current.Status)
		}
		resolved.Labels = append([]string(nil), current.Labels...)
		resolved.JobStatus = current.Status
	}
	return resolved, nil
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
	jobs, err := s.gh.QueuedJobs(s.context())
	if err != nil {
		s.logger.Warn("poll queued jobs failed", "err", err)
		var pollErr *github.RepoPollError
		if errors.As(err, &pollErr) && pollErr.AllFailed() {
			return
		}
	}
	for _, job := range jobs {
		s.launchFor(job)
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
