//go:build windows

package backgroundexec

import (
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"
)

func prepare(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: windows.CREATE_NO_WINDOW,
	}
}
