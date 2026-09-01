package winvm

import (
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
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
		runnerDigest, minGitSHA256,
		bakeNodeURL(), bakeNodeSHA256,
		bakeGoURL(), bakeGoSHA256,
		bakeVSURL, bakeVSSHA256, bakeVSChannelURL, bakeVSChannelSHA256,
	}
	for _, artifact := range bakeDotNetArtifacts() {
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
	artifacts, err := bakeArtifacts("", "", []string{"node", "go", "dotnet", "buildtools"})
	if err != nil {
		t.Fatal(err)
	}
	base := toolsHashResolved([]string{"node", "go", "dotnet", "buildtools"}, "iso-digest", artifacts)
	for i, artifact := range artifacts {
		if artifact.Version == "" {
			t.Errorf("%s has no exact version identity", artifact.Name)
		}
		changed := append([]bakeArtifact(nil), artifacts...)
		changed[i].Digest = strings.Repeat("f", len(artifact.Digest))
		if got := toolsHashResolved([]string{"node", "go", "dotnet", "buildtools"}, "iso-digest", changed); got == base {
			t.Errorf("%s digest did not affect ToolsHash", artifact.Name)
		}
		changed = append([]bakeArtifact(nil), artifacts...)
		changed[i].Version += ".changed"
		if got := toolsHashResolved([]string{"node", "go", "dotnet", "buildtools"}, "iso-digest", changed); got == base {
			t.Errorf("%s version did not affect ToolsHash", artifact.Name)
		}
	}
}

func TestBakeDotNetArtifactsFollowManifestTargets(t *testing.T) {
	original := imageVersionManifest
	defer func() { imageVersionManifest = original }()
	imageVersionManifest.DotNet.Channels = maps.Clone(original.DotNet.Channels)
	release := imageVersionManifest.DotNet.Channels["10.0"]
	release.Targets = append(release.Targets, "qemu-windows")
	imageVersionManifest.DotNet.Channels["10.0"] = release

	artifacts := bakeDotNetArtifacts()
	if len(artifacts) != 3 {
		t.Fatalf("got %d QEMU .NET artifacts, want 3", len(artifacts))
	}
	if script := bakeDotNetInstallScript(); !strings.Contains(script, "dotnet-10-0.zip") || !strings.Contains(script, release.WindowsX64SHA512) {
		t.Fatal("generated QEMU install script did not include newly targeted channel")
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
