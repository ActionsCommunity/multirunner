package servicehost

import (
	"fmt"
	"os/exec"
	"strings"
)

// ConfigureRecovery is encoded in the installed systemd unit.
func ConfigureRecovery(string) error {
	return nil
}

// ResetRecovery clears systemd's failed state and start-rate counter.
func ResetRecovery(name, platform string) error {
	if !strings.Contains(platform, "systemd") {
		return nil
	}
	if output, err := exec.Command("systemctl", "reset-failed", name+".service").CombinedOutput(); err != nil {
		return fmt.Errorf("systemctl reset-failed: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}
