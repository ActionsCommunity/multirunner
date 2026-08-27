package github

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/GerardSmit/multirunner/internal/config"
)

// ClientProvider abstracts access to one or more GitHub API clients for runner
// registration. Single-scope configs wrap a *Client directly; multi-repo configs
// use a RepoSet holding one client per repo.
//
// A repo-scoped runner registers to exactly one repo, so the client chosen for a
// launch decides which repo that runner can ever serve. Demand-driven launches
// must therefore place the runner on the repo that queued the job (QueuedJobs or
// ClientFor). Only warm capacity, where no job has been queued yet to place
// against, may fall back to NextClient rotation.
//
// Both GenerateJITConfig and DeleteRunner within a single RunOnce call must use
// the same *Client (the one chosen at the start of that call).
type ClientProvider interface {
	// NextClient returns a *Client for the next runner slot to register. It
	// rotates blindly, so it is only correct for warm capacity.
	NextClient() *Client

	// ClientFor returns the client for "owner/repo", or nil when this provider
	// does not manage that repo.
	ClientFor(repo string) *Client

	// QueuedJobs returns queued jobs across all managed scopes, each paired with
	// the client for the repo that queued it.
	QueuedJobs(ctx context.Context) ([]QueuedJob, error)

	// Scope returns the configured scope.
	Scope() config.Scope
}

// QueuedJob is a queued workflow job together with the client for the repo that
// queued it. Carrying the client is what lets the scaler register the new runner
// where the work actually is instead of wherever rotation happens to point.
type QueuedJob struct {
	Client *Client
	Labels []string
}

// Verify *Client satisfies ClientProvider at compile time.
var _ ClientProvider = (*Client)(nil)

// NextClient returns the client itself (single-scope case).
func (c *Client) NextClient() *Client { return c }

// ClientFor returns the client itself: a single-scope provider has exactly one
// registration target, so every job maps to it.
func (c *Client) ClientFor(string) *Client { return c }

// QueuedJobs returns this client's queued jobs, each paired with the client.
func (c *Client) QueuedJobs(ctx context.Context) ([]QueuedJob, error) {
	labels, err := c.QueuedJobLabels(ctx)
	if err != nil {
		return nil, err
	}
	return pairWith(c, labels), nil
}

// Target names the registration target for logging: "owner/repo" in repo scope,
// otherwise the org or enterprise slug.
func (c *Client) Target() string {
	if c.repo != "" {
		return c.owner + "/" + c.repo
	}
	return c.owner
}

// pairWith tags each label set with the client whose repo produced it.
func pairWith(c *Client, labels [][]string) []QueuedJob {
	jobs := make([]QueuedJob, 0, len(labels))
	for _, l := range labels {
		jobs = append(jobs, QueuedJob{Client: c, Labels: l})
	}
	return jobs
}

// RepoSet wraps multiple per-repo *Clients and distributes runner registrations
// across them via atomic round-robin. It is the ClientProvider for scope=repos.
type RepoSet struct {
	clients []*Client
	repos   []string // "owner/repo" labels, same order as clients
	counter atomic.Uint64
	mu      sync.Mutex // protects nothing currently; reserved for future expansion
}

// NewRepoSet builds a RepoSet from a list of per-repo clients. The repos slice
// provides "owner/repo" labels for logging (same length/order as clients).
func NewRepoSet(clients []*Client, repos []string) *RepoSet {
	return &RepoSet{clients: clients, repos: repos}
}

// NextClient returns the next *Client in round-robin order. Thread-safe.
func (rs *RepoSet) NextClient() *Client {
	n := rs.counter.Add(1)
	return rs.clients[(n-1)%uint64(len(rs.clients))]
}

// ClientFor returns the client for "owner/repo", or nil when the repo is not in
// this set. Matching is case-insensitive because GitHub treats repo names that
// way, and webhook payloads echo whatever casing the caller used.
func (rs *RepoSet) ClientFor(repo string) *Client {
	for i, r := range rs.repos {
		if strings.EqualFold(r, repo) {
			return rs.clients[i]
		}
	}
	return nil
}

// QueuedJobs aggregates queued jobs across all repos, tagging each with the
// client for the repo that queued it so the runner lands where the work is.
// Each per-repo client is queried sequentially (they share the same PAT, so the
// rate limit is pooled). Errors on individual repos are skipped rather than
// failing the aggregate: one unreachable repo should not stop the others from
// being polled. The caller (autoscale) retries on the next poll interval.
func (rs *RepoSet) QueuedJobs(ctx context.Context) ([]QueuedJob, error) {
	var all []QueuedJob
	for _, c := range rs.clients {
		labels, err := c.QueuedJobLabels(ctx)
		if err != nil {
			continue
		}
		all = append(all, pairWith(c, labels)...)
	}
	return all, nil
}

// Scope returns ScopeRepos.
func (rs *RepoSet) Scope() config.Scope { return config.ScopeRepos }

// Clients returns the underlying per-repo clients (for shutdown cleanup).
func (rs *RepoSet) Clients() []*Client { return rs.clients }

// Repos returns the repo names in registration order.
func (rs *RepoSet) Repos() []string { return rs.repos }

// Len returns the number of repos.
func (rs *RepoSet) Len() int { return len(rs.clients) }
