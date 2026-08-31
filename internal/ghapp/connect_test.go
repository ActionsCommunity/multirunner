package ghapp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestBuildManifest(t *testing.T) {
	org := buildManifest(Options{BaseURL: "https://github.com", Scope: "org", Org: "acme", Name: "mr"}, "http://127.0.0.1:9")
	var m map[string]any
	if err := json.Unmarshal([]byte(org), &m); err != nil {
		t.Fatalf("manifest not JSON: %v", err)
	}
	perms := m["default_permissions"].(map[string]any)
	if perms["organization_self_hosted_runners"] != "write" {
		t.Errorf("org perms = %v", perms)
	}
	if m["redirect_url"] != "http://127.0.0.1:9/callback" {
		t.Errorf("redirect_url = %v", m["redirect_url"])
	}
	if m["setup_url"] != "http://127.0.0.1:9/setup" {
		t.Errorf("setup_url = %v", m["setup_url"])
	}

	repo := buildManifest(Options{BaseURL: "https://github.com", Scope: "repo", Owner: "o", Repo: "r"}, "http://127.0.0.1:9")
	_ = json.Unmarshal([]byte(repo), &m)
	if m["default_permissions"].(map[string]any)["administration"] != "write" {
		t.Errorf("repo perms = %v", m["default_permissions"])
	}
	if m["default_permissions"].(map[string]any)["contents"] != "read" {
		t.Errorf("repo contents permission = %v, want read", m["default_permissions"])
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
