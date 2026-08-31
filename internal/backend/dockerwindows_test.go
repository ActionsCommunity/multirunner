package backend

import (
	"runtime"
	"testing"
)

// newDockerBackend only constructs the client, it never dials.
const testWinHost = "tcp://127.0.0.1:2375"

// Auto isolation cannot inspect a remote daemon's host edition, so it must fail
// closed unless the daemon is reached through a verified-local Windows pipe.
func TestNewDockerWindowsRejectsAutoIsolationForRemoteHost(t *testing.T) {
	for _, in := range []string{"", "auto"} {
		if _, err := NewDockerWindows(testWinHost, in); err == nil {
			t.Errorf("NewDockerWindows(%q) accepted remote auto isolation", in)
		}
	}
}

func TestNewDockerWindowsRecognizesLocalPipeForAutoIsolation(t *testing.T) {
	got := isLocalWindowsPipe(`NPIPE:////./pipe/docker_engine_windows`)
	if want := runtime.GOOS == "windows"; got != want {
		t.Fatalf("isLocalWindowsPipe() = %v, want %v", got, want)
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
