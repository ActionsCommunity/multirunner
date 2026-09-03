package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/GerardSmit/multirunner/internal/backend"
	"github.com/GerardSmit/multirunner/internal/config"
	"github.com/GerardSmit/multirunner/internal/ghapp"
)

// connectFlags carries the `connect` command-line options through the plan and
// apply paths.
type connectFlags struct {
	org        string
	repo       string
	name       string
	keyOut     string
	port       int
	webhookURL string
	detect     bool
}

// manifestOnlyFlags are the flags that only make sense for the --own-app
// manifest flow, which creates a dedicated App. The default device flow creates
// no App, so setting any of them there is a mistake worth rejecting.
var manifestOnlyFlags = []string{"webhook-url", "detect", "name", "key-out", "port"}

// stepIndent aligns a step's detail lines under the "[n/N] " heading prefix.
const stepIndent = "      "

// steps renders the interactive flow as numbered "[n/N] Title" headings. The
// total is fixed up front and the counter lives here so the numbering cannot
// drift out of sync as steps are added, reordered, or skipped at a call site.
type steps struct {
	out   io.Writer
	total int
	n     int
}

func newSteps(out io.Writer, total int) *steps {
	return &steps{out: out, total: total}
}

// begin advances to the next step, prints its heading, and returns a writer for
// the step's indented detail lines.
func (s *steps) begin(title string) *stepDetail {
	s.n++
	fmt.Fprintf(s.out, "\n[%d/%d] %s\n", s.n, s.total, title)
	return &stepDetail{out: s.out}
}

// stepDetail writes lines indented under the current step heading.
type stepDetail struct{ out io.Writer }

func (d *stepDetail) printf(format string, a ...any) {
	fmt.Fprint(d.out, stepIndent)
	fmt.Fprintf(d.out, format, a...)
	fmt.Fprintln(d.out)
}

func (d *stepDetail) blank() { fmt.Fprintln(d.out) }

// wantOwnApp asks, on an interactive terminal, which authentication model to use.
// It renders the credential-ownership question as the first step and returns true
// for the --own-app manifest flow, false for the shared-App device flow. The
// device flow is both the empty-answer choice and the non-interactive default (in
// which case nothing is printed and stdin is never read). The two options differ
// in who owns the credential and what happens when a person leaves the org, which
// the user cannot infer and cannot cheaply change later, so it is the one question
// connect still asks.
func wantOwnApp(in io.Reader, s *steps, interactive bool) bool {
	if !interactive {
		return false
	}
	d := s.begin("Authentication")
	d.printf(`Shared app  Authorize the public "Multirunner Connect" app with a device`)
	d.printf(`            code. Nothing to create, no browser callback, works over SSH.`)
	d.printf(`            The credential is yours: it ends when your org access does.`)
	d.printf(`            Its publisher can mint tokens for every org that installs it.`)
	d.printf(`Own app     Create a dedicated GitHub App in the org via your browser.`)
	d.printf(`            The credential belongs to the org and outlives you, and no`)
	d.printf(`            third party can act on it.`)
	d.blank()
	return !newPrompt(in, s.out).yesNo(stepIndent+"Use the shared app?", true)
}

// rejectManifestFlags fails when manifest-only flags were set on the device
// path. misused is the subset of manifestOnlyFlags the user explicitly changed.
func rejectManifestFlags(misused []string) error {
	if len(misused) == 0 {
		return nil
	}
	verb := "are"
	if len(misused) == 1 {
		verb = "is"
	}
	return fmt.Errorf("%s %s meaningless in the default device flow, which creates no GitHub App; pass --own-app to use the manifest flow that honors %s",
		strings.Join(misused, ", "), verb, map[bool]string{true: "it", false: "them"}[len(misused) == 1])
}

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

// validateWebhookFlags rejects contradictory or unreachable webhook flags. It is
// called before any interactive prompt so a bad flag fails immediately.
func validateWebhookFlags(f connectFlags) error {
	if f.webhookURL != "" {
		return ghapp.ValidateWebhookURL(f.webhookURL)
	}
	return nil
}

// prompt reads interactive input from a supplied reader/writer so the flow stays
// unit-testable and never touches os.Stdin/os.Stdout directly.
type prompt struct {
	r   *bufio.Reader
	out io.Writer
}

