// Package config defines the multirunner configuration schema and loader.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	imageversions "github.com/GerardSmit/multirunner/images"
	"github.com/GerardSmit/multirunner/internal/winvm"
	"gopkg.in/yaml.v3"
)

// Scope is the GitHub level a runner registers against.
type Scope string

const (
	ScopeRepo       Scope = "repo"
	ScopeRepos      Scope = "repos"
	ScopeOrg        Scope = "org"
	ScopeEnterprise Scope = "enterprise"
)

// Provisioning selects how runner slots are triggered.
type Provisioning string

const (
	ProvisioningPool      Provisioning = "pool"
	ProvisioningAutoscale Provisioning = "autoscale"
	ProvisioningWebhook   Provisioning = "webhook" // alias for autoscale (kept for compatibility)
	// ProvisioningScaleset lets GitHub drive capacity through a runner scale
	// set. Unlike pool and autoscale, the decision is not made here: a
	// long-poll session reports the desired runner count, which is the same
	// mechanism actions-runner-controller uses.
	ProvisioningScaleset Provisioning = "scaleset"
)

// IsAutoscale reports whether the provisioning mode scales on demand.
func (p Provisioning) IsAutoscale() bool {
	return p == ProvisioningAutoscale || p == ProvisioningWebhook
}

// IsScaleset reports whether GitHub drives capacity through a scale set.
func (p Provisioning) IsScaleset() bool {
	return p == ProvisioningScaleset
}

// Config is the root configuration.
type Config struct {
	GitHub       GitHub       `yaml:"github"`
	Auth         Auth         `yaml:"auth"`
	Provisioning Provisioning `yaml:"provisioning"`
	Cache        Cache        `yaml:"cache"`
	GitCache     GitCache     `yaml:"git_cache"`
	Webhook      Webhook      `yaml:"webhook"`
	Metrics      Metrics      `yaml:"metrics"`
	Pools        []Pool       `yaml:"pools"`
	Log          Log          `yaml:"log"`
}

// Webhook configures the workflow_job webhook receiver (provisioning: webhook).
type Webhook struct {
	Listen          string `yaml:"listen"`
	Path            string `yaml:"path"`
	Secret          string `yaml:"secret"`
	PollIntervalSec int    `yaml:"poll_interval_sec"` // 0 = default 300s; <0 disables polling
}

// Metrics configures the Prometheus metrics + health endpoint.
type Metrics struct {
	Listen string `yaml:"listen"` // empty disables it
}

// GitCache configures the host-resident bare-mirror git cache.
//   - mirror:       bind-mount the bare mirror into the runner (Docker/containers).
//   - dotgit-cache: serve the mirror as a git bundle over the cache server; a
//     runner job-started hook seeds the workspace from it so
//     checkout fetches only the delta. Works where mounts can't
//     (the QEMU VM), riding the cache server.
type GitCache struct {
	Mode string `yaml:"mode"` // mirror | dotgit-cache | off
	Path string `yaml:"path"`
	// MaxAgeDays removes bare mirrors not used (cloned/fetched/bundled) within
	// this many days. 0 => use the default; negative => never prune.
	MaxAgeDays int `yaml:"max_age_days"`
}

// Enabled reports whether any git mirror cache is active (both modes maintain a
// host bare mirror).
func (g GitCache) Enabled() bool {
	return (g.Mode == "mirror" || g.Mode == "dotgit-cache") && g.Path != ""
}

// DotGit reports whether the bundle-over-cache-server mode is selected.
func (g GitCache) DotGit() bool { return g.Mode == "dotgit-cache" && g.Path != "" }

// GitHub identifies the target and scope.
type GitHub struct {
	URL   string   `yaml:"url"`
	Scope Scope    `yaml:"scope"`
	Owner string   `yaml:"owner"`
	Repo  string   `yaml:"repo"`
	Repos []string `yaml:"repos"` // only for scope=repos: "repo" or "owner/repo"
}

// RepoRef is a resolved owner/repo pair from the repos list.
type RepoRef struct {
	Owner string
	Repo  string
}

