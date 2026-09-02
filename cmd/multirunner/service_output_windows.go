package main

import (
	"fmt"
	"os"
	"sync"

	"github.com/kardianos/service"
	"golang.org/x/sys/windows"
)

func captureServiceOutput(interactive bool, logger service.Logger) (func(), error) {
	if interactive {
		return func() {}, nil
	}
	reader, writer, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("create service log pipe: %w", err)
	}
	oldOut, err := windows.GetStdHandle(windows.STD_OUTPUT_HANDLE)
	if err != nil {
		reader.Close()
		writer.Close()
		return nil, fmt.Errorf("read stdout handle: %w", err)
	}
	oldErr, err := windows.GetStdHandle(windows.STD_ERROR_HANDLE)
	if err != nil {
		reader.Close()
		writer.Close()
		return nil, fmt.Errorf("read stderr handle: %w", err)
	}
	if err := windows.SetStdHandle(windows.STD_OUTPUT_HANDLE, windows.Handle(writer.Fd())); err != nil {
		reader.Close()
		writer.Close()
		return nil, fmt.Errorf("redirect stdout: %w", err)
	}
	if err := windows.SetStdHandle(windows.STD_ERROR_HANDLE, windows.Handle(writer.Fd())); err != nil {
		_ = windows.SetStdHandle(windows.STD_OUTPUT_HANDLE, oldOut)
		reader.Close()
		writer.Close()
		return nil, fmt.Errorf("redirect stderr: %w", err)
	}

	oldStdout, oldStderr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = writer, writer
	var wait sync.WaitGroup
	wait.Add(1)
	go func() {
		defer wait.Done()
		copyServiceOutput(reader, logger)
	}()

	return func() {
		os.Stdout, os.Stderr = oldStdout, oldStderr
		_ = windows.SetStdHandle(windows.STD_OUTPUT_HANDLE, oldOut)
		_ = windows.SetStdHandle(windows.STD_ERROR_HANDLE, oldErr)
		_ = writer.Close()
		wait.Wait()
		_ = reader.Close()
	}, nil
}
