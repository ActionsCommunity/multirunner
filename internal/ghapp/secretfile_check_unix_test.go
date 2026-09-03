//go:build !windows

package ghapp

import (
	"fmt"
	"os"
)

// checkOwnerOnly asserts the file carries no group or world permission bits.
func checkOwnerOnly(path string) error {
	fi, err := os.Stat(path)
	if err != nil {
		return err
	}
	if perm := fi.Mode().Perm(); perm&0o077 != 0 {
		return fmt.Errorf("mode is %04o, want no group/world bits", perm)
	}
	return nil
}
