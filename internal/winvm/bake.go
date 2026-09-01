package winvm

import (
	"context"
	"crypto/sha256"
	"crypto/sha512"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	imageversions "github.com/GerardSmit/multirunner/images"
	"github.com/GerardSmit/multirunner/internal/vmview"
)

// DefaultRunnerVersion is the actions/runner baked into golden VMs. It comes
// from the embedded images/versions.json manifest because the value is a hard
// dependency: GitHub rejects
// runners that are too old, and a rejected runner registers, idles briefly,
// then exits without claiming a job, which reads as a scheduling fault rather
// than a version one.
var imageVersionManifest = imageversions.MustEmbedded()

var DefaultRunnerVersion = imageVersionManifest.Minimal.Runner.Version
var DefaultRunnerSHA256 = imageVersionManifest.Minimal.Runner.WindowsX64SHA256

// minGitURL is the portable Git for Windows build staged into the golden so
// actions/checkout uses real git (incremental fetch + dotgit-cache bundle) and
// `run:`/job-hook steps can run git.
var minGitVersion = imageVersionManifest.Minimal.MinGit.Version
var minGitURL = imageVersionManifest.Minimal.MinGit.URL
var minGitSHA256 = imageVersionManifest.Minimal.MinGit.SHA256

// Toolchain identities baked into the golden when requested via --tools.
var bakeGoVersion = imageVersionManifest.Go.Version
var bakeGoSHA256 = imageVersionManifest.Go.WindowsAMD64SHA256

func bakeNodeURLForVersion(version string) string {
	return fmt.Sprintf("https://nodejs.org/dist/v%s/node-v%s-win-x64.zip", version, version)
}
func bakeGoURL() string {
	return fmt.Sprintf("https://go.dev/dl/go%s.windows-amd64.zip", bakeGoVersion)
}

func bakeDotNetURL(version string) string {
	return fmt.Sprintf("https://builds.dotnet.microsoft.com/dotnet/Sdk/%s/dotnet-sdk-%s-win-x64.zip", version, version)
}

func bakeDotNetArtifactName(channel string) string {
	return "dotnet-" + strings.ReplaceAll(channel, ".", "-") + ".zip"
}

func bakeDotNetArtifacts(channels []string) []bakeArtifact {
	artifacts := make([]bakeArtifact, 0, len(channels))
	for _, channel := range channels {
		release := imageVersionManifest.DotNet.Channels[channel]
		artifacts = append(artifacts, bakeArtifact{
			Name:      bakeDotNetArtifactName(channel),
			Version:   release.Version,
			URL:       bakeDotNetURL(release.Version),
			Algorithm: "SHA512",
			Digest:    release.WindowsX64SHA512,
		})
	}
	return artifacts
}

func bakeDotNetInstallScript(channels []string) string {
	var script strings.Builder
	for _, artifact := range bakeDotNetArtifacts(channels) {
		_, _ = fmt.Fprintf(&script,
			"                FetchOrStage '%s' '%s' C:\\%s SHA512 '%s'\n"+
				"                Expand-Archive C:\\%s C:\\dotnet -Force\n"+
				"                Remove-Item C:\\%s\n",
			artifact.Name, artifact.URL, artifact.Name, artifact.Digest, artifact.Name, artifact.Name)
	}
	return strings.TrimSuffix(script.String(), "\n")
}

func bakeNodeArtifactName(major string) string {
	return "node-" + major + ".zip"
}

func bakeNodeArtifacts(majors []string) []bakeArtifact {
	artifacts := make([]bakeArtifact, 0, len(majors))
	for _, major := range majors {
		release := imageVersionManifest.Node.Releases[major]
		artifacts = append(artifacts, bakeArtifact{
			Name:      bakeNodeArtifactName(major),
			Version:   release.Version,
			URL:       bakeNodeURLForVersion(release.Version),
			Algorithm: "SHA256",
			Digest:    release.WindowsX64SHA256,
		})
	}
	return artifacts
}

