package main

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/GerardSmit/multirunner/internal/ghapp"
)

func TestConnectReportsPartialSetupAndKeepsCredentials(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses Unix directory write permissions")
	}
	for _, ownApp := range []bool{false, true} {
		name := "device"
		if ownApp {
			name = "own-app"
		}
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "config.yaml")
			t.Cleanup(func() { _ = os.Chmod(dir, 0700) })
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/_ping") {
					w.Header().Set("API-Version", "1.47")
					w.Header().Set("OSType", "linux")
					return
				}
				if strings.HasSuffix(r.URL.Path, "/info") {
					// Authentication and config writing have finished. Fail only the
					// subsequent starter pool write, after daemon discovery.
					if _, err := os.Stat(path); err != nil {
						t.Error(err)
					}
					if err := os.Chmod(dir, 0500); err != nil {
						t.Error(err)
					}
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(`{"OSType":"linux","Architecture":"amd64"}`))
					return
				}
				http.NotFound(w, r)
			}))
			defer srv.Close()
			t.Setenv("DOCKER_HOST", "tcp://"+strings.TrimPrefix(srv.URL, "http://"))
			var out bytes.Buffer
			var err error
			credential := deviceTokenPath(path)
			if ownApp {
				credential = filepath.Join(dir, "multirunner-app.private-key.pem")
				err = runOwnAppConnect(path, connectFlags{org: "acme"}, strings.NewReader(""), &out, false, nil,
					func(context.Context, ghapp.Options) (*ghapp.Credentials, error) {
						return &ghapp.Credentials{Slug: "mr", AppID: 1, InstallationID: 2, PEM: "key"}, nil
					})
			} else {
				df := fakeDeviceFlow([][]ghapp.Installation{{orgInstall(1, "acme")}})
				err = runDeviceConnect(path, connectFlags{org: "acme"}, strings.NewReader(""), &out, false, nil, df)
			}
			if err == nil {
				t.Fatal("connect silently accepted a failed starter pool write")
			}
			if !strings.Contains(err.Error(), "credentials saved") || !strings.Contains(err.Error(), "manually") {
				t.Fatalf("error lacks partial-setup remediation: %v", err)
			}
			if _, err := os.Stat(credential); err != nil {
				t.Fatalf("saved credential lost: %v", err)
			}
			if data, err := os.ReadFile(path); err != nil || !strings.Contains(string(data), "auth:") {
				t.Fatalf("saved config lost: %s, %v", data, err)
			}
			if strings.Contains(out.String(), "Next:") {
				t.Fatalf("misleading success steps: %s", out.String())
			}
		})
	}
}

func TestDeviceConnectRejectsExcludedRepository(t *testing.T) {
	path := filepath.Join(t.TempDir(), "repo.yaml")
	df := fakeDeviceFlow([][]ghapp.Installation{{orgInstall(7, "acme")}})
	df.checkRepo = func(_ context.Context, token string, id int64, owner, repo string) error {
		if token != "fake-token" || id != 7 || owner != "acme" || repo != "excluded" {
			t.Fatal("wrong access check target")
		}
		return errors.New("repository excluded from installation")
	}
	var out bytes.Buffer
	err := runDeviceConnect(path, connectFlags{repo: "acme/excluded"}, strings.NewReader(""), &out, false, nil, df)
	if err == nil || !strings.Contains(err.Error(), "excluded") {
		t.Fatalf("excluded repository accepted: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("config written for inaccessible repository")
	}
	if _, err := os.Stat(deviceTokenPath(path)); !os.IsNotExist(err) {
		t.Fatal("token written for inaccessible repository")
	}
}