func newPrompt(in io.Reader, out io.Writer) *prompt {
	return &prompt{r: bufio.NewReader(in), out: out}
}

func (p *prompt) line(question string) string {
	fmt.Fprint(p.out, question)
	s, _ := p.r.ReadString('\n')
	return strings.TrimSpace(s)
}

func (p *prompt) yesNo(question string, def bool) bool {
	suffix := " [y/N]: "
	if def {
		suffix = " [Y/n]: "
	}
	switch strings.ToLower(p.line(question + suffix)) {
	case "y", "yes":
		return true
	case "n", "no":
		return false
	default:
		return def
	}
}

// webhookURL prompts for a public https webhook URL, re-prompting on an invalid
// entry. A blank answer means "configure later" and yields an empty string.
func (p *prompt) webhookURL(configListen string) string {
	if configListen != "" {
		fmt.Fprintf(p.out, "Config sets webhook.listen=%s (a local bind address, not a public URL).\n", configListen)
	}
	for {
		v := p.line("Public https URL for workflow_job delivery (blank to configure later): ")
		if v == "" {
			return ""
		}
		if err := ghapp.ValidateWebhookURL(v); err != nil {
			fmt.Fprintf(p.out, "  %v\n", err)
			continue
		}
		return v
	}
}

// resolveTarget determines the App scope/owner/repo, prompting interactively when
// no target flag was supplied.
func resolveTarget(f connectFlags, p *prompt, interactive bool) (config.Scope, string, string, error) {
	org, repo := f.org, f.repo
	if org == "" && repo == "" && interactive {
		switch strings.ToLower(p.line("Scope - create an [o]rg or [r]epo App? [o/r]: ")) {
		case "o", "org":
			org = p.line("Org login: ")
		case "r", "repo":
			repo = p.line("owner/repo: ")
		}
	}
	return connectTarget(org, repo)
}

// deriveNeeds resolves what the App will do from flags, then the config file,
// then interactive prompts, then safe defaults.
func deriveNeeds(scope config.Scope, f connectFlags, cfgPath string, p *prompt, interactive bool) (ghapp.ManifestNeeds, error) {
	if err := validateWebhookFlags(f); err != nil {
		return ghapp.ManifestNeeds{}, err
	}
	var needs ghapp.ManifestNeeds
	hints, hintsOK := config.ReadConnectHints(cfgPath)

	// Only autoscale consumes workflow_job. Pool and scaleset (the default) never
	// receive a delivery, so asking them for a public URL is asking for something
	// they must not supply. An unknown config means no autoscale has been chosen
	// yet, which is also not a reason to ask.
	switch {
	case f.webhookURL != "":
		needs.WebhookURL = f.webhookURL
	case interactive && hintsOK && hints.Provisioning.IsAutoscale():
		needs.WebhookURL = p.webhookURL(hints.WebhookListen)
	case hintsOK && hints.Provisioning.IsAutoscale() && scope == config.ScopeOrg:
		// Org autoscale has no polling fallback (the queue endpoints are
		// per-repository), so it only works over workflow_job deliveries. Without
		// a URL the manifest would omit both the event and actions:read, and
		// adding them later needs an App edit and a fresh org approval — so this
		// fails now rather than producing an App that cannot autoscale.
		return ghapp.ManifestNeeds{}, fmt.Errorf("config %s sets provisioning: %s for an org target, which needs webhook delivery;\n"+
			"pass --webhook-url <public https URL>, or run connect on a terminal to be asked for one",
			cfgPath, hints.Provisioning)
	}

	// A repo-scope autoscale host polls the workflow-run/job endpoints, which need
	// actions:read. An unset provisioning key (or a missing/unparseable config) is
	// treated as unknown, not as "not autoscale", so repo scope keeps the
	// permissive default and does not 403 after the operator later enables
	// autoscale (which would otherwise force an App-permission edit + re-approval).
	if scope == config.ScopeRepo {
		provisioningKnown := hintsOK && hints.Provisioning != ""
		needs.PollQueue = !provisioningKnown || hints.Provisioning.IsAutoscale()
	}

	// contents:read is opt-in for org targets and implied by repo scope, so it is
	// a flag rather than a question: the common path should ask nothing.
	needs.DetectRepo = f.detect
	return needs, nil
}

