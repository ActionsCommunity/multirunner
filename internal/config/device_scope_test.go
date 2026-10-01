package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDeviceAuthRejectsReposScope(t *testing.T) {
	for _, provisioning := range []string{"pool", "autoscale"} {
		t.Run(provisioning, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			body := "provisioning: " + provisioning + "\ngithub:\n  scope: repos\n  repos: [acme/a, acme/b]\nauth:\n  token_path: token.json\n" + ExamplePoolYAML
			if err := os.WriteFile(path, []byte(body), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "scope=repos requires") {
				t.Fatalf("unsupported auth accepted: %v", err)
			}
		})
	}
}
