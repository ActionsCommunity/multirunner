//go:build windows

package hostrunner

import (
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

const stillActive = 259

type windowsProcessOps struct{}

func platformProcessOps() processOps { return windowsProcessOps{} }

type jobGroup struct{ job windows.Handle }

// start runs cmd in a new process group and assigns it to a Job Object that
// kills every process in it when terminated or when multirunner exits. The
// runner spends seconds booting .NET before it spawns children, so assigning
// right after Start contains the whole tree in practice.
func (windowsProcessOps) start(cmd *exec.Cmd) (processGroup, error) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NEW_PROCESS_GROUP}
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, fmt.Errorf("create job object: %w", err)
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
		// #nosec G103 -- x/sys/windows requires this documented structure pointer.
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		_ = windows.CloseHandle(job)
		return nil, fmt.Errorf("configure job object: %w", err)
	}
	if err := cmd.Start(); err != nil {
		_ = windows.CloseHandle(job)
		return nil, err
	}
	pid, err := processID(cmd.Process.Pid)
	if err != nil {
		_ = windows.CloseHandle(job)
		return nil, err
	}
	process, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, pid)
	if err != nil {
		_ = windows.CloseHandle(job)
		return nil, fmt.Errorf("open runner process: %w", err)
	}
	defer windows.CloseHandle(process)
	if err := windows.AssignProcessToJobObject(job, process); err != nil {
		_ = windows.CloseHandle(job)
		return nil, fmt.Errorf("assign runner to job object: %w", err)
	}
	return &jobGroup{job: job}, nil
}

func (g *jobGroup) kill() error { return windows.TerminateJobObject(g.job, 1) }

func (g *jobGroup) close() error {
	if g.job == 0 {
		return nil
	}
	err := windows.CloseHandle(g.job)
	g.job = 0
	return err
}

func (windowsProcessOps) alive(pid int) (bool, error) {
	id, err := processID(pid)
	if err != nil {
		return false, err
	}
	handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, id)
	if err != nil {
		if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
			return false, nil
		}
		if errors.Is(err, windows.ERROR_ACCESS_DENIED) {
			return true, nil
		}
		return false, err
	}
	defer windows.CloseHandle(handle)
	var code uint32
	if err := windows.GetExitCodeProcess(handle, &code); err != nil {
		return false, err
	}
	return code == stillActive, nil
}

func (windowsProcessOps) startTime(pid int) (int64, error) {
	id, err := processID(pid)
	if err != nil {
		return 0, err
	}
	handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, id)
	if err != nil {
		return 0, err
	}
	defer windows.CloseHandle(handle)
	var created, exited, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(handle, &created, &exited, &kernel, &user); err != nil {
		return 0, err
	}
	return created.Nanoseconds(), nil
}

// killTree is the restart-recovery path, when the Job Object handle died with
// the previous multirunner process: taskkill /T walks the parent chain.
func (windowsProcessOps) killTree(pid int) error {
	out, err := exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(pid)).CombinedOutput()
	if err != nil {
		return fmt.Errorf("taskkill %d: %w: %s", pid, err, out)
	}
	return nil
}

func processID(pid int) (uint32, error) {
	if pid <= 0 || uint64(pid) > uint64(^uint32(0)) {
		return 0, fmt.Errorf("process ID %d is invalid", pid)
	}
	// #nosec G115 -- the bounds check above proves the conversion is lossless.
	return uint32(pid), nil
}
