// Package buildinfo exposes the identity embedded in a multirunner binary.
package buildinfo

import (
	"runtime/debug"
	"strings"
)

const (
	defaultVersion = "dev"
	unknownCommit  = "unknown"

	// VersionVariable and CommitVariable are the canonical linker variable paths.
	VersionVariable = "github.com/GerardSmit/multirunner/internal/buildinfo.Version"
	CommitVariable  = "github.com/GerardSmit/multirunner/internal/buildinfo.Commit"
)

// Version and Commit are set by release builds using the Go linker's -X flag.
var (
	Version = defaultVersion
	Commit  = unknownCommit
)

// Info identifies one multirunner build.
type Info struct {
	Version string
	Commit  string
}

// Current returns the linked build identity.
func Current() Info {
	build, ok := debug.ReadBuildInfo()
	return resolve(Version, Commit, build, ok)
}

func resolve(version, commit string, build *debug.BuildInfo, hasBuild bool) Info {
	version = strings.TrimSpace(version)
	linkedVersion := version != "" && version != defaultVersion
	if !linkedVersion && hasBuild && build != nil &&
		build.Main.Version != "" && build.Main.Version != "(devel)" {
		version = build.Main.Version
	}
	if version == "" {
		version = defaultVersion
	}
	commit = strings.TrimSpace(commit)
	linkedCommit := commit != "" && commit != unknownCommit
	dirty := false
	if hasBuild && build != nil {
		for _, setting := range build.Settings {
			switch setting.Key {
			case "vcs.revision":
				if !linkedCommit && strings.TrimSpace(setting.Value) != "" {
					commit = strings.TrimSpace(setting.Value)
				}
			case "vcs.modified":
				dirty = setting.Value == "true"
			}
		}
	}
	if commit == "" {
		commit = unknownCommit
	}
	if dirty && !linkedVersion && !strings.HasSuffix(version, "-dirty") {
		version += "-dirty"
	}
	return Info{Version: version, Commit: commit}
}

// String formats the build identity for command-line output.
func (i Info) String() string {
	return i.Version + " (commit " + i.Commit + ")"
}
