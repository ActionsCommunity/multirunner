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
func connectTarget(org, repo string) (scope config.Scope, owner, repoName string, err error) {
	switch {
	case org != "" && repo != "":
		return "", "", "", fmt.Errorf("specify exactly one of --org or --repo")
	case org != "":
		scope, owner = config.ScopeOrg, org
	case repo != "":
		parts := strings.SplitN(repo, "/", 2)
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			return "", "", "", fmt.Errorf("--repo must be owner/repo")
		}
		scope, owner, repoName = config.ScopeRepo, parts[0], parts[1]
	default:
		return "", "", "", fmt.Errorf("specify --org <org> or --repo <owner/repo>")
	}
	return scope, owner, repoName, nil
}

func connectCmd(cfgPath, org, repo, name string, port int, keyOut string) error {
	scope, owner, repoName, err := connectTarget(org, repo)
	if err != nil {
		return err
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

func connectPlan(w io.Writer, cfgPath, org, repo, name string, port int, keyOut string) error {
	scope, owner, repoName, err := connectTarget(org, repo)
	if err != nil {
		return err
	}
	if keyOut == "" {
		keyOut = filepath.Join(filepath.Dir(cfgPath), "multirunner-app.private-key.pem")
	}
	target := owner
	if repoName != "" {
		target += "/" + repoName
	}
	callback := "automatic local port"
	if port != 0 {
		callback = fmt.Sprintf("local port %d", port)
	}
	permissions := "organization_self_hosted_runners=write"
	if scope == config.ScopeRepo {
		permissions = "administration=write, contents=read"
	}
	_, err = fmt.Fprintf(w, `Dry run: no changes will be made.
Target:       %s (%s)
App name:     %s
Callback:     %s
Permissions:  %s; event=workflow_job; webhook initially inactive
Private key:  %s
Config:       %s
GitHub:       open browser; create and install a private App
Local writes: private key; github scope/owner/repo; App credentials; remove auth.pat
Apply with:   multirunner connect --%s "%s" --name "%s" --config "%s"
`, target, scope, name, callback, permissions, keyOut, cfgPath, scope, target, name, cfgPath)
	return err
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
