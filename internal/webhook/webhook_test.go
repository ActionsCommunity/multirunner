package webhook

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/GerardSmit/multirunner/internal/autoscale"
	"github.com/GerardSmit/multirunner/internal/config"
	"github.com/GerardSmit/multirunner/internal/github"
	"github.com/GerardSmit/multirunner/internal/pool"
)

// recordingProvider stands in for a real GitHub provider and records which repo
// the scaler was asked to place a runner on. Returning a nil client is the
// legitimate "repo not managed here" answer, so no network access is needed.
type recordingProvider struct{ asked []string }

func (p *recordingProvider) NextClient() *github.Client { return nil }
func (p *recordingProvider) ClientFor(repo string) *github.Client {
	p.asked = append(p.asked, repo)
	return nil
}
func (p *recordingProvider) QueuedJobs(context.Context) ([]github.QueuedJob, error) {
	return nil, nil
}
func (p *recordingProvider) Scope() config.Scope { return config.ScopeRepo }

func testServerWith(secret string, gh github.ClientProvider) *Server {
	sc := autoscale.New([]*pool.Launcher{}, gh, config.ScopeRepo, 0, slog.New(slog.NewTextHandler(io.Discard, nil)))
	return New("127.0.0.1:0", "/webhook", secret, sc, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func testServer(secret string) *Server {
	return testServerWith(secret, &recordingProvider{})
}

func sign(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func TestValidSignature(t *testing.T) {
	body := []byte(`{"hello":"world"}`)
	if !validSignature("s3cret", sign("s3cret", body), body) {
		t.Error("valid signature rejected")
	}
	if validSignature("s3cret", sign("wrong", body), body) {
		t.Error("bad signature accepted")
	}
	if validSignature("s3cret", "garbage", body) {
		t.Error("malformed header accepted")
	}
}

func do(t *testing.T, s *Server, event, sig string, body string) int {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(body))
	req.Header.Set("X-GitHub-Event", event)
	if sig != "" {
		req.Header.Set("X-Hub-Signature-256", sig)
	}
	rec := httptest.NewRecorder()
	s.handle(rec, req)
	return rec.Code
}

func TestHandlePing(t *testing.T) {
	s := testServer("")
	if code := do(t, s, "ping", "", "{}"); code != http.StatusOK {
		t.Errorf("ping = %d", code)
	}
}

func TestHandleWorkflowJobQueued(t *testing.T) {
	secret := "s3cret"
	s := testServer(secret)
	body := `{"action":"queued","workflow_job":{"labels":["self-hosted","linux","x64"]}}`
	if code := do(t, s, "workflow_job", sign(secret, []byte(body)), body); code != http.StatusOK {
		t.Errorf("queued = %d", code)
	}
}

// TestHandleWorkflowJobQueuedRoutesRepo proves the repo that queued the job
// reaches the scaler. A repo-scoped runner binds to exactly one repo, so losing
// this value means the runner can be registered somewhere with no work.
func TestHandleWorkflowJobQueuedRoutesRepo(t *testing.T) {
	secret := "s3cret"
	p := &recordingProvider{}
	s := testServerWith(secret, p)
	body := `{"action":"queued","repository":{"full_name":"o/repoB"},"workflow_job":{"labels":["self-hosted","windows"]}}`
	if code := do(t, s, "workflow_job", sign(secret, []byte(body)), body); code != http.StatusOK {
		t.Fatalf("queued = %d", code)
	}
	if len(p.asked) != 1 {
		t.Fatalf("scaler asked for %d repos, want 1: %v", len(p.asked), p.asked)
	}
	if p.asked[0] != "o/repoB" {
		t.Errorf("scaler asked to place on %q, want o/repoB", p.asked[0])
	}
}

func TestHandleBadSignature(t *testing.T) {
	s := testServer("s3cret")
	body := `{"action":"queued","workflow_job":{"labels":[]}}`
	if code := do(t, s, "workflow_job", "sha256=deadbeef", body); code != http.StatusUnauthorized {
		t.Errorf("bad sig = %d, want 401", code)
	}
}

func TestHandleUnknownEvent(t *testing.T) {
	s := testServer("")
	if code := do(t, s, "push", "", "{}"); code != http.StatusNoContent {
		t.Errorf("unknown event = %d, want 204", code)
	}
}
