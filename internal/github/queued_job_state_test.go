package github

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/GerardSmit/multirunner/internal/config"
)

func TestQueuedJobStateUsesAuthoritativeJobAcrossScopes(t *testing.T) {
	for _, scope := range []config.Scope{config.ScopeRepo, config.ScopeOrg, config.ScopeEnterprise} {
		for _, status := range []string{"queued", "in_progress", "completed"} {
			t.Run(string(scope)+"/"+status, func(t *testing.T) {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path != "/repos/octo/repo/actions/jobs/501" {
						t.Errorf("job request path = %q", r.URL.Path)
					}
					_ = json.NewEncoder(w).Encode(map[string]any{
						"id": 501, "run_id": 101, "run_attempt": 2,
						"status": status, "labels": []string{"actual-label"},
					})
				}))
				defer server.Close()
				repo := ""
				if scope == config.ScopeRepo {
					repo = "repo"
				}
				client := newTestClient(t, server, scope, "octo", repo)
				job, err := client.QueuedJobState(t.Context(), QueuedJob{
					Repository: "octo/repo", JobID: 501, RunID: 101, RunAttempt: 2,
					Labels: []string{"untrusted-label"},
				})
				if status == "queued" {
					if err != nil || len(job.Labels) != 1 || job.Labels[0] != "actual-label" {
						t.Fatalf("queued job = %+v, %v", job, err)
					}
				} else if !errors.Is(err, ErrJobInactive) {
					t.Fatalf("inactive job accepted: %+v, %v", job, err)
				}
			})
		}
	}
}

func TestQueuedJobStateSeparatesTransientFailureAndStaleIdentity(t *testing.T) {
	for _, test := range []struct {
		name     string
		status   int
		payload  string
		inactive bool
	}{
		{"transient", http.StatusServiceUnavailable, `{}`, false},
		{"removed", http.StatusNotFound, `{}`, true},
		{"wrong run", http.StatusOK, `{"id":501,"run_id":99,"run_attempt":2,"status":"queued"}`, true},
		{"wrong attempt", http.StatusOK, `{"id":501,"run_id":101,"run_attempt":3,"status":"queued"}`, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(test.status)
				_, _ = w.Write([]byte(test.payload))
			}))
			defer server.Close()
			client := newTestClient(t, server, config.ScopeOrg, "octo", "")
			_, err := client.QueuedJobState(t.Context(), QueuedJob{Repository: "octo/repo", JobID: 501, RunID: 101, RunAttempt: 2})
			if err == nil || errors.Is(err, ErrJobInactive) != test.inactive {
				t.Fatalf("job state error = %v, inactive=%v", err, test.inactive)
			}
		})
	}
}
