//go:build windows

package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"unsafe"

	"golang.org/x/sys/windows"
)

type windowsSupervisedProcessGroup struct {
	job windows.Handle
}

func prepareSupervisedProcess(*exec.Cmd) {}

func attachSupervisedProcess(cmd *exec.Cmd) (supervisedProcessGroup, error) {
	if cmd.Process == nil {
		return nil, errors.New("service worker process is unavailable")
	}
	processID, err := windowsProcessID(cmd.Process.Pid)
	if err != nil {
		return nil, err
	}
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, err
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(
		job,
		windows.JobObjectExtendedLimitInformation,
		// #nosec G103 -- x/sys/windows requires this documented structure pointer.
		uintptr(unsafe.Pointer(&info)),
		uint32(unsafe.Sizeof(info)),
	); err != nil {
		_ = windows.CloseHandle(job)
		return nil, err
	}
	process, err := windows.OpenProcess(
		windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE,
		false,
		processID,
	)
	if err != nil {
		_ = windows.CloseHandle(job)
		return nil, err
	}
	defer windows.CloseHandle(process)
	if err := windows.AssignProcessToJobObject(job, process); err != nil {
		_ = windows.CloseHandle(job)
		return nil, err
	}
	return &windowsSupervisedProcessGroup{job: job}, nil
}

func windowsProcessID(pid int) (uint32, error) {
	if pid <= 0 || uint64(pid) > uint64(^uint32(0)) {
		return 0, fmt.Errorf("service worker process ID %d is invalid", pid)
	}
	// #nosec G115 -- the bounds check above proves the conversion is lossless.
	return uint32(pid), nil
}

func (g *windowsSupervisedProcessGroup) Kill() error {
	return windows.TerminateJobObject(g.job, 1)
}

func (g *windowsSupervisedProcessGroup) Close() error {
	return windows.CloseHandle(g.job)
}

func describeSupervisedProcessExit(state *os.ProcessState, waitErr error) string {
	if state != nil {
		return fmt.Sprintf("service_worker_exit exit_code=%d", state.ExitCode())
	}
	return "service_worker_exit error=" + sanitizeLogText(waitErr.Error())
}