func bakeNodeInstallScript(majors []string) string {
	var script strings.Builder
	for _, major := range majors {
		release := imageVersionManifest.Node.Releases[major]
		archive := bakeNodeArtifactName(major)
		temp := `C:\node-` + major
		dest := `C:\hostedtoolcache\windows\node\` + release.Version + `\x64`
		_, _ = fmt.Fprintf(&script,
			"                FetchOrStage '%s' '%s' C:\\%s SHA256 '%s'\n"+
				"                Expand-Archive C:\\%s '%s' -Force\n"+
				"                New-Item -ItemType Directory -Force '%s' | Out-Null\n"+
				"                Copy-Item '%s\\node-v%s-win-x64\\*' '%s' -Recurse -Force\n"+
				"                New-Item -ItemType File -Force '%s.complete' | Out-Null\n"+
				"                Remove-Item C:\\%s, '%s' -Recurse -Force\n",
			archive, bakeNodeURLForVersion(release.Version), archive, release.WindowsX64SHA256,
			archive, temp, dest, temp, release.Version, dest, dest, archive, temp)
	}
	selected := ""
	defaultMajor := fmt.Sprint(imageVersionManifest.Node.DefaultMajor)
	for _, major := range majors {
		selected = major
		if major == defaultMajor {
			break
		}
	}
	if selected != "" {
		release := imageVersionManifest.Node.Releases[selected]
		dest := `C:\hostedtoolcache\windows\node\` + release.Version + `\x64`
		_, _ = fmt.Fprintf(&script,
			"                [Environment]::SetEnvironmentVariable('AGENT_TOOLSDIRECTORY', 'C:\\hostedtoolcache\\windows', 'Machine')\n"+
				"                [Environment]::SetEnvironmentVariable('RUNNER_TOOL_CACHE', 'C:\\hostedtoolcache\\windows', 'Machine')\n"+
				"                Add-MachinePath '%s'\n"+
				"                Add-MachinePath '%s\\node_modules\\npm\\bin'\n"+
				"                & '%s\\corepack.cmd' enable --install-directory '%s' 2>$null\n",
			dest, dest, dest, dest)
	}
	return strings.TrimSuffix(script.String(), "\n")
}

func bakeBuildToolsArtifactName(line, suffix string) string {
	return "vs-" + line + "." + suffix
}

func bakeBuildToolsArtifacts(lines []string) []bakeArtifact {
	artifacts := make([]bakeArtifact, 0, len(lines)*2)
	for _, line := range lines {
		release := imageVersionManifest.BuildTools.Lines[line]
		artifacts = append(artifacts,
			bakeArtifact{Name: bakeBuildToolsArtifactName(line, "exe"), Version: release.Version, URL: release.BootstrapperURL, Algorithm: "SHA256", Digest: release.BootstrapperSHA256},
			bakeArtifact{Name: bakeBuildToolsArtifactName(line, "channel"), Version: release.Version, URL: release.ChannelURL, Algorithm: "SHA256", Digest: release.ChannelSHA256},
		)
	}
	return artifacts
}

func bakeBuildToolsInstallScript(lines []string) string {
	var script strings.Builder
	for _, line := range lines {
		release := imageVersionManifest.BuildTools.Lines[line]
		exe := bakeBuildToolsArtifactName(line, "exe")
		channel := bakeBuildToolsArtifactName(line, "channel")
		installPath := `C:\BuildTools\` + line
		_, _ = fmt.Fprintf(&script,
			"                FetchOrStage '%s' '%s' C:\\%s SHA256 '%s'\n"+
				"                FetchOrStage '%s' '%s' C:\\%s SHA256 '%s'\n"+
				"                $p = Start-Process -FilePath C:\\%s -Wait -PassThru -ArgumentList `\n"+
				"                    '--quiet', '--wait', '--norestart', '--nocache', '--installPath', '%s', `\n"+
				"                    '--channelUri', 'file:///C:/%s', '--installChannelUri', 'file:///C:/%s', '--noUpdateInstaller', `\n"+
				"                    '--add', 'Microsoft.VisualStudio.Workload.VCTools', `\n"+
				"                    '--add', 'Microsoft.VisualStudio.Component.VC.Tools.x86.x64', `\n"+
				"                    '--add', 'Microsoft.VisualStudio.Component.Windows11SDK.26100', `\n"+
				"                    '--add', 'Microsoft.VisualStudio.Component.VC.CMake.Project', `\n"+
				"                    '--add', 'Microsoft.Net.Component.4.8.SDK', `\n"+
				"                    '--add', 'Microsoft.Net.Component.4.8.TargetingPack', `\n"+
				"                    '--add', 'Microsoft.VisualStudio.Component.Roslyn.Compiler', '--includeRecommended'\n"+
				"                if ($p.ExitCode -ne 0 -and $p.ExitCode -ne 3010) { throw \"vs_buildtools %s failed: $($p.ExitCode)\" }\n"+
				"                Remove-Item C:\\%s, C:\\%s\n"+
				"                [Environment]::SetEnvironmentVariable('VSBUILDTOOLS_%s', '%s', 'Machine')\n",
			exe, release.BootstrapperURL, exe, release.BootstrapperSHA256,
			channel, release.ChannelURL, channel, release.ChannelSHA256,
			exe, installPath, channel, channel, line, exe, channel, line, installPath)
	}
	defaultLine := imageVersionManifest.BuildTools.DefaultLine
	selected := ""
	for _, line := range lines {
		selected = line
		if line == defaultLine {
			selected = line
			break
		}
	}
	if selected != "" {
		_, _ = fmt.Fprintf(&script,
			"                [Environment]::SetEnvironmentVariable('VSBUILDTOOLS', 'C:\\BuildTools\\%s', 'Machine')\n", selected)
	}
	return strings.TrimSuffix(script.String(), "\n")
}

type bakeToolPlan struct {
	Canonical  []string
	Kinds      []string
	Node       []string
	DotNet     []string
	BuildTools []string
}

