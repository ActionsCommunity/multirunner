//go:build windows

package main

import (
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

// runningServiceConfig returns the config path of the installed multirunner
// service when it is running, or "" when it is not installed, not running, or
// cannot be inspected.
func runningServiceConfig() string {
	manager, err := connectServiceManagerReadOnly()
	if err != nil {
		return ""
	}
	defer manager.Disconnect()
	name, err := windows.UTF16PtrFromString("multirunner")
	if err != nil {
		return ""
	}
	handle, err := windows.OpenService(manager.Handle, name, windows.SERVICE_QUERY_STATUS|windows.SERVICE_QUERY_CONFIG)
	if err != nil {
		return ""
	}
	service := &mgr.Service{Name: "multirunner", Handle: handle}
	defer service.Close()
	status, err := service.Query()
	if err != nil || status.State != svc.Running {
		return ""
	}
	config, err := service.Config()
	if err != nil {
		return ""
	}
	return serviceConfigArgument(config.BinaryPathName)
}

func connectServiceManagerReadOnly() (*mgr.Mgr, error) {
	handle, err := windows.OpenSCManager(nil, nil, windows.SC_MANAGER_CONNECT)
	if err != nil {
		return nil, err
	}
	return &mgr.Mgr{Handle: handle}, nil
}

// serviceConfigArgument extracts the absolute --config value from a service
// command line.
func serviceConfigArgument(commandLine string) string {
	args, err := windows.DecomposeCommandLine(commandLine)
	if err != nil {
		return ""
	}
	for index, arg := range args {
		value := ""
		switch {
		case arg == "--config" && index+1 < len(args):
			value = args[index+1]
		case strings.HasPrefix(arg, "--config="):
			value = strings.TrimPrefix(arg, "--config=")
		default:
			continue
		}
		if absolute, err := filepath.Abs(value); err == nil {
			return absolute
		}
		return ""
	}
	return ""
}

func sameConfigPath(a, b string) bool {
	return strings.EqualFold(filepath.Clean(a), filepath.Clean(b))
}