// ParseRepoRef splits a repos entry into owner and repo. If the entry contains
// a slash, it is treated as "owner/repo". Otherwise the default owner is used.
func ParseRepoRef(entry, defaultOwner string) RepoRef {
	if i := strings.IndexByte(entry, '/'); i > 0 && i < len(entry)-1 {
		return RepoRef{Owner: entry[:i], Repo: entry[i+1:]}
	}
	return RepoRef{Owner: defaultOwner, Repo: entry}
}

// ResolvedRepos returns each repos entry parsed into owner/repo pairs.
func (gh GitHub) ResolvedRepos() []RepoRef {
	refs := make([]RepoRef, len(gh.Repos))
	for i, entry := range gh.Repos {
		refs[i] = ParseRepoRef(entry, gh.Owner)
	}
	return refs
}

// RepoTargets returns the concrete repositories available to repository-level
// diagnostics for both singular and plural repository scopes.
func (gh GitHub) RepoTargets() []RepoRef {
	switch gh.Scope {
	case ScopeRepo:
		return []RepoRef{{Owner: gh.Owner, Repo: gh.Repo}}
	case ScopeRepos:
		return gh.ResolvedRepos()
	default:
		return nil
	}
}

// Auth holds either a PAT or GitHub App credentials. PAT takes precedence when set.
type Auth struct {
	PAT            string `yaml:"pat"`
	AppID          int64  `yaml:"app_id"`
	InstallationID int64  `yaml:"installation_id"`
	PrivateKeyPath string `yaml:"private_key_path"`
}

// IsApp reports whether GitHub App auth is configured.
func (a Auth) IsApp() bool { return a.PAT == "" && a.AppID != 0 }

// Cache configures the self-hosted cache server.
type Cache struct {
	Enabled             bool   `yaml:"enabled"`
	Mode                string `yaml:"mode"` // local-server | off
	Storage             string `yaml:"storage"`
	Path                string `yaml:"path"`
	Listen              string `yaml:"listen"`
	AdvertiseURL        string `yaml:"advertise_url"` // URL of this cache as seen from inside runner containers
	ExternalURL         string `yaml:"external_url"`  // if set, use an already-running cache here instead of starting the embedded server
	AccessToken         string `yaml:"access_token"`  // optional shared path token for cache API URLs; generated when omitted
	SkipTokenValidation bool   `yaml:"skip_token_validation"`
	Upstream            string `yaml:"upstream"`
	// Housekeeping (garbage collection of stored entries):
	MaxAgeDays    int `yaml:"max_age_days"`    // evict entries unused this many days (0 => default; <0 => never)
	MaxSizeGB     int `yaml:"max_size_gb"`     // LRU-evict to keep total blob size under this cap (0 => unlimited)
	GCIntervalSec int `yaml:"gc_interval_sec"` // sweep cadence (0 => default 3600; <0 => disabled)
}

// Pool is one per-OS pool of ephemeral runner slots.
type Pool struct {
	Name                   string     `yaml:"name"`
	OS                     string     `yaml:"os"`      // linux | windows
	Backend                string     `yaml:"backend"` // docker (default) | containerd | qemu
	Size                   int        `yaml:"size"`
	ImageTier              string     `yaml:"image_tier"`
	Image                  string     `yaml:"image"`
	QEMU                   QEMU       `yaml:"qemu"`
	Containerd             Containerd `yaml:"containerd"`
	RunnerGroupID          int64      `yaml:"runner_group_id"`
	Labels                 []string   `yaml:"labels"`
	WorkFolder             string     `yaml:"work_folder"`
	NamePrefix             string     `yaml:"name_prefix"`
	Docker                 Docker     `yaml:"docker"`
	ToolCache              ToolCache  `yaml:"tool_cache"`
	MaxConsecutiveFailures int        `yaml:"max_consecutive_failures"`
	// ScaleSet names the runner scale set that feeds this pool when
	// provisioning is "scaleset". Each pool needs its own, because a scale set
	// carries one set of labels and therefore one runner OS.
	ScaleSet string `yaml:"scale_set"`
	// RunnerGroup is the runner group the scale set is created in. Empty means
	// the default group.
	RunnerGroup string `yaml:"runner_group"`
}

