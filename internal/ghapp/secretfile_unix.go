//go:build !windows

package ghapp

import (
	"fmt"
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
