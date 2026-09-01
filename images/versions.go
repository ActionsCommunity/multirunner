// Package imageversions exposes the canonical, embedded runner-image dependency manifest.
package imageversions

import (
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
)

//go:embed versions.json
var embeddedJSON []byte

type Manifest struct {
	Schema      int         `json:"schema"`
	Minimal     Minimal     `json:"minimal"`
	NativeBuild NativeBuild `json:"native_build"`
	Node        Node        `json:"node"`
	DotNet      DotNet      `json:"dotnet"`
	Go          Go          `json:"go"`
	Rust        Rust        `json:"rust"`
	BuildTools  BuildTools  `json:"buildtools"`
}

type BaseImage struct {
	Repository string `json:"repository"`
	Tag        string `json:"tag"`
	Digest     string `json:"digest"`
}

func (b BaseImage) Reference() string { return b.Repository + ":" + b.Tag + "@" + b.Digest }

type Minimal struct {
	LinuxBase   BaseImage  `json:"linux_base"`
	WindowsBase BaseImage  `json:"windows_base"`
	Runner      Runner     `json:"runner"`
	MinGit      MinGit     `json:"mingit"`
	PowerShell  PowerShell `json:"powershell"`
}

type Runner struct {
	Version          string `json:"version"`
	LinuxX64SHA256   string `json:"linux_x64_sha256"`
	LinuxARM64SHA256 string `json:"linux_arm64_sha256"`
	WindowsX64SHA256 string `json:"windows_x64_sha256"`
}

type MinGit struct {
	Version string `json:"version"`
	URL     string `json:"url"`
	SHA256  string `json:"sha256"`
}

type PowerShell struct {
	Version          string `json:"version"`
	WindowsX64SHA256 string `json:"windows_x64_sha256"`
}

type NativeBuild struct {
	PackageSource string `json:"package_source"`
	Strategy      string `json:"strategy"`
}

// NodeTrackSupportedLTS declares that the manifest carries every LTS major until
// its end-of-life, which includes majors that have moved to Maintenance LTS.
const NodeTrackSupportedLTS = "supported-lts"

type Node struct {
	Track        string                 `json:"track"`
	DefaultMajor int                    `json:"default_major"`
	Releases     map[string]NodeRelease `json:"releases"`
}

type NodeRelease struct {
	Version          string `json:"version"`
	EOL              string `json:"eol"`
	LinuxX64SHA256   string `json:"linux_x64_sha256"`
	LinuxARM64SHA256 string `json:"linux_arm64_sha256"`
	WindowsX64SHA256 string `json:"windows_x64_sha256"`
}

func (n Node) DefaultRelease() (NodeRelease, bool) {
	r, ok := n.Releases[fmt.Sprint(n.DefaultMajor)]
	return r, ok
}

type DotNet struct {
	TrackReleaseTypes           []string                 `json:"track_release_types"`
	UnassignedSupportedChannels []DotNetDiscovery        `json:"unassigned_supported_channels"`
	Channels                    map[string]DotNetChannel `json:"channels"`
}

type DotNetChannel struct {
	Targets          []string `json:"targets"`
	ReleaseType      string   `json:"release_type"`
	Version          string   `json:"version"`
	EOL              string   `json:"eol"`
	SupportPhase     string   `json:"support_phase"`
	LinuxX64SHA512   string   `json:"linux_x64_sha512,omitempty"`
	LinuxARM64SHA512 string   `json:"linux_arm64_sha512,omitempty"`
	WindowsX64SHA512 string   `json:"windows_x64_sha512,omitempty"`
}

type DotNetDiscovery struct {
	Channel      string `json:"channel"`
	ReleaseType  string `json:"release_type"`
	SupportPhase string `json:"support_phase"`
	EOL          string `json:"eol"`
	LatestSDK    string `json:"latest_sdk"`
}

const (
	DotNetTargetLinux            = "linux"
	DotNetTargetWindowsContainer = "windows-container"
	DotNetTargetQEMUWindows      = "qemu-windows"
)

func (d DotNet) ChannelsForTarget(target string) []string {
	var channels []string
	for channel, release := range d.Channels {
		for _, candidate := range release.Targets {
			if candidate == target {
				channels = append(channels, channel)
				break
			}
		}
	}
	sort.Strings(channels)
	return channels
}

