// Command build compiles multirunner with immutable source identity.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/GerardSmit/multirunner/internal/buildcmd"
)

func main() {
	if err := run(context.Background(), os.Args[1:], os.Stderr, buildcmd.Build); err != nil {
		fmt.Fprintln(os.Stderr, "build:", err)
		os.Exit(1)
	}
}

func run(
	ctx context.Context,
	args []string,
	stderr io.Writer,
	build func(context.Context, buildcmd.Options) error,
) error {
	opts, err := parseArgs(args, stderr)
	if err != nil {
		return err
	}
	return build(ctx, opts)
}

func parseArgs(args []string, stderr io.Writer) (buildcmd.Options, error) {
	flags := flag.NewFlagSet("build", flag.ContinueOnError)
	flags.SetOutput(stderr)
	var opts buildcmd.Options
	var version, commit explicitString
	flags.Var(&version, "version", "version to embed; defaults to dev-<commit>")
	flags.Var(&commit, "commit", "full source commit to embed; defaults to the current Git checkout")
	flags.StringVar(&opts.Output, "o", "", "output binary path")
	flags.StringVar(&opts.GOOS, "goos", "", "target operating system")
	flags.StringVar(&opts.GOARCH, "goarch", "", "target architecture")
	flags.BoolVar(&opts.AllowDirty, "allow-dirty", false, "allow an explicit version from a dirty Git checkout")
	if err := flags.Parse(args); err != nil {
		return buildcmd.Options{}, err
	}
	if flags.NArg() != 0 {
		return buildcmd.Options{}, fmt.Errorf("unexpected arguments: %s", strings.Join(flags.Args(), " "))
	}
	if version.set {
		if strings.TrimSpace(version.value) == "" {
			return buildcmd.Options{}, fmt.Errorf("-version cannot be empty")
		}
		opts.Version = version.value
	}
	if commit.set {
		if strings.TrimSpace(commit.value) == "" {
			return buildcmd.Options{}, fmt.Errorf("-commit cannot be empty")
		}
		opts.Commit = commit.value
	}
	return opts, nil
}

type explicitString struct {
	value string
	set   bool
}

func (s *explicitString) String() string {
	return s.value
}

func (s *explicitString) Set(value string) error {
	s.value = value
	s.set = true
	return nil
}
