package main

import (
	"errors"
	"fmt"
	"net/http"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"

	"github.com/kardianos/service"
	"github.com/spf13/cobra"

	"github.com/GerardSmit/multirunner/internal/config"
	"github.com/GerardSmit/multirunner/internal/consoleauth"
	"github.com/GerardSmit/multirunner/internal/restore"
	"github.com/GerardSmit/multirunner/internal/update"
)

func consoleCmd(configPath *string) *cobra.Command {
	var noBrowser bool
	runOpen := func(cmd *cobra.Command, _ []string) error {
		absolute, err := filepath.Abs(*configPath)
		if err != nil {
			return err
		}
		cfg, err := config.Load(absolute)
		if err != nil {
			return err
		}
		if !cfg.History.Enabled {
			return fmt.Errorf("operations console is disabled; set history.enabled to true in %s", absolute)
		}
		consoleURL := "http://" + cfg.History.Listen + "/"
		if err := requireConsoleReachable(consoleURL); err != nil {
			return err
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

		fmt.Fprintln(cmd.OutOrStdout(), "Operations Console:", consoleURL)
		fmt.Fprintln(cmd.OutOrStdout(), "One-time token (valid for 2 minutes):")
		fmt.Fprintln(cmd.OutOrStdout(), token)
		if noBrowser {
			fmt.Fprintln(cmd.OutOrStdout(), "\nOpen the console URL on this host and enter the token.")
			return nil
		}
		if err := openConsoleBrowser(consoleURL); err != nil {
			return fmt.Errorf(
				"open the default browser: %w; open %s manually before the token expires",
				err, consoleURL,
			)
		}
		fmt.Fprintln(cmd.OutOrStdout(), "\nBrowser opened. Enter the token to continue.")
		return nil
	}
	command := &cobra.Command{
		Use:   "console",
		Short: "Open the local Operations Console",
		Args:  cobra.NoArgs,
		RunE:  runOpen,
	}
	command.Flags().BoolVar(
		&noBrowser, "no-browser", false,
		"display the console URL and pairing token without launching a browser",
	)
	open := &cobra.Command{
		Use:   "open",
		Short: "Open the local Operations Console",
		Args:  cobra.NoArgs,
		RunE:  runOpen,
	}
	open.Flags().BoolVar(
		&noBrowser, "no-browser", false,
		"display the console URL and pairing token without launching a browser",
	)
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

func requireConsoleReachable(consoleURL string) error {
	client := &http.Client{
		Timeout: 2 * time.Second,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	response, err := client.Get(consoleURL + "api/v1/session")
	if err != nil {
		return fmt.Errorf(
			"operations console is not reachable at %s: %w; start Multirunner and try again",
			consoleURL, err,
		)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		return fmt.Errorf(
			"unexpected service response from %sapi/v1/session: %s",
			consoleURL, response.Status,
		)
	}
	return nil
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
