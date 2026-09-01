// Command update-image-versions refreshes the canonical runner-image dependency manifest.
package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"time"

	imageversions "github.com/GerardSmit/multirunner/images"
)

const defaultManifestPath = "images/versions.json"

func main() {
	manifestPath := flag.String("manifest", defaultManifestPath, "path to the image dependency manifest")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := run(ctx, *manifestPath); err != nil {
		fmt.Fprintln(os.Stderr, "update image versions:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, manifestPath string) error {
	m, err := imageversions.ReadForUpdate(manifestPath)
	if err != nil {
		return fmt.Errorf("read %s: %w", manifestPath, err)
	}
	u := updater{
		client: &http.Client{Timeout: 5 * time.Minute},
		today:  time.Now().UTC().Format(time.DateOnly),
		token:  os.Getenv("GH_TOKEN"),
	}
	if err := u.refresh(ctx, &m); err != nil {
		return err
	}
	if err := m.Validate(); err != nil {
		return fmt.Errorf("validate refreshed manifest: %w", err)
	}

	manifestJSON, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("encode manifest: %w", err)
	}
	manifestJSON = append(manifestJSON, '\n')

	linuxDockerfile := filepath.Join(filepath.Dir(manifestPath), "linux", "Dockerfile")
	windowsDockerfile := filepath.Join(filepath.Dir(manifestPath), "windows", "Dockerfile")
	updates := []fileUpdate{{path: manifestPath, data: manifestJSON}}
	for path, base := range map[string]string{
		linuxDockerfile:   m.Minimal.LinuxBase.Reference(),
		windowsDockerfile: m.Minimal.WindowsBase.Reference(),
	} {
		data, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read %s: %w", path, err)
		}
		data, err = replaceBaseArg(data, base)
		if err != nil {
			return fmt.Errorf("update %s: %w", path, err)
		}
		updates = append(updates, fileUpdate{path: path, data: data})
	}
	for _, update := range updates {
		if err := writeIfChanged(update.path, update.data); err != nil {
			return err
		}
	}

	defaultNode, _ := m.Node.DefaultRelease()
	fmt.Printf("Runner image versions refreshed (runner %s, Node %s, Go %s, Rust %s)\n",
		m.Minimal.Runner.Version, defaultNode.Version, m.Go.Version, m.Rust.Version)
	for _, discovery := range m.DotNet.UnassignedSupportedChannels {
		fmt.Printf("New supported .NET channel needs image targets: %s (%s, SDK %s, EOL %s)\n",
			discovery.Channel, discovery.ReleaseType, discovery.LatestSDK, discovery.EOL)
	}
	return nil
}

type fileUpdate struct {
	path string
	data []byte
}

type updater struct {
	client *http.Client
	today  string
	token  string
}

func (u updater) refresh(ctx context.Context, m *imageversions.Manifest) error {
	if err := u.refreshBases(ctx, m); err != nil {
		return err
	}
	if err := u.refreshRunner(ctx, m); err != nil {
		return err
	}
	if err := u.refreshWindowsMinimal(ctx, m); err != nil {
		return err
	}
	if err := u.refreshNode(ctx, m); err != nil {
		return err
	}
	if err := u.refreshDotNet(ctx, m); err != nil {
		return err
	}
	if err := u.refreshGo(ctx, m); err != nil {
		return err
	}
	if err := u.refreshRust(ctx, m); err != nil {
		return err
	}
	return u.refreshBuildTools(ctx, m)
}

func (u updater) refreshBases(ctx context.Context, m *imageversions.Manifest) error {
	for name, base := range map[string]*imageversions.BaseImage{
		"Linux":   &m.Minimal.LinuxBase,
		"Windows": &m.Minimal.WindowsBase,
	} {
		digest, err := u.registryDigest(ctx, *base)
		if err != nil {
			return fmt.Errorf("%s base image: %w", name, err)
		}
		base.Digest = digest
	}
	return nil
}

