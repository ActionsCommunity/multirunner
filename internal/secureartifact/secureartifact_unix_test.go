//go:build !windows

package secureartifact

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestPrepareRootRejectsSymlinkAndPermissiveDirectory(t *testing.T) {
	parent := t.TempDir()
	permissive := filepath.Join(parent, "permissive")
	if err := os.Mkdir(permissive, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := PrepareRoot(permissive); !errors.Is(err, ErrInsecure) {
		t.Fatalf("permissive root error = %v", err)
	}
	target := filepath.Join(parent, "target")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(parent, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := PrepareRoot(link); !errors.Is(err, ErrInsecure) {
		t.Fatalf("symlink root error = %v", err)
	}
}

func TestOpenRejectsSymlinkAndForeignOwnership(t *testing.T) {
	root, err := PrepareRoot(filepath.Join(t.TempDir(), "artifacts"))
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "target")
	if err := os.WriteFile(target, []byte("artifact"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(root, link); !errors.Is(err, ErrInsecure) {
		t.Fatalf("symlink artifact error = %v", err)
	}
	if os.Geteuid() != 0 {
		t.Skip("changing file ownership requires root")
	}
	if err := os.Chown(target, 65534, -1); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(root, target); !errors.Is(err, ErrInsecure) {
		t.Fatalf("foreign-owned artifact error = %v", err)
	}
}
