package main

import (
	"crypto/sha256"
	buildinfofile "debug/buildinfo"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"testing"
)

const testReleaseCommit = "0123456789abcdef0123456789abcdef01234567"

func TestParseArgsRequiresImmutableReleaseIdentity(t *testing.T) {
	opts, err := parseArgs([]string{
		"prepare",
		"-root", "root.json",
		"-dist", "dist",
		"-output", "request",
		"-version", "v1.2.3",
		"-commit", strings.Repeat("a", 40),
		"-repository", "GerardSmit/multirunner",
		"-workflow", ".github/workflows/release.yml",
		"-builder-id", "https://github.com/actions/runner",
		"-invocation-id", "run-1",
		"-source-run-id", "42",
		"-metadata-version", "42",
	})
	if err != nil {
		t.Fatal(err)
	}
	if opts.metadataVersion != 42 || opts.version != "v1.2.3" {
		t.Fatalf("options = %+v", opts)
	}
	if _, err := parseArgs([]string{
		"prepare",
		"-root", "root.json", "-version", "../v1",
		"-dist", "dist", "-output", "request",
		"-commit", strings.Repeat("a", 40),
		"-repository", "r", "-workflow", "w", "-builder-id", "b",
		"-invocation-id", "i", "-source-run-id", "1", "-metadata-version", "1",
	}); err == nil {
		t.Fatal("path-like version accepted")
	}
}

func TestParseArgsSeparatesPreparationAndVerificationInputs(t *testing.T) {
	identity := []string{
		"-version", "v1.2.3", "-commit", strings.Repeat("a", 40),
		"-repository", "ActionsCommunity/multirunner",
		"-workflow", ".github/workflows/release-sign.yml",
		"-builder-id", "https://github.com/actions/runner",
		"-invocation-id", "run-42", "-source-run-id", "42",
	}
	verifyPreparedArgs := append([]string{"verify-prepared", "-request", "request"}, identity...)
	if _, err := parseArgs(verifyPreparedArgs); err != nil {
		t.Fatal(err)
	}
	verifyFinalArgs := append(
		[]string{"verify-final", "-request", "request", "-repository-dir", "repository"},
		identity...,
	)
	if _, err := parseArgs(verifyFinalArgs); err != nil {
		t.Fatal(err)
	}
}

func TestDigestTreeIsStableAndPathSensitive(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("one"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "nested"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "nested", "b.txt"), []byte("two"), 0o600); err != nil {
		t.Fatal(err)
	}
	first, err := digestTree(root)
	if err != nil {
		t.Fatal(err)
	}
	second, err := digestTree(root)
	if err != nil || first != second {
		t.Fatalf("digests = %q, %q, %v", first, second, err)
	}
}

func TestDigestTreeRejectsSymbolicLinks(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target.txt")
	if err := os.WriteFile(target, []byte("target"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(root, "link.txt")); err != nil {
		t.Skipf("symbolic links unavailable: %v", err)
	}
	if _, err := digestTree(root); err == nil {
		t.Fatal("symbolic link was included in console digest")
	}
}

func TestBuildProvenanceBindsSubjectAndInvocation(t *testing.T) {
	digest := sha256.Sum256([]byte("binary"))
	data, err := buildProvenance(options{
		version: "v1.2.3", commit: strings.Repeat("a", 40),
		repository: "GerardSmit/multirunner",
		builderID:  "builder", invocationID: "run-1",
	}, "multirunner", "linux", "amd64", digest)
	if err != nil {
		t.Fatal(err)
	}
	var statement map[string]any
	if err := json.Unmarshal(data, &statement); err != nil {
		t.Fatal(err)
	}
	if statement["predicateType"] != "https://slsa.dev/provenance/v1" {
		t.Fatalf("statement = %s", data)
	}
}

func TestVerifyReleaseBuildInfoAcceptsCleanExpectedRevision(t *testing.T) {
	revision, err := verifyReleaseBuildInfo(&buildinfofile.BuildInfo{
		Settings: []debug.BuildSetting{
			{Key: "vcs.revision", Value: testReleaseCommit},
			{Key: "vcs.modified", Value: "false"},
		},
	}, strings.ToUpper(testReleaseCommit))
	if err != nil {
		t.Fatalf("verify release build info: %v", err)
	}
	if revision != testReleaseCommit {
		t.Fatalf("revision = %q, want %q", revision, testReleaseCommit)
	}
}

func TestVerifyReleaseBuildInfoRejectsModifiedSource(t *testing.T) {
	_, err := verifyReleaseBuildInfo(&buildinfofile.BuildInfo{
		Settings: []debug.BuildSetting{
			{Key: "vcs.revision", Value: testReleaseCommit},
			{Key: "vcs.modified", Value: "true"},
		},
	}, testReleaseCommit)
	if err == nil || !strings.Contains(err.Error(), `source modification state is "true", want false`) {
		t.Fatalf("error = %v", err)
	}
}

func TestVerifyReleaseBuildInfoRequiresModificationState(t *testing.T) {
	_, err := verifyReleaseBuildInfo(&buildinfofile.BuildInfo{
		Settings: []debug.BuildSetting{
			{Key: "vcs.revision", Value: testReleaseCommit},
		},
	}, testReleaseCommit)
	if err == nil || !strings.Contains(err.Error(), `source modification state is "", want false`) {
		t.Fatalf("error = %v", err)
	}
}

func TestVerifyReleaseBuildInfoRejectsUnexpectedRevision(t *testing.T) {
	_, err := verifyReleaseBuildInfo(&buildinfofile.BuildInfo{
		Settings: []debug.BuildSetting{
			{Key: "vcs.revision", Value: strings.Repeat("f", 40)},
			{Key: "vcs.modified", Value: "false"},
		},
	}, testReleaseCommit)
	if err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("error = %v", err)
	}
}
