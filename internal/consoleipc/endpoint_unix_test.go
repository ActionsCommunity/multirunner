//go:build !windows

package consoleipc

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestEndpointBoundsLongDatabasePaths(t *testing.T) {
	root := filepath.Join(t.TempDir(), strings.Repeat("nested", 30))
	first, err := Endpoint(filepath.Join(root, "first.db"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := Endpoint(filepath.Join(root, "second.db"))
	if err != nil {
		t.Fatal(err)
	}
	if len(first) >= 104 || len(second) >= 104 || first == second {
		t.Fatalf("long-path endpoints = %q, %q", first, second)
	}
}
