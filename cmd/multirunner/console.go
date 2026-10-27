package main

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"

	"github.com/spf13/cobra"

	"github.com/GerardSmit/multirunner/internal/config"
	"github.com/GerardSmit/multirunner/internal/consoleipc"
)

func consoleCmd(configPath *string) *cobra.Command {
	var noBrowser bool
	command := &cobra.Command{
		Use:   "console",
		Short: "Open the local Operations Console",
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
				return fmt.Errorf("operations console is disabled; set history.enabled to true in %s", absolute)
			}
			pairingContext, cancel := context.WithTimeout(cmd.Context(), 5*time.Second)
			defer cancel()
			pairing, err := consoleipc.RequestPairing(pairingContext, cfg.History.DatabasePath)
			if err != nil {
				return err
			}

			fmt.Fprintln(cmd.OutOrStdout(), "Operations Console:", pairing.URL)
			fmt.Fprintf(
				cmd.OutOrStdout(),
				"One-time token (expires %s):\n%s\n",
				pairing.ExpiresAt.Local().Format(time.RFC1123),
				pairing.Token,
			)
			if noBrowser {
				fmt.Fprintln(cmd.OutOrStdout(), "\nOpen the console URL on this host and enter the token.")
				return nil
			}
			if err := openConsoleBrowser(pairing.URL); err != nil {
				return fmt.Errorf(
					"open the default browser: %w; open %s manually before the token expires",
					err, pairing.URL,
				)
			}
			fmt.Fprintln(cmd.OutOrStdout(), "\nBrowser opened. Enter the token to continue.")
			return nil
		},
	}
	command.Flags().BoolVar(
		&noBrowser, "no-browser", false,
		"display the console URL and pairing token without launching a browser",
	)
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
