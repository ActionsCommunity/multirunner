package winvm

import (
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	imageversions "github.com/GerardSmit/multirunner/images"
)

func TestAutounattendFiles(t *testing.T) {
	runnerDigest := strings.Repeat("a", 64)
	files, err := AutounattendFiles("9.9.9", runnerDigest, "P@ss123", []string{"dotnet", "node", "go", "buildtools"})
	if err != nil {
		t.Fatal(err)
	}
	install := files["install-golden.ps1"]
	if !strings.Contains(install, "9.9.9") {
		t.Error("runner version not substituted")
	}
	if strings.Contains(install, "__RUNNER_VERSION__") {
		t.Error("runner version placeholder left")
	}
	if strings.Contains(install, "__") {
		t.Errorf("unresolved placeholder left in script")
	}
	if !strings.Contains(install, "buildtools,dotnet,go,node") {
		t.Error("tools list not substituted")
	}
	wants := []string{
		runnerDigest, minGitURL, minGitSHA256,
		bakeGoURL(), bakeGoSHA256,
	}
	// The URL and the digest must come from the same manifest entry, or the
	// in-guest fallback download can never satisfy the hash check.
	if !strings.Contains(minGitURL, minGitVersion) {
		t.Errorf("MinGit URL %q does not carry manifest version %q", minGitURL, minGitVersion)
	}
	plan, err := resolveToolPlan([]string{"node", "dotnet", "buildtools"})
	if err != nil {
		t.Fatal(err)
	}
	for _, artifact := range bakeDotNetArtifacts(plan.DotNet) {
		wants = append(wants, artifact.Name, artifact.URL, artifact.Digest)
	}
	for _, artifact := range bakeNodeArtifacts(plan.Node) {
		wants = append(wants, artifact.Name, artifact.URL, artifact.Digest)
	}
	for _, artifact := range bakeBuildToolsArtifacts(plan.BuildTools) {
		wants = append(wants, artifact.Name, artifact.URL, artifact.Digest)
	}
	for _, want := range wants {
		if !strings.Contains(install, want) {
			t.Errorf("provisioning identity %q missing from script", want)
		}
	}
	for _, mutable := range []string{"https://dot.net/v1/dotnet-install.ps1", "https://aka.ms/vs/17/release/vs_buildtools.exe", "-Channel 8.0", "-Channel 9.0"} {
		if strings.Contains(install, mutable) {
			t.Errorf("mutable provisioning input remains: %s", mutable)
		}
	}
	if !strings.Contains(files["autounattend.xml"], "P@ss123") {
		t.Error("admin password not substituted")
	}
	if _, ok := files["startup.ps1"]; !ok {
		t.Error("startup.ps1 missing")
	}
}

