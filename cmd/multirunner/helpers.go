package main

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/GerardSmit/multirunner/internal/backend"
	"github.com/GerardSmit/multirunner/internal/cache"
	"github.com/GerardSmit/multirunner/internal/config"
	"github.com/GerardSmit/multirunner/internal/gitcache"
)

// startCache starts the embedded cache server (or selects an external one) and
// returns the URL runners should be pointed at.
func startCache(ctx context.Context, cfg *config.Config, gitMgr *gitcache.Manager, logger *slog.Logger) (string, error) {
	if cfg.Cache.ExternalURL != "" {
		logger.Info("using external cache server", "url", cfg.Cache.ExternalURL)
		return cfg.Cache.ExternalURL, nil
	}
	srv, err := cache.New(ctx, cfg.Cache, logger)
	if err != nil {
		return "", fmt.Errorf("cache server: %w", err)
	}
	if cfg.GitCache.DotGit() && gitMgr != nil && cfg.GitHub.Scope == config.ScopeRepo {
		slug := cfg.GitHub.Owner + "/" + cfg.GitHub.Repo
		srv.SetGitBundler(slug, gitMgr.Bundle)
		logger.Info("dotgit-cache enabled: serving git bundles via the cache server", "repo", slug)
	}
	go func() {
		if err := srv.Start(ctx); err != nil {
			logger.Error("cache server stopped", "err", err)
		}
	}()
	advertise := srv.AdvertiseURL()
	if advertise == "" {
		logger.Warn("cache enabled but advertise_url is empty; runners will not be redirected")
	} else {
		logger.Info("cache redirect enabled", "advertise", srv.RedactedAdvertiseURL())
	}
	return advertise, nil
}

// setupGitCache builds the git mirror manager (if enabled) and, for repo scope,
// pre-mirrors the target repo and starts a periodic refresh.
func setupGitCache(ctx context.Context, cfg *config.Config, logger *slog.Logger) (*gitcache.Manager, error) {
	if !cfg.GitCache.Enabled() {
		return nil, nil
	}
	mgr, err := gitcache.New(cfg.GitCache.Path, cfg.GitHub.URL, cfg.Auth.PAT, logger)
	if err != nil {
		return nil, fmt.Errorf("git cache: %w", err)
	}
	if cfg.GitCache.MaxAgeDays > 0 {
		go sweepMirrors(ctx, mgr, time.Duration(cfg.GitCache.MaxAgeDays)*24*time.Hour, logger)
	}
	if cfg.GitHub.Scope == config.ScopeRepo {
		slug := cfg.GitHub.Owner + "/" + cfg.GitHub.Repo
		if _, err := mgr.EnsureMirror(ctx, slug); err != nil {
			logger.Warn("initial git mirror failed; continuing", "repo", slug, "err", err)
		}
		go refreshMirror(ctx, mgr, slug, logger)
	} else {
		logger.Warn("git cache configured but scope is not repo; per-repo mirroring via container hooks is not yet implemented",
			"scope", cfg.GitHub.Scope)
	}
	return mgr, nil
}

// sweepMirrors periodically removes bare mirrors not used within maxAge.
func sweepMirrors(ctx context.Context, mgr *gitcache.Manager, maxAge time.Duration, logger *slog.Logger) {
	t := time.NewTicker(6 * time.Hour)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if n, err := mgr.Sweep(ctx, maxAge); err != nil {
				logger.Warn("git mirror sweep failed", "err", err)
			} else if n > 0 {
				logger.Info("git mirror sweep removed stale mirrors", "count", n)
			}
		}
	}
}

func refreshMirror(ctx context.Context, mgr *gitcache.Manager, slug string, logger *slog.Logger) {
	t := time.NewTicker(5 * time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if _, err := mgr.EnsureMirror(ctx, slug); err != nil {
				logger.Warn("git mirror refresh failed", "repo", slug, "err", err)
			}
		}
	}
}

// poolEnvAndMounts derives the per-pool runner env and container mounts from the
// shared env plus the pool's tool-cache, DinD, and git-mirror settings.
func poolEnvAndMounts(cfg *config.Config, pc config.Pool, shared map[string]string, gitMgr *gitcache.Manager, logger *slog.Logger) (map[string]string, []backend.Mount) {
	env := make(map[string]string, len(shared)+2)
	for k, v := range shared {
		env[k] = v
	}
	var mounts []backend.Mount

	if pc.ToolCache.Mode == "shared-volume" && pc.ToolCache.Volume != "" {
		target := pc.ToolCachePath()
		env["RUNNER_TOOL_CACHE"] = target
		mounts = append(mounts, backend.Mount{
			Source: pc.ToolCache.Volume, Target: target, Volume: true, ReadOnly: pc.ToolCache.ReadOnly,
		})
	}

	if pc.Docker.EnableDinD {
		sock := pc.DockerSocketPath()
		mounts = append(mounts, backend.Mount{Source: sock, Target: sock})
	}
	if pc.Docker.ShareWorkspace {
		workspace := pc.SharedWorkspacePath()
		mounts = append(mounts, backend.Mount{Source: workspace, Target: workspace})
	}

	if gitMgr != nil && cfg.GitHub.Scope == config.ScopeRepo {
		slug := cfg.GitHub.Owner + "/" + cfg.GitHub.Repo
		switch {
		case cfg.GitCache.DotGit():
			// dotgit-cache: serve the mirror as a bundle over the cache server +
			// a job-started hook seeds the workspace. Works where mounts can't
			// (the QEMU VM). Needs the cache server (for the bundle endpoint).
			if base := strings.TrimRight(shared["ACTIONS_RESULTS_URL"], "/"); base != "" {
				env["MR_GIT_BUNDLE_URL"] = base + "/gitmirror/" + slug
				if pc.OS == "windows" {
					env["ACTIONS_RUNNER_HOOK_JOB_STARTED"] = `C:\mr-githook.ps1`
				}
			}
			logger.Debug("dotgit-cache enabled", "pool", pc.Name, "repo", slug)
		default:
			target := gitMgr.ContainerPath(slug, pc.OS)
			mounts = append(mounts, backend.Mount{
				Source: gitMgr.MirrorPath(slug), Target: target, ReadOnly: true,
			})
			env["MULTIRUNNER_GIT_MIRROR"] = target
			logger.Debug("git mirror mounted", "pool", pc.Name, "repo", slug)
		}
	}

	return env, mounts
}