func connectCmd(cfgPath string, f connectFlags, in io.Reader, out io.Writer, interactive bool, st *steps) error {
	return runOwnAppConnect(cfgPath, f, in, out, interactive, st, ghapp.Connect)
}

// runOwnAppConnect drives the manifest flow. connect is injected so the numbered
// output and local writes can be exercised in tests without opening a browser.
func runOwnAppConnect(cfgPath string, f connectFlags, in io.Reader, out io.Writer, interactive bool, st *steps, connect func(context.Context, ghapp.Options) (*ghapp.Credentials, error)) error {
	// Validate flag-only inputs before any prompt, so a bad --webhook-url fails
	// immediately rather than after the scope/target questions.
	if err := validateWebhookFlags(f); err != nil {
		return err
	}
	p := newPrompt(in, out)
	scope, owner, repoName, err := resolveTarget(f, p, interactive)
	if err != nil {
		return err
	}
	needs, err := deriveNeeds(scope, f, cfgPath, p, interactive)
	if err != nil {
		return err
	}

	// A nil tracker means the auth question was not asked (--own-app or
	// non-interactive), so the flow is just create + save.
	if st == nil {
		st = newSteps(out, 2)
	}

	orgLogin := ""
	if scope == config.ScopeOrg {
		orgLogin = owner
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	d := st.begin("Create and install the App on GitHub")
	d.printf("A browser opens GitHub's create-App page. Create the App, then install it")
	d.printf("on %s when GitHub prompts you.", connectTargetLabel(owner, repoName))
	// Only org scope can address an org's create-App page; a repo target lands on
	// the personal one, which produces a user-owned App. That is the opposite of
	// what --own-app is for, so it is stated rather than discovered later.
	if scope == config.ScopeRepo {
		d.printf("The App is created under your personal account. To have %s own it instead,", owner)
		d.printf("create it at %s/organizations/%s/settings/apps/new and install it there.", ghapp.DefaultBaseURL, owner)
	}
	creds, err := connect(ctx, ghapp.Options{
		Scope: string(scope), Org: orgLogin, Owner: owner, Repo: repoName,
		Name: f.name, Port: f.port, Needs: needs,
	})
	if err != nil {
		return err
	}

	st.begin("Save credentials")

	// The App now exists on GitHub. Every failure past this point must report the
	// identifiers so the operator recovers by hand instead of re-running connect,
	// which would create a duplicate App.
	postConnectErr := func(what string, cause error) error {
		return fmt.Errorf("%s: %w\n"+
			"The GitHub App was already created (slug=%q app_id=%d installation_id=%d). "+
			"Do not re-run connect (it creates a duplicate); finish setup by hand with these identifiers.",
			what, cause, creds.Slug, creds.AppID, creds.InstallationID)
	}

	keyOut := f.keyOut
	if keyOut == "" {
		keyOut = filepath.Join(filepath.Dir(cfgPath), "multirunner-app.private-key.pem")
	}
	if err := ghapp.WriteSecretFile(keyOut, []byte(creds.PEM)); err != nil {
		return postConnectErr("write private key", err)
	}

	// A webhook-secret write failure must not abort before the config is written,
	// or app_id/installation_id would never be persisted. Warn instead; the secret
	// can be regenerated in the App settings.
	var webhookSecretPath, webhookSecretWarning string
	if creds.WebhookSecret != "" {
		webhookSecretPath = filepath.Join(filepath.Dir(keyOut), "multirunner-app.webhook-secret")
		if err := ghapp.WriteSecretFile(webhookSecretPath, []byte(creds.WebhookSecret)); err != nil {
			webhookSecretPath = ""
			webhookSecretWarning = fmt.Sprintf("could not save the webhook secret (%v); regenerate it under App settings > Webhook secret", err)
		}
	}

	if err := config.WriteAppAuth(cfgPath, scope, owner, repoName, creds.AppID, creds.InstallationID, keyOut); err != nil {
		return postConnectErr("write config", err)
	}
	addStarterPool(ctx, cfgPath, out)

	return writeConnectSuccess(out, cfgPath, keyOut, webhookSecretPath, webhookSecretWarning, needs.WebhookURL, creds)
}

func connectPlan(w io.Writer, cfgPath string, f connectFlags) error {
	scope, owner, repoName, err := connectTarget(f.org, f.repo)
	if err != nil {
		return err
	}
	needs, err := deriveNeeds(scope, f, cfgPath, nil, false)
	if err != nil {
		return err
	}
	keyOut := f.keyOut
	if keyOut == "" {
		keyOut = filepath.Join(filepath.Dir(cfgPath), "multirunner-app.private-key.pem")
	}
	target := owner
	if repoName != "" {
		target += "/" + repoName
	}
	callback := "automatic local port"
	if f.port != 0 {
		callback = fmt.Sprintf("local port %d", f.port)
	}
	orgLogin := ""
	if scope == config.ScopeOrg {
		orgLogin = owner
	}
	manifest, err := ghapp.ManifestJSON(ghapp.Options{
		BaseURL: "https://github.com", Scope: string(scope),
		Org: orgLogin, Owner: owner, Repo: repoName, Name: f.name,
	}, needs, "http://127.0.0.1:PORT")
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, `Dry run: no changes will be made.
Target:       %s (%s)
App name:     %s
Callback:     %s
Manifest (POSTed to GitHub):
%s
Private key:  %s
Config:       %s
GitHub:       open browser; create and install a private App
Local writes: private key; github scope/owner/repo; App credentials; remove auth.pat
Apply: rerun this command without --dry-run.
`, target, scope, f.name, callback, manifest, keyOut, cfgPath)
	return err
}

