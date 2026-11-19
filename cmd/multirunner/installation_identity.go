package main

import "github.com/GerardSmit/multirunner/internal/config"

// Every per-repository GitHub client uses this same auth configuration. Do not
// infer credential locations from a profile, home directory, or another client.
func configuredServiceTokenPaths(cfg *config.Config) []string {
	if cfg == nil || !cfg.Auth.IsDeviceApp() {
		return nil
	}
	return []string{cfg.Auth.TokenPath}
}
