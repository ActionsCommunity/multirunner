package history

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// AcquireRuntimeLock reserves session recovery and command execution for one
// daemon. Keep the sidecar in place: deleting it would split the lock identity.
func AcquireRuntimeLock(databasePath string) (*os.File, error) {
	if strings.TrimSpace(databasePath) == "" {
		return nil, errors.New("history database path is required for runtime ownership")
	}
	if err := os.MkdirAll(filepath.Dir(databasePath), 0700); err != nil {
		return nil, fmt.Errorf("create history directory: %w", err)
	}
	file, err := os.OpenFile(databasePath+".runtime.lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, fmt.Errorf("open history runtime lock: %w", err)
	}
	if err := lockRuntimeFile(file); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("history runtime is already owned or cannot be locked: %w", err)
	}
	return file, nil
}
