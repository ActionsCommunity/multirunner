package main

import (
	"context"
	"io"
	"testing"

	"github.com/GerardSmit/multirunner/internal/buildcmd"
)

func TestParseArgsRejectsExplicitEmptyIdentity(t *testing.T) {
	for _, args := range [][]string{
		{"-version="},
		{"-version", " "},
		{"-commit="},
		{"-commit", " "},
	} {
		if _, err := parseArgs(args, io.Discard); err == nil {
			t.Fatalf("parseArgs(%q) accepted an empty identity", args)
		}
	}
}

func TestParseArgsPreservesExplicitValues(t *testing.T) {
	opts, err := parseArgs([]string{
		"-version", "v1.2.3",
		"-commit", testCommit,
		"-o", "output with spaces/multirunner",
		"-goos", "linux",
		"-goarch", "arm64",
	}, io.Discard)
	if err != nil {
		t.Fatalf("parseArgs: %v", err)
	}
	if opts.Version != "v1.2.3" || opts.Commit != testCommit ||
		opts.Output != "output with spaces/multirunner" ||
		opts.GOOS != "linux" || opts.GOARCH != "arm64" {
		t.Fatalf("options = %+v", opts)
	}
}

func TestRunPassesExplicitIdentityToBuilder(t *testing.T) {
	var got buildcmd.Options
	if err := run(context.Background(), []string{
		"-version", "v1.2.3",
		"-commit", testCommit,
		"-o", "output with spaces/multirunner",
	}, io.Discard, func(_ context.Context, opts buildcmd.Options) error {
		got = opts
		return nil
	}); err != nil {
		t.Fatalf("run: %v", err)
	}
	if got.Version != "v1.2.3" || got.Commit != testCommit ||
		got.Output != "output with spaces/multirunner" {
		t.Fatalf("builder options = %+v", got)
	}
}

const testCommit = "0123456789abcdef0123456789abcdef01234567"