// deviceTokenPath is where the device flow stores the rotating user token: a
// JSON sidecar next to the config, mirroring the private key's placement.
func deviceTokenPath(cfgPath string) string {
	return filepath.Join(filepath.Dir(cfgPath), "multirunner-user-token.json")
}

// deviceFlow holds the network-touching steps of the device connect, injected so
// the numbered output and the install-wait stage can be exercised without
// contacting GitHub. clientID and baseURL are carried for config writing and for
// building the install URL.
type deviceFlow struct {
	requestCode  func(context.Context) (*ghapp.DeviceCode, error)
	pollToken    func(context.Context, *ghapp.DeviceCode) (*ghapp.UserToken, error)
	listInstalls func(context.Context, string) ([]ghapp.Installation, error)
	clientID     string
	appSlug      string
	baseURL      string
	pollInterval time.Duration
}

// sharedApp names one of the two shared Apps the device flow can authorize. They
// are separate Apps rather than one with both permissions, because an App holding
// repository administration:write would be asking every org that installs it for
// far more than org runner management needs.
type sharedApp struct {
	clientID string
	slug     string
}

// sharedAppFor picks the App whose permissions match the target: repository
// runners need repository administration:write, organization runners need
// organization_self_hosted_runners:write, and no App carries both.
func sharedAppFor(f connectFlags) sharedApp {
	if f.repo != "" {
		return sharedApp{clientID: ghapp.DefaultPersonalClientID, slug: ghapp.DefaultPersonalAppSlug}
	}
	return sharedApp{clientID: ghapp.DefaultClientID, slug: ghapp.DefaultAppSlug}
}

// connectDeviceCmd runs the GitHub App device flow against the shared App: it
// prints a user code, polls for authorization, confirms the App is installed on
// the target, saves the rotating user token, and writes device auth to config.
// No App is created, no browser redirect, and no local port is opened.
func connectDeviceCmd(cfgPath string, f connectFlags, misused []string, in io.Reader, out io.Writer, interactive bool, st *steps) error {
	if err := rejectManifestFlags(misused); err != nil {
		return err
	}
	app := sharedAppFor(f)
	clientID := app.clientID
	baseURL := ghapp.DefaultBaseURL
	apiBase := ghapp.DefaultAPIBase
	df := deviceFlow{
		requestCode: func(ctx context.Context) (*ghapp.DeviceCode, error) {
			return ghapp.RequestDeviceCode(ctx, clientID, baseURL)
		},
		pollToken: func(ctx context.Context, dc *ghapp.DeviceCode) (*ghapp.UserToken, error) {
			return ghapp.PollDeviceToken(ctx, clientID, baseURL, dc)
		},
		listInstalls: func(ctx context.Context, accessToken string) ([]ghapp.Installation, error) {
			return ghapp.UserInstallations(ctx, apiBase, accessToken)
		},
		clientID:     clientID,
		appSlug:      app.slug,
		baseURL:      baseURL,
		pollInterval: 3 * time.Second,
	}
	return runDeviceConnect(cfgPath, f, in, out, interactive, st, df)
}

