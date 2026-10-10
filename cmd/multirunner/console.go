package main

import (
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"runtime"

	"github.com/kardianos/service"
	"github.com/spf13/cobra"

	"github.com/GerardSmit/multirunner/internal/config"
	"github.com/GerardSmit/multirunner/internal/consoleauth"
	"github.com/GerardSmit/multirunner/internal/restore"
	"github.com/GerardSmit/multirunner/internal/update"
)

func consoleCmd(configPath *string) *cobra.Command {
	command := &cobra.Command{
		Use:   "console",
		Short: "Open and manage the local Operations Console",
		Args:  cobra.NoArgs,
	}
	var noBrowser bool
	open := &cobra.Command{
		Use:   "open",
		Short: "Create a single-use pairing token and open the console",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			absolute, err := filepath.Abs(*configPath)
			if err != nil {
				return err
			}
			cfg, err := config.Load(absolute)
			if err != nil {
				return err
			}
			if !cfg.History.Enabled {
				return fmt.Errorf("console requires history.enabled")
			}
			secret, err := consoleauth.LoadSecret(consoleauth.SecretPath(cfg.History.DatabasePath))
			if err != nil {
				return fmt.Errorf("load console pairing secret: %w", err)
			}
			authenticator, err := consoleauth.New(secret)
			if err != nil {
				return err
			}
			token, err := authenticator.PairingToken()
			if err != nil {
				return err
			}
			consoleURL := "http://" + cfg.History.Listen + "/"
			fmt.Fprintln(cmd.OutOrStdout(), "Console URL:", consoleURL)
			fmt.Fprintln(cmd.OutOrStdout(), "Console pairing token (expires in 2 minutes):")
			fmt.Fprintln(cmd.OutOrStdout(), token)
			fmt.Fprintln(cmd.OutOrStdout(), "\nEnter this token in the console pairing form.")
			if !noBrowser {
				if err := openConsoleBrowser(consoleURL); err != nil {
					return fmt.Errorf("open browser: %w", err)
				}
			}
			return nil
		},
	}
	open.Flags().BoolVar(&noBrowser, "no-browser", false, "display a pairing token without launching a browser")
	rotate := &cobra.Command{
		Use:   "rotate-secret",
		Short: "Rotate the console secret and invalidate every browser session",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			absolute, err := filepath.Abs(*configPath)
			if err != nil {
				return err
			}
			cfg, err := config.Load(absolute)
			if err != nil {
				return err
			}
			if !cfg.History.Enabled {
				return fmt.Errorf("console requires history.enabled")
			}
			svc, _, err := newService(absolute, false)
			if err != nil {
				return err
			}
			status, statusErr := svc.Status()
			if statusErr != nil && !errors.Is(statusErr, service.ErrNotInstalled) {
				return fmt.Errorf("inspect service status: %w", statusErr)
			}
			if statusErr == nil && status != service.StatusStopped {
				return fmt.Errorf("console secret rotation requires the multirunner service to be stopped")
			}
			secretPath := consoleauth.SecretPath(cfg.History.DatabasePath)
			current, err := consoleauth.LoadSecret(secretPath)
			if err != nil {
				return fmt.Errorf("load console secret before rotation: %w", err)
			}
			activeRestore, err := restore.HasActiveHandoff(cfg.History.DatabasePath, current)
			if err != nil {
				return fmt.Errorf("inspect restore handoff: %w", err)
			}
			if activeRestore {
				return fmt.Errorf("refusing console secret rotation while a restore is staged or activating")
			}
			activeUpdate, err := update.HasActiveHandoff(cfg.History.DatabasePath+".updates", current)
			if err != nil {
				return fmt.Errorf("inspect update handoff: %w", err)
			}
			if activeUpdate {
				return fmt.Errorf("refusing console secret rotation while an update is staged or activating")
			}
			if _, err := consoleauth.RotateSecret(secretPath); err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), "Console secret rotated. All browser sessions were invalidated; restart the service before opening the console.")
			return nil
		},
	}
	command.AddCommand(open, rotate)
	return command
}

func openConsoleBrowser(url string) error {
	name, args := consoleBrowserCommand(runtime.GOOS, url)
	return exec.Command(name, args...).Start()
}

func consoleBrowserCommand(goos, url string) (string, []string) {
	switch goos {
	case "windows":
		return "rundll32", []string{"url.dll,FileProtocolHandler", url}
	case "darwin":
		return "open", []string{url}
	default:
		return "xdg-open", []string{url}
	}
}
