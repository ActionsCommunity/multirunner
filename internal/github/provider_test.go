package github

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/google/go-github/v66/github"

	"github.com/GerardSmit/multirunner/internal/config"
)

func TestClientForSlotReturnsSelf(t *testing.T) {
	c := &Client{scope: config.ScopeRepo, owner: "o", repo: "r"}
	if got := c.ClientForSlot(42); got != c {
		t.Error("ClientForSlot should return the same *Client")
	}
}

func TestClientScope(t *testing.T) {
	c := &Client{scope: config.ScopeOrg}
	if c.Scope() != config.ScopeOrg {
		t.Errorf("Scope = %q, want org", c.Scope())
	}
}

func TestRepoSetStableSlotPlacement(t *testing.T) {
	clients := make([]*Client, 3)
	repos := make([]string, 3)
	for i := range clients {
		repos[i] = "repo" + string(rune('A'+i))
		clients[i] = &Client{scope: config.ScopeRepo, owner: "o", repo: repos[i]}
	}
	rs := NewRepoSet(clients, repos)

	if rs.Scope() != config.ScopeRepos {
		t.Errorf("Scope = %q, want repos", rs.Scope())
	}
	if rs.Len() != 3 {
		t.Errorf("Len = %d, want 3", rs.Len())
	}

	// Each pool gets the same complete placement independently.
	for cycle := 0; cycle < 2; cycle++ {
		for i := 0; i < 3; i++ {
			got := rs.ClientForSlot(i)
			if got != clients[i] {
				t.Errorf("cycle %d, index %d: got client for %q, want %q",
					cycle, i, got.repo, clients[i].repo)
			}
		}
	}
}

func TestRepoSetSlotPlacementConcurrent(t *testing.T) {
	clients := make([]*Client, 4)
	repos := make([]string, 4)
	for i := range clients {
		repos[i] = "repo" + string(rune('0'+i))
		clients[i] = &Client{scope: config.ScopeRepo, owner: "o", repo: repos[i]}
	}
	rs := NewRepoSet(clients, repos)

	const pools = 20
	var wg sync.WaitGroup
	wg.Add(pools)
	for poolIndex := 0; poolIndex < pools; poolIndex++ {
		go func() {
			defer wg.Done()
			seen := make(map[*Client]bool, len(clients))
			for slot := range clients {
				c := rs.ClientForSlot(slot)
				if c == nil {
					t.Errorf("pool %d slot %d returned nil", poolIndex, slot)
					continue
				}
				seen[c] = true
			}
			if len(seen) != len(clients) {
				t.Errorf("pool %d covered %d unique repos, want %d", poolIndex, len(seen), len(clients))
			}
		}()
	}
	wg.Wait()
}

// TestRepoSetQueuedJobsTagsOriginatingRepo is the regression test for the
// placement bug: a repo-scoped runner binds to one repo, so a queued job must
// carry the client for the repo that queued it. Flattening the results (the old
// behavior) let a job queued on repoB spawn a runner on repoA, where it idled
// while the job stayed queued.
func TestRepoSetQueuedJobsTagsOriginatingRepo(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/o/repoA/actions/runs":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"workflow_runs": []map[string]any{{"id": 1}},
			})
		case "/repos/o/repoA/actions/runs/1/jobs":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"jobs": []map[string]any{
					{"status": "queued", "labels": []string{"self-hosted", "linux"}},
				},
			})
		case "/repos/o/repoB/actions/runs":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"workflow_runs": []map[string]any{{"id": 2}},
			})
		case "/repos/o/repoB/actions/runs/2/jobs":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"jobs": []map[string]any{
					{"status": "queued", "labels": []string{"self-hosted", "windows"}},
					{"status": "completed", "labels": []string{"self-hosted", "linux"}},
				},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	base, _ := url.Parse(srv.URL + "/")
	makeClient := func(repo string) *Client {
		ghc := github.NewClient(nil)
		ghc.BaseURL = base
		return &Client{gh: ghc, scope: config.ScopeRepo, owner: "o", repo: repo}
	}

	rs := NewRepoSet(
		[]*Client{makeClient("repoA"), makeClient("repoB")},
		[]string{"o/repoA", "o/repoB"},
	)

	jobs, err := rs.QueuedJobs(context.Background())
	if err != nil {
		t.Fatalf("QueuedJobs: %v", err)
	}
	// repoA has 1 queued job, repoB has 1 queued job (completed is filtered).
	if len(jobs) != 2 {
		t.Fatalf("got %d jobs, want 2: %v", len(jobs), jobs)
	}

	// The linux job came from repoA and the windows job from repoB. Each must
	// carry the client for its own repo, or the runner lands on the wrong one.
	want := map[string]string{"linux": "o/repoA", "windows": "o/repoB"}
	for _, job := range jobs {
		var os string
		for _, l := range job.Labels {
			if l == "linux" || l == "windows" {
				os = l
			}
		}
		if os == "" {
			t.Fatalf("job %v has neither linux nor windows label", job.Labels)
		}
		if job.Client == nil {
			t.Fatalf("job %v has nil client", job.Labels)
		}
		if got := job.Client.Target(); got != want[os] {
			t.Errorf("%s job tagged with %q, want %q", os, got, want[os])
		}
	}
}

