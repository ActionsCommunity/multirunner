//go:build !windows

package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestRotatingOutputRejectsSymlinkFiles(t *testing.T) {
	t.Run("current", func(t *testing.T) {
		directory := t.TempDir()
		target := filepath.Join(directory, "target.log")
		if err := os.WriteFile(target, nil, 0o640); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(directory, "service.log")
		if err := os.Symlink(target, path); err != nil {
			t.Fatal(err)
		}
		if _, err := openRotatingOutput(path, 4, 2, 0o640, openTestOutput); err == nil {
			t.Fatal("symlink output was accepted")
		}
	})
	t.Run("archive", func(t *testing.T) {
		directory := t.TempDir()
		target := filepath.Join(directory, "target.log")
		if err := os.WriteFile(target, nil, 0o640); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(directory, "service.log")
		if err := os.Symlink(target, archivePath(path, 1)); err != nil {
			t.Fatal(err)
		}
		if _, err := openRotatingOutput(path, 4, 2, 0o640, openTestOutput); err == nil {
			t.Fatal("symlink archive was accepted")
		}
	})
}

func TestRebindFailureRestoresEveryArchive(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "service.log")
	output := newTestRotatingOutput(t, path, 4, 5)
	if _, err := output.file.WriteString("current"); err != nil {
		t.Fatal(err)
	}
	expected := map[string]string{path: "current"}
	for index := 1; index <= 5; index++ {
		content := string(rune('0' + index))
		archive := archivePath(path, index)
		if err := os.WriteFile(archive, []byte(content), 0o640); err != nil {
			t.Fatal(err)
		}
		expected[archive] = content
	}

	rebindErr := errors.New("temporary rebind failure")
	if _, err := output.RotateIfNeeded(func(*os.File) error { return rebindErr }); !errors.Is(err, rebindErr) {
		t.Fatalf("rotation error = %v, want %v", err, rebindErr)
	}
	for file, content := range expected {
		assertFileContent(t, file, content)
	}
	backups, err := filepath.Glob(filepath.Join(directory, ".*.oldest"))
	if err != nil {
		t.Fatal(err)
	}
	if len(backups) != 0 {
		t.Fatalf("rollback retained transaction backups: %v", backups)
	}

	rotated, err := output.RotateIfNeeded(func(*os.File) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if !rotated {
		t.Fatal("retry did not rotate the output")
	}
	if err := output.Close(); err != nil {
		t.Fatal(err)
	}
	assertFileContent(t, archivePath(path, 1), "current")
	for index := 2; index <= 5; index++ {
		assertFileContent(t, archivePath(path, index), string(rune('0'+index-1)))
	}
}
