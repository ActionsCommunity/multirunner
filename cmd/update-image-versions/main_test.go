package main

import (
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"

	imageversions "github.com/GerardSmit/multirunner/images"
)

func TestReplaceBaseArg(t *testing.T) {
	got, err := replaceBaseArg([]byte("# comment\nARG BASE=old\nFROM ${BASE}\n"), "repo/image:tag@sha256:abc")
	if err != nil {
		t.Fatal(err)
	}
	want := "# comment\nARG BASE=repo/image:tag@sha256:abc\nFROM ${BASE}\n"
	if string(got) != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	for _, input := range []string{"FROM image\n", "ARG BASE=one\nARG BASE=two\n"} {
		if _, err := replaceBaseArg([]byte(input), "new"); err == nil {
			t.Errorf("replaceBaseArg(%q) unexpectedly succeeded", input)
		}
	}
}

func TestChecksumManifest(t *testing.T) {
	got := checksumManifest([]byte("ABCDEF  one.zip\n123456 *two.zip\nmalformed\n"))
	if got["one.zip"] != "abcdef" {
		t.Fatalf("first checksum = %q", got["one.zip"])
	}
	if _, ok := got["two.zip"]; ok {
		t.Fatal("star-prefixed checksum filename should not match release asset names")
	}
}

func TestRustVersion(t *testing.T) {
	manifest := strings.Join([]string{
		"[pkg.other]",
		"version = \"0.1.0\"",
		"[pkg.rust]",
		"version = \"1.98.0 (123456 2026-08-01)\"",
	}, "\n")
	if got := rustVersion([]byte(manifest)); got != "1.98.0" {
		t.Fatalf("rustVersion = %q", got)
	}
}

func TestRustupReleaseVersion(t *testing.T) {
	manifest := "schema-version = '1'\nversion = '1.29.1'\n"
	if got := rustupReleaseVersion([]byte(manifest)); got != "1.29.1" {
		t.Fatalf("rustupReleaseVersion = %q", got)
	}
	if got := rustupReleaseVersion([]byte("schema-version = '1'\n")); got != "" {
		t.Fatalf("missing version = %q", got)
	}
}

func TestSupported(t *testing.T) {
	u := updater{today: "2026-09-01"}
	if err := u.supported("tool", "2026-09-01"); err != nil {
		t.Fatalf("EOL date itself should remain supported: %v", err)
	}
	if err := u.supported("tool", "2026-08-31"); err != nil {
		t.Fatalf("past EOL must warn, not abort the refresh: %v", err)
	}
	if err := u.supported("tool", ""); err == nil {
		t.Fatal("missing EOL date unexpectedly accepted")
	}
}

func TestGitHubAssetDigest(t *testing.T) {
	asset := githubAsset{Name: "tool.zip", Digest: "sha256:" + strings.Repeat("A", 64)}
	got, err := asset.sha256()
	if err != nil {
		t.Fatal(err)
	}
	if got != strings.Repeat("a", 64) {
		t.Fatalf("digest = %q", got)
	}
	asset.Digest = ""
	if _, err := asset.sha256(); err == nil {
		t.Fatal("missing digest unexpectedly accepted")
	}
}

func TestDiscoverNodeLTS(t *testing.T) {
	node := imageversions.Node{
		Track:    imageversions.NodeTrackSupportedLTS,
		Releases: map[string]imageversions.NodeRelease{"24": {}},
	}
	schedule := map[string]nodeLifecycle{
		"v25": {End: "2026-06-01"},
		"v26": {LTS: "2026-10-28", End: "2029-04-30"},
		"v28": {LTS: "2028-10-24", End: "2031-04-30"},
	}
	if err := discoverNodeLTS(&node, schedule, "2026-10-27"); err != nil {
		t.Fatal(err)
	}
	if _, ok := node.Releases["26"]; ok {
		t.Fatal("future LTS major was added early")
	}
	if err := discoverNodeLTS(&node, schedule, "2026-10-28"); err != nil {
		t.Fatal(err)
	}
	if _, ok := node.Releases["26"]; !ok {
		t.Fatal("new active LTS major was not added")
	}
	if _, ok := node.Releases["25"]; ok {
		t.Fatal("non-LTS major was added")
	}
	if _, ok := node.Releases["28"]; ok {
		t.Fatal("future LTS major was added")
	}
}