func TestRepoSetClientForResolvesRepo(t *testing.T) {
	a := &Client{scope: config.ScopeRepo, owner: "o", repo: "repoA"}
	b := &Client{scope: config.ScopeRepo, owner: "o", repo: "repoB"}
	rs := NewRepoSet([]*Client{a, b}, []string{"o/repoA", "o/repoB"})

	if got := rs.ClientFor("o/repoB"); got != b {
		t.Errorf("ClientFor(o/repoB) = %v, want repoB client", got)
	}
	// GitHub treats repo names case-insensitively and webhook payloads echo the
	// caller's casing, so a case mismatch must still resolve.
	if got := rs.ClientFor("O/RepoA"); got != a {
		t.Errorf("ClientFor(O/RepoA) = %v, want repoA client", got)
	}
	if got := rs.ClientFor("o/unmanaged"); got != nil {
		t.Errorf("ClientFor(o/unmanaged) = %v, want nil", got)
	}
	if got := rs.ClientFor(""); got != nil {
		t.Errorf("ClientFor(empty) = %v, want nil", got)
	}
}

func TestClientQueuedJobsTagsItself(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/o/solo/actions/runs":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"workflow_runs": []map[string]any{{"id": 7}},
			})
		case "/repos/o/solo/actions/runs/7/jobs":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"jobs": []map[string]any{
					{"status": "queued", "labels": []string{"self-hosted"}},
				},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	base, _ := url.Parse(srv.URL + "/")
	ghc := github.NewClient(nil)
	ghc.BaseURL = base
	c := &Client{gh: ghc, scope: config.ScopeRepo, owner: "o", repo: "solo"}

	jobs, err := c.QueuedJobs(context.Background())
	if err != nil {
		t.Fatalf("QueuedJobs: %v", err)
	}
	if len(jobs) != 1 {
		t.Fatalf("got %d jobs, want 1", len(jobs))
	}
	if jobs[0].Client != c {
		t.Error("single-scope client must tag its own jobs with itself")
	}
}

func TestClientTarget(t *testing.T) {
	repo := &Client{scope: config.ScopeRepo, owner: "o", repo: "r"}
	if got := repo.Target(); got != "o/r" {
		t.Errorf("repo Target() = %q, want o/r", got)
	}
	org := &Client{scope: config.ScopeOrg, owner: "o"}
	if got := org.Target(); got != "o" {
		t.Errorf("org Target() = %q, want o", got)
	}
}

func TestRepoSetQueuedJobsPartialFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/o/good/actions/runs":
			_ = json.NewEncoder(w).Encode(map[string]any{"workflow_runs": []map[string]any{}})
		case "/repos/o/bad/actions/runs":
			w.WriteHeader(http.StatusInternalServerError)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	base, _ := url.Parse(srv.URL + "/")
	makeClient := func(repo string) *Client {
		ghc := github.NewClient(nil)
		ghc.BaseURL = base
		return &Client{gh: ghc, scope: config.ScopeRepo, owner: "o", repo: repo}
	}

	rs := NewRepoSet(
		[]*Client{makeClient("bad"), makeClient("good")},
		[]string{"o/bad", "o/good"},
	)

	jobs, err := rs.QueuedJobs(context.Background())
	var pollErr *RepoPollError
	if !errors.As(err, &pollErr) {
		t.Fatalf("QueuedJobs error = %v, want RepoPollError", err)
	}
	if pollErr.AllFailed() {
		t.Fatalf("partial failure reported as total: %v", err)
	}
	if _, ok := pollErr.Failures["o/bad"]; !ok {
		t.Errorf("failure does not identify o/bad: %v", err)
	}
	if len(jobs) != 0 {
		t.Errorf("got %d jobs from partial failure, want 0", len(jobs))
	}
}

func TestRepoSetQueuedJobsRotatesStartingRepo(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/actions/runs"):
			repo := strings.Split(r.URL.Path, "/")[3]
			id := map[string]int{"a": 1, "b": 2, "c": 3}[repo]
			_ = json.NewEncoder(w).Encode(map[string]any{
				"workflow_runs": []map[string]any{{"id": id}},
			})
		case strings.Contains(r.URL.Path, "/actions/runs/"):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"jobs": []map[string]any{{"status": "queued", "labels": []string{"self-hosted"}}},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	base, _ := url.Parse(srv.URL + "/")
	var clients []*Client
	var repos []string
	for _, repo := range []string{"a", "b", "c"} {
		ghc := github.NewClient(nil)
		ghc.BaseURL = base
		clients = append(clients, &Client{gh: ghc, scope: config.ScopeRepo, owner: "o", repo: repo})
		repos = append(repos, "o/"+repo)
	}
	rs := NewRepoSet(clients, repos)
	for call, want := range []string{"o/a", "o/b", "o/c"} {
		jobs, err := rs.QueuedJobs(context.Background())
		if err != nil {
			t.Fatalf("call %d: %v", call, err)
		}
		if len(jobs) != 3 || jobs[0].Client.Target() != want {
			t.Fatalf("call %d first target = %v, want %s", call, jobs[0].Client.Target(), want)
		}
	}
}

func TestRepoSetQueuedJobsAllFailed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "down", http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	base, _ := url.Parse(srv.URL + "/")
	clients := make([]*Client, 2)
	for i, repo := range []string{"a", "b"} {
		ghc := github.NewClient(nil)
		ghc.BaseURL = base
		clients[i] = &Client{gh: ghc, scope: config.ScopeRepo, owner: "o", repo: repo}
	}
	_, err := NewRepoSet(clients, []string{"o/a", "o/b"}).QueuedJobs(context.Background())
	var pollErr *RepoPollError
	if !errors.As(err, &pollErr) || !pollErr.AllFailed() {
		t.Fatalf("error = %v, want all-failed RepoPollError", err)
	}
}
