package main

import (
	"io"
	"log/slog"
	"testing"

	"github.com/GerardSmit/multirunner/internal/config"
)

func TestPoolEnvAndMountsSharesDockerWorkspace(t *testing.T) {
	cfg := &config.Config{}
	pool := config.Pool{
		OS:         "linux",
		WorkFolder: "_work-pr",
		Docker: config.Docker{
			EnableDinD:     true,
			ShareWorkspace: true,
		},
	}

	_, mounts := poolEnvAndMounts(
		cfg,
		pool,
		nil,
		nil,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)

	if len(mounts) != 2 {
		t.Fatalf("mount count = %d, want 2", len(mounts))
	}
	workspace := mounts[1]
	if workspace.Source != "/home/runner/_work-pr" || workspace.Target != workspace.Source || workspace.Volume {
		t.Fatalf("workspace mount = %+v", workspace)
	}
}
