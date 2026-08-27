package backend

import (
	"testing"

	"github.com/docker/docker/api/types/container"
)

// newDockerBackend only constructs the client, it never dials, so any
// well-formed host works here. It has to be transport-neutral rather than an
// npipe:// path: that protocol is unavailable off-Windows, and these tests also
// cover the non-Windows autoIsolation() fallback in autoisolation_other.go.
const testWinHost = "tcp://127.0.0.1:2375"

// NewDockerWindows must resolve "" and "auto" through autoIsolation() exactly
// like NewContainerdWindows. Pinning "process" breaks Windows client editions,
// where process isolation requires an exact host/container build match.
func TestNewDockerWindowsResolvesAutoIsolation(t *testing.T) {
	want := container.Isolation(autoIsolation())
	if want == "" {
		t.Fatal("autoIsolation() returned empty")
	}
	for _, in := range []string{"", "auto"} {
		b, err := NewDockerWindows(testWinHost, in)
		if err != nil {
			t.Fatalf("NewDockerWindows(%q): %v", in, err)
		}
		db, ok := b.(*dockerBackend)
		if !ok {
			t.Fatalf("NewDockerWindows(%q) returned %T, want *dockerBackend", in, b)
		}
		if db.isolation != want {
			t.Errorf("isolation for %q = %q, want %q", in, db.isolation, want)
		}
	}
}

// An explicit mode must be passed through untouched.
func TestNewDockerWindowsExplicitIsolation(t *testing.T) {
	for _, in := range []string{"process", "hyperv"} {
		b, err := NewDockerWindows(testWinHost, in)
		if err != nil {
			t.Fatalf("NewDockerWindows(%q): %v", in, err)
		}
		db, ok := b.(*dockerBackend)
		if !ok {
			t.Fatalf("NewDockerWindows(%q) returned %T, want *dockerBackend", in, b)
		}
		if string(db.isolation) != in {
			t.Errorf("isolation = %q, want %q", db.isolation, in)
		}
	}
}