func runDeviceConnect(cfgPath string, f connectFlags, in io.Reader, out io.Writer, interactive bool, st *steps, df deviceFlow) error {
	p := newPrompt(in, out)

	if err := requireDotComTarget(cfgPath, df.baseURL); err != nil {
		return err
	}

	// The target is resolved after authorization, not before: GitHub knows which
	// accounts installed the App, so asking the user to type a login they might
	// get wrong is worse than offering the list.
	var scope config.Scope
	var owner, repoName string
	var err error
	if f.org != "" || f.repo != "" {
		scope, owner, repoName, err = connectTarget(f.org, f.repo)
		if err != nil {
			return err
		}
	}

	// A nil tracker means the auth question was not asked (non-interactive, or the
	// user pre-chose with --own-app), so the flow is just authorize + organization.
	if st == nil {
		st = newSteps(out, 2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	dc, err := df.requestCode(ctx)
	if err != nil {
		return err
	}
	d := st.begin("Authorize on GitHub")
	d.printf("Open   %s", dc.VerificationURI)
	d.printf("Code   %s", dc.UserCode)
	d.blank()
	d.printf("Only enter a code you started yourself; never one someone sends you.")
	d.printf("Waiting for authorization...")

	tok, err := df.pollToken(ctx, dc)
	if err != nil {
		return err
	}

	d = st.begin("Organization")
	named := owner != ""
	installs, err := awaitInstallation(ctx, func(ctx context.Context) ([]ghapp.Installation, error) {
		return df.listInstalls(ctx, tok.AccessToken)
	}, owner, df.baseURL, df.appSlug, d, interactive, df.pollInterval)
	if err != nil {
		return err
	}
	// Only an org target has to be an organization: the repository App is
	// installed on whichever account owns the repo, personal or org.
	inst, err := selectInstallation(installs, owner, df.baseURL, df.appSlug, f.repo == "", p, interactive)
	if err != nil {
		return err
	}
	if !named {
		scope, owner = config.ScopeOrg, inst.Account
	}
	// Report the resolved account, except when selectInstallation already had the
	// user pick it from a list (several org installations, interactively).
	switch {
	case named:
		d.printf("using %s", inst.Account)
	case !interactive || countOrgInstalls(installs) == 1:
		d.printf("found %s", inst.Account)
	}

	tokenPath := deviceTokenPath(cfgPath)
	if err := ghapp.SaveUserToken(tokenPath, tok); err != nil {
		return err
	}
	if err := config.WriteDeviceAuth(cfgPath, scope, owner, repoName, df.clientID, tokenPath); err != nil {
		return fmt.Errorf("write config: %w\n"+
			"The user token was saved to %s; set auth.client_id and auth.token_path by hand to finish.", err, tokenPath)
	}
	addStarterPool(ctx, cfgPath, out)
	return writeDeviceConnectSuccess(out, cfgPath, tokenPath, scope, owner, repoName, inst)
}

// awaitInstallation resolves the installations connect will pick from. A user who
// authorized but has not installed the App is the common first-run state
// (GET /user/installations is empty). Interactively, connect waits for the install
// to appear rather than discarding the user access token it just obtained (which
// would force a fresh device authorization on the next run). Non-interactive
// callers deliberately do NOT wait: a script should fail fast with the install URL
// (surfaced by selectInstallation) and a non-zero exit rather than hang.
//
// list is injected so the wait can be exercised in tests without contacting
// GitHub; interval defaults to 3s.
func awaitInstallation(ctx context.Context, list func(context.Context) ([]ghapp.Installation, error), owner, baseURL, appSlug string, d *stepDetail, interactive bool, interval time.Duration) ([]ghapp.Installation, error) {
	installs, err := list(ctx)
	if err != nil {
		return nil, err
	}
	if !interactive || hasUsableInstall(installs, owner) {
		return installs, nil
	}

	if interval <= 0 {
		interval = 3 * time.Second
	}
	installURL := installNewURL(installs, baseURL, appSlug)
	d.printf("The app is not installed yet. Install it, then this continues automatically:")
	d.printf("  %s", installURL)
	d.printf("Waiting for an installation...")

	var lastNote string
	timeout := time.NewTimer(5 * time.Minute)
	defer timeout.Stop()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-timeout.C:
			return nil, fmt.Errorf("timed out waiting for the Multirunner Connect app to be installed.\n"+
				"Your authorization is still valid but was not saved, so re-running `multirunner connect` will require authorizing again.\n"+
				"Install the app, then re-run:\n  %s", installURL)
		case <-ticker.C:
		}
		installs, err = list(ctx)
		if err != nil {
			return nil, err
		}
		if hasUsableInstall(installs, owner) {
			return installs, nil
		}
		// An installation that appeared but cannot be used looks identical to no
		// installation at all from here, so say which one arrived and why it does
		// not count instead of waiting silently.
		if note := unusableNote(installs, owner); note != "" && note != lastNote {
			lastNote = note
			d.printf("%s", note)
		}
	}
}