// publishedFlavors lists the per-OS image flavors CI builds and pushes as tags
// on gerardsmit/multirunner-runner-<os>. A pool's image_tier naming one of these
// resolves to the published tag; unknown tiers fall back to a local :dev build.
var publishedFlavors = newPublishedFlavors()

// newPublishedFlavors builds the flavor table, deriving the per-line Build Tools
// entries from the embedded image manifest so adding a release line there is the
// only edit needed.
func newPublishedFlavors() map[string]map[string]string {
	windows := map[string]string{
		"node":       "node",
		"dotnet":     "dotnet",
		"buildtools": "buildtools",
	}
	// Docker tags cannot contain a second colon, so image_tier buildtools:<line>
	// maps to the :buildtools-<line> tag CI pushes for that manifest line.
	for _, line := range imageversions.MustEmbedded().BuildTools.ReleaseLines() {
		windows["buildtools:"+line] = "buildtools-" + line
	}
	return map[string]map[string]string{
		"linux": {
			"native-build": "native-build",
			"node":         "node",
			"dotnet":       "dotnet",
			"rust":         "rust",
			"go":           "go",
		},
		"windows": windows,
	}
}

// validImageTiers lists the tiers accepted for an OS: minimal, then the
// published flavors sorted.
func validImageTiers(goos string) []string {
	tiers := make([]string, 0, len(publishedFlavors[goos])+1)
	for tier := range publishedFlavors[goos] {
		tiers = append(tiers, tier)
	}
	sort.Strings(tiers)
	return append([]string{"minimal"}, tiers...)
}

// ImageRef resolves the container image for a pool, in priority order:
//   - an explicit image: wins outright;
//   - tier "" / "minimal" -> the published :latest base image;
//   - a known published flavor -> the published :<flavor> tag;
//   - any other tier -> a local multirunner/runner-<os>-<tier>:dev build.
func (p Pool) ImageRef() string {
	if p.Image != "" {
		return p.Image
	}
	tier := p.ImageTier
	if tier == "" || tier == "minimal" {
		// Published image (built + pushed by CI), auto-pulled on first run — no
		// local build needed for the common case.
		return "gerardsmit/multirunner-runner-" + p.OS + ":latest"
	}
	if tag := publishedFlavors[p.OS][tier]; tag != "" {
		return "gerardsmit/multirunner-runner-" + p.OS + ":" + tag
	}
	return "multirunner/runner-" + p.OS + "-" + tier + ":dev"
}

// localImageTierPattern is Docker's grammar for one repository path component,
// which is where an unpublished tier ends up in the local :dev reference.
var localImageTierPattern = regexp.MustCompile(`^[a-z0-9]+(?:(?:[._]|__|-+)[a-z0-9]+)*$`)

// maxLocalImageTierLength keeps the generated local reference well inside
// Docker's 255-character limit for the full repository name.
const maxLocalImageTierLength = 64

// validateImageTier rejects a tier that has no published flavor for the pool's
// OS and cannot be expressed as a local :dev build either, because it does not
// form a valid Docker repository component. Left unchecked those tiers only
// fail at launch time, as an "invalid reference format" from the daemon that
// names no pool and wastes a JIT runner registration. An unknown tier that is
// valid keeps falling back to a local build — a supported dev workflow.
func (p Pool) validateImageTier() error {
	if p.Image != "" || p.Backend == "qemu" {
		return nil
	}
	tier := p.ImageTier
	if tier == "" || tier == "minimal" || publishedFlavors[p.OS][tier] != "" {
		return nil
	}
	if len(tier) <= maxLocalImageTierLength && localImageTierPattern.MatchString(tier) {
		return nil
	}
	return fmt.Errorf("pools[%q].image_tier %q is not published for os=%s and cannot be built locally "+
		"(a local tier must be lowercase letters and digits separated by '.', '_' or '-', at most %d characters); valid tiers: %s",
		p.Name, tier, p.OS, maxLocalImageTierLength, strings.Join(validImageTiers(p.OS), ", "))
}

// ToolCachePath is the hostedtoolcache directory for the pool's OS.
func (p Pool) ToolCachePath() string {
	if p.OS == "windows" {
		return `C:\hostedtoolcache\windows`
	}
	return "/opt/hostedtoolcache"
}

