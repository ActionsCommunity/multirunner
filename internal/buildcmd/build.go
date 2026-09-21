// Package buildcmd builds multirunner with immutable source identity.
package buildcmd

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"

	"github.com/GerardSmit/multirunner/internal/buildinfo"
)

const targetPackage = "./cmd/multirunner"

var (
	versionPattern = regexp.MustCompile(`^[0-9A-Za-z][0-9A-Za-z._/+~-]*$`)
	commitPattern  = regexp.MustCompile(`^(?:[0-9a-fA-F]{40}|[0-9a-fA-F]{64})$`)
)

// Options controls one multirunner build.
type Options struct {
	Directory  string
	Output     string
	Version    string
	Commit     string
	GOOS       string
	GOARCH     string
	AllowDirty bool
}

type commands struct {
	output func(context.Context, string, string, ...string) ([]byte, error)
	run    func(context.Context, string, []string, string, ...string) error
}

// Build compiles multirunner after resolving and validating its source identity.
func Build(ctx context.Context, opts Options) error {
	return build(ctx, opts, commands{output: commandOutput, run: commandRun})
}

func build(ctx context.Context, opts Options, command commands) error {
	directory := opts.Directory
	if directory == "" {
		var err error
		directory, err = os.Getwd()
		if err != nil {
			return fmt.Errorf("get working directory: %w", err)
		}
	}

	commit := strings.TrimSpace(opts.Commit)
	directory, sourceCommit, dirty, inGit, err := gitIdentity(ctx, directory, command.output)
	if err != nil {
		return err
	}
	if !inGit {
		if commit == "" {
			return fmt.Errorf("resolve source checkout: not a Git checkout; provide -commit explicitly")
		}
		directory, err = moduleRoot(directory)
		if err != nil {
			return err
		}
	} else if commit == "" {
		commit = sourceCommit
	}
	if !commitPattern.MatchString(commit) {
		return fmt.Errorf("commit must be a full 40 or 64 character hexadecimal object ID")
	}

	version := strings.TrimSpace(opts.Version)
	if version == "" {
		version = "dev-" + commit[:12]
		if dirty {
			version += "-dirty"
		}
	} else if dirty && !opts.AllowDirty {
		return fmt.Errorf("source checkout is dirty; explicit version %q requires -allow-dirty", version)
	}
	if !versionPattern.MatchString(version) {
		return fmt.Errorf("version %q contains unsupported characters", version)
	}

	goos := firstNonempty(opts.GOOS, os.Getenv("GOOS"), runtime.GOOS)
	goarch := firstNonempty(opts.GOARCH, os.Getenv("GOARCH"), runtime.GOARCH)
	output := opts.Output
	if output == "" {
		output = "multirunner"
		if goos == "windows" {
			output += ".exe"
		}
	}
	ldflags := strings.Join([]string{
		"-s",
		"-w",
		"-X", buildinfo.VersionVariable + "=" + version,
		"-X", buildinfo.CommitVariable + "=" + commit,
	}, " ")
	args := []string{"build", "-trimpath", "-ldflags", ldflags, "-o", output, targetPackage}
	env := setEnv(os.Environ(), "CGO_ENABLED", "0")
	env = setEnv(env, "GOOS", goos)
	env = setEnv(env, "GOARCH", goarch)
	if err := command.run(ctx, directory, env, "go", args...); err != nil {
		return fmt.Errorf("build multirunner for %s/%s: %w", goos, goarch, err)
	}
	return nil
}

func gitIdentity(
	ctx context.Context,
	directory string,
	output func(context.Context, string, string, ...string) ([]byte, error),
) (string, string, bool, bool, error) {
	root, err := output(ctx, directory, "git", "rev-parse", "--show-toplevel")
	if err != nil {
		return directory, "", false, false, nil
	}
	directory = strings.TrimSpace(string(root))
	commit, err := output(ctx, directory, "git", "rev-parse", "HEAD")
	if err != nil {
		return "", "", false, true, fmt.Errorf("resolve source commit: %w", err)
	}
	// Normal untracked files count as dirty, while files excluded by Git ignore
	// rules do not appear. This catches new source without treating ignored build
	// outputs as source changes.
	status, err := output(ctx, directory, "git", "status", "--porcelain", "--untracked-files=normal")
	if err != nil {
		return "", "", false, true, fmt.Errorf("resolve source status: %w", err)
	}
	return directory, strings.TrimSpace(string(commit)), len(strings.TrimSpace(string(status))) != 0, true, nil
}

func moduleRoot(directory string) (string, error) {
	current, err := filepath.Abs(directory)
	if err != nil {
		return "", fmt.Errorf("resolve build directory: %w", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(current, "go.mod")); err == nil {
			return current, nil
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", fmt.Errorf("find module root from %q: go.mod not found", directory)
		}
		current = parent
	}
}

func commandOutput(ctx context.Context, directory, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = directory
	return cmd.Output()
}

func commandRun(ctx context.Context, directory string, env []string, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = directory
	cmd.Env = env
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func setEnv(env []string, key, value string) []string {
	prefix := key + "="
	result := make([]string, 0, len(env)+1)
	for _, entry := range env {
		if !strings.EqualFold(entry[:strings.IndexByte(entry, '=')+1], prefix) {
			result = append(result, entry)
		}
	}
	return append(result, prefix+value)
}

func firstNonempty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