func TestIntegritySHA512Hex(t *testing.T) {
	sum := sha512.Sum512([]byte("corepack tarball"))
	integrity := "sha512-" + base64.StdEncoding.EncodeToString(sum[:])
	got, err := integritySHA512Hex(integrity)
	if err != nil {
		t.Fatal(err)
	}
	if want := hex.EncodeToString(sum[:]); got != want {
		t.Fatalf("hex = %q, want %q", got, want)
	}
	if err := validHex(got, 128); err != nil {
		t.Fatalf("converted digest is not a manifest digest: %v", err)
	}
	for name, value := range map[string]string{
		"sha1 algorithm":  "sha1-" + base64.StdEncoding.EncodeToString(sum[:]),
		"no algorithm":    base64.StdEncoding.EncodeToString(sum[:]),
		"not base64":      "sha512-not base64!",
		"truncated hash":  "sha512-" + base64.StdEncoding.EncodeToString(sum[:32]),
		"empty integrity": "",
	} {
		if _, err := integritySHA512Hex(value); err == nil {
			t.Errorf("%s unexpectedly accepted", name)
		}
	}
}

func TestNpmPackageVersionCorepack(t *testing.T) {
	sum := sha512.Sum512([]byte("corepack tarball"))
	fixture := `{
	  "name": "corepack",
	  "version": "0.36.0",
	  "bin": {"corepack": "dist/corepack.js"},
	  "dist": {
	    "shasum": "04e62edc9b34b932cc09124d0181ae5c922fbce8",
	    "tarball": "https://registry.npmjs.org/corepack/-/corepack-0.36.0.tgz",
	    "integrity": "sha512-` + base64.StdEncoding.EncodeToString(sum[:]) + `"
	  }
	}`
	var latest npmPackageVersion
	if err := json.Unmarshal([]byte(fixture), &latest); err != nil {
		t.Fatal(err)
	}
	got, err := latest.corepack()
	if err != nil {
		t.Fatal(err)
	}
	want := imageversions.Corepack{
		Version: "0.36.0",
		URL:     "https://registry.npmjs.org/corepack/-/corepack-0.36.0.tgz",
		SHA512:  hex.EncodeToString(sum[:]),
	}
	if got != want {
		t.Fatalf("corepack = %#v, want %#v", got, want)
	}

	var empty npmPackageVersion
	if _, err := empty.corepack(); err == nil {
		t.Error("registry metadata without a version or tarball unexpectedly accepted")
	}
	latest.Dist.Integrity = "sha1-" + base64.StdEncoding.EncodeToString(sum[:20])
	if _, err := latest.corepack(); err == nil {
		t.Error("non-sha512 integrity unexpectedly accepted")
	}
}

func TestDiscoverDotNetChannels(t *testing.T) {
	dotnet := imageversions.DotNet{
		TrackReleaseTypes: []string{"lts", "sts"},
		Channels: map[string]imageversions.DotNetChannel{
			"10.0": {},
		},
	}
	discoverDotNetChannels(&dotnet, []dotNetIndexChannel{
		{Channel: "10.0", ReleaseType: "lts", SupportPhase: "active", EOL: "2028-11-14", LatestSDK: "10.0.100"},
		{Channel: "11.0", ReleaseType: "sts", SupportPhase: "preview", EOL: "2028-05-09", LatestSDK: "11.0.100-preview.7"},
		{Channel: "12.0", ReleaseType: "lts", SupportPhase: "active", EOL: "2031-11-11", LatestSDK: "12.0.100"},
		{Channel: "13.0", ReleaseType: "go-live", SupportPhase: "active", EOL: "2030-05-14", LatestSDK: "13.0.100"},
		{Channel: "7.0", ReleaseType: "sts", SupportPhase: "eol", EOL: "2024-05-14", LatestSDK: "7.0.410"},
	}, "2029-01-01")

	if len(dotnet.UnassignedSupportedChannels) != 1 {
		t.Fatalf("discoveries = %#v", dotnet.UnassignedSupportedChannels)
	}
	discovery := dotnet.UnassignedSupportedChannels[0]
	if discovery.Channel != "12.0" || discovery.LatestSDK != "12.0.100" {
		t.Fatalf("discovery = %#v", discovery)
	}
}

func TestDiscoverDotNetChannelsUsesEmptyArray(t *testing.T) {
	dotnet := imageversions.DotNet{
		TrackReleaseTypes: []string{"lts"},
		Channels:          map[string]imageversions.DotNetChannel{"10.0": {}},
	}
	discoverDotNetChannels(&dotnet, nil, "2026-09-01")
	if dotnet.UnassignedSupportedChannels == nil {
		t.Fatal("empty discoveries must encode as [] rather than null")
	}
}
