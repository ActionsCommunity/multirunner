// Package buildinfo exposes the identity embedded in a multirunner binary.
package buildinfo

import (
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
	return resolve(Version, Commit)
}

func resolve(version, commit string) Info {
	version = strings.TrimSpace(version)
	if version == "" {
		version = defaultVersion
	}
	commit = strings.TrimSpace(commit)
	if commit == "" {
		commit = unknownCommit
	}
	return Info{Version: version, Commit: commit}
}

// String formats the build identity for command-line output.
func (i Info) String() string {
	return i.Version + " (commit " + i.Commit + ")"
}
