//go:build !windows

package main

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"syscall"
)

type unixSupervisedProcessGroup struct {
	pid int
}

func prepareSupervisedProcess(cmd *exec.Cmd, verified *os.File) error {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if verified == nil {
		return nil
	}
	cmd.ExtraFiles = append(cmd.ExtraFiles, verified)
	if runtime.GOOS == "linux" {
		cmd.Path = "/proc/self/fd/3"
	} else {
		cmd.Path = "/dev/fd/3"
	}
	return nil
}

func attachSupervisedProcess(cmd *exec.Cmd) (supervisedProcessGroup, error) {
	return &unixSupervisedProcessGroup{pid: cmd.Process.Pid}, nil
}

func (g *unixSupervisedProcessGroup) Kill() error {
	err := syscall.Kill(-g.pid, syscall.SIGKILL)
	if err == syscall.ESRCH {
		return nil
	}
	return err
}

func (g *unixSupervisedProcessGroup) Close() error {
	return nil
}

func describeSupervisedProcessExit(state *os.ProcessState, waitErr error) string {
	if state != nil {
		if status, ok := state.Sys().(syscall.WaitStatus); ok && status.Signaled() {
			return fmt.Sprintf("service_worker_exit signal=%s", status.Signal())
		}
		return fmt.Sprintf("service_worker_exit exit_code=%d", state.ExitCode())
	}
	return "service_worker_exit error=" + sanitizeLogText(waitErr.Error())
}
