//go:build darwin

package main

import (
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/kardianos/service"
	"golang.org/x/sys/unix"

	"github.com/GerardSmit/multirunner/internal/servicehost"
)

const (
	darwinOutputMaxBytes = 1024 * 1024
	darwinOutputArchives = 5
	darwinOutputMode     = 0o640
	darwinRotationCheck  = time.Second
)

func captureServiceOutput(interactive bool, logger service.Logger) (func(), error) {
	if interactive {
		return func() {}, nil
	}
	return captureDarwinServiceOutput(servicehost.LaunchdOutputPath, logger)
}

func captureDarwinServiceOutput(path string, logger service.Logger) (func(), error) {
	output, err := openRotatingOutput(path, darwinOutputMaxBytes, darwinOutputArchives, darwinOutputMode, openDarwinOutput)
	if err != nil {
		return nil, fmt.Errorf("open rotating service output: %w", err)
	}
	restore, err := redirectDarwinDescriptors(int(output.file.Fd()))
	if err != nil {
		output.Close()
		return nil, err
	}
	if _, err := output.RotateIfNeeded(rebindDarwinDescriptors); err != nil {
		restore()
		output.Close()
		return nil, fmt.Errorf("rotate initial service output: %w", err)
	}
	watcher, err := newDarwinOutputWatcher(output)
	if err != nil {
		restore()
		output.Close()
		return nil, fmt.Errorf("watch service output: %w", err)
	}
	stop := make(chan struct{})
	var wait sync.WaitGroup
	wait.Add(1)
	go func() {
		defer wait.Done()
		runDarwinRotation(output, watcher, logger, stop, rebindDarwinDescriptors)
	}()

	var once sync.Once
	return func() {
		once.Do(func() {
			close(stop)
			wait.Wait()
			restore()
			_ = output.Close()
		})
	}, nil
}

func runDarwinRotation(
	output *rotatingOutput,
	watcher *darwinOutputWatcher,
	logger service.Logger,
	stop <-chan struct{},
	rebind outputFileRebinder,
) {
	defer watcher.Close()
	for {
		if _, err := watcher.Wait(stop); err != nil {
			logDarwinRotationError(logger, fmt.Errorf("wait for output growth: %w", err))
			if !waitForDarwinRotationRetry(stop) {
				return
			}
		}
		select {
		case <-stop:
			return
		default:
		}
		rotated, err := output.RotateIfNeeded(rebind)
		if err != nil {
			logDarwinRotationError(logger, err)
			if errors.Is(err, os.ErrClosed) {
				return
			}
			continue
		}
		if rotated {
			if err := watcher.Rebind(output); err != nil {
				logDarwinRotationError(logger, fmt.Errorf("watch rotated output: %w", err))
			}
		}
	}
}

func waitForDarwinRotationRetry(stop <-chan struct{}) bool {
	timer := time.NewTimer(darwinRotationCheck)
	defer timer.Stop()
	select {
	case <-stop:
		return false
	case <-timer.C:
		return true
	}
}

func logDarwinRotationError(logger service.Logger, err error) {
	if logger != nil {
		_ = logger.Error("rotate service output: " + sanitizeLogText(err.Error()))
	}
}

func redirectDarwinDescriptors(target int) (func(), error) {
	stdout, err := unix.Dup(unix.Stdout)
	if err != nil {
		return nil, fmt.Errorf("duplicate stdout: %w", err)
	}
	unix.CloseOnExec(stdout)
	stderr, err := unix.Dup(unix.Stderr)
	if err != nil {
		unix.Close(stdout)
		return nil, fmt.Errorf("duplicate stderr: %w", err)
	}
	unix.CloseOnExec(stderr)
	if err := unix.Dup2(target, unix.Stdout); err != nil {
		unix.Close(stdout)
		unix.Close(stderr)
		return nil, fmt.Errorf("redirect stdout: %w", err)
	}
	if err := unix.Dup2(target, unix.Stderr); err != nil {
		_ = unix.Dup2(stdout, unix.Stdout)
		unix.Close(stdout)
		unix.Close(stderr)
		return nil, fmt.Errorf("redirect stderr: %w", err)
	}
	return func() {
		_ = unix.Dup2(stdout, unix.Stdout)
		_ = unix.Dup2(stderr, unix.Stderr)
		unix.Close(stdout)
		unix.Close(stderr)
	}, nil
}

func rebindDarwinDescriptors(file *os.File) error {
	stdout, err := unix.Dup(unix.Stdout)
	if err != nil {
		return fmt.Errorf("duplicate stdout for rotation: %w", err)
	}
	unix.CloseOnExec(stdout)
	stderr, err := unix.Dup(unix.Stderr)
	if err != nil {
		unix.Close(stdout)
		return fmt.Errorf("duplicate stderr for rotation: %w", err)
	}
	unix.CloseOnExec(stderr)
	if err := unix.Dup2(int(file.Fd()), unix.Stdout); err != nil {
		unix.Close(stdout)
		unix.Close(stderr)
		return fmt.Errorf("rebind stdout after rotation: %w", err)
	}
	if err := unix.Dup2(int(file.Fd()), unix.Stderr); err != nil {
		_ = unix.Dup2(stdout, unix.Stdout)
		unix.Close(stdout)
		unix.Close(stderr)
		return fmt.Errorf("rebind stderr after rotation: %w", err)
	}
	unix.Close(stdout)
	unix.Close(stderr)
	return nil
}

func openDarwinOutput(path string, mode os.FileMode) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_APPEND|unix.O_CLOEXEC|unix.O_CREAT|unix.O_NOFOLLOW|unix.O_WRONLY, uint32(mode.Perm()))
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		unix.Close(fd)
		return nil, fmt.Errorf("create file handle for %s", path)
	}
	return file, nil
}