// unusableNote explains why the installations seen so far still do not satisfy
// the target, or "" when there are none to explain.
func unusableNote(installs []ghapp.Installation, owner string) string {
	if len(installs) == 0 {
		return ""
	}
	accounts := make([]string, 0, len(installs))
	for _, in := range installs {
		accounts = append(accounts, in.Account)
	}
	list := strings.Join(accounts, ", ")
	if owner != "" {
		return fmt.Sprintf("Installed on %s, but this run targets %s. Install it there too, or re-run with --org/--repo naming one of those.", list, owner)
	}
	return fmt.Sprintf("Installed on %s, which is a personal account. Organization runners need the app installed on an organization.", list)
}

// hasUsableInstall reports whether installs already contains one connect can use:
// the named target when owner is set, otherwise any organization installation
// (a personal installation cannot manage org runners).
func hasUsableInstall(installs []ghapp.Installation, owner string) bool {
	if owner != "" {
		_, _, ok := ghapp.MatchInstallation(installs, owner)
		return ok
	}
	return countOrgInstalls(installs) > 0
}

func countOrgInstalls(installs []ghapp.Installation) int {
	n := 0
	for _, in := range installs {
		if in.IsOrg {
			n++
		}
	}
	return n
}

// installNewURL builds the "install this App" URL, preferring an App slug reported
// by an existing installation and falling back to the shared App's default slug.
func installNewURL(installs []ghapp.Installation, baseURL, fallbackSlug string) string {
	slug := orDefaultString(fallbackSlug, ghapp.DefaultAppSlug)
	for _, in := range installs {
		if in.AppSlug != "" {
			slug = in.AppSlug
		}
	}
	return fmt.Sprintf("%s/apps/%s/installations/new", strings.TrimRight(baseURL, "/"), slug)
}

func connectDevicePlan(w io.Writer, cfgPath string, f connectFlags, misused []string) error {
	if err := rejectManifestFlags(misused); err != nil {
		return err
	}
	scope, owner, repoName, err := connectTarget(f.org, f.repo)
	if err != nil {
		return err
	}
	target := owner
	if repoName != "" {
		target += "/" + repoName
	}
	app := sharedAppFor(f)
	_, err = fmt.Fprintf(w, `Dry run: no changes will be made.
Target:       %s (%s)
Auth:         GitHub App device flow (shared %q App)
Client ID:    %s
GitHub:       request a device code; you enter the user code at https://github.com/login/device and authorize the App
Install:      the App must be installed on %s; connect prints the install URL if it is not
Token store:  %s (mode 0600; holds the user access + refresh tokens, rotated on refresh)
Config:       %s
Local writes: user token sidecar; github scope/owner/repo; auth.client_id + auth.token_path; remove auth.pat and any installation-App keys
No App is created, no browser redirect, and no local callback port is opened.
Apply: rerun this command without --dry-run.
`, target, scope, app.slug, app.clientID, owner, deviceTokenPath(cfgPath), cfgPath)
	return err
}

// connectTargetLabel renders an owner/repo pair as a single target string.
func connectTargetLabel(owner, repo string) string {
	if repo != "" {
		return owner + "/" + repo
	}
	return owner
}

