package winsetup

import (
	"errors"
	"testing"

	"golang.org/x/sys/windows/registry"
)

func TestRebootPendingOnlyChecksComponentServicing(t *testing.T) {
	original := openRegistryKey
	t.Cleanup(func() { openRegistryKey = original })

	var paths []string
	openRegistryKey = func(_ registry.Key, path string, _ uint32) (registry.Key, error) {
		paths = append(paths, path)
		return 0, errors.New("not found")
	}

	if rebootPending() {
		t.Fatal("rebootPending = true, want false")
	}
	if len(paths) != 1 {
		t.Fatalf("registry paths = %v, want only component servicing", paths)
	}
	if got, want := paths[0], `SOFTWARE\Microsoft\Windows\CurrentVersion\Component Based Servicing\RebootPending`; got != want {
		t.Fatalf("registry path = %q, want %q", got, want)
	}
}
