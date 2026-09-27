//go:build windows

package main

import (
	"os/exec"
	"strconv"
	"testing"

	"golang.org/x/sys/windows"
)

func TestPrepareSupervisedProcessCreatesNewProcessGroup(t *testing.T) {
	cmd := exec.Command("cmd.exe")
	prepareSupervisedProcess(cmd)
	if cmd.SysProcAttr == nil {
		t.Fatal("prepareSupervisedProcess did not set process attributes")
	}
	if got := cmd.SysProcAttr.CreationFlags; got != windows.CREATE_NEW_PROCESS_GROUP {
		t.Fatalf("creation flags = %#x, want %#x", got, uint32(windows.CREATE_NEW_PROCESS_GROUP))
	}
}

func TestWindowsProcessID(t *testing.T) {
	for _, pid := range []int{-1, 0} {
		if _, err := windowsProcessID(pid); err == nil {
			t.Errorf("windowsProcessID(%d) succeeded", pid)
		}
	}
	if got, err := windowsProcessID(42); err != nil || got != 42 {
		t.Fatalf("windowsProcessID(42) = %d, %v", got, err)
	}
}

func TestWindowsProcessIDRejectsOverflow(t *testing.T) {
	if strconv.IntSize <= 32 {
		t.Skip("int cannot represent a process ID larger than uint32")
	}
	pid := int(uint64(^uint32(0)) + 1)
	if _, err := windowsProcessID(pid); err == nil {
		t.Fatalf("windowsProcessID(%d) succeeded", pid)
	}
}
