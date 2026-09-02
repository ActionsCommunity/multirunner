package buildcmd

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testCommit = "0123456789abcdef0123456789abcdef01234567"

func TestBuildConstructsQuotedArgumentsAndTargetEnvironment(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module example.test/build\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(root, "output with spaces", "multirunner.exe")
	var gotArgs []string
	var gotEnv []string
	command := commands{
		output: func(context.Context, string, string, ...string) ([]byte, error) {
			t.Fatal("explicit commit unexpectedly invoked Git")
			return nil, nil
		},
		run: func(_ context.Context, directory string, env []string, name string, args ...string) error {
			if directory != root || name != "go" {
				t.Fatalf("command = %q in %q", name, directory)
			}
			gotArgs = append([]string(nil), args...)
			gotEnv = append([]string(nil), env...)
			return nil
		},
	}

	err := build(context.Background(), Options{
		Directory: root,
		Output:    output,
		Version:   "v1.2.3-test",
		Commit:    testCommit,
		GOOS:      "windows",
		GOARCH:    "arm64",
	}, command)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	wantArgs := []string{
		"build", "-trimpath", "-ldflags",
		"-s -w -X github.com/GerardSmit/multirunner/internal/buildinfo.Version=v1.2.3-test -X github.com/GerardSmit/multirunner/internal/buildinfo.Commit=" + testCommit,
		"-o", output, "./cmd/multirunner",
	}
	if strings.Join(gotArgs, "\x00") != strings.Join(wantArgs, "\x00") {
		t.Fatalf("arguments = %#v, want %#v", gotArgs, wantArgs)
	}
	for _, want := range []string{"CGO_ENABLED=0", "GOOS=windows", "GOARCH=arm64"} {
		if !containsFold(gotEnv, want) {
			t.Errorf("environment is missing %q", want)
		}
	}
}

func TestBuildReadsCommitAndCreatesDevelopmentVersion(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module example.test/build\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var calls int
	command := commands{
		output: func(_ context.Context, directory, name string, args ...string) ([]byte, error) {
			calls++
			if name != "git" {
				t.Fatalf("command = %q", name)
			}
			if calls == 1 {
				return []byte(root + "\n"), nil
			}
			if directory != root || strings.Join(args, " ") != "rev-parse HEAD" {
				t.Fatalf("commit command = %q in %q", args, directory)
			}
			return []byte(testCommit + "\n"), nil
		},
		run: func(_ context.Context, _ string, _ []string, _ string, args ...string) error {
			if !strings.Contains(strings.Join(args, " "), "Version=dev-0123456789ab") {
				t.Fatalf("development version is missing from %q", args)
			}
			return nil
		},
	}
	if err := build(context.Background(), Options{Directory: root}, command); err != nil {
		t.Fatalf("build: %v", err)
	}
}

func TestBuildRejectsMalformedIdentity(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module example.test/build\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	command := commands{run: func(context.Context, string, []string, string, ...string) error {
		t.Fatal("invalid input reached go build")
		return nil
	}}
	for _, tc := range []struct {
		name    string
		version string
		commit  string
	}{
		{name: "short commit", version: "v1", commit: "abc123"},
		{name: "non hexadecimal commit", version: "v1", commit: strings.Repeat("z", 40)},
		{name: "version with whitespace", version: "release candidate", commit: testCommit},
		{name: "version with quote", version: `v1"bad`, commit: testCommit},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := build(context.Background(), Options{
				Directory: root,
				Version:   tc.version,
				Commit:    tc.commit,
			}, command)
			if err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}

func TestBuildFailsClearlyOutsideGitWithoutCommit(t *testing.T) {
	root := t.TempDir()
	command := commands{output: func(context.Context, string, string, ...string) ([]byte, error) {
		return nil, errors.New("not a git repository")
	}}
	err := build(context.Background(), Options{Directory: root}, command)
	if err == nil || !strings.Contains(err.Error(), "provide -commit when building outside Git") {
		t.Fatalf("error = %v", err)
	}
}

func TestBuildReportsGoBuildFailure(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module example.test/build\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	command := commands{run: func(context.Context, string, []string, string, ...string) error {
		return errors.New("compiler failed")
	}}
	err := build(context.Background(), Options{
		Directory: root,
		Version:   "v1.2.3",
		Commit:    testCommit,
		GOOS:      "linux",
		GOARCH:    "amd64",
	}, command)
	if err == nil || !strings.Contains(err.Error(), "build multirunner for linux/amd64: compiler failed") {
		t.Fatalf("error = %v", err)
	}
}

func TestReleaseWorkflowUsesCanonicalBuildCommandForEveryTarget(t *testing.T) {
	workflow, err := os.ReadFile(filepath.Join("..", "..", ".github", "workflows", "release.yml"))
	if err != nil {
		t.Fatalf("read release workflow: %v", err)
	}
	text := string(workflow)
	for _, want := range []string{
		"linux/amd64 linux/arm64 windows/amd64 windows/arm64 darwin/amd64 darwin/arm64",
		`COMMIT="${GITHUB_SHA}"`,
		"go run ./cmd/build",
		`-version "$VER" -commit "$COMMIT"`,
		`-goos "$os" -goarch "$arch" -o "$out"`,
		"multirunner --version",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("release workflow is missing %q", want)
		}
	}
	if strings.Contains(text, "internal/buildinfo.Version") ||
		strings.Contains(text, "internal/buildinfo.Commit") {
		t.Error("release workflow duplicates canonical linker variable paths")
	}
	if got := strings.Count(text, "go run ./cmd/build"); got != 1 {
		t.Errorf("release workflow has %d build helper calls, want one loop body", got)
	}
}

func containsFold(values []string, want string) bool {
	for _, value := range values {
		if strings.EqualFold(value, want) {
			return true
		}
	}
	return false
}
