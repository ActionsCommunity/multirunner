package github

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"

	"github.com/google/go-github/v66/github"

	"github.com/GerardSmit/multirunner/internal/config"
)

func TestClientNextClientReturnsSelf(t *testing.T) {
	c := &Client{scope: config.ScopeRepo, owner: "o", repo: "r"}
	if got := c.NextClient(); got != c {
		t.Error("NextClient should return the same *Client")
	}
}

func TestClientScope(t *testing.T) {
	c := &Client{scope: config.ScopeOrg}
	if c.Scope() != config.ScopeOrg {
		t.Errorf("Scope = %q, want org", c.Scope())
	}
}

func TestRepoSetRoundRobin(t *testing.T) {
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

	// First 6 calls should cycle through repos twice.
	for cycle := 0; cycle < 2; cycle++ {
		for i := 0; i < 3; i++ {
			got := rs.NextClient()
			if got != clients[i] {
				t.Errorf("cycle %d, index %d: got client for %q, want %q",
					cycle, i, got.repo, clients[i].repo)
			}
		}
	}
}

func TestRepoSetRoundRobinConcurrent(t *testing.T) {
	clients := make([]*Client, 4)
	repos := make([]string, 4)
	for i := range clients {
		repos[i] = "repo" + string(rune('0'+i))
		clients[i] = &Client{scope: config.ScopeRepo, owner: "o", repo: repos[i]}
	}
	rs := NewRepoSet(clients, repos)

	const goroutines = 100
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for g := 0; g < goroutines; g++ {
		go func() {
			defer wg.Done()
			c := rs.NextClient()
			if c == nil {
				t.Error("NextClient returned nil")
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

	// Should not error even though "bad" repo fails.
	jobs, err := rs.QueuedJobs(context.Background())
	if err != nil {
		t.Fatalf("QueuedJobs: %v", err)
	}
	if len(jobs) != 0 {
		t.Errorf("got %d jobs from partial failure, want 0", len(jobs))
	}
}
