package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GerardSmit/multirunner/internal/config"
	"github.com/GerardSmit/multirunner/internal/ghapp"
)

func TestDeviceConnectKeepsSeparateConfigCredentials(t *testing.T) {
	dir := t.TempDir()
	for _, tc := range []struct {
		name, client, token string
		flags               connectFlags
	}{
		{"repo.yaml", "repo-client", "repo-token", connectFlags{repo: "acme/repo"}},
		{"org.yaml", "org-client", "org-token", connectFlags{org: "acme"}},
	} {
		df := fakeDeviceFlow([][]ghapp.Installation{{orgInstall(1, "acme")}})
		df.clientID = tc.client
		df.pollToken = func(context.Context, *ghapp.DeviceCode) (*ghapp.UserToken, error) {
			return &ghapp.UserToken{AccessToken: tc.token}, nil
		}
		var out bytes.Buffer
		if err := runDeviceConnect(filepath.Join(dir, tc.name), tc.flags, strings.NewReader(""), &out, false, nil, df); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct{ name, client, token string }{
		{"repo.yaml", "repo-client", "repo-token"}, {"org.yaml", "org-client", "org-token"},
	} {
		cfg, err := config.Load(filepath.Join(dir, tc.name))
		if err != nil {
			t.Fatal(err)
		}
		tok, err := ghapp.LoadUserToken(cfg.Auth.TokenPath)
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Auth.ClientID != tc.client || tok.AccessToken != tc.token {
			t.Fatalf("%s credential changed: client=%s token=%s", tc.name, cfg.Auth.ClientID, tok.AccessToken)
		}
	}
}

func TestStarterPoolUsesSelectedDaemonArchitecture(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/_ping") {
			w.Header().Set("API-Version", "1.47")
			w.Header().Set("OSType", "linux")
			_, _ = w.Write([]byte("OK"))
			return
		}
		if strings.HasSuffix(r.URL.Path, "/info") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"OSType":"linux","Architecture":"aarch64"}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()
	host := "tcp://" + strings.TrimPrefix(srv.URL, "http://")
	t.Setenv("DOCKER_HOST", host)
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := config.WriteDeviceAuth(path, config.ScopeOrg, "acme", "", "cid", "tok.json"); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	addStarterPool(context.Background(), path, &out)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	p := cfg.Pools[0]
	if p.Docker.Host != host || strings.Join(p.Labels, ",") != "self-hosted,linux,arm64" {
		t.Fatalf("selected daemon metadata lost: host=%q labels=%v output=%s", p.Docker.Host, p.Labels, out.String())
	}
}