// DockerSocketPath is the in-container docker socket path for DinD.
func (p Pool) DockerSocketPath() string {
	return "/var/run/docker.sock"
}

// QEMU configures the x86-64 Windows VM backend.
type QEMU struct {
	Golden  string `yaml:"golden"`   // path to the golden qcow2 (built by `multirunner bake`)
	WorkDir string `yaml:"work_dir"` // where per-job overlays/ISOs are written
	MemMB   int    `yaml:"mem_mb"`
	CPUs    int    `yaml:"cpus"`
	Accel   string `yaml:"accel"` // "" = auto (hardware acceleration on x86-64; TCG on ARM)
	// Housekeeping (golden eval license + rebuilds):
	BakeISO       string   `yaml:"bake_iso"`        // Windows ISO for rebuilds (enables auto-rebuild)
	BakeISOSHA256 string   `yaml:"bake_iso_sha256"` // optional expected SHA256 of bake_iso
	RunnerVersion string   `yaml:"runner_version"`  // runner version to bake
	RunnerSHA256  string   `yaml:"runner_sha256"`   // required with a non-default runner_version
	Licensed      bool     `yaml:"licensed"`        // real key/KMS -> skip eval housekeeping
	Tools         []string `yaml:"tools"`           // golden selectors: dotnet[:major] | node[:major] | go | buildtools[:line]
}

// Containerd configures the containerd/runhcs Windows-container backend. The
// runner is launched via nerdctl; isolation auto-detects (process on Server,
// hyperv on client) when left empty.
type Containerd struct {
	Address   string `yaml:"address"`   // containerd pipe (default \\.\pipe\containerd-containerd)
	Nerdctl   string `yaml:"nerdctl"`   // path to nerdctl.exe ("" => from PATH)
	Namespace string `yaml:"namespace"` // containerd namespace (default "multirunner")
	Isolation string `yaml:"isolation"` // process | hyperv | auto (default)
}

// Docker configures a pool's backend daemon.
type Docker struct {
	Host        string `yaml:"host"`
	EnableDinD  bool   `yaml:"enable_dind"`
	Isolation   string `yaml:"isolation"`    // process | hyperv | auto (default, windows)
	WindowsDinD string `yaml:"windows_dind"` // off | host-pipe | hyperv
}

// ToolCache configures hostedtoolcache sharing.
type ToolCache struct {
	Mode     string `yaml:"mode"` // shared-volume | off
	Volume   string `yaml:"volume"`
	ReadOnly bool   `yaml:"readonly"`
}

// Log configures logging output.
type Log struct {
	Level  string `yaml:"level"`
	Format string `yaml:"format"`
}

// Load reads and validates a YAML config file, applying defaults.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	// Load .env (config dir, then CWD) so ${VAR} refs resolve without an explicit
	// export. Real environment variables always take precedence.
	loadDotEnv(filepath.Dir(path))
	var c Config
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	c.resolveSecrets()
	c.applyDefaults()
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

// resolveSecrets expands ${VAR} / $VAR references in sensitive fields so secrets
// can be supplied via environment variables instead of being written to disk.
func (c *Config) resolveSecrets() {
	c.Auth.PAT = expandEnvRef(c.Auth.PAT)
	c.Auth.PrivateKeyPath = expandEnvRef(c.Auth.PrivateKeyPath)
	c.Webhook.Secret = expandEnvRef(c.Webhook.Secret)
	c.Cache.AccessToken = expandEnvRef(c.Cache.AccessToken)
}

// expandEnvRef resolves a value of the form "${NAME}" or "$NAME" to the named
// environment variable. Values without a leading '$' are returned unchanged so
// literal secrets still work.
func expandEnvRef(v string) string {
	if !strings.HasPrefix(v, "$") {
		return v
	}
	name := strings.TrimPrefix(v, "$")
	name = strings.TrimPrefix(name, "{")
	name = strings.TrimSuffix(name, "}")
	if resolved, ok := os.LookupEnv(name); ok {
		return resolved
	}
	return ""
}