func resolveToolPlan(tools []string) (bakeToolPlan, error) {
	canonical := map[string]bool{}
	kinds := map[string]bool{}
	node := map[string]bool{}
	dotnet := map[string]bool{}
	buildtools := map[string]bool{}
	for _, selector := range normalizeTools(tools) {
		parts := strings.Split(selector, ":")
		if len(parts) > 2 {
			return bakeToolPlan{}, fmt.Errorf("invalid tool selector %q", selector)
		}
		kind := parts[0]
		version := ""
		if len(parts) == 2 {
			version = parts[1]
			if version == "" {
				return bakeToolPlan{}, fmt.Errorf("tool selector %q has an empty version; drop the colon to use the default", selector)
			}
		}
		switch kind {
		case "node":
			majors := []string{}
			if version == "" {
				for major := range imageVersionManifest.Node.Releases {
					majors = append(majors, major)
				}
			} else {
				majors = []string{version}
			}
			for _, major := range majors {
				if _, ok := imageVersionManifest.Node.Releases[major]; !ok {
					return bakeToolPlan{}, fmt.Errorf("unsupported Node major %q", major)
				}
				node[major] = true
				canonical["node:"+major] = true
			}
			kinds[kind] = true
		case "go":
			if version != "" {
				return bakeToolPlan{}, fmt.Errorf("go does not support a version selector")
			}
			canonical[kind] = true
			kinds[kind] = true
		case "dotnet":
			if version == "" {
				// The bare selector follows the manifest's target assignment: a
				// channel carried only by the Windows container images must not be
				// baked into every golden. An assigned channel is allowed to reach
				// EOL in the manifest, so unsupported ones are skipped here rather
				// than failing every existing config on the EOL date; an exact
				// selector below still errors.
				added := 0
				for _, channel := range imageVersionManifest.DotNet.ChannelsForTarget(imageversions.DotNetTargetQEMUWindows) {
					if !bakeableDotNetChannel(imageVersionManifest.DotNet.Channels[channel]) {
						continue
					}
					dotnet[channel] = true
					canonical["dotnet:"+dotNetChannelMajor(channel)] = true
					added++
				}
				if added == 0 {
					return bakeToolPlan{}, fmt.Errorf("no supported .NET channel is assigned to the golden image; select an exact major")
				}
			} else {
				channel, ok := dotNetChannelForMajor(version)
				if !ok || !bakeableDotNetChannel(imageVersionManifest.DotNet.Channels[channel]) {
					return bakeToolPlan{}, fmt.Errorf("unsupported stable .NET major %q", version)
				}
				dotnet[channel] = true
				canonical["dotnet:"+dotNetChannelMajor(channel)] = true
			}
			kinds[kind] = true
		case "buildtools":
			line := version
			if line == "" {
				line = imageVersionManifest.BuildTools.DefaultLine
			}
			if _, ok := imageVersionManifest.BuildTools.Lines[line]; !ok {
				return bakeToolPlan{}, fmt.Errorf("unsupported Build Tools release line %q", line)
			}
			buildtools[line] = true
			canonical["buildtools:"+line] = true
			kinds[kind] = true
		default:
			return bakeToolPlan{}, fmt.Errorf("unknown tool selector %q", selector)
		}
	}
	plan := bakeToolPlan{
		Canonical:  sortedKeys(canonical),
		Kinds:      sortedKeys(kinds),
		Node:       sortedVersionKeys(node),
		DotNet:     sortedVersionKeys(dotnet),
		BuildTools: sortedVersionKeys(buildtools),
	}
	return plan, nil
}

// ValidateToolSelectors reports whether every golden tool selector resolves, so
// a typo in a pool's qemu.tools fails at config load instead of at bake time (or
// never, when the pool has no bake_iso).
func ValidateToolSelectors(tools []string) error {
	_, err := resolveToolPlan(tools)
	return err
}

// dotNetChannelMajor returns the major component of a manifest channel key
// ("8.0" -> "8").
func dotNetChannelMajor(channel string) string {
	return strings.SplitN(channel, ".", 2)[0]
}

// dotNetChannelForMajor resolves a `dotnet:<major>` selector by comparing the
// numeric major of each manifest channel key, rather than fabricating a
// "<major>.0" key that a channel such as "8.1" could never match. When a major
// carries several channels the highest minor wins.
func dotNetChannelForMajor(major string) (string, bool) {
	want, err := strconv.Atoi(major)
	if err != nil {
		return "", false
	}
	best, bestMinor := "", -1
	for channel := range imageVersionManifest.DotNet.Channels {
		got, err := strconv.Atoi(dotNetChannelMajor(channel))
		if err != nil || got != want {
			continue
		}
		minor := 0
		if parts := strings.SplitN(channel, ".", 2); len(parts) == 2 {
			if value, err := strconv.Atoi(parts[1]); err == nil {
				minor = value
			}
		}
		if minor > bestMinor {
			best, bestMinor = channel, minor
		}
	}
	return best, best != ""
}

func bakeableDotNetChannel(release imageversions.DotNetChannel) bool {
	return release.WindowsX64SHA512 != "" &&
		(release.SupportPhase == "active" || release.SupportPhase == "maintenance")
}

func sortedKeys(values map[string]bool) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func sortedVersionKeys(values map[string]bool) []string {
	keys := sortedKeys(values)
	sort.Slice(keys, func(i, j int) bool {
		left, leftErr := strconv.Atoi(strings.SplitN(keys[i], ".", 2)[0])
		right, rightErr := strconv.Atoi(strings.SplitN(keys[j], ".", 2)[0])
		if leftErr != nil || rightErr != nil {
			return keys[i] < keys[j]
		}
		return left < right
	})
	return keys
}

