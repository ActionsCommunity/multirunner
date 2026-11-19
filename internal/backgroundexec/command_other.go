//go:build !windows

package backgroundexec

import "os/exec"

func prepare(cmd *exec.Cmd) {}
