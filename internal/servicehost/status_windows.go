package servicehost

import (
	"errors"
	"fmt"
	"syscall"

	"github.com/kardianos/service"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

const serviceDoesNotExist syscall.Errno = 1060

// Status queries SCM with status-only access so doctor works without service
// control privileges.
func Status(name string, _ service.Service) (service.Status, error) {
	handle, err := windows.OpenSCManager(nil, nil, windows.SC_MANAGER_CONNECT)
	if err != nil {
		return service.StatusUnknown, err
	}
	manager := &mgr.Mgr{Handle: handle}
	defer manager.Disconnect()

	serviceHandle, err := windows.OpenService(manager.Handle, syscall.StringToUTF16Ptr(name), windows.SERVICE_QUERY_STATUS)
	if err != nil {
		if errors.Is(err, serviceDoesNotExist) {
			return service.StatusUnknown, service.ErrNotInstalled
		}
		return service.StatusUnknown, err
	}
	nativeService := &mgr.Service{Handle: serviceHandle, Name: name}
	defer nativeService.Close()
	status, err := nativeService.Query()
	if err != nil {
		return service.StatusUnknown, err
	}
	switch status.State {
	case svc.StartPending, svc.Running:
		return service.StatusRunning, nil
	case svc.PausePending, svc.Paused, svc.ContinuePending, svc.StopPending, svc.Stopped:
		return service.StatusStopped, nil
	default:
		return service.StatusUnknown, fmt.Errorf("unknown service status %v", status.State)
	}
}