type bakeArtifact struct {
	Name      string
	Version   string
	URL       string
	Algorithm string
	Digest    string
}

func bakeArtifacts(runnerVersion, runnerSHA256 string, tools []string) ([]bakeArtifact, error) {
	plan, err := resolveToolPlan(tools)
	if err != nil {
		return nil, err
	}
	return bakeArtifactsForPlan(runnerVersion, runnerSHA256, plan)
}

func bakeArtifactsForPlan(runnerVersion, runnerSHA256 string, plan bakeToolPlan) ([]bakeArtifact, error) {
	if runnerVersion == "" {
		runnerVersion = DefaultRunnerVersion
	}
	if runnerSHA256 == "" {
		if runnerVersion != DefaultRunnerVersion {
			return nil, fmt.Errorf("runner SHA256 is required for custom version %s", runnerVersion)
		}
		runnerSHA256 = DefaultRunnerSHA256
	}
	if err := validateSHA256(runnerSHA256); err != nil {
		return nil, fmt.Errorf("runner SHA256: %w", err)
	}
	artifacts := []bakeArtifact{
		{
			Name:    "runner.zip",
			Version: runnerVersion,
			URL: fmt.Sprintf("https://github.com/actions/runner/releases/download/v%s/actions-runner-win-x64-%s.zip",
				runnerVersion, runnerVersion),
			Algorithm: "SHA256",
			Digest:    runnerSHA256,
		},
		{Name: "mingit.zip", Version: minGitVersion, URL: minGitURL, Algorithm: "SHA256", Digest: minGitSHA256},
	}
	for _, tool := range plan.Kinds {
		switch tool {
		case "node":
			artifacts = append(artifacts, bakeNodeArtifacts(plan.Node)...)
		case "go":
			artifacts = append(artifacts, bakeArtifact{
				Name: "go.zip", Version: bakeGoVersion, URL: bakeGoURL(), Algorithm: "SHA256", Digest: bakeGoSHA256,
			})
		case "dotnet":
			artifacts = append(artifacts, bakeDotNetArtifacts(plan.DotNet)...)
		case "buildtools":
			artifacts = append(artifacts, bakeBuildToolsArtifacts(plan.BuildTools)...)
		}
	}
	return artifacts, nil
}

// ToolsHash fingerprints every input that changes the provisioned golden image.
// It is non-empty even without optional tools so template and runner updates
// rebuild existing minimal goldens.
func ToolsHash(o BakeOptions) (string, error) {
	isoDigest := "none"
	if o.WindowsISO != "" {
		if o.WindowsISOSHA256 != "" {
			if err := validateSHA256(o.WindowsISOSHA256); err != nil {
				return "", fmt.Errorf("Windows ISO SHA256: %w", err)
			}
		}
		var err error
		isoDigest, err = fileDigest(o.WindowsISO, "SHA256")
		if err != nil {
			return "", fmt.Errorf("hash Windows ISO: %w", err)
		}
		if o.WindowsISOSHA256 != "" && !strings.EqualFold(isoDigest, o.WindowsISOSHA256) {
			return "", fmt.Errorf("Windows ISO SHA256 mismatch: got %s, want %s", isoDigest, o.WindowsISOSHA256)
		}
	}
	plan, err := resolveToolPlan(o.Tools)
	if err != nil {
		return "", err
	}
	runnerVersion := o.RunnerVersion
	if runnerVersion == "" {
		runnerVersion = DefaultRunnerVersion
	}
	artifacts, err := bakeArtifactsForPlan(runnerVersion, o.RunnerSHA256, plan)
	if err != nil {
		return "", err
	}
	files, err := AutounattendFiles(runnerVersion, o.RunnerSHA256, toolsHashAdminPassword, o.Tools)
	if err != nil {
		return "", err
	}
	return toolsHashResolved(plan.Canonical, isoDigest, artifacts, files), nil
}

// toolsHashAdminPassword stands in for the real administrator password while
// fingerprinting, so rotating the password does not force a golden rebuild.
const toolsHashAdminPassword = "__GOLDEN_FINGERPRINT_PASSWORD__"

// toolsHashResolved hashes the *rendered* provisioning files rather than the raw
// templates: manifest policy such as node.default_major or
// buildtools.default_line changes what the guest installs and puts on PATH
// without changing any template byte or artifact identity.
func toolsHashResolved(tools []string, isoDigest string, artifacts []bakeArtifact, files map[string]string) string {
	norm := normalizeTools(tools)
	h := sha256.New()
	_, _ = io.WriteString(h, "golden-inputs:v4\n")
	_, _ = io.WriteString(h, "windows-iso:sha256="+isoDigest+"\n")
	_, _ = io.WriteString(h, "tools="+strings.Join(norm, ",")+"\n")
	for _, artifact := range artifacts {
		_, _ = fmt.Fprintf(h, "artifact=%s|%s|%s|%s|%s\n",
			artifact.Name, artifact.Version, artifact.URL, artifact.Algorithm, strings.ToLower(artifact.Digest))
	}
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		_, _ = io.WriteString(h, name+"\x00")
		_, _ = io.WriteString(h, files[name])
	}
	return "golden:" + hex.EncodeToString(h.Sum(nil)[:8])
}