type Go struct {
	Strategy           string `json:"strategy"`
	Version            string `json:"version"`
	LinuxAMD64SHA256   string `json:"linux_amd64_sha256"`
	LinuxARM64SHA256   string `json:"linux_arm64_sha256"`
	WindowsAMD64SHA256 string `json:"windows_amd64_sha256"`
}

type Rust struct {
	Channel           string `json:"channel"`
	Version           string `json:"version"`
	RustupVersion     string `json:"rustup_version"`
	RustupX64SHA256   string `json:"rustup_x64_sha256"`
	RustupARM64SHA256 string `json:"rustup_arm64_sha256"`
}

type BuildTools struct {
	DefaultLine string                    `json:"default_line"`
	Lines       map[string]BuildToolsLine `json:"lines"`
}

type BuildToolsLine struct {
	Channel            string `json:"channel"`
	ReleaseLine        string `json:"release_line"`
	Version            string `json:"version"`
	BootstrapperURL    string `json:"bootstrapper_url"`
	BootstrapperSHA256 string `json:"bootstrapper_sha256"`
	ChannelURL         string `json:"channel_url"`
	ChannelSHA256      string `json:"channel_sha256"`
}

func (b BuildTools) DefaultRelease() (BuildToolsLine, bool) {
	r, ok := b.Lines[b.DefaultLine]
	return r, ok
}

func (b BuildTools) ReleaseLines() []string {
	lines := make([]string, 0, len(b.Lines))
	for line := range b.Lines {
		lines = append(lines, line)
	}
	sort.Strings(lines)
	return lines
}

func Embedded() (Manifest, error) { return parse(embeddedJSON) }

func MustEmbedded() Manifest {
	m, err := Embedded()
	if err != nil {
		panic("invalid embedded image version manifest: " + err.Error())
	}
	return m
}

func Read(path string) (Manifest, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Manifest{}, err
	}
	return parse(b)
}

// ReadForUpdate validates policy fields but permits unresolved versions and
// digests. This lets a maintainer add an empty Node major, .NET channel, or
// Build Tools line and have the updater populate every upstream-derived field.
func ReadForUpdate(path string) (Manifest, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Manifest{}, err
	}
	var m Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return Manifest{}, err
	}
	if err := m.ValidatePolicy(); err != nil {
		return Manifest{}, err
	}
	return m, nil
}

func parse(b []byte) (Manifest, error) {
	var m Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return Manifest{}, err
	}
	if err := m.Validate(); err != nil {
		return Manifest{}, err
	}
	return m, nil
}

