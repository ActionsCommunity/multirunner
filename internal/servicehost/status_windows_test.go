package servicehost

import (
	"errors"
	"testing"

	"github.com/kardianos/service"
)

func TestStatusMissingServiceNeedsNoControlPrivileges(t *testing.T) {
	status, err := Status("multirunner-test-service-that-does-not-exist", nil)
	if status != service.StatusUnknown {
		t.Errorf("status = %v, want unknown", status)
	}
	if !errors.Is(err, service.ErrNotInstalled) {
		t.Fatalf("error = %v, want ErrNotInstalled", err)
	}
}
