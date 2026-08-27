package github

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/google/go-github/v66/github"

	"github.com/GerardSmit/multirunner/internal/config"
)

// newTestClient points a Client at an httptest server.
func newTestClient(t *testing.T, server *httptest.Server, scope config.Scope, owner, repo string) *Client {
	t.Helper()
	ghc := github.NewClient(nil)
	base, err := url.Parse(server.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	ghc.BaseURL = base
	return &Client{gh: ghc, scope: scope, owner: owner, repo: repo}
}

func TestGenerateJITConfig_Scopes(t *testing.T) {
	cases := []struct {
		name     string
		scope    config.Scope
		owner    string
		repo     string
		wantPath string
	}{
		{"repo", config.ScopeRepo, "octo", "hello", "/repos/octo/hello/actions/runners/generate-jitconfig"},
		{"org", config.ScopeOrg, "myorg", "", "/orgs/myorg/actions/runners/generate-jitconfig"},
		{"enterprise", config.ScopeEnterprise, "myent", "", "/enterprises/myent/actions/runners/generate-jitconfig"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var gotBody map[string]any
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost {
					t.Errorf("method = %s, want POST", r.Method)
				}
				if r.URL.Path != tc.wantPath {
					t.Errorf("path = %s, want %s", r.URL.Path, tc.wantPath)
				}
				body, _ := io.ReadAll(r.Body)
				_ = json.Unmarshal(body, &gotBody)
				w.WriteHeader(http.StatusCreated)
				_ = json.NewEncoder(w).Encode(map[string]any{
					"encoded_jit_config": "BASE64BLOB",
					"runner":             map[string]any{"id": 42, "name": tc.owner + "-runner"},
				})
			}))
			defer srv.Close()

			c := newTestClient(t, srv, tc.scope, tc.owner, tc.repo)
			out, err := c.GenerateJITConfig(context.Background(), JITConfigRequest{
				Name:          "runner-1",
				RunnerGroupID: 1,
				Labels:        []string{"self-hosted", "linux"},
				WorkFolder:    "_work",
			})
			if err != nil {
				t.Fatalf("GenerateJITConfig: %v", err)
			}
			if out.EncodedJITConfig != "BASE64BLOB" {
				t.Errorf("EncodedJITConfig = %q", out.EncodedJITConfig)
			}
			if out.Runner.ID != 42 {
				t.Errorf("Runner.ID = %d, want 42", out.Runner.ID)
			}
			if gotBody["name"] != "runner-1" {
				t.Errorf("body name = %v", gotBody["name"])
			}
			if gotBody["work_folder"] != "_work" {
				t.Errorf("body work_folder = %v", gotBody["work_folder"])
			}
			labels, ok := gotBody["labels"].([]any)
			if !ok || len(labels) != 2 {
				t.Errorf("body labels = %v", gotBody["labels"])
			}
		})
	}
}

func TestGenerateJITConfig_EmptyConfigErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"encoded_jit_config": ""})
	}))
	defer srv.Close()

	c := newTestClient(t, srv, config.ScopeOrg, "myorg", "")
	if _, err := c.GenerateJITConfig(context.Background(), JITConfigRequest{Name: "r", RunnerGroupID: 1}); err == nil {
		t.Fatal("expected error on empty encoded_jit_config")
	}
}

func TestCreateRegistrationToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/orgs/myorg/actions/runners/registration-token" {
			t.Errorf("path = %s", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"token": "REGTOKEN"})
	}))
	defer srv.Close()

	c := newTestClient(t, srv, config.ScopeOrg, "myorg", "")
	tok, err := c.CreateRegistrationToken(context.Background())
	if err != nil {
		t.Fatalf("CreateRegistrationToken: %v", err)
	}
	if tok != "REGTOKEN" {
		t.Errorf("token = %q", tok)
	}
}

func TestDeleteRunner(t *testing.T) {
	var gotMethod, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c := newTestClient(t, srv, config.ScopeOrg, "myorg", "")
	if err := c.DeleteRunner(context.Background(), 42); err != nil {
		t.Fatalf("DeleteRunner: %v", err)
	}
	if gotMethod != http.MethodDelete {
		t.Errorf("method = %s", gotMethod)
	}
	if gotPath != "/orgs/myorg/actions/runners/42" {
		t.Errorf("path = %s", gotPath)
	}
}