func (c *Config) applyDefaults() {
	c.GitHub.Owner = strings.TrimSpace(c.GitHub.Owner)
	for i := range c.GitHub.Repos {
		c.GitHub.Repos[i] = strings.TrimSpace(c.GitHub.Repos[i])
	}
	if c.GitHub.URL == "" {
		c.GitHub.URL = "https://github.com"
	}
	if c.Provisioning == "" {
		c.Provisioning = ProvisioningPool
	}
	if c.Provisioning.IsAutoscale() {
		if c.Webhook.Path == "" {
			c.Webhook.Path = "/webhook"
		}
		if c.Webhook.PollIntervalSec == 0 {
			c.Webhook.PollIntervalSec = 300
		}
	}
	if c.Log.Level == "" {
		c.Log.Level = "info"
	}
	if c.Log.Format == "" {
		c.Log.Format = "text"
	}
	if c.Cache.Enabled {
		if c.Cache.Mode == "" {
			c.Cache.Mode = "local-server"
		}
		if c.Cache.Storage == "" {
			c.Cache.Storage = "filesystem"
		}
		if c.Cache.Listen == "" {
			c.Cache.Listen = "0.0.0.0:3000"
		}
		if c.Cache.Upstream == "" {
			c.Cache.Upstream = "https://results-receiver.actions.githubusercontent.com"
		}
		if c.Cache.MaxAgeDays == 0 {
			c.Cache.MaxAgeDays = 7
		}
		if c.Cache.GCIntervalSec == 0 {
			c.Cache.GCIntervalSec = 3600
		}
	}
	if c.GitCache.Enabled() && c.GitCache.MaxAgeDays == 0 {
		c.GitCache.MaxAgeDays = 30
	}
	for i := range c.Pools {
		p := &c.Pools[i]
		if p.Size == 0 {
			p.Size = 1
		}
		if p.WorkFolder == "" {
			p.WorkFolder = "_work"
		}
		if p.NamePrefix == "" {
			p.NamePrefix = "multirunner"
		}
		if p.RunnerGroupID == 0 {
			p.RunnerGroupID = 1
		}
		if p.ImageTier == "" {
			p.ImageTier = "minimal"
		}
		if p.MaxConsecutiveFailures == 0 {
			p.MaxConsecutiveFailures = 5
		}
		// Windows isolation is intentionally left empty here. The backend
		// resolves "" / "auto" via autoIsolation() (process on Server, hyperv
		// on client), matching the containerd backend. Defaulting to "process"
		// here broke client editions, where process isolation needs an exact
		// host/image build match.
	}
}

// Warnings returns non-fatal configuration smells worth surfacing at startup.
// Unlike Validate (which rejects broken configs), these are settings that are
// silently ineffective — e.g. an image flavor on a QEMU pool, which boots a
// baked golden image and ignores image/image_tier entirely.
func (c *Config) Warnings() []string {
	var w []string
	if c.GitHub.Scope == ScopeRepos && c.GitHub.Repo != "" {
		w = append(w, "github.repo is ignored when scope=repos; use github.repos instead")
	}
	for i := range c.Pools {
		p := &c.Pools[i]
		if p.Backend == "qemu" && (p.Image != "" || (p.ImageTier != "" && p.ImageTier != "minimal")) {
			w = append(w, fmt.Sprintf(
				"pool %q: backend=qemu ignores image/image_tier (it boots the baked golden image); "+
					"bake toolchains into the golden instead (multirunner bake --tools ...)", p.Name))
		}
	}
	return w
}