func writeDeviceConnectSuccess(w io.Writer, cfgPath, tokenPath string, scope config.Scope, owner, repo string, inst ghapp.Installation) error {
	var b strings.Builder
	fmt.Fprintf(&b, "\nConnected to %s (%s) via device flow.\n", connectTargetLabel(owner, repo), scope)
	fmt.Fprintf(&b, "  %-12s : %d\n", "installation", inst.ID)
	fmt.Fprintf(&b, "  %-12s : %s (mode 0600; rotates on refresh, never inlined into YAML)\n", "user token", tokenPath)
	fmt.Fprintf(&b, "  %-12s : %s (auth.client_id + auth.token_path written; auth.pat removed)\n", "config", cfgPath)
	// The repository App carries administration + metadata only, which registers
	// runners but not the reads the other modes want. Saying so here beats a 403
	// the first time someone switches to autoscale.
	if scope == config.ScopeRepo {
		fmt.Fprintf(&b, "  %-12s : registers runners only (administration + metadata). Repo autoscale polling\n", "limits")
		fmt.Fprintf(&b, "                 and doctor's workflow scan need actions/contents read: use --own-app for those.\n")
	}
	writeNextSteps(&b, cfgPath)
	_, err := io.WriteString(w, b.String())
	return err
}

func writeConnectSuccess(w io.Writer, cfgPath, keyOut, webhookSecretPath, webhookSecretWarning, webhookURL string, creds *ghapp.Credentials) error {
	var b strings.Builder
	fmt.Fprintf(&b, "\nConnected. App %q (id=%d) installed (installation=%d).\n", creds.Slug, creds.AppID, creds.InstallationID)
	fmt.Fprintf(&b, "  private key : %s\n", keyOut)
	fmt.Fprintf(&b, "  config      : %s (GitHub App authentication configured)\n", cfgPath)
	fmt.Fprintf(&b, "  app settings: %s\n", creds.HTMLURL)
	if webhookURL != "" {
		fmt.Fprintf(&b, "  webhook     : App subscribes to workflow_job and delivers to %s\n", webhookURL)
	} else {
		fmt.Fprintf(&b, "  webhook     : hook registered inactive with a placeholder URL. To enable webhook mode later,\n")
		fmt.Fprintf(&b, "                set the real public URL in the App settings and add the workflow_job event (needs actions:read).\n")
	}
	switch {
	case webhookSecretWarning != "":
		fmt.Fprintf(&b, "  WARNING     : %s\n", webhookSecretWarning)
	case webhookSecretPath != "":
		fmt.Fprintf(&b, "  webhook key : %s\n", webhookSecretPath)
		fmt.Fprintf(&b, "                (GitHub returns it once; point webhook.secret at an env var holding this file's contents)\n")
	}
	writeNextSteps(&b, cfgPath)
	_, err := io.WriteString(w, b.String())
	return err
}

// selectInstallation resolves which installation to use. A named target must
// match one. With no target, a single installation is taken silently and several
// are offered as a list, because GitHub already knows the valid answers and the
// user should not have to type one.
func selectInstallation(installs []ghapp.Installation, owner, baseURL, appSlug string, requireOrg bool, p *prompt, interactive bool) (ghapp.Installation, error) {
	installURL := installNewURL(installs, baseURL, appSlug)

	if owner != "" {
		inst, _, ok := ghapp.MatchInstallation(installs, owner)
		if !ok {
			return ghapp.Installation{}, fmt.Errorf("authorized, but the Multirunner Connect App is not installed on %q.\n"+
				"Install it, then re-run `multirunner connect`:\n  %s", owner, installURL)
		}
		// A personal-account installation carries no
		// organization_self_hosted_runners permission, so it cannot register org
		// runners however it was named. A repository target has no such
		// requirement: its App is installed wherever the repo lives.
		if requireOrg && !inst.IsOrg {
			return ghapp.Installation{}, fmt.Errorf("%q is a personal account, not an organization.\n"+
				"The shared App can only manage organization runners; install it on an organization,\n"+
				"or use `multirunner connect --own-app` for a personal repository:\n  %s", owner, installURL)
		}
		return inst, nil
	}

	// Only organization installations can manage org runners; a personal one
	// carries no organization_self_hosted_runners permission.
	var orgs []ghapp.Installation
	for _, in := range installs {
		if in.IsOrg {
			orgs = append(orgs, in)
		}
	}
	switch {
	case len(orgs) == 0:
		return ghapp.Installation{}, fmt.Errorf("authorized, but the Multirunner Connect App is not installed on any organization.\n"+
			"Install it, then re-run `multirunner connect`:\n  %s", installURL)
	case len(orgs) == 1:
		return orgs[0], nil
	case !interactive:
		names := make([]string, len(orgs))
		for i, in := range orgs {
			names[i] = in.Account
		}
		return ghapp.Installation{}, fmt.Errorf("the App is installed on %d organizations (%s); pass --org to choose one",
			len(orgs), strings.Join(names, ", "))
	}

	fmt.Fprintln(p.out, "\nThe App is installed on these organizations:")
	for i, in := range orgs {
		fmt.Fprintf(p.out, "  %d) %s\n", i+1, in.Account)
	}
	for {
		answer := p.line(fmt.Sprintf("Which organization should multirunner use? [1-%d]: ", len(orgs)))
		if n, err := strconv.Atoi(answer); err == nil && n >= 1 && n <= len(orgs) {
			return orgs[n-1], nil
		}
		for _, in := range orgs {
			if strings.EqualFold(answer, in.Account) {
				return in, nil
			}
		}
		fmt.Fprintf(p.out, "  enter a number between 1 and %d, or the organization name\n", len(orgs))
	}
}

