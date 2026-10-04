// Command cacheserver runs the multirunner self-hosted GitHub Actions cache
// (v2) as a standalone process — useful for running the cache as a container or
// on a separate host shared by multiple multirunner instances.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/GerardSmit/multirunner/internal/cache"
	"github.com/GerardSmit/multirunner/internal/config"
)

func main() {
	listen := flag.String("listen", "0.0.0.0:3000", "listen address")
	path := flag.String("path", "/data", "storage path for cache db + blobs")
	advertise := flag.String("advertise", "", "external URL of this cache (informational)")
	accessToken := flag.String("access-token", "", "shared path token for cache API URLs (generated when empty)")
	upstream := flag.String("upstream", "https://results-receiver.actions.githubusercontent.com",
		"catch-all proxy and runtime-token validation upstream (HTTPS unless loopback)")
	skipToken := flag.Bool("skip-token-validation", true,
		"deprecated compatibility flag (ignored; bearer tokens are always validated)")
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	srv, err := cache.New(ctx, config.Cache{
		Enabled:             true,
		Mode:                "local-server",
		Storage:             "filesystem",
		Path:                *path,
		Listen:              *listen,
		AdvertiseURL:        *advertise,
		AccessToken:         *accessToken,
		SkipTokenValidation: *skipToken,
		Upstream:            *upstream,
	}, logger)
	if err != nil {
		fmt.Fprintln(os.Stderr, "cacheserver: "+err.Error())
		os.Exit(1)
	}

	if err := srv.Start(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "cacheserver: "+err.Error())
		os.Exit(1)
	}
}