// Validate checks required fields and cross-field consistency.
func (c *Config) Validate() error {
	switch c.GitHub.Scope {
	case ScopeRepo:
		if c.GitHub.Owner == "" || c.GitHub.Repo == "" {
			return fmt.Errorf("github.owner and github.repo are required for scope=repo")
		}
	case ScopeRepos:
		if len(c.GitHub.Repos) == 0 {
			return fmt.Errorf("github.repos must list at least one repo for scope=repos")
		}
		// owner is optional when every entry uses explicit "owner/repo" format.
		seenRepos := make(map[string]struct{}, len(c.GitHub.Repos))
		var appOwner string
		for _, entry := range c.GitHub.Repos {
			parts := strings.Split(entry, "/")
			if len(parts) > 2 || entry == "" {
				return fmt.Errorf("invalid github.repos entry %q: expected repo or owner/repo", entry)
			}
			for _, part := range parts {
				if part == "" || strings.TrimSpace(part) != part || strings.ContainsAny(part, " \t\r\n") {
					return fmt.Errorf("invalid github.repos entry %q: owner and repo must be non-empty and contain no whitespace", entry)
				}
			}
			ref := ParseRepoRef(entry, c.GitHub.Owner)
			if ref.Owner == "" || ref.Repo == "" {
				return fmt.Errorf("github.owner is required (or use owner/repo format) for repos entry %q", entry)
			}
			key := strings.ToLower(ref.Owner + "/" + ref.Repo)
			if _, ok := seenRepos[key]; ok {
				return fmt.Errorf("duplicate github.repos entry %q", entry)
			}
			seenRepos[key] = struct{}{}
			if c.Auth.IsApp() {
				if appOwner == "" {
					appOwner = ref.Owner
				} else if !strings.EqualFold(appOwner, ref.Owner) {
					return fmt.Errorf("scope=repos with GitHub App auth requires every repo to belong to one installation account; %q and %q differ", appOwner, ref.Owner)
				}
			}
		}
	case ScopeOrg, ScopeEnterprise:
		if c.GitHub.Owner == "" {
			return fmt.Errorf("github.owner is required for scope=%s", c.GitHub.Scope)
		}
	default:
		return fmt.Errorf("github.scope must be one of repo|repos|org|enterprise, got %q", c.GitHub.Scope)
	}

	if c.Auth.PAT == "" {
		if c.Auth.AppID == 0 || c.Auth.InstallationID == 0 || c.Auth.PrivateKeyPath == "" {
			return fmt.Errorf("auth requires either pat or (app_id + installation_id + private_key_path)")
		}
	}

	if c.Provisioning != ProvisioningPool && !c.Provisioning.IsAutoscale() && !c.Provisioning.IsScaleset() {
		return fmt.Errorf("provisioning must be pool|autoscale|webhook|scaleset, got %q", c.Provisioning)
	}

	if len(c.Pools) == 0 {
		return fmt.Errorf("at least one pool is required")
	}
	seen := map[string]bool{}
	for i := range c.Pools {
		p := &c.Pools[i]
		if p.Name == "" {
			return fmt.Errorf("pools[%d].name is required", i)
		}
		if seen[p.Name] {
			return fmt.Errorf("duplicate pool name %q", p.Name)
		}
		seen[p.Name] = true
		if p.OS != "linux" && p.OS != "windows" {
			return fmt.Errorf("pools[%q].os must be linux|windows, got %q", p.Name, p.OS)
		}
		if p.Backend == "qemu" {
			if p.QEMU.Golden == "" {
				return fmt.Errorf("pools[%q].qemu.golden is required for backend: qemu", p.Name)
			}
			// Without this a typo'd selector is only caught when an auto-rebuild
			// runs, and never at all when bake_iso is unset.
			if err := winvm.ValidateToolSelectors(p.QEMU.Tools); err != nil {
				return fmt.Errorf("pools[%q].qemu.tools: %w", p.Name, err)
			}
		} else if p.Docker.Host == "" {
			return fmt.Errorf("pools[%q].docker.host is required", p.Name)
		}
		if p.Size < 1 {
			return fmt.Errorf("pools[%q].size must be >= 1", p.Name)
		}
		if c.Provisioning.IsScaleset() && p.ScaleSet == "" {
			// Without this the pool would start, hold a session against nothing,
			// and never receive an assignment. Fail at startup instead.
			return fmt.Errorf("pools[%q].scale_set is required for provisioning: scaleset", p.Name)
		}
		if err := p.validateImageTier(); err != nil {
			return err
		}
	}

	if c.Provisioning.IsScaleset() {
		seen := make(map[string]string, len(c.Pools))
		for _, p := range c.Pools {
			if prev, dup := seen[p.ScaleSet]; dup {
				return fmt.Errorf("pools[%q] and pools[%q] share scale_set %q; each pool needs its own", prev, p.Name, p.ScaleSet)
			}
			seen[p.ScaleSet] = p.Name
		}
	}
	return nil
}