// requireDotComTarget refuses shared-App device login when the config already
// points at another GitHub host. The App, its client id and its device endpoints
// only exist on github.com, so logging in there and then writing credentials into
// a config whose github.url names a GHES instance would send a github.com token
// to that host on every request.
func requireDotComTarget(cfgPath, baseURL string) error {
	hints, ok := config.ReadConnectHints(cfgPath)
	if !ok || hints.GitHubURL == "" {
		return nil
	}
	if sameGitHubHost(hints.GitHubURL, baseURL) {
		return nil
	}
	return fmt.Errorf("config %s targets %s, but the shared Multirunner Connect App only exists on %s.\n"+
		"Create an App on that host instead:  multirunner connect --own-app",
		cfgPath, hints.GitHubURL, baseURL)
}

// sameGitHubHost compares two GitHub base URLs by host, tolerating the
// www.github.com spelling and a trailing slash.
func sameGitHubHost(a, b string) bool {
	host := func(raw string) string {
		u, err := url.Parse(strings.TrimRight(raw, "/"))
		if err != nil || u.Host == "" {
			return strings.ToLower(strings.TrimRight(raw, "/"))
		}
		return strings.ToLower(strings.TrimPrefix(u.Host, "www."))
	}
	return host(a) == host(b)
}

// orDefaultString returns v, or def when v is empty.
func orDefaultString(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// writeNextSteps names the next command. connect writes a working starter pool
// into a config that has none, but its Docker endpoint is a platform default
// rather than something connect can verify, so the follow-up is doctor - which
// checks it - rather than run.
func writeNextSteps(b *strings.Builder, cfgPath string) {
	hints, ok := config.ReadConnectHints(cfgPath)
	if ok && hints.Pools > 0 {
		fmt.Fprintf(b, "\nNext:  multirunner doctor --config %s\n", cfgPath)
		fmt.Fprintf(b, "       (checks the pool connect wrote, including its docker.host)\n")
		fmt.Fprintf(b, "\nThen:  multirunner run --config %s\n", cfgPath)
		return
	}
	fmt.Fprintf(b, "\nNext:  multirunner run --config %s\n", cfgPath)
}

// addStarterPool gives a config with no pools a working one, using the container
// daemon actually reachable on this machine. Credentials alone do not run
// anything, and the endpoint is the one value that cannot be guessed from the
// GitHub side - so it is probed rather than assumed.
//
// A failure here is reported and then dropped: the credentials are already
// written, and connect must not fail after the part that cannot be repeated.
func addStarterPool(ctx context.Context, cfgPath string, out io.Writer) {
	if hints, ok := config.ReadConnectHints(cfgPath); ok && hints.Pools > 0 {
		return
	}
	host := backend.PickDockerHost(ctx, "linux")
	added, err := config.EnsureStarterPool(cfgPath, host)
	switch {
	case err != nil:
		fmt.Fprintf(out, "\nCould not add a starter pool to %s: %v\n", cfgPath, err)
	case added && host == "":
		fmt.Fprintf(out, "\nNo container daemon answered on this host, so the starter pool was written with\n"+
			"docker.host: %s as a placeholder. Start Docker or Podman, then set the real endpoint.\n",
			config.FallbackDockerHost())
	}
}
