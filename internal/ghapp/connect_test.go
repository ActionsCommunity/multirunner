package ghapp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestManifestProperties exercises every combination of scope, webhook, poll,
// and detect and asserts the invariants that keep issue #22 from regressing.
func TestManifestProperties(t *testing.T) {
	scopes := []struct {
		scope       string
		opt         Options
		runnerAdmin string
	}{
		{"repo", Options{BaseURL: "https://github.com", Scope: "repo", Owner: "o", Repo: "r"}, "administration"},
		{"org", Options{BaseURL: "https://github.com", Scope: "org", Org: "acme"}, "organization_self_hosted_runners"},
	}
	const webhook = "https://mr.example.com/webhook"

	for _, sc := range scopes {
		for _, hook := range []string{"", webhook} {
			for _, poll := range []bool{false, true} {
				for _, detect := range []bool{false, true} {
					needs := ManifestNeeds{WebhookURL: hook, PollQueue: poll, DetectRepo: detect}
					name := sc.scope
					if hook != "" {
						name += "+webhook"
					}
					if poll {
						name += "+poll"
					}
					if detect {
						name += "+detect"
					}
					t.Run(name, func(t *testing.T) {
						raw := buildManifest(sc.opt, needs, "http://127.0.0.1:9")
						if !strings.Contains(raw, `"default_events":[`) {
							t.Errorf("default_events must be a JSON array, never null: %s", raw)
						}
						var m map[string]any
						if err := json.Unmarshal([]byte(raw), &m); err != nil {
							t.Fatalf("manifest not JSON: %v", err)
						}
						perms := toStringMap(m["default_permissions"])
						events := toStringSlice(m["default_events"])
						hookAttr := m["hook_attributes"].(map[string]any)
						active, _ := hookAttr["active"].(bool)
						hookURL, _ := hookAttr["url"].(string)

						if perms["metadata"] != "read" {
							t.Errorf("metadata:read always required, got perms=%v", perms)
						}
						if perms[sc.runnerAdmin] != "write" {
							t.Errorf("scope %q needs %s:write, got perms=%v", sc.scope, sc.runnerAdmin, perms)
						}

						wantActions := poll || hook != ""
						if (perms["actions"] == "read") != wantActions {
							t.Errorf("actions:read = %v, want %v (perms=%v)", perms["actions"] == "read", wantActions, perms)
						}
						wantContents := sc.scope == "repo" || detect
						if (perms["contents"] == "read") != wantContents {
							t.Errorf("contents:read = %v, want %v (perms=%v)", perms["contents"] == "read", wantContents, perms)
						}

						// active iff there is an event to deliver.
						if active != (len(events) > 0) {
							t.Errorf("active=%v but events=%v", active, events)
						}
						// workflow_job requires actions:read.
						if contains(events, "workflow_job") && perms["actions"] != "read" {
							t.Errorf("workflow_job requested without actions:read: perms=%v", perms)
						}
						// An active hook must carry a publicly reachable URL.
						if active {
							if err := ValidateWebhookURL(hookURL); err != nil {
								t.Errorf("active hook url %q not reachable: %v", hookURL, err)
							}
						}
						if m["redirect_url"] != "http://127.0.0.1:9/callback" {
							t.Errorf("redirect_url = %v", m["redirect_url"])
						}
					})
				}
			}
		}
	}
}

func TestValidateWebhookURL(t *testing.T) {
	valid := []string{
		"https://mr.example.com/webhook",
		"https://runners.acme.io:8443/github/events",
	}
	for _, u := range valid {
		if err := ValidateWebhookURL(u); err != nil {
			t.Errorf("ValidateWebhookURL(%q) = %v, want nil", u, err)
		}
	}
	invalid := []string{
		"http://mr.example.com/webhook", // not https
		"https://localhost/webhook",
		"https://localhost./webhook", // trailing dot
		"https://runner.local/webhook",
		"https://box.internal/webhook",
		"https://box.lan/webhook",
		"https://svc.home.arpa/webhook",
		"https://myhost/webhook", // single-label host
		"https://127.0.0.1/webhook",
		"https://10.1.2.3/webhook",
		"https://192.168.1.10/webhook",
		"https://172.16.5.9/webhook",
		"https://100.64.1.2/webhook",       // CGNAT / Tailscale
		"https://224.0.0.1/webhook",        // multicast
		"https://169.254.10.1/webhook",     // link-local
		"https://[::1]/webhook",            // loopback v6
		"https://[::]/webhook",             // unspecified v6
		"https://0.0.0.0/webhook",          // unspecified v4
		"https://[fd00::1]/webhook",        // unique-local v6
		"https://[fe80::1%25eth0]/webhook", // zoned link-local v6
		"https://[::ffff:127.0.0.1]/webhook",
		"https://user:pw@example.com/webhook", // embedded credentials
		"ftp://example.com/webhook",
	}
	for _, u := range invalid {
		if err := ValidateWebhookURL(u); err == nil {
			t.Errorf("ValidateWebhookURL(%q) = nil, want error", u)
		}
	}
}

func TestCreateAppURL(t *testing.T) {
	if got := createAppURL(Options{BaseURL: "https://github.com", Scope: "org", Org: "acme"}); got != "https://github.com/organizations/acme/settings/apps/new" {
		t.Errorf("org url = %s", got)
	}
	if got := createAppURL(Options{BaseURL: "https://github.com", Scope: "repo", Owner: "o", Repo: "r"}); got != "https://github.com/settings/apps/new" {
		t.Errorf("repo url = %s", got)
	}
}

func TestConnectValidatesWebhookURL(t *testing.T) {
	_, err := Connect(context.Background(), Options{
		Scope: "repo", Owner: "o", Repo: "r",
		Needs: ManifestNeeds{WebhookURL: "https://127.0.0.1/webhook"},
	})
	if err == nil || !strings.Contains(err.Error(), "publicly reachable") {
		t.Fatalf("Connect error = %v, want webhook validation error before any listener", err)
	}
}

func TestExchangeManifest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/app-manifests/CODE123/conversions") {
			t.Errorf("path = %s", r.URL.Path)
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": 4242, "slug": "multirunner-xyz", "pem": "-----KEY-----",
			"webhook_secret": "whsec", "html_url": "https://github.com/apps/multirunner-xyz",
		})
	}))
	defer srv.Close()

	c, err := exchangeManifest(context.Background(), srv.URL, "CODE123")
	if err != nil {
		t.Fatalf("exchange: %v", err)
	}
	if c.AppID != 4242 || c.Slug != "multirunner-xyz" || c.PEM != "-----KEY-----" || c.WebhookSecret != "whsec" {
		t.Errorf("creds = %+v", c)
	}
}

func toStringMap(v any) map[string]string {
	m := v.(map[string]any)
	out := make(map[string]string, len(m))
	for k, val := range m {
		out[k] = val.(string)
	}
	return out
}

func toStringSlice(v any) []string {
	raw, _ := v.([]any)
	out := make([]string, 0, len(raw))
	for _, e := range raw {
		out = append(out, e.(string))
	}
	return out
}

func contains(s []string, want string) bool {
	for _, e := range s {
		if e == want {
			return true
		}
	}
	return false
}
