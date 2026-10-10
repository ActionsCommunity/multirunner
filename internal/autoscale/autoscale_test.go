package autoscale

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GerardSmit/multirunner/internal/config"
	"github.com/GerardSmit/multirunner/internal/github"
)

type metadataTestProvider struct {
	client *github.Client
}

func (p metadataTestProvider) ClientForSlot(int) *github.Client { return p.client }
func (p metadataTestProvider) ClientFor(string) *github.Client  { return p.client }
func (p metadataTestProvider) QueuedJobs(context.Context) ([]github.QueuedJob, error) {
	return nil, nil
}
func (p metadataTestProvider) Scope() config.Scope { return config.ScopeRepo }

func TestLabelsMatch(t *testing.T) {
	pool := []string{"self-hosted", "linux", "x64"}
	cases := []struct {
		job  []string
		want bool
	}{
		{[]string{"self-hosted", "linux"}, true},
		{[]string{"self-hosted", "linux", "x64"}, true},
		{[]string{"self-hosted"}, true},
		{[]string{"self-hosted", "windows"}, false}, // windows not on pool
		{[]string{"gpu"}, false},
		{nil, true}, // no requested labels -> any runner matches
		// GitHub's default self-hosted labels are capitalized. Matching must be
		// case-insensitive or autoscale never launches for a standard workflow.
		{[]string{"self-hosted", "Linux", "X64"}, true},
		{[]string{"Self-Hosted", "LINUX"}, true},
	}
	for _, c := range cases {
		if got := labelsMatch(pool, c.job); got != c.want {
			t.Errorf("labelsMatch(%v, %v) = %v, want %v", pool, c.job, got, c.want)
		}
	}
}

// TestLabelsMatchGitHubWindowsCasing pins the exact live regression: a Windows
// pool configured with lowercase labels must serve `runs-on: [self-hosted,
// Windows, X64]`, which is what GitHub reports for every standard Windows job.
func TestLabelsMatchGitHubWindowsCasing(t *testing.T) {
	pool := []string{"self-hosted", "windows", "x64"}
	job := []string{"self-hosted", "Windows", "X64"}
	if !labelsMatch(pool, job) {
		t.Fatalf("labelsMatch(%v, %v) = false, want true", pool, job)
	}
}

// TestLabelsMatchStillRejectsWrongOS guards against the fix over-matching: a
// Linux pool must not pick up a Windows job just because casing is ignored.
func TestLabelsMatchStillRejectsWrongOS(t *testing.T) {
	pool := []string{"self-hosted", "linux", "x64"}
	job := []string{"self-hosted", "Windows", "X64"}
	if labelsMatch(pool, job) {
		t.Fatalf("labelsMatch(%v, %v) = true, want false", pool, job)
	}
}

func TestContainerBuildLabelDoesNotMatchGenericLinuxJob(t *testing.T) {
	pool := []string{"container-build"}
	if labelsMatch(pool, []string{"self-hosted", "Linux", "X64"}) {
		t.Fatal("custom-only container-build pool matched a generic Linux job")
	}
	if !labelsMatch(pool, []string{"container-build"}) {
		t.Fatal("custom-only container-build pool rejected its explicit label")
	}
}

func TestMetadataResolutionIsBoundedAndCancelled(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	scaler := New(nil, metadataTestProvider{client: &github.Client{}},
		config.ScopeRepo, 0, logger)
	scaler.metadataWorkers = 2
	scaler.metadataQueue = make(chan metadataRequest, 64)
	var active atomic.Int32
	var maximum atomic.Int32
	started := make(chan struct{}, 16)
	scaler.resolveMetadata = func(
		ctx context.Context, _ *github.Client, job github.QueuedJob,
	) (github.QueuedJob, error) {
		current := active.Add(1)
		defer active.Add(-1)
		for {
			observed := maximum.Load()
			if current <= observed || maximum.CompareAndSwap(observed, current) {
				break
			}
		}
		started <- struct{}{}
		<-ctx.Done()
		return github.QueuedJob{}, ctx.Err()
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- scaler.Run(ctx) }()
	for id := int64(1); id <= 20; id++ {
		scaler.metadataQueue <- metadataRequest{
			client: &github.Client{},
			job:    github.QueuedJob{Repository: "o/r", RunID: id, JobID: id},
		}
	}
	for range 2 {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("metadata workers did not start")
		}
	}
	if got := maximum.Load(); got != 2 {
		t.Fatalf("maximum metadata concurrency = %d, want 2", got)
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) && err != nil {
			t.Fatalf("Run error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("metadata workers did not stop after cancellation")
	}
}