func validateSHA256(digest string) error {
	if len(digest) != sha256.Size*2 {
		return fmt.Errorf("must contain %d hexadecimal characters", sha256.Size*2)
	}
	if _, err := hex.DecodeString(digest); err != nil {
		return fmt.Errorf("must be hexadecimal: %w", err)
	}
	return nil
}

func fileDigest(path, algorithm string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	var h interface {
		io.Writer
		Sum([]byte) []byte
	}
	switch strings.ToUpper(algorithm) {
	case "SHA256":
		h = sha256.New()
	case "SHA512":
		h = sha512.New()
	default:
		return "", fmt.Errorf("unsupported digest algorithm %q", algorithm)
	}
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// normalizeTools lowercases, de-dups, and sorts a tool list for stable hashing
// and substitution.
func normalizeTools(tools []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, t := range tools {
		t = strings.ToLower(strings.TrimSpace(t))
		if t == "" || seen[t] {
			continue
		}
		seen[t] = true
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

//go:embed templates/autounattend.xml templates/install-golden.ps1 templates/startup.ps1 templates/setupcomplete.cmd templates/githook.ps1
var templatesFS embed.FS

// BakeOptions configures building (or rebuilding) a golden Windows image.
type BakeOptions struct {
	WindowsISO       string // path to a Windows Server ISO (Server Core selected by the answer file)
	WindowsISOSHA256 string // optional expected SHA256; the computed digest always enters the golden fingerprint
	Golden           string // output golden qcow2
	DiskGB           int
	MemMB            int
	CPUs             int
	Accel            string // "" = auto
	RunnerVersion    string
	RunnerSHA256     string   // required when RunnerVersion is not DefaultRunnerVersion
	Tools            []string // golden selectors: dotnet[:major] | node[:major] | go | buildtools[:line]
	AdminPassword    string
	EvalDays         int  // 180 (server) / 90 (client)
	MaxRearms        int  // ~5
	Licensed         bool // a real key/KMS is configured -> skip eval housekeeping
	WorkflowsHash    string
	QEMUBin          string
	ImgBin           string
	OVMFCode         string        // UEFI firmware code (auto-detected if empty)
	OVMFVarsTemplate string        // UEFI vars template to copy
	VNCWeb           string        // if set (host:port), serve a noVNC viewer to watch the install
	Timeout          time.Duration // max install wall-clock before the bake kills a hung guest (default: derived from Tools)
}

const (
	// bakeBaseTimeout covers Windows Setup plus the runner and MinGit staging.
	bakeBaseTimeout = 45 * time.Minute
	// bakeBuildToolsTimeout is charged per requested Build Tools line: each one is
	// an independent multi-GB Visual Studio install inside the guest, fetched over
	// user-mode networking, so combined lines add up instead of sharing a budget.
	bakeBuildToolsTimeout = 90 * time.Minute
	// bakeToolchainTimeout is the floor once any .NET/Node/Go payload is selected.
	bakeToolchainTimeout = 75 * time.Minute
)

// bakeTimeoutForPlan derives the install deadline from the resolved tool plan.
func bakeTimeoutForPlan(plan bakeToolPlan) time.Duration {
	timeout := bakeBaseTimeout + time.Duration(len(plan.BuildTools))*bakeBuildToolsTimeout
	for _, kind := range plan.Kinds {
		switch kind {
		case "dotnet", "node", "go":
			if timeout < bakeToolchainTimeout {
				timeout = bakeToolchainTimeout
			}
		}
	}
	return timeout
}

func (o *BakeOptions) defaults() {
	if o.DiskGB <= 0 {
		o.DiskGB = 40
	}
	if o.MemMB <= 0 {
		o.MemMB = 4096
	}
	if o.CPUs <= 0 {
		o.CPUs = 2
	}
	if o.RunnerVersion == "" {
		o.RunnerVersion = DefaultRunnerVersion
	}
	if o.AdminPassword == "" {
		o.AdminPassword = "Multirunner!1"
	}
	if o.EvalDays <= 0 {
		o.EvalDays = 180
	}
	if o.MaxRearms <= 0 {
		o.MaxRearms = 5
	}
	if o.Timeout <= 0 {
		o.Timeout = bakeBaseTimeout
		// A selector list that does not resolve fails in Prepare; until then the
		// bare base budget is enough.
		if plan, err := resolveToolPlan(o.Tools); err == nil {
			o.Timeout = bakeTimeoutForPlan(plan)
		}
	}
	if o.Accel == "" {
		o.Accel = DetectAccel(runtime.GOOS, runtime.GOARCH)
	}
	if o.QEMUBin == "" {
		o.QEMUBin = "qemu-system-x86_64"
	}
	if o.ImgBin == "" {
		o.ImgBin = "qemu-img"
	}
	if o.OVMFCode == "" {
		o.OVMFCode, o.OVMFVarsTemplate = DetectOVMF(o.QEMUBin)
	}
}

// GoldenVarsPath is the UEFI NVRAM file paired with a golden image.
func GoldenVarsPath(golden string) string { return golden + ".vars.fd" }

// GoldenSerialPath is the bake's COM1 capture file. install-golden.ps1 writes
// progress markers here (MR:...), ending with MR:GOLDEN_OK on success, so the
// host can verify the bake finished without mounting the guest disk.
func GoldenSerialPath(golden string) string { return golden + ".serial.log" }

// goldenOKMarker is the sentinel install-golden.ps1 writes to COM1 right before
// the final power-off. Its absence means the bake did not provision the runner.
const goldenOKMarker = "MR:GOLDEN_OK"

func copyFile(dst, src string) error {
	b, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, b, 0o644)
}

// AutounattendFiles returns the answer file + provisioning scripts with bake
// substitutions applied (for the autounattend ISO).
func AutounattendFiles(runnerVersion, runnerSHA256, adminPassword string, tools []string) (map[string]string, error) {
	plan, err := resolveToolPlan(tools)
	if err != nil {
		return nil, err
	}
	read := func(name string) (string, error) {
		b, err := templatesFS.ReadFile("templates/" + name)
		return string(b), err
	}
	unattend, err := read("autounattend.xml")
	if err != nil {
		return nil, err
	}
	install, err := read("install-golden.ps1")
	if err != nil {
		return nil, err
	}
	startup, err := read("startup.ps1")
	if err != nil {
		return nil, err
	}
	setupComplete, err := read("setupcomplete.cmd")
	if err != nil {
		return nil, err
	}
	gitHook, err := read("githook.ps1")
	if err != nil {
		return nil, err
	}
	unattend = strings.ReplaceAll(unattend, "__ADMIN_PASSWORD__", adminPassword)
	install = strings.ReplaceAll(install, "__RUNNER_VERSION__", runnerVersion)
	if runnerSHA256 == "" && runnerVersion == DefaultRunnerVersion {
		runnerSHA256 = DefaultRunnerSHA256
	}
	install = strings.ReplaceAll(install, "__RUNNER_SHA256__", runnerSHA256)
	install = strings.ReplaceAll(install, "__TOOLS__", strings.Join(plan.Kinds, ","))
	install = strings.ReplaceAll(install, "__NODE_INSTALLS__", bakeNodeInstallScript(plan.Node))
	install = strings.ReplaceAll(install, "__GO_URL__", bakeGoURL())
	install = strings.ReplaceAll(install, "__GO_SHA256__", bakeGoSHA256)
	install = strings.ReplaceAll(install, "__MINGIT_URL__", minGitURL)
	install = strings.ReplaceAll(install, "__MINGIT_SHA256__", minGitSHA256)
	install = strings.ReplaceAll(install, "__DOTNET_INSTALLS__", bakeDotNetInstallScript(plan.DotNet))
	install = strings.ReplaceAll(install, "__BUILDTOOLS_INSTALLS__", bakeBuildToolsInstallScript(plan.BuildTools))
	return map[string]string{
		"autounattend.xml":   unattend,
		"setupcomplete.cmd":  setupComplete,
		"githook.ps1":        gitHook,
		"install-golden.ps1": install,
		"startup.ps1":        startup,
	}, nil
}

// bakeQEMUArgs builds the installer-boot args: base disk + Windows ISO + the
// autounattend ISO, booting via UEFI (no "press any key" prompt). The guest
// installs Windows, runs install-golden.ps1, then powers off -> QEMU exits.
func bakeQEMUArgs(o BakeOptions, autounattendISO, varsFD string) []string {
	accel := o.Accel
	if accel == "" {
		accel = "tcg"
	}
	args := []string{
		"-machine", "q35",
		"-accel", accel,
		"-cpu", cpuArg(accel),
		"-m", strconv.Itoa(o.MemMB),
		"-smp", strconv.Itoa(o.CPUs),
	}
	if o.OVMFCode != "" {
		args = append(args,
			"-drive", "if=pflash,format=raw,unit=0,readonly=on,file="+o.OVMFCode,
			"-drive", "if=pflash,format=raw,unit=1,file="+varsFD)
	}
	args = append(args,
		"-drive", fmt.Sprintf("file=%s,if=none,id=osdisk,format=qcow2", o.Golden),
		"-device", "ahci,id=ahci",
		"-device", "ide-hd,drive=osdisk,bus=ahci.0",
		// Install CD carries a block-backend id so it can be ejected on the first
		// reset (bare media=cdrom, unchanged topology — an explicit AHCI device
		// crashes the first boot under WHPX). The autounattend CD stays for later.
		"-drive", fmt.Sprintf("file=%s,media=cdrom,id=%s", o.WindowsISO, bakeInstallCDDev),
		"-drive", fmt.Sprintf("file=%s,media=cdrom", autounattendISO),
		"-netdev", "user,id=n0",
		"-device", "e1000,netdev=n0",
		// No -boot d: forcing the DVD first means every Setup reboot re-enters the
		// CD bootloader, which triple-faults under WHPX ("Unexpected VP exit code
		// 4"). With default order, the empty HDD is skipped on first boot (so the
		// DVD installer still runs) but once Windows is applied the HDD is
		// bootable and reboots go straight to Windows Boot Manager.
		// NO -no-reboot: Windows Setup reboots several times mid-install; the
		// final Stop-Computer power-off is what exits QEMU.
		"-qmp", "tcp:"+bakeQMPAddr+",server,nowait",
		// COM1 -> file: install-golden.ps1 writes progress + the GOLDEN_OK
		// sentinel here so the host can verify the bake actually provisioned.
		"-serial", "file:"+GoldenSerialPath(o.Golden),
	)
	if o.VNCWeb != "" {
		args = append(args, "-vnc", fmt.Sprintf("0.0.0.0:%d,websocket=%d", bakeVNCDisplay, bakeVNCWSPort))
	} else {
		args = append(args, "-display", "none")
	}
	return args
}

const (
	// bakeQMPAddr is where the bake's QEMU exposes QMP (to dismiss the "press any
	// key to boot from CD" prompt via keypresses).
	bakeQMPAddr = "127.0.0.1:4455"
	// VNC display + websocket port for the optional noVNC viewer.
	bakeVNCDisplay = 1
	bakeVNCWSPort  = 5701
	// bakeInstallCDDev is the block-backend id of the Windows install CD, ejected
	// on the first guest reset so reboots boot the HDD instead of re-entering the
	// DVD UEFI loader (which triple-faults under WHPX).
	bakeInstallCDDev = "instcd"
)

// cpuArg picks the QEMU -cpu model for an accelerator. The bake and runtime must
// agree so the golden boots on the vCPU it was installed on.
//
// WHPX (Windows) gets Hyper-V enlightenments on a conservative qemu64 base:
// Windows then drives a paravirtual timer/APIC/spinlock interface instead of the
// hardware WHPX emulates poorly — without them the guest hangs at the
// specialize/OOBE boot. ("host" passthrough is NOT usable under WHPX here: it
// exposes APX, which makes OVMF #GP-fault in PlatformPei.) KVM/HVF use host
// passthrough; TCG uses the richest emulated model.
func cpuArg(accel string) string {
	switch accel {
	case "whpx":
		return "qemu64,hv-relaxed,hv-vapic,hv-spinlocks=0x1fff,hv-time,hv-synic,hv-stimer,hv-reset"
	case "tcg", "":
		return "max"
	default: // kvm, hvf
		return "host"
	}
}

// stageBakeBinaries downloads immutable, digest-pinned provisioning artifacts
// on the host into a temp dir and returns ISO file refs plus a cleanup func.
// Best-effort: a file that fails to download is simply omitted, and the guest
// falls back to downloading and verifying it itself.
func stageBakeBinaries(ctx context.Context, runnerVersion, runnerSHA256 string, tools []string) (map[string]string, func()) {
	artifacts, err := bakeArtifacts(runnerVersion, runnerSHA256, tools)
	if err != nil {
		return nil, func() {}
	}
	return stageArtifacts(ctx, artifacts)
}

func stageArtifacts(ctx context.Context, artifacts []bakeArtifact) (map[string]string, func()) {
	dir, err := os.MkdirTemp("", "mr-bakestage")
	if err != nil {
		return nil, func() {}
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	refs := map[string]string{}
	for _, artifact := range artifacts {
		dst := filepath.Join(dir, artifact.Name)
		if err := downloadFile(ctx, artifact.URL, dst); err != nil {
			continue
		}
		got, err := fileDigest(dst, artifact.Algorithm)
		if err != nil || !strings.EqualFold(got, artifact.Digest) {
			_ = os.Remove(dst)
			continue
		}
		refs[artifact.Name] = dst
	}
	return refs, cleanup
}

// downloadFile fetches url to dst.
func downloadFile(ctx context.Context, url, dst string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download %s: status %d", url, resp.StatusCode)
	}
	f, err := os.Create(dst)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(f, resp.Body)
	closeErr := f.Close()
	if err := errors.Join(copyErr, closeErr); err != nil {
		_ = os.Remove(dst)
		return err
	}
	return nil
}

// Prepare creates the base disk + autounattend ISO for a bake and returns the
// autounattend ISO path and the QEMU install args (without running QEMU).
func Prepare(ctx context.Context, o *BakeOptions) (autoISO string, args []string, err error) {
	o.defaults()
	if o.WindowsISO == "" || o.Golden == "" {
		return "", nil, fmt.Errorf("bake: WindowsISO and Golden are required")
	}
	if _, err := os.Stat(o.WindowsISO); err != nil {
		return "", nil, fmt.Errorf("bake: windows iso: %w", err)
	}
	hash, err := ToolsHash(*o)
	if err != nil {
		return "", nil, fmt.Errorf("bake inputs: %w", err)
	}
	o.WorkflowsHash = hash
	if out, err := exec.CommandContext(ctx, o.ImgBin, "create", "-f", "qcow2", o.Golden, fmt.Sprintf("%dG", o.DiskGB)).CombinedOutput(); err != nil {
		return "", nil, fmt.Errorf("create golden disk: %w: %s", err, out)
	}
	_ = os.Remove(GoldenSerialPath(o.Golden)) // stale markers must not satisfy the bake check
	files, err := AutounattendFiles(o.RunnerVersion, o.RunnerSHA256, o.AdminPassword, o.Tools)
	if err != nil {
		return "", nil, err
	}
	autoISO = o.Golden + ".autounattend.iso"
	// Stage the runner and selected tool payloads onto the ISO so the guest reads
	// them from the virtual CD. The VM's user-mode network is slow for large
	// downloads, so the guest downloads directly only when a verified staged file
	// is unavailable.
	refs, cleanup := stageBakeBinaries(ctx, o.RunnerVersion, o.RunnerSHA256, o.Tools)
	defer cleanup()
	if err := BuildISOFiles(autoISO, "AUTOUNATTEND", files, refs); err != nil {
		return "", nil, err
	}

	// UEFI: create the golden's writable NVRAM from the vars template.
	varsFD := GoldenVarsPath(o.Golden)
	if o.OVMFCode != "" {
		if o.OVMFVarsTemplate == "" {
			return "", nil, fmt.Errorf("bake: UEFI code found but no vars template; set OVMFVarsTemplate")
		}
		if err := copyFile(varsFD, o.OVMFVarsTemplate); err != nil {
			return "", nil, fmt.Errorf("bake: create nvram: %w", err)
		}
	}
	return autoISO, bakeQEMUArgs(*o, autoISO, varsFD), nil
}

// Bake builds the golden image: create base disk, boot the unattended installer,
// wait for it to finish (guest powers off), and write the metadata sidecar.
func Bake(ctx context.Context, o BakeOptions, now time.Time) error {
	autoISO, args, err := Prepare(ctx, &o)
	if err != nil {
		return err
	}
	defer os.Remove(autoISO)

	cmd := exec.CommandContext(ctx, o.QEMUBin, args...)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start qemu: %w", err)
	}

	// Optional live viewer (noVNC) so the operator can watch the install.
	if o.VNCWeb != "" {
		viewCtx, cancelView := context.WithCancel(ctx)
		defer cancelView()
		logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
		fmt.Printf("\n  ┌─────────────────────────────────────────────────────────┐\n")
		fmt.Printf("  │  Watch the install live:  http://%-24s │\n", o.VNCWeb)
		fmt.Printf("  └─────────────────────────────────────────────────────────┘\n\n")
		go func() { _ = vmview.Serve(viewCtx, o.VNCWeb, bakeVNCWSPort, logger) }()
	}

	// Dismiss the "Press any key to boot from CD" prompt on first boot. QMP
	// keypresses are host-OS-agnostic (same path on Windows and Linux), so the
	// bake is exercised identically wherever it runs. (A no-prompt ISO is not an
	// option: Windows install media is UDF-primary with only a stub ISO9660 tree,
	// so the EFI boot files can't be located/replaced without a UDF reader.)
	go func() {
		time.Sleep(4 * time.Second)
		// First boot: dismiss "press any key to boot from CD" (QMPBootKeys closes
		// its QMP connection on return, freeing the single QMP socket).
		_ = QMPBootKeys(bakeQMPAddr, 20, time.Second)
		// Then eject the install CD on Setup's first reboot so it lands on the
		// HDD, avoiding the WHPX DVD-loader triple-fault.
		_ = QMPEjectOnReset(bakeQMPAddr, bakeInstallCDDev, 20*time.Minute)
	}()
	// Watchdog: WHPX can wedge the guest vCPU (frozen, no power-off) instead of
	// crashing — without a deadline the bake would wait forever. Kill QEMU if the
	// install hasn't powered off within the timeout.
	waitErr := make(chan error, 1)
	go func() { waitErr <- cmd.Wait() }()
	select {
	case err := <-waitErr:
		if err != nil {
			return fmt.Errorf("golden install run: %w", err)
		}
	case <-time.After(o.Timeout):
		_ = cmd.Process.Kill()
		<-waitErr
		serial, _ := os.ReadFile(GoldenSerialPath(o.Golden))
		return fmt.Errorf("golden bake timed out after %s (guest hung); serial tail:\n%s",
			o.Timeout, serialTail(serial, 2000))
	}

	// Verify the guest actually provisioned the runner. The bake's QEMU exits on
	// any guest power-off, including ones that happen before install-golden runs
	// (e.g. an OOBE hiccup) — without this check a half-baked golden ships
	// silently. install-golden.ps1 writes MR:GOLDEN_OK to COM1 as its last step.
	serial, _ := os.ReadFile(GoldenSerialPath(o.Golden))
	if !strings.Contains(string(serial), goldenOKMarker) {
		return fmt.Errorf("golden bake did not complete: %q not found in serial log %s\n--- serial tail ---\n%s",
			goldenOKMarker, GoldenSerialPath(o.Golden), serialTail(serial, 2000))
	}

	meta := GoldenMeta{
		CreatedAt: now, EvalDays: o.EvalDays, MaxRearms: o.MaxRearms,
		Licensed: o.Licensed, WorkflowsHash: o.WorkflowsHash,
	}
	return SaveMeta(o.Golden, meta)
}

// serialTail returns the last n bytes of b as a string (for error context).
func serialTail(b []byte, n int) string {
	if len(b) > n {
		b = b[len(b)-n:]
	}
	return string(b)
}
