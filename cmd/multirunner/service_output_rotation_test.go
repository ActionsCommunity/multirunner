package main

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
)

func TestRotatingOutputEnforcesThresholdAndPrunesArchives(t *testing.T) {
	path := filepath.Join(t.TempDir(), "service.log")
	output := newTestRotatingOutput(t, path, 4, 2)
	for index, content := range []string{"abcd", "efgh", "ijkl"} {
		if _, err := output.file.WriteString(content); err != nil {
			t.Fatal(err)
		}
		if index < 2 {
			if _, err := output.RotateIfNeeded(nil); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := output.Close(); err != nil {
		t.Fatal(err)
	}
	assertFileContent(t, path, "ijkl")
	assertFileContent(t, archivePath(path, 1), "efgh")
	assertFileContent(t, archivePath(path, 2), "abcd")
	if _, err := os.Stat(archivePath(path, 3)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("archive beyond retention exists: %v", err)
	}
	for index := 0; index <= 2; index++ {
		candidate := path
		if index > 0 {
			candidate = archivePath(path, index)
		}
		info, err := os.Stat(candidate)
		if err != nil {
			t.Fatal(err)
		}
		if info.Size() > 4 {
			t.Errorf("%s size = %d, want at most 4", candidate, info.Size())
		}
	}
}

func TestRotatingOutputReopensIdempotently(t *testing.T) {
	path := filepath.Join(t.TempDir(), "service.log")
	first := newTestRotatingOutput(t, path, 32, 5)
	if _, err := first.file.WriteString("first\n"); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second := newTestRotatingOutput(t, path, 32, 5)
	if _, err := second.file.WriteString("second\n"); err != nil {
		t.Fatal(err)
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
	assertFileContent(t, path, "first\nsecond\n")
}

func TestRotatingOutputRejectsUnsafePaths(t *testing.T) {
	t.Run("relative path", func(t *testing.T) {
		if _, err := openRotatingOutput("service.log", 4, 2, 0o640, openTestOutput); err == nil {
			t.Fatal("relative path was accepted")
		}
	})
	t.Run("non-regular current file", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "service.log")
		if err := os.Mkdir(path, 0o755); err != nil {
			t.Fatal(err)
		}
		if _, err := openRotatingOutput(path, 4, 2, 0o640, openTestOutput); err == nil {
			t.Fatal("directory output was accepted")
		}
	})
	t.Run("non-regular archive", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "service.log")
		if err := os.Mkdir(archivePath(path, 1), 0o755); err != nil {
			t.Fatal(err)
		}
		if _, err := openRotatingOutput(path, 4, 2, 0o640, openTestOutput); err == nil {
			t.Fatal("directory archive was accepted")
		}
	})
}

func TestRotatingOutputConcurrentRotationAndShutdown(t *testing.T) {
	path := filepath.Join(t.TempDir(), "service.log")
	output := newTestRotatingOutput(t, path, 1024, 5)
	start := make(chan struct{})
	var wait sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			for sequence := 0; sequence < 20; sequence++ {
				if _, err := output.RotateIfNeeded(nil); err != nil {
					if !errors.Is(err, os.ErrClosed) {
						t.Errorf("rotation during shutdown: %v", err)
					}
					return
				}
			}
		}()
	}
	close(start)
	if err := output.Close(); err != nil {
		t.Fatal(err)
	}
	wait.Wait()
	if _, err := output.RotateIfNeeded(nil); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("rotation after close error = %v, want os.ErrClosed", err)
	}
}

func TestRotatingOutputConfigurationValidation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "service.log")
	tests := []struct {
		name     string
		maxBytes int64
		archives int
		opener   outputFileOpener
	}{
		{name: "zero size", maxBytes: 0, archives: 1, opener: openTestOutput},
		{name: "zero archives", maxBytes: 1, archives: 0, opener: openTestOutput},
		{name: "missing opener", maxBytes: 1, archives: 1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := openRotatingOutput(path, test.maxBytes, test.archives, 0o640, test.opener); err == nil {
				t.Fatal("invalid rotation configuration was accepted")
			}
		})
	}
}

