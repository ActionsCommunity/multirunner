package buildinfo

import "testing"

func TestResolveUsesLocalBuildDefaults(t *testing.T) {
	got := resolve("", "")
	if got.Version != "dev" || got.Commit != "unknown" {
		t.Fatalf("resolve local build = %+v, want dev at unknown", got)
	}
}

func TestResolvePrefersInjectedValues(t *testing.T) {
	got := resolve("v1.2.3", "release456")
	if got.Version != "v1.2.3" || got.Commit != "release456" {
		t.Fatalf("resolve release build = %+v", got)
	}
}

func TestCurrentReturnsInjectedValues(t *testing.T) {
	originalVersion, originalCommit := Version, Commit
	Version, Commit = "v1.2.3", "release456"
	t.Cleanup(func() {
		Version, Commit = originalVersion, originalCommit
	})

	got := Current()
	if got.Version != "v1.2.3" || got.Commit != "release456" {
		t.Fatalf("Current() = %+v", got)
	}
}

func TestInfoString(t *testing.T) {
	got := (Info{Version: "v1.2.3", Commit: "abc123"}).String()
	if got != "v1.2.3 (commit abc123)" {
		t.Fatalf("Info.String() = %q", got)
	}
}