func (u updater) refreshRunner(ctx context.Context, m *imageversions.Manifest) error {
	release, err := u.githubRelease(ctx, "actions/runner")
	if err != nil {
		return fmt.Errorf("actions/runner: %w", err)
	}
	version := strings.TrimPrefix(release.TagName, "v")
	linuxX64, err := release.digest("actions-runner-linux-x64-" + version + ".tar.gz")
	if err != nil {
		return err
	}
	linuxARM64, err := release.digest("actions-runner-linux-arm64-" + version + ".tar.gz")
	if err != nil {
		return err
	}
	windowsX64, err := release.digest("actions-runner-win-x64-" + version + ".zip")
	if err != nil {
		return err
	}
	m.Minimal.Runner = imageversions.Runner{
		Version:          version,
		LinuxX64SHA256:   linuxX64,
		LinuxARM64SHA256: linuxARM64,
		WindowsX64SHA256: windowsX64,
	}
	return nil
}

func (u updater) refreshWindowsMinimal(ctx context.Context, m *imageversions.Manifest) error {
	pwsh, err := u.githubRelease(ctx, "PowerShell/PowerShell")
	if err != nil {
		return fmt.Errorf("PowerShell: %w", err)
	}
	pwshVersion := strings.TrimPrefix(pwsh.TagName, "v")
	pwshDigest, err := pwsh.digest("PowerShell-" + pwshVersion + "-win-x64.zip")
	if err != nil {
		return fmt.Errorf("PowerShell: %w", err)
	}
	m.Minimal.PowerShell = imageversions.PowerShell{Version: pwshVersion, WindowsX64SHA256: pwshDigest}

	git, err := u.githubRelease(ctx, "git-for-windows/git")
	if err != nil {
		return fmt.Errorf("Git for Windows: %w", err)
	}
	var asset githubAsset
	for _, candidate := range git.Assets {
		if strings.HasPrefix(candidate.Name, "MinGit-") && strings.HasSuffix(candidate.Name, "-64-bit.zip") && !strings.Contains(candidate.Name, "busybox") {
			if asset.Name != "" {
				return errors.New("Git for Windows release has multiple matching MinGit x64 assets")
			}
			asset = candidate
		}
	}
	if asset.Name == "" {
		return errors.New("Git for Windows release has no MinGit x64 asset")
	}
	digest, err := asset.sha256()
	if err != nil {
		return fmt.Errorf("MinGit: %w", err)
	}
	m.Minimal.MinGit = imageversions.MinGit{
		Version: strings.TrimPrefix(git.TagName, "v"),
		URL:     asset.BrowserDownloadURL,
		SHA256:  digest,
	}
	return nil
}

func (u updater) refreshNode(ctx context.Context, m *imageversions.Manifest) error {
	var index []struct {
		Version string `json:"version"`
	}
	if err := u.getJSON(ctx, "https://nodejs.org/dist/index.json", &index); err != nil {
		return fmt.Errorf("Node release index: %w", err)
	}
	var schedule map[string]nodeLifecycle
	if err := u.getJSON(ctx, "https://raw.githubusercontent.com/nodejs/Release/main/schedule.json", &schedule); err != nil {
		return fmt.Errorf("Node release schedule: %w", err)
	}

	if err := discoverNodeLTS(&m.Node, schedule, u.today); err != nil {
		return err
	}

	releases := make(map[string]imageversions.NodeRelease, len(m.Node.Releases))
	for key := range m.Node.Releases {
		eol := schedule["v"+key].End
		if err := u.supported("Node "+key, eol); err != nil {
			return err
		}
		prefix := "v" + key + "."
		version := ""
		for _, candidate := range index {
			if strings.HasPrefix(candidate.Version, prefix) {
				version = strings.TrimPrefix(candidate.Version, "v")
				break
			}
		}
		if version == "" {
			return fmt.Errorf("Node %s has no release", key)
		}
		body, _, err := u.download(ctx, "https://nodejs.org/dist/v"+version+"/SHASUMS256.txt")
		if err != nil {
			return fmt.Errorf("Node %s checksums: %w", version, err)
		}
		checksums := checksumManifest(body)
		release := imageversions.NodeRelease{
			Version:          version,
			EOL:              eol,
			LinuxX64SHA256:   checksums["node-v"+version+"-linux-x64.tar.xz"],
			LinuxARM64SHA256: checksums["node-v"+version+"-linux-arm64.tar.xz"],
			WindowsX64SHA256: checksums["node-v"+version+"-win-x64.zip"],
		}
		for name, value := range map[string]string{"linux x64": release.LinuxX64SHA256, "linux arm64": release.LinuxARM64SHA256, "windows x64": release.WindowsX64SHA256} {
			if err := validHex(value, 64); err != nil {
				return fmt.Errorf("Node %s %s checksum: %w", version, name, err)
			}
		}
		releases[key] = release
	}
	m.Node.Releases = releases
	if _, ok := m.Node.DefaultRelease(); !ok {
		return fmt.Errorf("Node default major %d is not supported", m.Node.DefaultMajor)
	}
	return nil
}

