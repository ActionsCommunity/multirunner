package buildcmd

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestBuildInjectsIdentityIntoRealCLI(t *testing.T) {
	repository := filepath.Join("..", "..")
	commitOutput, err := commandOutput(
		context.Background(), repository, "git", "rev-parse", "HEAD",
	)
	if err != nil {
		t.Fatal(err)
	}
	commit := strings.TrimSpace(string(commitOutput))
	outputDir := filepath.Join(t.TempDir(), "output with spaces")
	if err := os.MkdirAll(outputDir, 0o700); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(outputDir, "multirunner")
	if runtime.GOOS == "windows" {
		binary += ".exe"
	}
	const version = "v9.8.7-integration"
	if err := Build(context.Background(), Options{
		Directory:  repository,
		Output:     binary,
		Version:    version,
		Commit:     commit,
		GOOS:       runtime.GOOS,
		GOARCH:     runtime.GOARCH,
		AllowDirty: true,
	}); err != nil {
		t.Fatalf("build real CLI: %v", err)
	}

	output, err := exec.Command(binary, "--version").CombinedOutput()
	if err != nil {
		t.Fatalf("execute real CLI: %v\n%s", err, output)
	}
	want := "multirunner version " + version + " (commit " + commit + ")"
	if strings.TrimSpace(string(output)) != want {
		t.Fatalf("--version output = %q, want %q", output, want)
	}
}
