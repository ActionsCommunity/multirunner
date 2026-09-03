package buildinfo

import (
	"runtime/debug"
	"testing"
)

func TestResolveUsesLocalBuildDefaults(t *testing.T) {
	got := resolve("", "", nil, false)
	if got.Version != "dev" || got.Commit != "unknown" {
		t.Fatalf("resolve local build = %+v, want dev at unknown", got)
	}
}

func TestResolvePrefersInjectedValues(t *testing.T) {
	got := resolve("v1.2.3", "release456", &debug.BuildInfo{
		Main: debug.Module{Version: "v9.9.9"},
		Settings: []debug.BuildSetting{
			{Key: "vcs.revision", Value: "fallback"},
			{Key: "vcs.modified", Value: "true"},
		},
	}, true)
	if got.Version != "v1.2.3" || got.Commit != "release456" {
		t.Fatalf("resolve release build = %+v", got)
	}
}

func TestResolveUsesModuleVersion(t *testing.T) {
	got := resolve("dev", "unknown", &debug.BuildInfo{
		Main: debug.Module{Version: "v1.2.3"},
	}, true)
	if got.Version != "v1.2.3" || got.Commit != "unknown" {
		t.Fatalf("resolve module build = %+v", got)
	}
}

func TestResolveIgnoresDevelopmentModuleVersion(t *testing.T) {
	got := resolve("dev", "unknown", &debug.BuildInfo{
		Main: debug.Module{Version: "(devel)"},
	}, true)
	if got.Version != "dev" {
		t.Fatalf("resolve development module = %+v", got)
	}
}

func TestResolveUsesCleanVCSFallback(t *testing.T) {
	got := resolve("dev", "unknown", &debug.BuildInfo{
		Settings: []debug.BuildSetting{
			{Key: "vcs.revision", Value: "abc123"},
			{Key: "vcs.modified", Value: "false"},
		},
	}, true)
	if got.Version != "dev" || got.Commit != "abc123" {
		t.Fatalf("resolve clean VCS build = %+v", got)
	}
}

func TestResolveMarksModifiedVCSFallback(t *testing.T) {
	got := resolve("dev", "unknown", &debug.BuildInfo{
		Main: debug.Module{Version: "v1.2.3"},
		Settings: []debug.BuildSetting{
			{Key: "vcs.revision", Value: "abc123"},
			{Key: "vcs.modified", Value: "true"},
		},
	}, true)
	if got.Version != "v1.2.3-dirty" || got.Commit != "abc123" {
		t.Fatalf("resolve modified VCS build = %+v", got)
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