func TestQueuedJobLabels(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/octo/hello/actions/runs":
			if r.URL.Query().Get("status") != "queued" {
				t.Errorf("status query = %q", r.URL.Query().Get("status"))
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"workflow_runs": []map[string]any{{"id": 101}},
			})
		case "/repos/octo/hello/actions/runs/101/jobs":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"jobs": []map[string]any{
					{"status": "queued", "labels": []string{"self-hosted", "linux", "x64"}},
					{"status": "completed", "labels": []string{"self-hosted", "windows"}},
				},
			})
		default:
			t.Errorf("unexpected path = %s", r.URL.String())
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	c := newTestClient(t, srv, config.ScopeRepo, "octo", "hello")
	labels, err := c.QueuedJobLabels(context.Background())
	if err != nil {
		t.Fatalf("QueuedJobLabels: %v", err)
	}
	if len(labels) != 1 || len(labels[0]) != 3 || labels[0][1] != "linux" {
		t.Fatalf("labels = %#v", labels)
	}
}

func TestPATTransportSetsAuth(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	tr := &patTransport{token: "ghp_secret", base: http.DefaultTransport}
	client := &http.Client{Transport: tr}
	resp, err := client.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if gotAuth != "Bearer ghp_secret" {
		t.Errorf("Authorization = %q, want Bearer ghp_secret", gotAuth)
	}
}

func TestRunnersPath(t *testing.T) {
	c := &Client{scope: config.Scope("bogus")}
	if _, err := c.runnersPath("x"); err == nil {
		t.Fatal("expected error for unsupported scope")
	}
}

func TestActionsEnabled(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{"enabled", `{"enabled":true}`, true},
		{"disabled", `{"enabled":false}`, false},
		// An omitted field must read as disabled, not as a silent "true".
		{"omitted", `{}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var gotPath string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotPath = r.URL.Path
				w.Header().Set("Content-Type", "application/json")
				if _, err := io.WriteString(w, tc.body); err != nil {
					t.Errorf("write: %v", err)
				}
			}))
			defer srv.Close()

			c := newTestClient(t, srv, config.ScopeRepo, "octo", "hello")
			got, err := c.ActionsEnabled(context.Background())
			if err != nil {
				t.Fatalf("ActionsEnabled: %v", err)
			}
			if want := "/repos/octo/hello/actions/permissions"; gotPath != want {
				t.Errorf("path = %q, want %q", gotPath, want)
			}
			if got != tc.want {
				t.Errorf("enabled = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestActionsEnabledRequiresRepoScope pins that org and enterprise clients are
// rejected rather than silently querying /repos//actions/permissions.
func TestActionsEnabledRequiresRepoScope(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request to %s", r.URL.Path)
	}))
	defer srv.Close()

	c := newTestClient(t, srv, config.ScopeOrg, "myorg", "")
	if _, err := c.ActionsEnabled(context.Background()); err == nil {
		t.Fatal("want error for org-scoped client, got nil")
	}
}

// TestDeleteRunnerNotFound pins the race with GitHub's own cleanup: an ephemeral
// runner that finished its job is already deregistered, so callers doing
// best-effort cleanup need to tell that apart from a real failure.
func TestDeleteRunnerNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
	}))
	defer srv.Close()

	c := newTestClient(t, srv, config.ScopeRepo, "octo", "hello")
	err := c.DeleteRunner(context.Background(), 42)
	if !errors.Is(err, ErrRunnerNotFound) {
		t.Fatalf("DeleteRunner error = %v, want ErrRunnerNotFound", err)
	}
}

// TestDeleteRunnerServerErrorIsNotNotFound pins that only 404 is treated as
// "already gone". A 500 must stay a real failure so it gets logged.
func TestDeleteRunnerServerErrorIsNotNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"message":"boom"}`, http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := newTestClient(t, srv, config.ScopeRepo, "octo", "hello")
	err := c.DeleteRunner(context.Background(), 42)
	if err == nil {
		t.Fatal("want error on 500")
	}
	if errors.Is(err, ErrRunnerNotFound) {
		t.Fatalf("500 reported as ErrRunnerNotFound: %v", err)
	}
}