type nodeLifecycle struct {
	LTS string `json:"lts"`
	End string `json:"end"`
}

func discoverNodeLTS(node *imageversions.Node, schedule map[string]nodeLifecycle, today string) error {
	if node.Track != "active-lts" {
		return fmt.Errorf("unsupported Node tracking policy %q", node.Track)
	}
	for version, lifecycle := range schedule {
		major := strings.TrimPrefix(version, "v")
		if lifecycle.LTS != "" && lifecycle.LTS <= today && lifecycle.End >= today {
			if _, declared := node.Releases[major]; !declared {
				node.Releases[major] = imageversions.NodeRelease{}
			}
		}
	}
	return nil
}

func (u updater) refreshDotNet(ctx context.Context, m *imageversions.Manifest) error {
	var index struct {
		Channels []dotNetIndexChannel `json:"releases-index"`
	}
	if err := u.getJSON(ctx, "https://dotnetcli.blob.core.windows.net/dotnet/release-metadata/releases-index.json", &index); err != nil {
		return fmt.Errorf(".NET release index: %w", err)
	}
	metadata := make(map[string]struct {
		ReleaseType, EOL, SupportPhase, ReleasesURL, LatestSDK string
	})
	for _, channel := range index.Channels {
		metadata[channel.Channel] = struct{ ReleaseType, EOL, SupportPhase, ReleasesURL, LatestSDK string }{
			channel.ReleaseType, channel.EOL, channel.SupportPhase, channel.ReleasesURL, channel.LatestSDK,
		}
	}
	discoverDotNetChannels(&m.DotNet, index.Channels, u.today)

	channels := make(map[string]imageversions.DotNetChannel, len(m.DotNet.Channels))
	for channel := range m.DotNet.Channels {
		meta, ok := metadata[channel]
		if !ok {
			return fmt.Errorf(".NET channel %s is absent from the release index", channel)
		}
		if err := u.supported(".NET "+channel, meta.EOL); err != nil {
			return err
		}
		var releases struct {
			LatestSDK string `json:"latest-sdk"`
			Releases  []struct {
				SDK struct {
					Version string `json:"version"`
					Files   []struct {
						RID  string `json:"rid"`
						URL  string `json:"url"`
						Hash string `json:"hash"`
					} `json:"files"`
				} `json:"sdk"`
			} `json:"releases"`
		}
		if err := u.getJSON(ctx, meta.ReleasesURL, &releases); err != nil {
			return fmt.Errorf(".NET %s releases: %w", channel, err)
		}
		resolved := imageversions.DotNetChannel{
			Targets:      append([]string(nil), m.DotNet.Channels[channel].Targets...),
			ReleaseType:  meta.ReleaseType,
			Version:      releases.LatestSDK,
			EOL:          meta.EOL,
			SupportPhase: meta.SupportPhase,
		}
		for _, release := range releases.Releases {
			if release.SDK.Version != releases.LatestSDK {
				continue
			}
			for _, file := range release.SDK.Files {
				switch {
				case file.RID == "linux-x64" && strings.HasSuffix(file.URL, ".tar.gz"):
					resolved.LinuxX64SHA512 = strings.ToLower(file.Hash)
				case file.RID == "linux-arm64" && strings.HasSuffix(file.URL, ".tar.gz"):
					resolved.LinuxARM64SHA512 = strings.ToLower(file.Hash)
				case file.RID == "win-x64" && strings.HasSuffix(file.URL, ".zip"):
					resolved.WindowsX64SHA512 = strings.ToLower(file.Hash)
				}
			}
		}
		for name, value := range map[string]string{"linux x64": resolved.LinuxX64SHA512, "linux arm64": resolved.LinuxARM64SHA512, "windows x64": resolved.WindowsX64SHA512} {
			if err := validHex(value, 128); err != nil {
				return fmt.Errorf(".NET %s %s checksum: %w", channel, name, err)
			}
		}
		channels[channel] = resolved
	}
	m.DotNet.Channels = channels
	return nil
}

