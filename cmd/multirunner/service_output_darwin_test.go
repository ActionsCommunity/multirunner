//go:build darwin

package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestCaptureDarwinServiceOutputCapturesBothDescriptors(t *testing.T) {
	path := filepath.Join(t.TempDir(), "service.log")
	restore, err := captureDarwinServiceOutput(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fmt.Fprintln(os.Stdout, "runtime stdout"); err != nil {
		t.Fatal(err)
	}
	if _, err := fmt.Fprintln(os.Stderr, "runtime stderr"); err != nil {
		t.Fatal(err)
	}
	restore()

	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"runtime stdout", "runtime stderr"} {
		if !strings.Contains(string(content), expected) {
			t.Errorf("captured output missing %q: %q", expected, content)
		}
	}
}

func TestDarwinRotationUsesBoundedConfiguration(t *testing.T) {
	if darwinOutputMaxBytes != 1024*1024 {
		t.Fatalf("rotation threshold = %d, want 1 MiB", darwinOutputMaxBytes)
	}
	if darwinOutputArchives != 5 {
		t.Fatalf("archive count = %d, want 5", darwinOutputArchives)
	}
}

func TestCaptureDarwinServiceOutputRejectsSymlink(t *testing.T) {
	directory := t.TempDir()
	target := filepath.Join(directory, "target.log")
	if err := os.WriteFile(target, nil, 0o640); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "service.log")
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	if _, err := captureDarwinServiceOutput(path, nil); err == nil {
		t.Fatal("symlink output was accepted")
	}
}

func TestDarwinDescriptorRotationRebindsWithoutLosingOutput(t *testing.T) {
	path := filepath.Join(t.TempDir(), "service.log")
	output, err := openRotatingOutput(path, 8, 2, 0o640, openDarwinOutput)
	if err != nil {
		t.Fatal(err)
	}

	restore, err := redirectDarwinDescriptors(int(output.file.Fd()))
	if err != nil {
		output.Close()
		t.Fatal(err)
	}
	if _, err := fmt.Fprint(os.Stdout, "before rotation"); err != nil {
		t.Fatal(err)
	}
	rotated, err := output.RotateIfNeeded(rebindDarwinDescriptors)
	if err != nil {
		t.Fatal(err)
	}
	if !rotated {
		t.Fatal("output did not rotate after crossing the threshold")
	}
	if _, err := fmt.Fprint(os.Stderr, "after rotation"); err != nil {
		t.Fatal(err)
	}
	restore()
	if err := output.Close(); err != nil {
		t.Fatal(err)
	}
	assertFileContent(t, archivePath(path, 1), "before rotation")
	assertFileContent(t, path, "after rotation")
}

func TestDarwinDescriptorRotationAndShutdownAreRaceFree(t *testing.T) {
	path := filepath.Join(t.TempDir(), "service.log")
	output, err := openRotatingOutput(path, 128, 5, 0o640, openDarwinOutput)
	if err != nil {
		t.Fatal(err)
	}
	restore, err := redirectDarwinDescriptors(int(output.file.Fd()))
	if err != nil {
		output.Close()
		t.Fatal(err)
	}
	var wait sync.WaitGroup
	for worker := 0; worker < 4; worker++ {
		wait.Add(1)
		go func(worker int) {
			defer wait.Done()
			for sequence := 0; sequence < 100; sequence++ {
				_, _ = fmt.Fprintf(os.Stdout, "%d:%d\n", worker, sequence)
				_, _ = output.RotateIfNeeded(rebindDarwinDescriptors)
			}
		}(worker)
	}
	wait.Wait()
	restore()
	if err := output.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestDarwinOutputWatcherDetectsFileGrowth(t *testing.T) {
	path := filepath.Join(t.TempDir(), "service.log")
	output, err := openRotatingOutput(path, 8, 2, 0o640, openDarwinOutput)
	if err != nil {
		t.Fatal(err)
	}
	defer output.Close()
	watcher, err := newDarwinOutputWatcher(output)
	if err != nil {
		t.Fatal(err)
	}
	defer watcher.Close()

	if _, err := output.file.WriteString("extended"); err != nil {
		t.Fatal(err)
	}
	extended, err := watcher.Wait(make(chan struct{}))
	if err != nil {
		t.Fatal(err)
	}
	if !extended {
		t.Fatal("file growth did not produce a vnode extension event")
	}
}

func TestDarwinRotationRetriesAfterTransientRebindFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "service.log")
	output, err := openRotatingOutput(path, 8, 2, 0o640, openDarwinOutput)
	if err != nil {
		t.Fatal(err)
	}
	watcher, err := newDarwinOutputWatcher(output)
	if err != nil {
		output.Close()
		t.Fatal(err)
	}
	stop := make(chan struct{})
	done := make(chan struct{})
	var rebindAttempts atomic.Int32
	go func() {
		defer close(done)
		runDarwinRotation(output, watcher, nil, stop, func(*os.File) error {
			if rebindAttempts.Add(1) == 1 {
				return errors.New("temporary rebind failure")
			}
			return nil
		})
	}()

	output.mu.Lock()
	_, writeErr := output.file.WriteString("cross threshold")
	output.mu.Unlock()
	if writeErr != nil {
		t.Fatal(writeErr)
	}
	deadline := time.Now().Add(3 * darwinRotationCheck)
	for rebindAttempts.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	close(stop)
	<-done
	if err := output.Close(); err != nil {
		t.Fatal(err)
	}
	if rebindAttempts.Load() < 2 {
		t.Fatalf("rebind attempts = %d, want at least 2", rebindAttempts.Load())
	}
	assertFileContent(t, archivePath(path, 1), "cross threshold")
}
