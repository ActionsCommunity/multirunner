//go:build !windows

package securefile

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestReadRejectsPermissiveSymlinkAndMultipleLinks(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "secret")
	if err := os.WriteFile(path, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Read(path); !errors.Is(err, ErrInsecurePermissions) {
		t.Fatalf("permissive read error = %v", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "linked")
	if err := os.Link(path, link); err != nil {
		t.Fatal(err)
	}
	if _, err := Read(path); !errors.Is(err, ErrInsecurePermissions) {
		t.Fatalf("multiple-link read error = %v", err)
	}
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	symlink := filepath.Join(dir, "symlink")
	if err := os.Symlink(path, symlink); err != nil {
		t.Fatal(err)
	}
	if _, err := Read(symlink); !errors.Is(err, ErrInsecurePermissions) {
		t.Fatalf("symlink read error = %v", err)
	}
}

func TestReadRejectsSymlinkInParentChain(t *testing.T) {
	root := t.TempDir()
	realDir := filepath.Join(root, "real")
	if err := os.Mkdir(realDir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(realDir, "secret")
	if err := CreateExclusive(path, []byte("secret")); err != nil {
		t.Fatal(err)
	}
	linkedDir := filepath.Join(root, "linked")
	if err := os.Symlink(realDir, linkedDir); err != nil {
		t.Fatal(err)
	}
	if _, err := Read(filepath.Join(linkedDir, "secret")); !errors.Is(err, ErrInsecurePermissions) {
		t.Fatalf("parent symlink read error = %v", err)
	}
}

func TestReadRejectsUnexpectedFileOwner(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("changing file ownership requires root")
	}
	path := filepath.Join(t.TempDir(), "secret")
	if err := CreateExclusive(path, []byte("secret")); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(path, 1, -1); err != nil {
		t.Fatal(err)
	}
	if _, err := Read(path); !errors.Is(err, ErrInsecurePermissions) {
		t.Fatalf("unexpected-owner read error = %v", err)
	}
}