func TestToolsHash(t *testing.T) {
	iso := filepath.Join(t.TempDir(), "windows.iso")
	if err := os.WriteFile(iso, []byte("iso one"), 0o600); err != nil {
		t.Fatal(err)
	}
	hash := func(o BakeOptions) string {
		t.Helper()
		o.WindowsISO = iso
		got, err := ToolsHash(o)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	if hash(BakeOptions{}) == "" {
		t.Error("empty tools must still fingerprint ISO, runner, and templates")
	}
	// Order- and case-insensitive, de-duped.
	if hash(BakeOptions{Tools: []string{"node", "dotnet"}}) !=
		hash(BakeOptions{RunnerVersion: DefaultRunnerVersion, Tools: []string{"DotNet", "node", "node"}}) {
		t.Error("ToolsHash should be normalized")
	}
	if hash(BakeOptions{Tools: []string{"node"}}) == hash(BakeOptions{Tools: []string{"go"}}) {
		t.Error("different tools should hash differently")
	}
	allNodeSelectors := make([]string, 0, len(imageVersionManifest.Node.Releases))
	for major := range imageVersionManifest.Node.Releases {
		allNodeSelectors = append(allNodeSelectors, "node:"+major)
	}
	if hash(BakeOptions{Tools: []string{"node"}}) != hash(BakeOptions{Tools: allNodeSelectors}) {
		t.Error("unqualified Node should expand to every declared LTS major")
	}
	if hash(BakeOptions{Tools: []string{"node:24"}}) == hash(BakeOptions{Tools: []string{"node"}}) {
		t.Error("an exact Node selector should not hash like every Node major")
	}
	minimal := hash(BakeOptions{})
	for _, tool := range []string{"node", "go", "dotnet", "buildtools"} {
		if minimal == hash(BakeOptions{Tools: []string{tool}}) {
			t.Errorf("selecting %s did not affect ToolsHash", tool)
		}
	}
	if hash(BakeOptions{}) == hash(BakeOptions{
		RunnerVersion: "1.2.3", RunnerSHA256: strings.Repeat("b", 64),
	}) {
		t.Error("runner identity must affect ToolsHash")
	}

	before := hash(BakeOptions{})
	if err := os.WriteFile(iso, []byte("iso two"), 0o600); err != nil {
		t.Fatal(err)
	}
	if before == hash(BakeOptions{}) {
		t.Error("Windows ISO content must affect ToolsHash")
	}
}

func TestToolsHashFingerprintsEveryPayloadIdentity(t *testing.T) {
	tools := []string{"node", "go", "dotnet", "buildtools"}
	artifacts, err := bakeArtifacts("", "", tools)
	if err != nil {
		t.Fatal(err)
	}
	files, err := AutounattendFiles(DefaultRunnerVersion, "", toolsHashAdminPassword, tools)
	if err != nil {
		t.Fatal(err)
	}
	base := toolsHashResolved(tools, "iso-digest", artifacts, files)
	for i, artifact := range artifacts {
		if artifact.Version == "" {
			t.Errorf("%s has no exact version identity", artifact.Name)
		}
		changed := append([]bakeArtifact(nil), artifacts...)
		changed[i].Digest = strings.Repeat("f", len(artifact.Digest))
		if got := toolsHashResolved(tools, "iso-digest", changed, files); got == base {
			t.Errorf("%s digest did not affect ToolsHash", artifact.Name)
		}
		changed = append([]bakeArtifact(nil), artifacts...)
		changed[i].Version += ".changed"
		if got := toolsHashResolved(tools, "iso-digest", changed, files); got == base {
			t.Errorf("%s version did not affect ToolsHash", artifact.Name)
		}
	}
	for name := range files {
		mutated := map[string]string{}
		for key, value := range files {
			mutated[key] = value
		}
		mutated[name] += "\n# changed\n"
		if got := toolsHashResolved(tools, "iso-digest", artifacts, mutated); got == base {
			t.Errorf("rendered %s did not affect ToolsHash", name)
		}
	}
}

// The rendered install script decides which Node major lands on PATH and which
// Build Tools line VSBUILDTOOLS points at. Neither is visible in the canonical
// selectors, the artifact identities, or the raw templates.
func TestToolsHashTracksManifestDefaults(t *testing.T) {
	iso := filepath.Join(t.TempDir(), "windows.iso")
	if err := os.WriteFile(iso, []byte("iso"), 0o600); err != nil {
		t.Fatal(err)
	}
	hash := func(tools ...string) string {
		t.Helper()
		got, err := ToolsHash(BakeOptions{WindowsISO: iso, Tools: tools})
		if err != nil {
			t.Fatal(err)
		}
		return got
	}

	other := 0
	for major := range imageVersionManifest.Node.Releases {
		value, err := strconv.Atoi(major)
		if err != nil {
			t.Fatalf("invalid Node major %q", major)
		}
		if value != imageVersionManifest.Node.DefaultMajor {
			other = value
		}
	}
	if other == 0 {
		t.Skip("manifest declares a single Node major")
	}
	before := hash("node")
	savedMajor := imageVersionManifest.Node.DefaultMajor
	t.Cleanup(func() { imageVersionManifest.Node.DefaultMajor = savedMajor })
	imageVersionManifest.Node.DefaultMajor = other
	if after := hash("node"); after == before {
		t.Errorf("node.default_major %d -> %d did not change ToolsHash (%s)", savedMajor, other, after)
	}
	imageVersionManifest.Node.DefaultMajor = savedMajor

	otherLine := ""
	for line := range imageVersionManifest.BuildTools.Lines {
		if line != imageVersionManifest.BuildTools.DefaultLine {
			otherLine = line
		}
	}
	if otherLine == "" {
		return
	}
	beforeTools := hash("buildtools:17", "buildtools:18")
	savedLine := imageVersionManifest.BuildTools.DefaultLine
	t.Cleanup(func() { imageVersionManifest.BuildTools.DefaultLine = savedLine })
	imageVersionManifest.BuildTools.DefaultLine = otherLine
	if after := hash("buildtools:17", "buildtools:18"); after == beforeTools {
		t.Errorf("buildtools.default_line %q -> %q did not change ToolsHash", savedLine, otherLine)
	}
	imageVersionManifest.BuildTools.DefaultLine = savedLine
}

func TestToolsHashIgnoresAdminPassword(t *testing.T) {
	iso := filepath.Join(t.TempDir(), "windows.iso")
	if err := os.WriteFile(iso, []byte("iso"), 0o600); err != nil {
		t.Fatal(err)
	}
	first, err := ToolsHash(BakeOptions{WindowsISO: iso, AdminPassword: "One!1"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := ToolsHash(BakeOptions{WindowsISO: iso, AdminPassword: "Two!2"})
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Error("rotating the admin password must not force a golden rebuild")
	}
}

func TestBakeTimeoutScalesWithToolPlan(t *testing.T) {
	timeout := func(tools ...string) time.Duration {
		t.Helper()
		o := BakeOptions{Tools: tools}
		o.defaults()
		return o.Timeout
	}
	if got := timeout(); got != bakeBaseTimeout {
		t.Errorf("minimal bake timeout = %s, want %s", got, bakeBaseTimeout)
	}
	if got, want := timeout("node"), bakeToolchainTimeout; got != want {
		t.Errorf("node bake timeout = %s, want %s", got, want)
	}
	if got, want := timeout("buildtools"), bakeBaseTimeout+bakeBuildToolsTimeout; got != want {
		t.Errorf("single Build Tools bake timeout = %s, want %s", got, want)
	}
	both := timeout("buildtools:17", "buildtools:18")
	if want := bakeBaseTimeout + 2*bakeBuildToolsTimeout; both != want {
		t.Errorf("combined Build Tools bake timeout = %s, want %s", both, want)
	}
	if single := timeout("buildtools:17"); both <= single {
		t.Errorf("combining Build Tools lines did not extend the timeout (%s vs %s)", both, single)
	}
	if got := timeout("buildtools:17", "buildtools:18", "dotnet", "node", "go"); got != both {
		t.Errorf("toolchain floor lowered the Build Tools timeout: %s", got)
	}
	explicit := BakeOptions{Tools: []string{"buildtools"}, Timeout: 3 * time.Minute}
	explicit.defaults()
	if explicit.Timeout != 3*time.Minute {
		t.Errorf("explicit Timeout override = %s, want 3m0s", explicit.Timeout)
	}
}

func TestBareDotNetSelectorSkipsUnsupportedChannels(t *testing.T) {
	channels := imageVersionManifest.DotNet.ChannelsForTarget(imageversions.DotNetTargetQEMUWindows)
	if len(channels) < 2 {
		t.Skip("need at least two qemu-windows channels")
	}
	saved := imageVersionManifest.DotNet.Channels
	t.Cleanup(func() { imageVersionManifest.DotNet.Channels = saved })
	withPhase := func(eol ...string) {
		replaced := make(map[string]imageversions.DotNetChannel, len(saved))
		for channel, release := range saved {
			for _, name := range eol {
				if channel == name {
					release.SupportPhase = "eol"
				}
			}
			replaced[channel] = release
		}
		imageVersionManifest.DotNet.Channels = replaced
	}

	withPhase(channels[0])
	plan, err := resolveToolPlan([]string{"dotnet"})
	if err != nil {
		t.Fatalf("bare dotnet selector must survive an EOL channel: %v", err)
	}
	for _, channel := range plan.DotNet {
		if channel == channels[0] {
			t.Errorf("EOL channel %q was still baked", channel)
		}
	}
	if len(plan.DotNet) != len(channels)-1 {
		t.Errorf("bare dotnet selector resolved %v, want the remaining %d channels", plan.DotNet, len(channels)-1)
	}
	// An exact selector stays strict: silently dropping it would ship a golden
	// without the SDK the config asked for.
	if _, err := resolveToolPlan([]string{"dotnet:" + dotNetChannelMajor(channels[0])}); err == nil {
		t.Error("exact selector for an EOL channel should fail")
	}

	withPhase(channels...)
	if _, err := resolveToolPlan([]string{"dotnet"}); err == nil {
		t.Error("bare dotnet selector should fail when no channel is supported")
	}
}

func TestToolSelectorRejectsEmptyVersion(t *testing.T) {
	for _, selector := range []string{"node:", "buildtools:", "go:", "dotnet:"} {
		_, err := resolveToolPlan([]string{selector})
		if err == nil {
			t.Errorf("selector %q should fail", selector)
			continue
		}
		if !strings.Contains(err.Error(), "empty version") {
			t.Errorf("selector %q error = %v, want an empty-version message", selector, err)
		}
	}
}

func TestDotNetSelectorMatchesChannelMajorNumerically(t *testing.T) {
	saved := imageVersionManifest.DotNet.Channels
	t.Cleanup(func() { imageVersionManifest.DotNet.Channels = saved })
	replaced := make(map[string]imageversions.DotNetChannel, len(saved)+1)
	for channel, release := range saved {
		replaced[channel] = release
	}
	base := saved[imageVersionManifest.DotNet.ChannelsForTarget(imageversions.DotNetTargetQEMUWindows)[0]]
	base.SupportPhase = "active"
	replaced["41.1"] = base
	imageVersionManifest.DotNet.Channels = replaced

	plan, err := resolveToolPlan([]string{"dotnet:41"})
	if err != nil {
		t.Fatalf("dotnet:41 should match channel 41.1: %v", err)
	}
	if got, want := strings.Join(plan.DotNet, ","), "41.1"; got != want {
		t.Errorf("resolved channels = %q, want %q", got, want)
	}
	if got, want := strings.Join(plan.Canonical, ","), "dotnet:41"; got != want {
		t.Errorf("canonical selector = %q, want %q", got, want)
	}
	if _, err := resolveToolPlan([]string{"dotnet:41.1"}); err == nil {
		t.Error("a dotted .NET selector should not resolve")
	}
}

func TestVersionedGoldenToolSelectors(t *testing.T) {
	defaultPlan, err := resolveToolPlan([]string{"buildtools"})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Join(defaultPlan.Canonical, ","), "buildtools:18"; got != want {
		t.Fatalf("default Build Tools selector = %q, want %q", got, want)
	}
	plan, err := resolveToolPlan([]string{"node", "node:24", "dotnet", "dotnet:10", "buildtools:17", "buildtools:18"})
	if err != nil {
		t.Fatal(err)
	}
	wantCanonical := "buildtools:17,buildtools:18,dotnet:10,dotnet:8,dotnet:9"
	for _, major := range plan.Node {
		wantCanonical += ",node:" + major
	}
	if got := strings.Join(plan.Canonical, ","); got != wantCanonical {
		t.Fatalf("canonical selectors = %q, want %q", got, wantCanonical)
	}
	if got, want := strings.Join(plan.DotNet, ","), "8.0,9.0,10.0"; got != want {
		t.Fatalf(".NET install order = %q, want %q", got, want)
	}
	if len(plan.Node) != len(imageVersionManifest.Node.Releases) {
		t.Fatalf("Node install majors = %v, want all %d declared majors", plan.Node, len(imageVersionManifest.Node.Releases))
	}
	nodeScript := bakeNodeInstallScript(plan.Node)
	for _, major := range plan.Node {
		release := imageVersionManifest.Node.Releases[major]
		for _, want := range []string{bakeNodeArtifactName(major), release.Version, release.WindowsX64SHA256} {
			if !strings.Contains(nodeScript, want) {
				t.Errorf("generated Node script missing %q", want)
			}
		}
	}
	corepack := bakeCorepackArtifact()
	for _, want := range []string{
		corepack.Name, corepack.URL, corepack.Digest,
		`npm.cmd' install -g --no-audit --no-fund --no-update-notifier C:\corepack.tgz`,
		`if ($LASTEXITCODE -ne 0) { throw 'Corepack install failed' }`,
		`corepack.cmd' enable`,
	} {
		if !strings.Contains(nodeScript, want) {
			t.Errorf("generated Node script missing %q", want)
		}
	}
	nodeArtifacts := bakeNodeArtifacts(plan.Node)
	if len(nodeArtifacts) != len(plan.Node)+1 {
		t.Fatalf("Node artifacts = %d, want one per major plus Corepack", len(nodeArtifacts))
	}
	staged := nodeArtifacts[len(nodeArtifacts)-1]
	if staged.Name != "corepack.tgz" || staged.Algorithm != "SHA512" || staged.Version == "" ||
		staged.URL != imageVersionManifest.Node.Corepack.URL || staged.Digest != imageVersionManifest.Node.Corepack.SHA512 {
		t.Errorf("Corepack artifact = %#v", staged)
	}
	exactNode, err := resolveToolPlan([]string{"node:24"})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Join(exactNode.Canonical, ","), "node:24"; got != want {
		t.Fatalf("exact Node selector = %q, want %q", got, want)
	}
	if script := bakeNodeInstallScript(exactNode.Node); strings.Contains(script, imageVersionManifest.Node.Releases["22"].Version) || !strings.Contains(script, imageVersionManifest.Node.Releases["24"].Version) {
		t.Fatal("exact Node install script did not select only Node 24")
	}
	targeted, err := resolveToolPlan([]string{"dotnet"})
	if err != nil {
		t.Fatal(err)
	}
	wantChannels := imageVersionManifest.DotNet.ChannelsForTarget(imageversions.DotNetTargetQEMUWindows)
	if got, want := strings.Join(targeted.DotNet, ","), strings.Join(wantChannels, ","); got != want {
		t.Fatalf("bare .NET selector = %q, want the qemu-windows channels %q", got, want)
	}
	artifacts := bakeDotNetArtifacts(plan.DotNet)
	if len(artifacts) != len(plan.DotNet) {
		t.Fatalf("got %d .NET artifacts, want %d", len(artifacts), len(plan.DotNet))
	}
	if len(bakeDotNetArtifacts(targeted.DotNet)) != len(wantChannels) {
		t.Fatalf("bare .NET selector baked %d artifacts, want %d", len(bakeDotNetArtifacts(targeted.DotNet)), len(wantChannels))
	}
	if script := bakeDotNetInstallScript(plan.DotNet); !strings.Contains(script, "dotnet-10-0.zip") || !strings.Contains(script, imageVersionManifest.DotNet.Channels["10.0"].WindowsX64SHA512) {
		t.Fatal("generated install script did not include .NET 10")
	}
	buildToolsScript := bakeBuildToolsInstallScript(plan.BuildTools)
	for _, want := range []string{"C:\\BuildTools\\17", "C:\\BuildTools\\18", "VSBUILDTOOLS_17", "VSBUILDTOOLS_18"} {
		if !strings.Contains(buildToolsScript, want) {
			t.Errorf("generated Build Tools script missing %q", want)
		}
	}
	for _, selector := range []string{"dotnet:7", "buildtools:19", "node:20", "go:1", "unknown"} {
		if _, err := resolveToolPlan([]string{selector}); err == nil {
			t.Errorf("selector %q should fail", selector)
		}
	}
}

func TestToolsHashValidatesConfiguredIdentities(t *testing.T) {
	iso := filepath.Join(t.TempDir(), "windows.iso")
	data := []byte("licensed windows media")
	if err := os.WriteFile(iso, data, 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	expected := hex.EncodeToString(sum[:])
	if _, err := ToolsHash(BakeOptions{WindowsISO: iso, WindowsISOSHA256: strings.ToUpper(expected)}); err != nil {
		t.Fatalf("matching ISO checksum: %v", err)
	}
	if _, err := ToolsHash(BakeOptions{WindowsISO: iso, WindowsISOSHA256: strings.Repeat("0", 64)}); err == nil ||
		!strings.Contains(err.Error(), "Windows ISO SHA256 mismatch") {
		t.Fatalf("mismatched ISO checksum error = %v", err)
	}
	if _, err := ToolsHash(BakeOptions{WindowsISO: iso, RunnerVersion: "1.2.3"}); err == nil ||
		!strings.Contains(err.Error(), "runner SHA256 is required") {
		t.Fatalf("custom runner checksum error = %v", err)
	}
	if _, err := ToolsHash(BakeOptions{
		WindowsISO: iso, RunnerVersion: "1.2.3", RunnerSHA256: "not-a-digest",
	}); err == nil || !strings.Contains(err.Error(), "64 hexadecimal characters") {
		t.Fatalf("malformed runner checksum error = %v", err)
	}
}

func TestStageArtifactsRejectsDigestMismatch(t *testing.T) {
	payload := []byte("verified payload")
	sum := sha256.Sum256(payload)
	sum512 := sha512.Sum512(payload)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(payload)
	}))
	defer server.Close()

	refs, cleanup := stageArtifacts(t.Context(), []bakeArtifact{
		{Name: "good.zip", URL: server.URL + "/good", Algorithm: "SHA256", Digest: hex.EncodeToString(sum[:])},
		{Name: "good512.zip", URL: server.URL + "/good512", Algorithm: "SHA512", Digest: hex.EncodeToString(sum512[:])},
		{Name: "bad.zip", URL: server.URL + "/bad", Algorithm: "SHA256", Digest: strings.Repeat("0", 64)},
	})
	defer cleanup()
	if refs["good.zip"] == "" {
		t.Fatal("verified payload was not staged")
	}
	if refs["good512.zip"] == "" {
		t.Fatal("SHA512-verified payload was not staged")
	}
	if _, ok := refs["bad.zip"]; ok {
		t.Fatal("payload with mismatched digest was staged")
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(refs["good.zip"]), "bad.zip")); !os.IsNotExist(err) {
		t.Fatalf("rejected payload was not removed: %v", err)
	}
}

