//go:build !windows

package hostrunner

import (
	"errors"
	"os/exec"
)

var errUnsupported = errors.New("host backend: supported on Windows only")

type unsupportedProcessOps struct{}

func platformProcessOps() processOps { return unsupportedProcessOps{} }

func (unsupportedProcessOps) start(*exec.Cmd) (processGroup, error) { return nil, errUnsupported }
func (unsupportedProcessOps) alive(int) (bool, error)               { return false, errUnsupported }
func (unsupportedProcessOps) startTime(int) (int64, error)          { return 0, errUnsupported }
func (unsupportedProcessOps) killTree(int) error                    { return errUnsupported }
