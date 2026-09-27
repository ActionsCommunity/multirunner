//go:build !windows

package servicehost

import "github.com/kardianos/service"

// Status delegates to the native kardianos implementation off Windows.
func Status(_ string, svc service.Service) (service.Status, error) {
	return svc.Status()
}
