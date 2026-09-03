//go:build !windows

package ghapp

import (
	"fmt"
	"log/slog"
	"os"
)

// restrictToOwner enforces owner-only permissions. The mode passed at creation
// is already 0600, so this only re-asserts it against a permissive umask.
func restrictToOwner(path string) error {
	if err := os.Chmod(path, 0o600); err != nil {
		return fmt.Errorf("restrict %s to owner: %w", path, err)
	}
	return nil
}

// CheckOwnerOnly reports whether path is reachable only by its owner: no group
// or world permission bits.
func CheckOwnerOnly(path string) error {
	fi, err := os.Stat(path)
	if err != nil {
		return err
	}
	if perm := fi.Mode().Perm(); perm&0o077 != 0 {
		return fmt.Errorf("mode is %04o, want no group/world bits", perm)
	}
	return nil
}

// warnIfPermissive reports a credential file other accounts on this host can read.
func warnIfPermissive(path string) {
	if err := CheckOwnerOnly(path); err != nil {
		slog.Warn("credential file is readable by other accounts on this host; restore owner-only access with chmod 600",
			slog.String("path", path), slog.Any("error", err))
	}
}