func (m Manifest) Validate() error {
	if err := m.ValidatePolicy(); err != nil {
		return err
	}
	for name, base := range map[string]BaseImage{"linux": m.Minimal.LinuxBase, "windows": m.Minimal.WindowsBase} {
		if err := digest(base.Digest, "sha256:", 64); err != nil {
			return fmt.Errorf("%s base digest: %w", name, err)
		}
	}
	for name, value := range map[string]string{
		"runner linux x64":   m.Minimal.Runner.LinuxX64SHA256,
		"runner linux arm64": m.Minimal.Runner.LinuxARM64SHA256,
		"runner windows x64": m.Minimal.Runner.WindowsX64SHA256,
		"MinGit":             m.Minimal.MinGit.SHA256,
		"PowerShell":         m.Minimal.PowerShell.WindowsX64SHA256,
		"Go linux amd64":     m.Go.LinuxAMD64SHA256,
		"Go linux arm64":     m.Go.LinuxARM64SHA256,
		"Go windows amd64":   m.Go.WindowsAMD64SHA256,
		"rustup x64":         m.Rust.RustupX64SHA256,
		"rustup arm64":       m.Rust.RustupARM64SHA256,
	} {
		if err := digest(value, "", 64); err != nil {
			return fmt.Errorf("%s digest: %w", name, err)
		}
	}
	if m.Minimal.Runner.Version == "" || m.Minimal.MinGit.Version == "" || m.Minimal.MinGit.URL == "" || m.Minimal.PowerShell.Version == "" || m.Go.Version == "" || m.Rust.Channel == "" || m.Rust.Version == "" || m.Rust.RustupVersion == "" {
		return fmt.Errorf("resolved tool versions and URLs are required")
	}
	for major, r := range m.Node.Releases {
		if r.Version == "" || r.EOL == "" {
			return fmt.Errorf("Node major %s has no resolved release", major)
		}
		for name, value := range map[string]string{"linux x64": r.LinuxX64SHA256, "linux arm64": r.LinuxARM64SHA256, "windows x64": r.WindowsX64SHA256} {
			if err := digest(value, "", 64); err != nil {
				return fmt.Errorf("Node %s %s digest: %w", major, name, err)
			}
		}
	}
	for channel, r := range m.DotNet.Channels {
		if r.Version == "" || r.EOL == "" || r.ReleaseType == "" || r.SupportPhase == "" {
			return fmt.Errorf(".NET channel %q has no resolved release", channel)
		}
		for name, value := range map[string]string{"linux x64": r.LinuxX64SHA512, "linux arm64": r.LinuxARM64SHA512, "windows x64": r.WindowsX64SHA512} {
			if value != "" {
				if err := digest(value, "", 128); err != nil {
					return fmt.Errorf(".NET %s %s digest: %w", channel, name, err)
				}
			}
		}
	}
	for _, channel := range m.DotNet.ChannelsForTarget(DotNetTargetLinux) {
		r := m.DotNet.Channels[channel]
		if r.LinuxX64SHA512 == "" || r.LinuxARM64SHA512 == "" {
			return fmt.Errorf(".NET Linux channel %q lacks Linux archives", channel)
		}
	}
	for _, channel := range append(m.DotNet.ChannelsForTarget(DotNetTargetWindowsContainer), m.DotNet.ChannelsForTarget(DotNetTargetQEMUWindows)...) {
		if m.DotNet.Channels[channel].WindowsX64SHA512 == "" {
			return fmt.Errorf(".NET Windows channel %q lacks a Windows archive", channel)
		}
	}
	for line, release := range m.BuildTools.Lines {
		if release.ReleaseLine != line || release.Version == "" || release.BootstrapperURL == "" || release.ChannelURL == "" {
			return fmt.Errorf("Visual Studio Build Tools %s release identity is incomplete", line)
		}
		if !strings.HasPrefix(release.Version, line+".") {
			return fmt.Errorf("Visual Studio Build Tools %s resolved unexpected version %q", line, release.Version)
		}
		for name, value := range map[string]string{
			"bootstrapper": release.BootstrapperSHA256,
			"channel":      release.ChannelSHA256,
		} {
			if err := digest(value, "", 64); err != nil {
				return fmt.Errorf("Visual Studio Build Tools %s %s digest: %w", line, name, err)
			}
		}
	}
	return nil
}

