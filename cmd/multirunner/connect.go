package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"

	"github.com/GerardSmit/multirunner/internal/config"
	"github.com/GerardSmit/multirunner/internal/ghapp"
)

// connectCmd runs the GitHub App manifest flow and writes the resulting App
// credentials into the config file (App auth, no PAT).
func connectCmd(cfgPath, org, repo, name string, port int, keyOut string) error {
	var scope config.Scope
	var owner, repoName string
	switch {
	case org != "":
		scope, owner = config.ScopeOrg, org
	case repo != "":
		parts := strings.SplitN(repo, "/", 2)
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			return fmt.Errorf("--repo must be owner/repo")
		}
		scope, owner, repoName = config.ScopeRepo, parts[0], parts[1]
	default:
		return fmt.Errorf("specify --org <org> or --repo <owner/repo>")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	creds, err := ghapp.Connect(ctx, ghapp.Options{
		Scope: string(scope), Org: org, Owner: owner, Repo: repoName, Name: name, Port: port,
	})
	if err != nil {
		return err
	}

	if keyOut == "" {
		keyOut = filepath.Join(filepath.Dir(cfgPath), "multirunner-app.private-key.pem")
	}
	if err := os.WriteFile(keyOut, []byte(creds.PEM), 0o600); err != nil {
		return fmt.Errorf("write private key: %w", err)
	}
	if err := config.WriteAppAuth(cfgPath, scope, owner, repoName, creds.AppID, creds.InstallationID, keyOut); err != nil {
		return fmt.Errorf("write config: %w", err)
	}

	return writeConnectSuccess(os.Stdout, cfgPath, keyOut, creds)
}

func writeConnectSuccess(w io.Writer, cfgPath, keyOut string, creds *ghapp.Credentials) error {
	_, err := fmt.Fprintf(w, `
Connected. App %q (id=%d) installed (installation=%d).
  private key : %s
  config      : %s (GitHub App authentication configured)
  app settings: %s
  webhook     : configure webhook mode separately if needed

Run:  multirunner run --config %s
`, creds.Slug, creds.AppID, creds.InstallationID, keyOut, cfgPath, creds.HTMLURL, cfgPath)
	return err
}
