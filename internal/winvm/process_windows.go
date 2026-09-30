//go:build windows

package winvm

import "golang.org/x/sys/windows"

const stillActiveProcess = 259

func processAlive(pid int) (bool, error) {
	handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		if err == windows.ERROR_INVALID_PARAMETER {
			return false, nil
		}
		if err == windows.ERROR_ACCESS_DENIED {
			return true, nil
		}
		return false, err
	}
	defer windows.CloseHandle(handle)

	var code uint32
	if err := windows.GetExitCodeProcess(handle, &code); err != nil {
		return false, err
	}
	return code == stillActiveProcess, nil
}
