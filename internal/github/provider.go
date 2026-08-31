package github

import (
	"context"
	"fmt"
	"sort"
	"strings"
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
// ClientFor). Warm capacity, where no job has been queued yet to place against,
// uses deterministic per-pool slot placement.
//
// Both GenerateJITConfig and DeleteRunner within a single RunOnce call must use
// the same *Client (the one chosen at the start of that call).
type ClientProvider interface {
	// ClientForSlot returns the registration client for a stable warm-pool slot.
	// The same slot must keep the same target across retries and every pool must
	// distribute its own slots independently.
	ClientForSlot(slot int) *Client

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

// ClientForSlot returns the client itself (single-scope case).
func (c *Client) ClientForSlot(int) *Client { return c }

// ClientFor returns this client when it can serve repo.
func (c *Client) ClientFor(repo string) *Client {
	if c.scope == config.ScopeRepo && !strings.EqualFold(c.Target(), repo) {
		return nil
	}
	return c
}

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

// RepoSet wraps multiple per-repo *Clients. Warm-pool placement is stable by
// slot, while each queued-job poll rotates its starting repo for fairness.
type RepoSet struct {
	clients []*Client
	repos   []string // "owner/repo" labels, same order as clients
	poll    atomic.Uint64
}

// NewRepoSet builds a RepoSet from a list of per-repo clients. The repos slice
// provides "owner/repo" labels for logging (same length/order as clients).
func NewRepoSet(clients []*Client, repos []string) *RepoSet {
	return &RepoSet{clients: clients, repos: repos}
}

// ClientForSlot maps each warm-pool slot deterministically. Because the slot
// index is local to a pool, concurrent pools cannot consume each other's
// placement sequence.
func (rs *RepoSet) ClientForSlot(slot int) *Client {
	if len(rs.clients) == 0 || slot < 0 {
		return nil
	}
	return rs.clients[slot%len(rs.clients)]
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

// RepoPollError reports the repositories that could not be polled. Results from
// healthy repositories remain usable, but callers can distinguish a partial
// result from a fully successful poll.
type RepoPollError struct {
	Failures map[string]error
	Total    int
}

func (e *RepoPollError) Error() string {
	names := make([]string, 0, len(e.Failures))
	for name := range e.Failures {
		names = append(names, name)
	}
	sort.Strings(names)
	details := make([]string, 0, len(names))
	for _, name := range names {
		details = append(details, fmt.Sprintf("%s: %v", name, e.Failures[name]))
	}
	return fmt.Sprintf("queued-job polling failed for %d of %d repos: %s",
		len(e.Failures), e.Total, strings.Join(details, "; "))
}

// AllFailed reports whether no configured repository could be polled.
func (e *RepoPollError) AllFailed() bool {
	return e.Total > 0 && len(e.Failures) == e.Total
}

// QueuedJobs aggregates queued jobs across all repos, rotating the first repo
// on every call so constrained autoscale capacity cannot perpetually favor
// earlier config entries.
func (rs *RepoSet) QueuedJobs(ctx context.Context) ([]QueuedJob, error) {
	if len(rs.clients) == 0 {
		return nil, &RepoPollError{Total: 0}
	}
	var all []QueuedJob
	failures := make(map[string]error)
	start := int(rs.poll.Add(1)-1) % len(rs.clients)
	for offset := range rs.clients {
		i := (start + offset) % len(rs.clients)
		c := rs.clients[i]
		labels, err := c.QueuedJobLabels(ctx)
		if err != nil {
			failures[rs.repos[i]] = err
			continue
		}
		all = append(all, pairWith(c, labels)...)
	}
	if len(failures) > 0 {
		return all, &RepoPollError{Failures: failures, Total: len(rs.clients)}
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
