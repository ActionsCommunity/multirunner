// Package buildinfo exposes the identity embedded in a multirunner binary.
package buildinfo

import (
	"runtime/debug"
	"strings"
)

const (
	defaultVersion = "dev"
	unknownCommit  = "unknown"
)

// Version and Commit are set by release builds using the Go linker's -X flag.
var (
	Version = defaultVersion
	Commit  string
)

// Info identifies one multirunner build.
type Info struct {
	Version string
	Commit  string
}

// Current returns the linked release identity, falling back to Go's embedded
// VCS revision for local builds.
func Current() Info {
	build, ok := debug.ReadBuildInfo()
	if !ok {
		return resolve(Version, Commit, nil)
	}
	return resolve(Version, Commit, build.Settings)
}

func resolve(version, commit string, settings []debug.BuildSetting) Info {
	version = strings.TrimSpace(version)
	if version == "" {
		version = defaultVersion
	}
	commit = strings.TrimSpace(commit)
	if commit == "" {
		for _, setting := range settings {
			if setting.Key == "vcs.revision" {
				commit = strings.TrimSpace(setting.Value)
				break
			}
		}
	}
	if commit == "" {
		commit = unknownCommit
	}
	return Info{Version: version, Commit: commit}
}

// String formats the build identity for command-line output.
func (i Info) String() string {
	return i.Version + " (commit " + i.Commit + ")"
}
