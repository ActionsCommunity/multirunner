//go:build windows

package hostrunner

import (
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

const stillActive = 259

type windowsProcessOps struct{}

func platformProcessOps() processOps { return windowsProcessOps{} }

type jobGroup struct {
	mu  sync.Mutex
	job windows.Handle
}

var procNtResumeProcess = windows.NewLazySystemDLL("ntdll.dll").NewProc("NtResumeProcess")

// start creates cmd suspended, assigns it to a Job Object that kills every
// process in it when terminated or when multirunner exits, and only then
// resumes it. Nothing can run, so nothing can spawn, outside the job.
func (windowsProcessOps) start(cmd *exec.Cmd) (processGroup, error) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: windows.CREATE_NEW_PROCESS_GROUP | windows.CREATE_SUSPENDED,
	}
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
	const processSuspendResume = 0x0800
	process, err := windows.OpenProcess(
		windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE|processSuspendResume, false, pid)
	if err != nil {
		_ = cmd.Process.Kill()
		_ = windows.CloseHandle(job)
		return nil, fmt.Errorf("open runner process: %w", err)
	}
	defer windows.CloseHandle(process)
	if err := windows.AssignProcessToJobObject(job, process); err != nil {
		_ = windows.TerminateProcess(process, 1)
		_ = windows.CloseHandle(job)
		return nil, fmt.Errorf("assign runner to job object: %w", err)
	}
	if status, _, _ := procNtResumeProcess.Call(uintptr(process)); status != 0 {
		_ = windows.TerminateJobObject(job, 1)
		_ = windows.CloseHandle(job)
		return nil, fmt.Errorf("resume runner process: NTSTATUS 0x%x", status)
	}
	return &jobGroup{job: job}, nil
}

// runningAsLocalSystem reports whether this process runs as NT AUTHORITY\SYSTEM.
func runningAsLocalSystem() (bool, error) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return false, err
	}
	system, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	if err != nil {
		return false, err
	}
	return user.User.Sid.Equals(system), nil
}

func (g *jobGroup) kill() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.job == 0 {
		return nil
	}
	return windows.TerminateJobObject(g.job, 1)
}

func (g *jobGroup) close() error {
	g.mu.Lock()
	defer g.mu.Unlock()
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

// identify reads a running process's creation time and image name from
// NtQuerySystemInformation(SystemProcessInformation) so PID reuse recovery works
// even when the PID now belongs to a process OpenProcess cannot open.
func (windowsProcessOps) identify(pid int) (processIdentity, bool, error) {
	id, err := processID(pid)
	if err != nil {
		return processIdentity{}, false, err
	}
	buf, err := querySystemProcessInformation()
	if err != nil {
		return processIdentity{}, false, err
	}
	offset := 0
	for {
		if offset+int(unsafe.Sizeof(windows.SYSTEM_PROCESS_INFORMATION{})) > len(buf) {
			return processIdentity{}, false, errors.New("truncated SYSTEM_PROCESS_INFORMATION buffer")
		}
		// #nosec G103 -- walking the variable-length SYSTEM_PROCESS_INFORMATION list returned by NtQuerySystemInformation.
		spi := (*windows.SYSTEM_PROCESS_INFORMATION)(unsafe.Pointer(&buf[offset]))
		if spi.UniqueProcessID == uintptr(id) {
			return processIdentity{
				startTime: filetimeNanoseconds(spi.CreateTime),
				imageName: spi.ImageName.String(),
			}, true, nil
		}
		if spi.NextEntryOffset == 0 {
			return processIdentity{}, false, nil
		}
		next := offset + int(spi.NextEntryOffset)
		if next <= offset || next > len(buf) {
			return processIdentity{}, false, errors.New("invalid SYSTEM_PROCESS_INFORMATION offset")
		}
		offset = next
	}
}

func querySystemProcessInformation() ([]byte, error) {
	size := uint32(256 * 1024)
	for attempt := 0; attempt < 8; attempt++ {
		words := make([]uint64, (int(size)+7)/8)
		// #nosec G103 -- 8-byte-aligned backing buffer for SYSTEM_PROCESS_INFORMATION.
		buf := unsafe.Slice((*byte)(unsafe.Pointer(&words[0])), len(words)*8)
		var needed uint32
		err := windows.NtQuerySystemInformation(
			windows.SystemProcessInformation,
			// #nosec G103 -- passing output buffer to NtQuerySystemInformation.
			unsafe.Pointer(&buf[0]),
			uint32(len(buf)),
			&needed,
		)
		if err == nil {
			if needed > 0 && int(needed) <= len(buf) {
				return buf[:needed], nil
			}
			return buf, nil
		}
		if !errors.Is(err, windows.STATUS_INFO_LENGTH_MISMATCH) {
			return nil, fmt.Errorf("NtQuerySystemInformation: %w", err)
		}
		if needed > size {
			size = needed + 64*1024
		} else {
			size *= 2
		}
	}
	return nil, errors.New("NtQuerySystemInformation: process table kept growing")
}

func filetimeNanoseconds(createTime int64) int64 {
	if createTime <= 0 {
		return 0
	}
	// #nosec G115 -- splitting a 64-bit Windows FILETIME into low and high 32-bit words.
	ft := windows.Filetime{
		LowDateTime:  uint32(uint64(createTime)),
		HighDateTime: uint32(uint64(createTime) >> 32),
	}
	return ft.Nanoseconds()
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
