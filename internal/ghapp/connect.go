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
	"net/netip"
	"net/url"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// Options configures the connect flow.
type Options struct {
	BaseURL string        // https://github.com (or GHES base)
	APIBase string        // https://api.github.com (or GHES <base>/api/v3)
	Scope   string        // "org" | "repo" | "user"
	Org     string        // org login (org scope)
	Owner   string        // repo owner (repo scope)
	Repo    string        // repo name (repo scope)
	Name    string        // desired App name
	Port    int           // local callback port (0 = auto)
	Needs   ManifestNeeds // what the host will do; drives permissions/events/hook
}

// ManifestNeeds describes what the host will do with the App, which determines
// the permissions and events the manifest is allowed to request. The scope is
// taken from Options.Scope, not duplicated here.
type ManifestNeeds struct {
	WebhookURL string // public https URL; empty means no active webhook
	PollQueue  bool   // repo-scope autoscale polls workflow runs/jobs
	DetectRepo bool   // `multirunner detect` reads trees/contents outside repo scope
}

// permissions derives the App permissions from the scope and what the host will
// do. metadata is always read; runner administration follows the scope;
// actions:read is added whenever we poll the job queue or subscribe to
// workflow_job (the event and the runs/jobs endpoints both require it). Repo
// scope always gets contents:read because doctor's workflow scan reads
// .github/workflows; other scopes only need it for `detect`.
func permissions(scope string, n ManifestNeeds) map[string]string {
	perms := map[string]string{"metadata": "read"}
	if scope == "repo" {
		perms["administration"] = "write"
	} else {
		perms["organization_self_hosted_runners"] = "write"
	}
	if n.PollQueue || n.WebhookURL != "" {
		perms["actions"] = "read"
	}
	if n.DetectRepo || scope == "repo" {
		perms["contents"] = "read"
	}
	return perms
}

// events subscribes to workflow_job only when a real receiver is configured;
// workflow_job additionally requires actions:read, which permissions() adds.
func (n ManifestNeeds) events() []string {
	if n.WebhookURL != "" {
		return []string{"workflow_job"}
	}
	return []string{}
}

// hookAttributes returns the manifest hook block. GitHub rejects a manifest
// whose hook URL is not publicly reachable even when active is false, so an
// inactive public placeholder is used when no real receiver is configured yet.
func (n ManifestNeeds) hookAttributes() map[string]any {
	if n.WebhookURL != "" {
		return map[string]any{"url": n.WebhookURL, "active": true}
	}
	return map[string]any{"url": "https://example.com/github/events", "active": false}
}

// cgnatV4 is the RFC 6598 shared address space (100.64.0.0/10), which carriers
// and overlay networks such as Tailscale hand out; it is not publicly routable.
var cgnatV4 = netip.MustParsePrefix("100.64.0.0/10")

// ValidateWebhookURL rejects a webhook URL that GitHub cannot deliver to, so the
// failure surfaces before a browser opens rather than as a manifest rejection.
// connect only targets GitHub.com today (BaseURL is hardcoded), so requiring a
// publicly reachable https endpoint is correct.
func ValidateWebhookURL(raw string) error {
	unreachable := func(what string) error {
		return fmt.Errorf("webhook URL %s; GitHub requires a publicly reachable https URL, so expose your local receiver with a tunnel or reverse proxy", what)
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("invalid webhook URL %q: %w", raw, err)
	}
	if u.Scheme != "https" {
		return unreachable("must use https")
	}
	if u.User != nil {
		return unreachable("must not embed credentials")
	}
	host := strings.TrimSuffix(u.Hostname(), ".")
	if host == "" {
		return unreachable("must include a host")
	}

	if addr, err := netip.ParseAddr(host); err == nil {
		addr = addr.Unmap()
		if addr.Zone() != "" ||
			addr.IsLoopback() || addr.IsPrivate() || addr.IsUnspecified() ||
			addr.IsLinkLocalUnicast() || addr.IsLinkLocalMulticast() ||
			addr.IsMulticast() || addr.IsInterfaceLocalMulticast() ||
			cgnatV4.Contains(addr) {
			return unreachable(fmt.Sprintf("host %q is not publicly reachable", host))
		}
		return nil
	}

	lower := strings.ToLower(host)
	switch {
	case lower == "localhost",
		lower == "home.arpa",
		!strings.Contains(lower, "."), // single-label hosts are not public
		strings.HasSuffix(lower, ".localhost"),
		strings.HasSuffix(lower, ".local"),
		strings.HasSuffix(lower, ".internal"),
		strings.HasSuffix(lower, ".lan"),
		strings.HasSuffix(lower, ".home.arpa"):
		return unreachable(fmt.Sprintf("host %q is not publicly reachable", host))
	}
	return nil
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
	// Validate before opening a listener or browser so a library caller cannot
	// produce the issue-#22 manifest with an unreachable hook URL.
	if opt.Needs.WebhookURL != "" {
		if err := ValidateWebhookURL(opt.Needs.WebhookURL); err != nil {
			return nil, err
		}
	}
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

	manifest := buildManifest(opt, opt.Needs, base)
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
		return nil, fmt.Errorf("App created but timed out waiting for installation; install it and re-run with the printed app_id")
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return result, nil
}

// manifestMap builds the GitHub App manifest. json.Marshal emits map keys in
// sorted order, so both the POSTed form and the dry-run rendering are stable.
func manifestMap(opt Options, needs ManifestNeeds, callbackBase string) map[string]any {
	appURL := strings.TrimRight(opt.BaseURL, "/") + "/"
	if opt.Org != "" {
		appURL += opt.Org
	} else if opt.Owner != "" {
		appURL += opt.Owner
	}
	return map[string]any{
		"name":                opt.Name,
		"url":                 appURL,
		"redirect_url":        callbackBase + "/callback",
		"setup_url":           callbackBase + "/setup",
		"setup_on_update":     false,
		"public":              false,
		"default_permissions": permissions(opt.Scope, needs),
		"default_events":      needs.events(),
		"hook_attributes":     needs.hookAttributes(),
	}
}

func buildManifest(opt Options, needs ManifestNeeds, callbackBase string) string {
	b, _ := json.Marshal(manifestMap(opt, needs, callbackBase))
	return string(b)
}

// ManifestJSON renders the manifest that would be POSTed, indented, for review
// before any browser opens.
func ManifestJSON(opt Options, needs ManifestNeeds, callbackBase string) (string, error) {
	b, err := json.MarshalIndent(manifestMap(opt, needs, callbackBase), "", "  ")
	return string(b), err
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