type dotNetIndexChannel struct {
	Channel      string `json:"channel-version"`
	ReleaseType  string `json:"release-type"`
	EOL          string `json:"eol-date"`
	SupportPhase string `json:"support-phase"`
	ReleasesURL  string `json:"releases.json"`
	LatestSDK    string `json:"latest-sdk"`
}

func discoverDotNetChannels(dotnet *imageversions.DotNet, candidates []dotNetIndexChannel, today string) {
	trackedTypes := map[string]bool{}
	for _, releaseType := range dotnet.TrackReleaseTypes {
		trackedTypes[releaseType] = true
	}
	dotnet.UnassignedSupportedChannels = []imageversions.DotNetDiscovery{}
	for _, candidate := range candidates {
		_, declared := dotnet.Channels[candidate.Channel]
		stable := candidate.SupportPhase == "active" || candidate.SupportPhase == "maintenance"
		if !declared && trackedTypes[candidate.ReleaseType] && stable && candidate.EOL >= today {
			dotnet.UnassignedSupportedChannels = append(dotnet.UnassignedSupportedChannels, imageversions.DotNetDiscovery{
				Channel:      candidate.Channel,
				ReleaseType:  candidate.ReleaseType,
				SupportPhase: candidate.SupportPhase,
				EOL:          candidate.EOL,
				LatestSDK:    candidate.LatestSDK,
			})
		}
	}
	sort.Slice(dotnet.UnassignedSupportedChannels, func(i, j int) bool {
		return dotnet.UnassignedSupportedChannels[i].Channel < dotnet.UnassignedSupportedChannels[j].Channel
	})
}

func (u updater) refreshGo(ctx context.Context, m *imageversions.Manifest) error {
	var releases []struct {
		Version string `json:"version"`
		Stable  bool   `json:"stable"`
		Files   []struct {
			OS, Arch, Kind, SHA256 string
		} `json:"files"`
	}
	if err := u.getJSON(ctx, "https://go.dev/dl/?mode=json&include=all", &releases); err != nil {
		return fmt.Errorf("Go releases: %w", err)
	}
	for _, release := range releases {
		if !release.Stable {
			continue
		}
		resolved := imageversions.Go{Strategy: m.Go.Strategy, Version: strings.TrimPrefix(release.Version, "go")}
		for _, file := range release.Files {
			if file.Kind != "archive" {
				continue
			}
			switch file.OS + "/" + file.Arch {
			case "linux/amd64":
				resolved.LinuxAMD64SHA256 = file.SHA256
			case "linux/arm64":
				resolved.LinuxARM64SHA256 = file.SHA256
			case "windows/amd64":
				resolved.WindowsAMD64SHA256 = file.SHA256
			}
		}
		for name, value := range map[string]string{"linux amd64": resolved.LinuxAMD64SHA256, "linux arm64": resolved.LinuxARM64SHA256, "windows amd64": resolved.WindowsAMD64SHA256} {
			if err := validHex(value, 64); err != nil {
				return fmt.Errorf("Go %s %s checksum: %w", resolved.Version, name, err)
			}
		}
		m.Go = resolved
		return nil
	}
	return errors.New("Go has no stable release")
}

