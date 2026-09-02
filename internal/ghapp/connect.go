// Package ghapp implements the GitHub App "manifest" connect flow: it creates a
// GitHub App in the user's org/account (no pre-registration needed), captures
// the generated credentials, and captures the installation id after the user
// installs the App. This gives multirunner production-grade App auth without a
// hand-made PAT.
package ghapp

import (
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"net"
	"net/http"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// Options configures the connect flow.
type Options struct {
	BaseURL string // https://github.com (or GHES base)
	APIBase string // https://api.github.com (or GHES <base>/api/v3)
	Scope   string // "org" | "repo" | "user"
	Org     string // org login (org scope)
	Owner   string // repo owner (repo scope)
	Repo    string // repo name (repo scope)
	Name    string // desired App name
	Port    int    // local callback port (0 = auto)
}

// Credentials is what the flow yields.
type Credentials struct {
	AppID          int64
	Slug           string
	PEM            string
	WebhookSecret  string
	InstallationID int64
	HTMLURL        string
}

// Connect runs the full browser flow and returns the App credentials.
func Connect(ctx context.Context, opt Options) (*Credentials, error) {
	if opt.BaseURL == "" {
		opt.BaseURL = "https://github.com"
	}
	if opt.APIBase == "" {
		opt.APIBase = "https://api.github.com"
	}
	if opt.Name == "" {
		opt.Name = "multirunner"
	}

	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", opt.Port))
	if err != nil {
		return nil, fmt.Errorf("listen: %w", err)
	}
	defer ln.Close()
	base := "http://" + ln.Addr().String()

	creds := make(chan *Credentials, 1)
	installID := make(chan int64, 1)
	errc := make(chan error, 1)

	manifest := buildManifest(opt, base)
	createURL := createAppURL(opt)

	mux := http.NewServeMux()
	// Landing page: auto-POST the manifest to GitHub's "create app" form.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		_ = manifestFormTmpl.Execute(w, map[string]string{"Action": createURL, "Manifest": manifest, "State": "multirunner"})
	})
	// GitHub redirects here with ?code= after the App is created.
	mux.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
		code := r.URL.Query().Get("code")
		if code == "" {
			http.Error(w, "missing code", http.StatusBadRequest)
			return
		}
		c, err := exchangeManifest(ctx, opt.APIBase, code)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			errc <- err
			return
		}
		creds <- c
		installURL := fmt.Sprintf("%s/apps/%s/installations/new", strings.TrimRight(opt.BaseURL, "/"), c.Slug)
		_ = redirectTmpl.Execute(w, map[string]string{
			"Title": "App created", "Message": "GitHub App created. Continue to install it on your " + opt.Scope + ".",
			"URL": installURL,
		})
	})
	// GitHub redirects here (App "setup URL") with ?installation_id= after install.
	mux.HandleFunc("/setup", func(w http.ResponseWriter, r *http.Request) {
		idStr := r.URL.Query().Get("installation_id")
		id, _ := strconv.ParseInt(idStr, 10, 64)
		if id != 0 {
			installID <- id
		}
		_ = donePageTmpl.Execute(w, map[string]string{"Title": "Connected", "Message": "multirunner is connected to GitHub. You can close this tab."})
	})

	srv := &http.Server{Handler: mux}
	go func() {
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			errc <- err
		}
	}()
	defer srv.Close()

	fmt.Printf("Opening browser to create a GitHub App (%s)...\n", createURL)
	fmt.Printf("If it does not open, visit: %s\n", base)
	_ = openBrowser(base)

	var result *Credentials
	select {
	case result = <-creds:
	case err := <-errc:
		return nil, err
	case <-time.After(5 * time.Minute):
		return nil, fmt.Errorf("timed out waiting for App creation")
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	fmt.Println("App created; waiting for you to install it...")
	select {
	case id := <-installID:
		result.InstallationID = id
	case <-time.After(5 * time.Minute):
		//lint:ignore ST1005 App is the GitHub product noun in this user-facing message.
		return nil, fmt.Errorf("App created but timed out waiting for installation; install it and re-run with the printed app_id")
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return result, nil
}

func buildManifest(opt Options, callbackBase string) string {
	perms := map[string]string{}
	switch opt.Scope {
	case "repo":
		perms["administration"] = "write"
		perms["contents"] = "read"
	default: // org / user
		perms["organization_self_hosted_runners"] = "write"
	}
	url := strings.TrimRight(opt.BaseURL, "/") + "/"
	if opt.Org != "" {
		url += opt.Org
	} else if opt.Owner != "" {
		url += opt.Owner
	}
	m := map[string]any{
		"name":                opt.Name,
		"url":                 url,
		"redirect_url":        callbackBase + "/callback",
		"setup_url":           callbackBase + "/setup",
		"setup_on_update":     false,
		"public":              false,
		"default_permissions": perms,
		"default_events":      []string{"workflow_job"},
		"hook_attributes":     map[string]any{"url": callbackBase + "/webhook", "active": false},
	}
	b, _ := json.Marshal(m)
	return string(b)
}

func createAppURL(opt Options) string {
	b := strings.TrimRight(opt.BaseURL, "/")
	if opt.Scope == "org" && opt.Org != "" {
		return fmt.Sprintf("%s/organizations/%s/settings/apps/new", b, opt.Org)
	}
	return b + "/settings/apps/new"
}

func exchangeManifest(ctx context.Context, apiBase, code string) (*Credentials, error) {
	url := fmt.Sprintf("%s/app-manifests/%s/conversions", strings.TrimRight(apiBase, "/"), code)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("exchange manifest: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		return nil, fmt.Errorf("exchange manifest: status %d", resp.StatusCode)
	}
	var out struct {
		ID            int64  `json:"id"`
		Slug          string `json:"slug"`
		PEM           string `json:"pem"`
		WebhookSecret string `json:"webhook_secret"`
		HTMLURL       string `json:"html_url"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return &Credentials{
		AppID: out.ID, Slug: out.Slug, PEM: out.PEM,
		WebhookSecret: out.WebhookSecret, HTMLURL: out.HTMLURL,
	}, nil
}

func openBrowser(url string) error {
	switch runtime.GOOS {
	case "windows":
		return exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
	case "darwin":
		return exec.Command("open", url).Start()
	default:
		return exec.Command("xdg-open", url).Start()
	}
}

var manifestFormTmpl = template.Must(template.New("form").Parse(`<!doctype html><html><body>
<p>Redirecting to GitHub to create the multirunner App…</p>
<form id="f" action="{{.Action}}" method="post">
  <input type="hidden" name="manifest" value='{{.Manifest}}'>
  <input type="hidden" name="state" value="{{.State}}">
  <noscript><button type="submit">Create GitHub App</button></noscript>
</form>
<script>document.getElementById('f').submit()</script>
</body></html>`))

var redirectTmpl = template.Must(template.New("redir").Parse(`<!doctype html><html><body>
<h3>{{.Title}}</h3><p>{{.Message}}</p>
<p><a id="go" href="{{.URL}}">Continue →</a></p>
<script>location.href={{.URL}}</script>
</body></html>`))

var donePageTmpl = template.Must(template.New("done").Parse(`<!doctype html><html><body>
<h3>{{.Title}}</h3><p>{{.Message}}</p></body></html>`))
