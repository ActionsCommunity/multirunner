//go:build windows

package secureartifact

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestPrepareRootRejectsInheritedDirectorySecurity(t *testing.T) {
	root := filepath.Join(t.TempDir(), "existing")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := PrepareRoot(root); !errors.Is(err, ErrInsecure) {
		t.Fatalf("inherited root security error = %v", err)
	}
}