func TestOpenRotatingOutputReportsFilesystemFailures(t *testing.T) {
	t.Run("missing parent", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "missing", "service.log")
		if _, err := openRotatingOutput(path, 4, 2, 0o640, openTestOutput); err == nil {
			t.Fatal("missing parent directory was accepted")
		}
	})
	t.Run("parent is a file", func(t *testing.T) {
		parent := filepath.Join(t.TempDir(), "parent")
		if err := os.WriteFile(parent, nil, 0o640); err != nil {
			t.Fatal(err)
		}
		if _, err := openRotatingOutput(filepath.Join(parent, "service.log"), 4, 2, 0o640, openTestOutput); err == nil {
			t.Fatal("file parent was accepted")
		}
	})
	t.Run("opener error", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "service.log")
		expected := errors.New("open failed")
		_, err := openRotatingOutput(path, 4, 2, 0o640, func(string, os.FileMode) (*os.File, error) {
			return nil, expected
		})
		if !errors.Is(err, expected) {
			t.Fatalf("open error = %v, want %v", err, expected)
		}
	})
	t.Run("closed file", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "service.log")
		file, err := os.Create(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := file.Close(); err != nil {
			t.Fatal(err)
		}
		if _, err := openRotatingOutput(path, 4, 2, 0o640, func(string, os.FileMode) (*os.File, error) {
			return file, nil
		}); err == nil {
			t.Fatal("closed output handle was accepted")
		}
	})
}

func TestRotateIfNeededReportsDescriptorFailure(t *testing.T) {
	output := newTestRotatingOutput(t, filepath.Join(t.TempDir(), "service.log"), 4, 2)
	if err := output.file.Close(); err != nil {
		t.Fatal(err)
	}
	output.closed = false
	if _, err := output.RotateIfNeeded(nil); err == nil {
		t.Fatal("closed descriptor was accepted")
	}
	output.closed = true
}

func TestRotateIfNeededRejectsArchiveIntroducedAfterOpen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "service.log")
	output := newTestRotatingOutput(t, path, 4, 2)
	if _, err := output.file.WriteString("full"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(archivePath(path, 1), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := output.RotateIfNeeded(nil); err == nil {
		t.Fatal("non-regular archive introduced after open was accepted")
	}
	if err := output.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestRotatingOutputLiveRebindPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "service.log")
	output := newTestRotatingOutput(t, path, 4, 2)
	if _, err := output.file.WriteString("full"); err != nil {
		t.Fatal(err)
	}
	rebinds := 0
	rotated, err := output.RotateIfNeeded(func(*os.File) error {
		rebinds++
		return nil
	})
	if runtime.GOOS == "windows" {
		if err == nil {
			t.Fatal("Windows unexpectedly renamed an open output file")
		}
		if rebinds != 0 || rotated {
			t.Fatalf("failed rotation rebound descriptors: rebinds=%d rotated=%t", rebinds, rotated)
		}
	} else {
		if err != nil {
			t.Fatal(err)
		}
		if rebinds != 1 || !rotated {
			t.Fatalf("rotation result: rebinds=%d rotated=%t", rebinds, rotated)
		}
	}
	if err := output.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestRotateOutputPathsReportsUnsafeTransitions(t *testing.T) {
	t.Run("oldest archive cannot be pruned", func(t *testing.T) {
		directory := t.TempDir()
		path := filepath.Join(directory, "service.log")
		replacement := filepath.Join(directory, "replacement")
		writeTestFile(t, path)
		writeTestFile(t, replacement)
		oldest := archivePath(path, 2)
		if err := os.Mkdir(oldest, 0o755); err != nil {
			t.Fatal(err)
		}
		writeTestFile(t, filepath.Join(oldest, "child"))
		if err := rotateOutputPaths(path, replacement, 2); err == nil {
			t.Fatal("unprunable oldest archive was accepted")
		}
	})
	t.Run("current output is missing", func(t *testing.T) {
		directory := t.TempDir()
		path := filepath.Join(directory, "service.log")
		replacement := filepath.Join(directory, "replacement")
		writeTestFile(t, replacement)
		if err := rotateOutputPaths(path, replacement, 2); err == nil {
			t.Fatal("missing current output was accepted")
		}
	})
	t.Run("replacement is missing", func(t *testing.T) {
		directory := t.TempDir()
		path := filepath.Join(directory, "service.log")
		writeTestFile(t, path)
		if err := rotateOutputPaths(path, filepath.Join(directory, "missing"), 2); err == nil {
			t.Fatal("missing replacement output was accepted")
		}
	})
}

func TestStopAfterRotationErrorClosesOutput(t *testing.T) {
	output := newTestRotatingOutput(t, filepath.Join(t.TempDir(), "service.log"), 4, 2)
	expected := errors.New("rotation failed")
	if err := output.stopAfterRotationError(expected); !errors.Is(err, expected) {
		t.Fatalf("stop error = %v, want %v", err, expected)
	}
	if !output.closed || output.file != nil {
		t.Fatalf("broken output remains usable: closed=%t file=%v", output.closed, output.file)
	}
}

func TestStopAfterRotationErrorReportsCloseFailure(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "closed-*")
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	output := &rotatingOutput{file: file}
	if err := output.stopAfterRotationError(errors.New("rotation failed")); err == nil {
		t.Fatal("close failure was ignored")
	}
	if !output.closed || output.file != nil {
		t.Fatal("failed output retained a stale handle")
	}
}