func (u updater) refreshRust(ctx context.Context, m *imageversions.Manifest) error {
	body, _, err := u.download(ctx, "https://static.rust-lang.org/dist/channel-rust-"+m.Rust.Channel+".toml")
	if err != nil {
		return fmt.Errorf("Rust channel manifest: %w", err)
	}
	version := rustVersion(body)
	if version == "" {
		return errors.New("Rust channel manifest has no pkg.rust version")
	}
	x64, _, err := u.download(ctx, "https://static.rust-lang.org/rustup/dist/x86_64-unknown-linux-gnu/rustup-init.sha256")
	if err != nil {
		return fmt.Errorf("rustup x64 checksum: %w", err)
	}
	arm64, _, err := u.download(ctx, "https://static.rust-lang.org/rustup/dist/aarch64-unknown-linux-gnu/rustup-init.sha256")
	if err != nil {
		return fmt.Errorf("rustup arm64 checksum: %w", err)
	}
	x64Fields := strings.Fields(string(x64))
	arm64Fields := strings.Fields(string(arm64))
	if len(x64Fields) == 0 || len(arm64Fields) == 0 {
		return errors.New("rustup checksum response is empty")
	}
	m.Rust.Version = version
	m.Rust.RustupX64SHA256 = x64Fields[0]
	m.Rust.RustupARM64SHA256 = arm64Fields[0]
	return nil
}

func (u updater) refreshBuildTools(ctx context.Context, m *imageversions.Manifest) error {
	base := "https://aka.ms/vs/" + m.BuildTools.ReleaseLine + "/release/"
	bootstrapperSHA, bootstrapperURL, _, err := u.downloadSHA256(ctx, base+"vs_buildtools.exe", false)
	if err != nil {
		return fmt.Errorf("Visual Studio Build Tools bootstrapper: %w", err)
	}
	channelSHA, channelURL, body, err := u.downloadSHA256(ctx, base+"channel", true)
	if err != nil {
		return fmt.Errorf("Visual Studio Build Tools channel: %w", err)
	}
	var channel struct {
		Info struct {
			ProductDisplayVersion string `json:"productDisplayVersion"`
		} `json:"info"`
	}
	if err := json.Unmarshal(body, &channel); err != nil {
		return fmt.Errorf("decode Visual Studio channel: %w", err)
	}
	version := strings.Fields(channel.Info.ProductDisplayVersion)
	if len(version) == 0 {
		return errors.New("Visual Studio channel has no product version")
	}
	m.BuildTools.Version = version[0]
	m.BuildTools.BootstrapperURL = bootstrapperURL
	m.BuildTools.BootstrapperSHA256 = bootstrapperSHA
	m.BuildTools.ChannelURL = channelURL
	m.BuildTools.ChannelSHA256 = channelSHA
	return nil
}

func (u updater) supported(name, eol string) error {
	if eol == "" {
		return fmt.Errorf("%s has no EOL date", name)
	}
	if eol < u.today {
		return fmt.Errorf("%s reached end of life on %s; update the declared support policy intentionally", name, eol)
	}
	return nil
}

type githubRelease struct {
	TagName string        `json:"tag_name"`
	Assets  []githubAsset `json:"assets"`
}

type githubAsset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
	Digest             string `json:"digest"`
}

func (r githubRelease) digest(name string) (string, error) {
	for _, asset := range r.Assets {
		if asset.Name == name {
			return asset.sha256()
		}
	}
	return "", fmt.Errorf("release asset %q not found", name)
}

func (a githubAsset) sha256() (string, error) {
	value := strings.TrimPrefix(a.Digest, "sha256:")
	if a.Digest == value {
		return "", fmt.Errorf("asset %q has no SHA256 digest", a.Name)
	}
	if err := validHex(value, 64); err != nil {
		return "", fmt.Errorf("asset %q digest: %w", a.Name, err)
	}
	return strings.ToLower(value), nil
}

func (u updater) githubRelease(ctx context.Context, repo string) (githubRelease, error) {
	var release githubRelease
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.github.com/repos/"+repo+"/releases/latest", nil)
	if err != nil {
		return release, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	if u.token != "" {
		req.Header.Set("Authorization", "Bearer "+u.token)
	}
	if err := u.doJSON(req, &release); err != nil {
		return release, err
	}
	if release.TagName == "" {
		return release, errors.New("latest release has no tag")
	}
	return release, nil
}

func (u updater) registryDigest(ctx context.Context, base imageversions.BaseImage) (string, error) {
	parts := strings.SplitN(base.Repository, "/", 2)
	if len(parts) != 2 {
		return "", fmt.Errorf("invalid repository %q", base.Repository)
	}
	endpoint := "https://" + parts[0] + "/v2/" + parts[1] + "/manifests/" + url.PathEscape(base.Tag)
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, endpoint, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.oci.image.index.v1+json, application/vnd.docker.distribution.manifest.list.v2+json")
	resp, err := u.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("%s returned %s", endpoint, resp.Status)
	}
	digest := strings.ToLower(resp.Header.Get("Docker-Content-Digest"))
	if !strings.HasPrefix(digest, "sha256:") {
		return "", fmt.Errorf("invalid Docker-Content-Digest %q", digest)
	}
	if err := validHex(strings.TrimPrefix(digest, "sha256:"), 64); err != nil {
		return "", err
	}
	return digest, nil
}