func (m Manifest) ValidatePolicy() error {
	if m.Schema != 1 {
		return fmt.Errorf("unsupported schema %d", m.Schema)
	}
	for name, base := range map[string]BaseImage{"linux": m.Minimal.LinuxBase, "windows": m.Minimal.WindowsBase} {
		if base.Repository == "" || base.Tag == "" {
			return fmt.Errorf("%s base repository and tag are required", name)
		}
	}
	if len(m.Node.Releases) == 0 {
		return fmt.Errorf("at least one Node major is required")
	}
	if m.Node.Track != NodeTrackSupportedLTS {
		return fmt.Errorf("unsupported Node tracking policy %q", m.Node.Track)
	}
	if _, ok := m.Node.DefaultRelease(); !ok {
		return fmt.Errorf("Node default major %d is not declared", m.Node.DefaultMajor)
	}
	for major := range m.Node.Releases {
		value, err := strconv.Atoi(major)
		if err != nil || value <= 0 || strconv.Itoa(value) != major {
			return fmt.Errorf("invalid Node major key %q", major)
		}
	}
	trackedReleaseTypes := map[string]bool{}
	if len(m.DotNet.TrackReleaseTypes) == 0 {
		return fmt.Errorf("at least one .NET release type must be tracked")
	}
	for _, releaseType := range m.DotNet.TrackReleaseTypes {
		if releaseType != "lts" && releaseType != "sts" {
			return fmt.Errorf("unsupported .NET release type %q", releaseType)
		}
		if trackedReleaseTypes[releaseType] {
			return fmt.Errorf("duplicate .NET release type %q", releaseType)
		}
		trackedReleaseTypes[releaseType] = true
	}
	allowedTargets := map[string]bool{
		DotNetTargetLinux:            true,
		DotNetTargetWindowsContainer: true,
		DotNetTargetQEMUWindows:      true,
	}
	targetCounts := map[string]int{}
	if len(m.DotNet.Channels) == 0 {
		return fmt.Errorf("at least one .NET channel is required")
	}
	for channel, release := range m.DotNet.Channels {
		if !validDotNetChannel(channel) {
			return fmt.Errorf("invalid .NET channel key %q", channel)
		}
		if release.ReleaseType != "" && !trackedReleaseTypes[release.ReleaseType] {
			return fmt.Errorf(".NET channel %q has untracked release type %q", channel, release.ReleaseType)
		}
		if len(release.Targets) == 0 {
			return fmt.Errorf(".NET channel %q requires at least one target", channel)
		}
		seen := map[string]bool{}
		for _, target := range release.Targets {
			if !allowedTargets[target] {
				return fmt.Errorf(".NET channel %q has unknown target %q", channel, target)
			}
			if seen[target] {
				return fmt.Errorf(".NET channel %q repeats target %q", channel, target)
			}
			seen[target] = true
			targetCounts[target]++
		}
	}
	for target := range allowedTargets {
		if targetCounts[target] == 0 {
			return fmt.Errorf("no .NET channel targets %q", target)
		}
	}
	seenDiscoveries := map[string]bool{}
	for _, discovery := range m.DotNet.UnassignedSupportedChannels {
		if !validDotNetChannel(discovery.Channel) {
			return fmt.Errorf("invalid unassigned .NET channel %q", discovery.Channel)
		}
		if seenDiscoveries[discovery.Channel] {
			return fmt.Errorf("duplicate unassigned .NET channel %q", discovery.Channel)
		}
		seenDiscoveries[discovery.Channel] = true
		if !trackedReleaseTypes[discovery.ReleaseType] {
			return fmt.Errorf("unassigned .NET channel %q has untracked release type %q", discovery.Channel, discovery.ReleaseType)
		}
		if discovery.SupportPhase != "active" && discovery.SupportPhase != "maintenance" {
			return fmt.Errorf("unassigned .NET channel %q has unsupported phase %q", discovery.Channel, discovery.SupportPhase)
		}
		if discovery.EOL == "" || discovery.LatestSDK == "" {
			return fmt.Errorf("unassigned .NET channel %q has incomplete discovery metadata", discovery.Channel)
		}
	}
	if m.Rust.Channel == "" {
		return fmt.Errorf("Rust channel is required")
	}
	if len(m.BuildTools.Lines) == 0 {
		return fmt.Errorf("at least one Build Tools release line is required")
	}
	if _, ok := m.BuildTools.DefaultRelease(); !ok {
		return fmt.Errorf("Build Tools default line %q is not declared", m.BuildTools.DefaultLine)
	}
	for line, release := range m.BuildTools.Lines {
		value, err := strconv.Atoi(line)
		if err != nil || value <= 0 || strconv.Itoa(value) != line {
			return fmt.Errorf("invalid Build Tools release line %q", line)
		}
		if release.Channel == "" || !strings.HasPrefix(release.Channel, line+"/") {
			return fmt.Errorf("Build Tools %s channel must start with %q", line, line+"/")
		}
	}
	return nil
}

func validDotNetChannel(channel string) bool {
	parts := strings.Split(channel, ".")
	if len(parts) != 2 {
		return false
	}
	for _, part := range parts {
		value, err := strconv.Atoi(part)
		if err != nil || value < 0 || strconv.Itoa(value) != part {
			return false
		}
	}
	return true
}

func digest(value, prefix string, hexLen int) error {
	if prefix != "" {
		if len(value) < len(prefix) || value[:len(prefix)] != prefix {
			return fmt.Errorf("must start with %q", prefix)
		}
		value = value[len(prefix):]
	}
	if len(value) != hexLen {
		return fmt.Errorf("must contain %d hexadecimal characters", hexLen)
	}
	if _, err := hex.DecodeString(value); err != nil {
		return fmt.Errorf("must be hexadecimal: %w", err)
	}
	return nil
}