func TestBakeQEMUArgs(t *testing.T) {
	got := strings.Join(bakeQEMUArgs(BakeOptions{
		Golden: "g.qcow2", WindowsISO: "win.iso", MemMB: 4096, CPUs: 2, Accel: "kvm",
		OVMFCode: "code.fd",
	}, "auto.iso", "vars.fd"), " ")
	for _, want := range []string{
		"-accel kvm", "file=g.qcow2,if=none,id=osdisk", "file=win.iso,media=cdrom",
		"file=auto.iso,media=cdrom", "e1000", "-qmp", "-serial",
		"if=pflash,format=raw,unit=0,readonly=on,file=code.fd", "unit=1,file=vars.fd",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("bake args missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "-no-reboot") {
		t.Error("bake must NOT use -no-reboot (Windows Setup reboots mid-install)")
	}
}

func TestBakeDefaultsUseHostCompatibleAccel(t *testing.T) {
	var opts BakeOptions
	opts.defaults()
	if want := DetectAccel(runtime.GOOS, runtime.GOARCH); opts.Accel != want {
		t.Fatalf("default accel = %q, want %q", opts.Accel, want)
	}
}

func TestMetaRoundTrip(t *testing.T) {
	golden := t.TempDir() + "/golden.qcow2"
	now := time.Date(2026, 6, 17, 0, 0, 0, 0, time.UTC)
	in := GoldenMeta{CreatedAt: now, EvalDays: 180, MaxRearms: 5, RearmsUsed: 1, WorkflowsHash: "abc"}
	if err := SaveMeta(golden, in); err != nil {
		t.Fatal(err)
	}
	out, err := LoadMeta(golden)
	if err != nil {
		t.Fatal(err)
	}
	if !out.CreatedAt.Equal(now) || out.EvalDays != 180 || out.RearmsUsed != 1 || out.WorkflowsHash != "abc" {
		t.Errorf("round trip mismatch: %+v", out)
	}
}

func TestLoadMetaMissing(t *testing.T) {
	m, err := LoadMeta(t.TempDir() + "/nope.qcow2")
	if err != nil {
		t.Fatalf("missing meta should not error: %v", err)
	}
	if m.EvalDays != 0 {
		t.Errorf("expected zero meta, got %+v", m)
	}
}
