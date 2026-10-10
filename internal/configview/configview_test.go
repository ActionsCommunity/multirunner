package configview

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GerardSmit/multirunner/internal/config"
)

func TestInspectorRedactsSecretsAndDetectsDrift(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	initial := `github: {scope: org, owner: example}
auth: {pat: secret-canary}
history:
  enabled: true
  database: history.db
  listen: 127.0.0.1:9092
  notifications:
    webhooks:
      - {name: operations, url: "https://alerts.example.com", secret: notification-canary}
pools:
  - name: linux
    os: linux
    docker: {host: unix:///var/run/docker.sock}
`
	if err := os.WriteFile(path, []byte(initial), 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	inspector, err := New(path, loaded)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := inspector.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Drifted || strings.Contains(snapshot.RawRedacted, "secret-canary") ||
		strings.Contains(snapshot.NormalizedRedacted, "secret-canary") ||
		strings.Contains(snapshot.RawRedacted, "notification-canary") ||
		strings.Contains(snapshot.NormalizedRedacted, "notification-canary") {
		t.Fatalf("initial snapshot leaked or drifted: %+v", snapshot)
	}
	if err := os.WriteFile(path, []byte(strings.Replace(initial, "owner: example", "owner: changed", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	snapshot, err = inspector.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if !snapshot.Drifted || !containsPath(snapshot.ChangedPaths, "github.owner") {
		t.Fatalf("drift snapshot = %+v", snapshot)
	}
}

func containsPath(paths []string, target string) bool {
	for _, path := range paths {
		if path == target {
			return true
		}
	}
	return false
}
