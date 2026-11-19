package history

import (
	"path/filepath"
	"testing"
)

func TestRuntimeLockExcludesOtherDaemonsUntilClose(t *testing.T) {
	database := filepath.Join(t.TempDir(), "new-directory", "history.db")
	first, err := AcquireRuntimeLock(database)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	if other, err := AcquireRuntimeLock(database); err == nil {
		other.Close()
		t.Fatal("second daemon obtained the live runtime's session-recovery lock")
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	next, err := AcquireRuntimeLock(database)
	if err != nil {
		t.Fatalf("restart could not acquire the runtime lock: %v", err)
	}
	defer next.Close()
}