func (u updater) getJSON(ctx context.Context, endpoint string, target any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	return u.doJSON(req, target)
}

func (u updater) doJSON(req *http.Request, target any) error {
	resp, err := u.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("%s returned %s", req.URL, resp.Status)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 128<<20)).Decode(target); err != nil {
		return err
	}
	return nil
}

func (u updater) download(ctx context.Context, endpoint string) ([]byte, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, "", err
	}
	resp, err := u.client.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, "", fmt.Errorf("%s returned %s", endpoint, resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 128<<20))
	if err != nil {
		return nil, "", err
	}
	return body, resp.Request.URL.String(), nil
}

func (u updater) downloadSHA256(ctx context.Context, endpoint string, keepBody bool) (string, string, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", "", nil, err
	}
	resp, err := u.client.Do(req)
	if err != nil {
		return "", "", nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", "", nil, fmt.Errorf("%s returned %s", endpoint, resp.Status)
	}
	h := sha256.New()
	var body bytes.Buffer
	w := io.Writer(h)
	if keepBody {
		w = io.MultiWriter(h, &body)
	}
	if _, err := io.Copy(w, resp.Body); err != nil {
		return "", "", nil, err
	}
	return hex.EncodeToString(h.Sum(nil)), resp.Request.URL.String(), body.Bytes(), nil
}

func checksumManifest(body []byte) map[string]string {
	checksums := map[string]string{}
	scanner := bufio.NewScanner(bytes.NewReader(body))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) == 2 {
			checksums[fields[1]] = strings.ToLower(fields[0])
		}
	}
	return checksums
}

func rustVersion(body []byte) string {
	scanner := bufio.NewScanner(bytes.NewReader(body))
	inRust := false
	for scanner.Scan() {
		line := scanner.Text()
		if line == "[pkg.rust]" {
			inRust = true
			continue
		}
		if inRust && strings.HasPrefix(line, "[") {
			return ""
		}
		if inRust && strings.HasPrefix(line, "version = \"") {
			value := strings.TrimPrefix(line, "version = \"")
			if i := strings.IndexByte(value, ' '); i >= 0 {
				value = value[:i]
			}
			return strings.Trim(value, "\"")
		}
	}
	return ""
}

func validHex(value string, length int) error {
	if len(value) != length {
		return fmt.Errorf("must contain %d hexadecimal characters", length)
	}
	if _, err := hex.DecodeString(value); err != nil {
		return fmt.Errorf("must be hexadecimal: %w", err)
	}
	return nil
}

func replaceBaseArg(data []byte, reference string) ([]byte, error) {
	lines := strings.SplitAfter(string(data), "\n")
	count := 0
	for i, line := range lines {
		trimmed := strings.TrimSuffix(line, "\n")
		trimmed = strings.TrimSuffix(trimmed, "\r")
		if strings.HasPrefix(trimmed, "ARG BASE=") {
			ending := strings.TrimPrefix(line, trimmed)
			lines[i] = "ARG BASE=" + reference + ending
			count++
		}
	}
	if count != 1 {
		return nil, fmt.Errorf("expected one ARG BASE line, found %d", count)
	}
	return []byte(strings.Join(lines, "")), nil
}

func writeIfChanged(path string, data []byte) error {
	current, err := os.ReadFile(path)
	if err == nil && bytes.Equal(current, data) {
		return nil
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read %s: %w", path, err)
	}
	mode := os.FileMode(0o644)
	if info, statErr := os.Stat(path); statErr == nil {
		mode = info.Mode().Perm()
	}
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".versions-*.tmp")
	if err != nil {
		return fmt.Errorf("create temporary file for %s: %w", path, err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("replace %s: %w", path, err)
	}
	return nil
}