func TestRotatingOutputCloseReportsClosedHandle(t *testing.T) {
	output := newTestRotatingOutput(t, filepath.Join(t.TempDir(), "service.log"), 4, 2)
	if err := output.file.Close(); err != nil {
		t.Fatal(err)
	}
	if err := output.Close(); err == nil {
		t.Fatal("closing an invalid output handle produced no error")
	}
}

func TestRotateOwnedFileReportsClosedCurrentHandle(t *testing.T) {
	path := filepath.Join(t.TempDir(), "service.log")
	output := newTestRotatingOutput(t, path, 4, 2)
	if err := output.file.Close(); err != nil {
		t.Fatal(err)
	}
	replacement, err := os.CreateTemp(filepath.Dir(path), "replacement-*")
	if err != nil {
		t.Fatal(err)
	}
	defer replacement.Close()
	if err := output.rotateOwnedFile(replacement, replacement.Name()); err == nil {
		t.Fatal("rotation with a closed current handle produced no error")
	}
	output.closed = true
}

func TestRotateOwnedFileReportsReopenFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "service.log")
	output := newTestRotatingOutput(t, path, 4, 2)
	output.open = func(string, os.FileMode) (*os.File, error) {
		return nil, errors.New("reopen failed")
	}
	replacement, err := os.CreateTemp(filepath.Dir(path), "replacement-*")
	if err != nil {
		t.Fatal(err)
	}
	if err := output.rotateOwnedFile(replacement, replacement.Name()); err == nil {
		t.Fatal("reopen failure was ignored")
	}
	if !output.closed {
		t.Fatal("output remained active after reopen failure")
	}
}

func TestCreateReplacementOutputReportsMissingDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing", "service.log")
	if _, _, err := createReplacementOutput(path, 0o640); err == nil {
		t.Fatal("replacement in a missing directory was created")
	}
}

func TestSecureOutputFileRejectsDirectoryHandle(t *testing.T) {
	file, err := os.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if err := secureOutputFile(file, 0o640); err == nil {
		t.Fatal("directory handle was accepted as service output")
	}
}

func newTestRotatingOutput(t *testing.T, path string, maxBytes int64, archives int) *rotatingOutput {
	t.Helper()
	output, err := openRotatingOutput(path, maxBytes, archives, 0o640, openTestOutput)
	if err != nil {
		t.Fatal(err)
	}
	return output
}

func openTestOutput(path string, mode os.FileMode) (*os.File, error) {
	return os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, mode)
}

func writeTestFile(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, nil, 0o640); err != nil {
		t.Fatal(err)
	}
}

func assertFileContent(t *testing.T, path, want string) {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != want {
		t.Fatalf("%s = %q, want %q", filepath.Base(path), content, want)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o640 {
		t.Errorf("%s permissions = %s, want %s", path, info.Mode().Perm(), os.FileMode(0o640))
	}
}
