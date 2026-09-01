// Command multirunner orchestrates a pool of ephemeral GitHub Actions
// self-hosted runners across one or more container backends on a single host.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/kardianos/service"
	"github.com/spf13/cobra"
	"golang.org/x/sync/errgroup"

	"github.com/GerardSmit/multirunner/internal/autoscale"
	"github.com/GerardSmit/multirunner/internal/backend"
	"github.com/GerardSmit/multirunner/internal/cache"
	"github.com/GerardSmit/multirunner/internal/config"
	"github.com/GerardSmit/multirunner/internal/gitcache"
	"github.com/GerardSmit/multirunner/internal/github"
	"github.com/GerardSmit/multirunner/internal/metrics"
	"github.com/GerardSmit/multirunner/internal/pool"
	scalesetmode "github.com/GerardSmit/multirunner/internal/scaleset"
	"github.com/GerardSmit/multirunner/internal/vmview"
	"github.com/GerardSmit/multirunner/internal/webhook"
	"github.com/GerardSmit/multirunner/internal/winsetup"
	"github.com/GerardSmit/multirunner/internal/winvm"
)

func main() {
	if err := rootCmd().Execute(); err != nil {
		os.Exit(1)
	}
}

const version = "0.1.0-dev"

// remoteCheckTimeout bounds doctor's GitHub API phase. Sized for the workflow
// scan, which is the only check that scales with repo count times workflow count
// rather than repo count alone.
const remoteCheckTimeout = 90 * time.Second
const remoteRepoCheckTimeout = 20 * time.Second

func rootCmd() *cobra.Command {
	var cfgPath string
	var installDeps bool

	root := &cobra.Command{
		Use:     "multirunner",
		Short:   "Parallel ephemeral GitHub Actions self-hosted runner pools",
		Version: version,
		Long: `multirunner runs many ephemeral GitHub Actions self-hosted runners in
parallel on one host. Each runner takes a single job, then is torn down and
re-provisioned with a fresh just-in-time registration. It also bundles a
self-hosted Actions cache (v2) so actions/cache stays on your host.

Run with no command to start the orchestrator (same as "run").`,
		Example: `  multirunner connect --org my-org      # one-time: create + install a GitHub App
  multirunner doctor                    # verify daemons are reachable
  multirunner run                       # start the runner pools`,
		SilenceUsage: true,
		Args:         cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runService(cfgPath, installDeps)
		},
	}
	root.PersistentFlags().StringVar(&cfgPath, "config", "config.yaml", "path to the YAML config file")
	root.Flags().BoolVar(&installDeps, "install-deps", false, "auto-install missing container daemons (elevates)")

	run := &cobra.Command{
		Use:   "run",
		Short: "Run the orchestrator (default)",
		Long: `Run the orchestrator: validate each pool's container daemon, start the
self-hosted cache (if enabled), and keep every pool's ephemeral runner slots
filled, re-provisioning each runner after it finishes its one job.

Runs identically in the foreground or under a service manager (see "service").`,
		Example: `  multirunner run --config config.yaml
  multirunner run --install-deps        # install a missing Windows daemon, then run`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runService(cfgPath, installDeps)
		},
	}
	run.Flags().BoolVar(&installDeps, "install-deps", false, "auto-install missing container daemons (elevates)")

	doctorC := &cobra.Command{
		Use:   "doctor",
		Short: "Check daemons, repository access, and scheduling without starting runners",
		Long: `Run preflight checks against the config: for every pool, confirm its Docker/
Podman daemon is reachable and running in the expected container mode (a Linux
daemon assigned to a Windows pool, or vice versa, is reported as a problem).
For repo/repositories scope, also verify that Actions is enabled and perform a
heuristic workflow scan for the self-hosted label. The heuristic can miss custom
label and matrix expressions, but incomplete API checks still fail preflight.
Exits non-zero if any required check is incomplete or misconfigured.`,
		Example: "  multirunner doctor --config config.yaml",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			abs, err := filepath.Abs(cfgPath)
			if err != nil {
				return err
			}
			return doctor(abs)
		},
	}

	var connOrg, connRepo, connName, connKeyOut string
	var connPort int
	connectC := &cobra.Command{
		Use:   "connect",
		Short: "Create + install a GitHub App and write its credentials to the config",
		Long: `Connect multirunner to GitHub using a GitHub App, without registering anything
in advance. A browser flow opens GitHub's "create App" page pre-filled with the
right permissions (self-hosted runner admin); after you create and install the
App, multirunner captures the App id, private key, installation id, and webhook
secret, writes the private key next to your config, and updates the config's
auth section to use the App (any existing pat is removed).

Specify exactly one target: --org for an org-scoped App, or --repo owner/name
for a repo-scoped App.`,
		Example: `  multirunner connect --org my-org
  multirunner connect --repo my-org/my-repo --config config.yaml`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			abs, err := filepath.Abs(cfgPath)
			if err != nil {
				return err
			}
			return connectCmd(abs, connOrg, connRepo, connName, connPort, connKeyOut)
		},
	}
	connectC.Flags().StringVar(&connOrg, "org", "", "GitHub org login (org-scoped App)")
	connectC.Flags().StringVar(&connRepo, "repo", "", "owner/repo (repo-scoped App)")
	connectC.Flags().StringVar(&connName, "name", "multirunner", "GitHub App name")
	connectC.Flags().IntVar(&connPort, "port", 0, "local callback port (0 = auto)")
	connectC.Flags().StringVar(&connKeyOut, "key-out", "", "path to write the App private key")

	var winDataRoot string
	winDaemon := &cobra.Command{
		Use:   "install-windows-daemon",
		Short: "Install the standalone Windows-container dockerd (elevates)",
		Long: `Install a standalone Moby dockerd in Windows-container mode as a service, so
multirunner can run Windows runners without Docker Desktop. Triggers a UAC
prompt, enables the Windows Containers feature (may require a reboot), and
registers the daemon on its own named pipe (npipe:////./pipe/docker_engine_windows)
so it coexists with Podman/Docker Desktop and the WSL Linux daemon. Windows only.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return winsetup.Install(winsetup.InstallOptions{DataRoot: winDataRoot})
		},
	}
	winDaemon.Flags().StringVar(&winDataRoot, "data-root", "",
		"where the daemon stores images and containers "+
			"(default %ProgramData%\\multirunner\\docker\\data); "+
			"point at an existing store to keep its images")

	installContainerd := &cobra.Command{
		Use:   "install-containerd",
		Short: "Install containerd + runhcs + nerdctl for Windows containers (elevates)",
		Long: `Install containerd, the runhcs shim, nerdctl, and the Windows CNI plugins as
a service so multirunner can run Windows-container runners (pool backend:
containerd). Triggers a UAC prompt, enables the Containers + Hyper-V features
(may require a reboot), and registers containerd on \\.\pipe\containerd-containerd.
Process isolation is used on Windows Server, Hyper-V isolation on client. Windows only.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return winsetup.InstallContainerd()
		},
	}

	root.AddCommand(run, doctorC, connectC, winDaemon, installContainerd, bakeCmd(), detectCmd(&cfgPath), screenshotCmd(), bootKeysCmd(), vmViewCmd(), jitISOCmd(), serviceCmd(&cfgPath))
	return root
}

// bootKeysCmd is a hidden debug helper: reset a VM and spam Enter to get past
// the "Press any key to boot from CD" prompt.
func bootKeysCmd() *cobra.Command {
	var qmp string
	var presses int
	c := &cobra.Command{
		Use: "_bootkeys", Hidden: true, Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return winvm.QMPBootKeys(qmp, presses, time.Second)
		},
	}
	c.Flags().StringVar(&qmp, "qmp", "127.0.0.1:4445", "QMP address")
	c.Flags().IntVar(&presses, "presses", 15, "number of Enter presses")
	return c
}

// vmViewCmd is a hidden debug helper: serve the noVNC viewer for a VM whose
// QEMU exposes websocket VNC at --ws-port.
func vmViewCmd() *cobra.Command {
	var httpAddr string
	var wsPort int
	c := &cobra.Command{
		Use: "_vmview", Hidden: true, Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
			defer stop()
			logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
			return vmview.Serve(ctx, httpAddr, wsPort, logger)
		},
	}
	c.Flags().StringVar(&httpAddr, "http", "127.0.0.1:8090", "viewer HTTP address")
	c.Flags().IntVar(&wsPort, "ws-port", 5701, "QEMU VNC websocket port")
	return c
}

// jitISOCmd is a hidden debug helper: build a JIT config ISO from a blob.
func jitISOCmd() *cobra.Command {
	var jit, out string
	c := &cobra.Command{
		Use: "_jitiso", Hidden: true, Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := winvm.BuildJITISO(out, jit, nil); err != nil {
				return err
			}
			fmt.Println("wrote " + out)
			return nil
		},
	}
	c.Flags().StringVar(&jit, "jit", "", "encoded JIT config blob")
	c.Flags().StringVar(&out, "out", "jit.iso", "output ISO path")
	return c
}

// screenshotCmd is a hidden debug helper: capture a QMP PNG screenshot of a VM.
func screenshotCmd() *cobra.Command {
	var qmp, out string
	c := &cobra.Command{
		Use: "_screenshot", Hidden: true, Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := winvm.QMPScreenshot(qmp, out); err != nil {
				return err
			}
			fmt.Println("wrote " + out)
			return nil
		},
	}
	c.Flags().StringVar(&qmp, "qmp", "127.0.0.1:4445", "QMP address")
	c.Flags().StringVar(&out, "out", "shot.png", "output PNG path")
	return c
}

// bakeCmd builds a golden Windows Server Core image for the QEMU backend.
func bakeCmd() *cobra.Command {
	var iso, isoSHA256, golden, accel, runnerVer, runnerSHA256, vncWeb string
	var diskGB, memMB, cpus int
	var licensed, prepareOnly bool
	var tools []string
	c := &cobra.Command{
		Use:   "bake",
		Short: "Build a golden Windows Server Core image for the QEMU (vm) backend",
		Long: `Build (or rebuild) the golden Windows Server Core qcow2 used by qemu pools:
creates a base disk, boots the Windows installer unattended, installs the runner
+ boot task + cache patch, then powers off. Needs QEMU and a Windows Server ISO.
Uses hardware acceleration on x86-64 hosts and TCG software emulation on ARM.

--prepare-only creates the base disk + autounattend ISO and prints the QEMU
command without running it (for manual/observed installs).`,
		Example: "  multirunner bake --iso ./WinServer2022.iso --golden /var/lib/multirunner/golden.qcow2",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			opts := winvm.BakeOptions{
				WindowsISO: iso, Golden: golden, DiskGB: diskGB, MemMB: memMB,
				CPUs: cpus, Accel: accel, RunnerVersion: runnerVer, Licensed: licensed,
				VNCWeb: vncWeb, Tools: tools, WindowsISOSHA256: isoSHA256, RunnerSHA256: runnerSHA256,
			}
			hash, err := winvm.ToolsHash(opts)
			if err != nil {
				return fmt.Errorf("bake inputs: %w", err)
			}
			opts.WorkflowsHash = hash
			if prepareOnly {
				autoISO, args, err := winvm.Prepare(cmd.Context(), &opts)
				if err != nil {
					return err
				}
				fmt.Printf("disk:        %s\nautounattend: %s\nqemu:        %s %s\n",
					opts.Golden, autoISO, opts.QEMUBin, strings.Join(args, " "))
				return nil
			}
			return winvm.Bake(cmd.Context(), opts, time.Now())
		},
	}
	c.Flags().BoolVar(&prepareOnly, "prepare-only", false, "create disk + autounattend ISO and print the QEMU command, don't run")
	c.Flags().StringVar(&iso, "iso", "", "path to a Windows Server ISO (required)")
	c.Flags().StringVar(&isoSHA256, "iso-sha256", "", "expected SHA256 of the Windows Server ISO")
	c.Flags().StringVar(&golden, "golden", "", "output golden qcow2 path (required)")
	c.Flags().IntVar(&diskGB, "disk-gb", 40, "golden disk size (GB)")
	c.Flags().IntVar(&memMB, "mem-mb", 4096, "install VM memory (MB)")
	c.Flags().IntVar(&cpus, "cpus", 2, "install VM vCPUs")
	c.Flags().StringVar(&accel, "accel", "", "QEMU accel: kvm|whpx|hvf|tcg ('' = auto)")
	c.Flags().StringVar(&runnerVer, "runner-version", winvm.DefaultRunnerVersion, "actions/runner version to bake in")
	c.Flags().StringVar(&runnerSHA256, "runner-sha256", "", "runner archive SHA256 (required for a non-default version)")
	c.Flags().StringSliceVar(&tools, "tools", nil, "toolchains to bake: dotnet[:major],node[:major],go,buildtools[:line]")
	c.Flags().BoolVar(&licensed, "licensed", false, "a real Windows key/KMS is configured (skip eval housekeeping)")
	c.Flags().StringVar(&vncWeb, "vnc-web", "127.0.0.1:8090", "serve a browser VNC viewer at host:port to watch the install (empty to disable)")
	return c
}

// serviceCmd builds `multirunner service {install,uninstall,start,stop,restart}`.
func serviceCmd(cfgPath *string) *cobra.Command {
	c := &cobra.Command{
		Use:   "service",
		Short: "Install/manage multirunner as an OS service",
		Long: `Manage multirunner as a native OS service: a Windows SCM service, a Linux
systemd unit (Debian/Ubuntu), or a macOS launchd job. "install" bakes the
resolved --config path into the service definition so the daemon finds it.
install/uninstall/start/stop require administrator/root.`,
		Example: `  sudo multirunner service install --config /etc/multirunner/config.yaml
  sudo multirunner service start`,
		Args: cobra.NoArgs,
	}
	shortFor := map[string]string{
		"install":   "Install multirunner as an OS service (admin/root)",
		"uninstall": "Remove the multirunner service (admin/root)",
		"start":     "Start the multirunner service (admin/root)",
		"stop":      "Stop the multirunner service (admin/root)",
		"restart":   "Restart the multirunner service (admin/root)",
	}
	for _, action := range []string{"install", "uninstall", "start", "stop", "restart"} {
		action := action
		c.AddCommand(&cobra.Command{
			Use:   action,
			Short: shortFor[action],
			Args:  cobra.NoArgs,
			RunE: func(cmd *cobra.Command, _ []string) error {
				abs, err := filepath.Abs(*cfgPath)
				if err != nil {
					return err
				}
				svc, _, err := newService(abs, false)
				if err != nil {
					return err
				}
				if err := service.Control(svc, action); err != nil {
					return err
				}
				fmt.Printf("service 'multirunner': %s ok\n", action)
				return nil
			},
		})
	}
	return c
}

// newService builds the kardianos service wrapper around the orchestrator.
func newService(absCfg string, installDeps bool) (service.Service, *program, error) {
	cfg := &service.Config{
		Name:             "multirunner",
		DisplayName:      "multirunner",
		Description:      "Parallel ephemeral GitHub Actions self-hosted runner pool",
		Arguments:        []string{"run", "--config", absCfg},
		WorkingDirectory: filepath.Dir(absCfg),
	}
	prg := &program{cfgPath: absCfg, installDeps: installDeps}
	svc, err := service.New(prg, cfg)
	if err != nil {
		return nil, nil, fmt.Errorf("service: %w", err)
	}
	return svc, prg, nil
}

// runService runs the orchestrator under the service lifecycle (works the same
// interactively and under systemd / launchd / Windows SCM).
func runService(cfgPath string, installDeps bool) error {
	abs, err := filepath.Abs(cfgPath)
	if err != nil {
		return err
	}
	svc, prg, err := newService(abs, installDeps)
	if err != nil {
		return err
	}
	prg.interactive = service.Interactive()
	return svc.Run()
}

// program adapts the orchestrator to the kardianos service lifecycle.
type program struct {
	cfgPath     string
	installDeps bool
	interactive bool
	cancel      context.CancelFunc
	done        chan struct{}
}

func (p *program) Start(s service.Service) error {
	ctx, cancel := context.WithCancel(context.Background())
	p.cancel = cancel
	p.done = make(chan struct{})
	go func() {
		defer close(p.done)
		if err := runOrchestrator(ctx, p.cfgPath, p.interactive, p.installDeps); err != nil && ctx.Err() == nil {
			fmt.Fprintln(os.Stderr, "multirunner: "+err.Error())
		}
	}()
	return nil
}

func (p *program) Stop(s service.Service) error {
	if p.cancel != nil {
		p.cancel()
	}
	select {
	case <-p.done:
	case <-time.After(20 * time.Second):
	}
	return nil
}

// runOrchestrator loads config, runs preflight, starts the cache + pools, and
// blocks until ctx is cancelled.
func runOrchestrator(ctx context.Context, configPath string, interactive, installDeps bool) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	logger := newLogger(cfg.Log)
	for _, warn := range cfg.Warnings() {
		logger.Warn(warn)
	}
	for _, pc := range cfg.Pools {
		if pc.Backend != "qemu" {
			continue
		}
		accel := pc.QEMU.Accel
		detected := accel == ""
		if detected {
			accel = winvm.DetectAccel(runtime.GOOS, runtime.GOARCH)
		}
		if accel == "tcg" {
			logger.Warn("QEMU Windows guest is using software CPU emulation; "+tcgReason(detected, runtime.GOOS, runtime.GOARCH),
				"pool", pc.Name, "host_os", runtime.GOOS, "host_arch", runtime.GOARCH)
		}
	}

	// Build the GitHub client provider. For scope=repos, create a per-repo
	// client for each listed repo and wrap them in a RepoSet.
	var ghProvider github.ClientProvider
	if cfg.GitHub.Scope == config.ScopeRepos {
		refs := cfg.GitHub.ResolvedRepos()
		clients := make([]*github.Client, len(refs))
		repoLabels := make([]string, len(refs))
		for i, ref := range refs {
			repoGH := cfg.GitHub
			repoGH.Scope = config.ScopeRepo
			repoGH.Owner = ref.Owner
			repoGH.Repo = ref.Repo
			c, err := github.New(ctx, repoGH, cfg.Auth)
			if err != nil {
				return fmt.Errorf("github client for %s/%s: %w", ref.Owner, ref.Repo, err)
			}
			clients[i] = c
			repoLabels[i] = ref.Owner + "/" + ref.Repo
		}
		ghProvider = github.NewRepoSet(clients, repoLabels)
		logger.Info("starting",
			"scope", cfg.GitHub.Scope,
			"repos", repoLabels,
			"provisioning", cfg.Provisioning, "pools", len(cfg.Pools))
	} else {
		ghClient, err := github.New(ctx, cfg.GitHub, cfg.Auth)
		if err != nil {
			return fmt.Errorf("github client: %w", err)
		}
		ghProvider = ghClient
		logger.Info("starting",
			"scope", cfg.GitHub.Scope, "owner", cfg.GitHub.Owner,
			"provisioning", cfg.Provisioning, "pools", len(cfg.Pools))
	}

	if err := runQEMUHousekeeping(ctx, cfg, logger); err != nil {
		return err
	}

	gitMgr, err := setupGitCache(ctx, cfg, logger)
	if err != nil {
		return err
	}

	sharedEnv := map[string]string{}
	if cfg.Cache.Enabled && cfg.Cache.Mode != "off" {
		advertise, err := startCache(ctx, cfg, gitMgr, logger)
		if err != nil {
			return err
		}
		if advertise != "" {
			for k, v := range cache.RunnerEnv(advertise) {
				sharedEnv[k] = v
			}
		}
	}

	// Metrics + lifecycle hooks (served only when a listen addr is set).
	m := metrics.New()
	if cfg.Metrics.Listen != "" {
		go func() {
			if err := m.Serve(ctx, cfg.Metrics.Listen, logger); err != nil {
				logger.Error("metrics server error", "err", err)
			}
		}()
	}
	hooks := m.Hooks()

	var launchers []*pool.Launcher
	var backends []backend.Backend
	var scaleSetPools []scaleSetPool
	for _, pc := range cfg.Pools {
		be, err := preparePool(ctx, pc, interactive, installDeps, logger)
		if err != nil {
			return err
		}
		if be == nil {
			continue
		}
		backends = append(backends, be)

		env, mounts := poolEnvAndMounts(cfg, pc, sharedEnv, gitMgr, logger)
		image := pc.ImageRef()
		launchers = append(launchers, pool.NewLauncher(pc, image, be, ghProvider, env, mounts, logger, hooks))
		scaleSetPools = append(scaleSetPools, scaleSetPool{
			cfg:       pc,
			be:        be,
			image:     image,
			env:       env,
			mounts:    mounts,
			container: normalizedContainerSettings(pc.Container),
		})
		logger.Info("pool ready", "name", pc.Name, "os", pc.OS, "size", pc.Size, "image", image,
			"dind", pc.Docker.EnableDinD, "tool_cache", pc.ToolCache.Mode, "mounts", len(mounts))
	}
	defer func() {
		for _, be := range backends {
			_ = be.Close()
		}
	}()

	if len(launchers) == 0 {
		return fmt.Errorf("no runnable pools")
	}

	logger.Info("orchestrator running", "mode", cfg.Provisioning)
	switch {
	case cfg.Provisioning.IsScaleset():
		err = runScaleset(ctx, cfg, scaleSetPools, hooks, logger)
	case cfg.Provisioning.IsAutoscale():
		err = runAutoscale(ctx, cfg, ghProvider, launchers, logger)
	default:
		pools := make([]*pool.Pool, len(launchers))
		for i, l := range launchers {
			pools[i] = pool.NewPool(l, logger)
		}
		err = pool.NewManager(pools, logger).Run(ctx)
	}
	if err != nil {
		return err
	}
	logger.Info("shutdown complete")
	return nil
}

// scaleSetPool is everything the scaleset mode needs to launch runners for one
// pool. It deliberately skips pool.Launcher, because in this mode the JIT
// config comes from the scale set session rather than from generate-jitconfig.
type scaleSetPool struct {
	cfg       config.Pool
	be        backend.Backend
	image     string
	env       map[string]string
	mounts    []backend.Mount
	container backend.ContainerSettings
}

func normalizedContainerSettings(cfg config.ContainerConfig) backend.ContainerSettings {
	return backend.ContainerSettings{
		CPUCount:        int64(cfg.CPUs),
		MemoryBytes:     cfg.MemoryBytes(),
		MemorySwapBytes: cfg.MemorySwapBytes(),
		DNS:             append([]string(nil), cfg.DNS...),
	}
}

// runScaleset holds one long-poll session per pool and provisions runners as
// GitHub reports demand. Each pool has its own scale set, because a scale set
// carries one label set and therefore one runner OS.
func runScaleset(ctx context.Context, cfg *config.Config, pools []scaleSetPool, hooks pool.Hooks, logger *slog.Logger) error {
	target, err := scalesetmode.TargetURL(cfg.GitHub.URL, string(cfg.GitHub.Scope), cfg.GitHub.Owner, cfg.GitHub.Repo)
	if err != nil {
		return err
	}

	clientOpts := scalesetmode.ClientOptions{
		TargetURL:      target,
		PAT:            cfg.Auth.PAT,
		AppID:          cfg.Auth.AppID,
		InstallationID: cfg.Auth.InstallationID,
		PrivateKeyPath: cfg.Auth.PrivateKeyPath,
	}

	g, gctx := errgroup.WithContext(ctx)
	for _, p := range pools {
		p := p
		g.Go(func() error {
			client, err := scalesetmode.NewClient(clientOpts)
			if err != nil {
				return err
			}
			return scalesetmode.Run(gctx, client, p.be, scalesetmode.SessionOptions{
				Name:        p.cfg.ScaleSet,
				RunnerGroup: p.cfg.RunnerGroup,
				Labels:      p.cfg.Labels,
				Launch: scalesetmode.Options{
					Image:      p.image,
					WorkFolder: p.cfg.WorkFolder,
					Labels:     p.cfg.Labels,
					Env:        p.env,
					Mounts:     p.mounts,
					Container:  p.container,
					MaxRunners: p.cfg.Size,
					OnStart: func() {
						if hooks.OnStart != nil {
							hooks.OnStart(p.cfg.Name)
						}
					},
					OnStop: func(code int, err error) {
						if hooks.OnStop != nil {
							hooks.OnStop(p.cfg.Name, code, err)
						}
					},
				},
			}, logger.With("pool", p.cfg.Name))
		})
	}
	return g.Wait()
}

func runQEMUHousekeeping(ctx context.Context, cfg *config.Config, logger *slog.Logger) error {
	var refs []winvm.GoldenRef
	for _, pc := range cfg.Pools {
		if pc.Backend != "qemu" {
			continue
		}
		bake := winvm.BakeOptions{
			WindowsISO:       pc.QEMU.BakeISO,
			WindowsISOSHA256: pc.QEMU.BakeISOSHA256,
			Golden:           pc.QEMU.Golden,
			MemMB:            pc.QEMU.MemMB,
			CPUs:             pc.QEMU.CPUs,
			Accel:            pc.QEMU.Accel,
			RunnerVersion:    pc.QEMU.RunnerVersion,
			RunnerSHA256:     pc.QEMU.RunnerSHA256,
			Licensed:         pc.QEMU.Licensed,
			Tools:            pc.QEMU.Tools,
		}
		if bake.WindowsISO != "" {
			hash, err := winvm.ToolsHash(bake)
			if err != nil {
				return fmt.Errorf("qemu pool %q bake inputs: %w", pc.Name, err)
			}
			bake.WorkflowsHash = hash
		}
		refs = append(refs, winvm.GoldenRef{Bake: bake})
	}
	if len(refs) == 0 {
		return nil
	}
	winvm.NewHousekeeper(refs, winvm.DefaultPolicy(), 0, logger).CheckOnce(ctx)
	return nil
}

// runAutoscale runs the on-demand scaler (polling) plus an optional webhook
// receiver (when webhook.listen is set and reachable from GitHub).
func runAutoscale(ctx context.Context, cfg *config.Config, ghProvider github.ClientProvider, launchers []*pool.Launcher, logger *slog.Logger) error {
	scaler := autoscale.New(launchers, ghProvider, cfg.GitHub.Scope, cfg.Webhook.PollIntervalSec, logger)
	if cfg.Webhook.Listen != "" {
		if cfg.Webhook.Secret == "" {
			logger.Warn("webhook listener has no secret; signatures will not be verified")
		}
		wh := webhook.New(cfg.Webhook.Listen, cfg.Webhook.Path, cfg.Webhook.Secret, scaler, logger)
		go func() {
			if err := wh.Start(ctx); err != nil {
				logger.Error("webhook server error", "err", err)
			}
		}()
	} else if cfg.GitHub.Scope != config.ScopeRepo && cfg.GitHub.Scope != config.ScopeRepos {
		logger.Warn("autoscale without a webhook on non-repo scope cannot poll queued jobs; set webhook.listen (behind NAT use a tunnel) or use provisioning: pool")
	}
	return scaler.Run(ctx)
}

// preparePool builds and validates a pool's backend, offering to install a
// missing Windows-container daemon when running interactively.
func preparePool(ctx context.Context, pc config.Pool, interactive, installDeps bool, logger *slog.Logger) (backend.Backend, error) {
	be, err := newBackend(pc)
	if err != nil {
		return nil, err
	}
	if be == nil {
		logger.Warn("skipping pool with unknown os", "pool", pc.Name, "os", pc.OS)
		return nil, nil
	}

	if err := be.Ping(ctx); err != nil {
		switch {
		case pc.OS == "windows" && (pc.Backend == "" || pc.Backend == "docker"):
			if ierr := maybeInstallWindowsDaemon(pc, interactive, installDeps, logger); ierr != nil {
				return nil, ierr
			}
			if err = be.Ping(ctx); err != nil {
				return nil, fmt.Errorf("pool %s: windows daemon still unreachable at %s: %w", pc.Name, pc.Docker.Host, err)
			}
		case pc.Backend == "containerd":
			return nil, fmt.Errorf("pool %s: containerd unreachable at %s: %w (install with `multirunner install-containerd`)", pc.Name, pc.Containerd.Address, err)
		default:
			return nil, fmt.Errorf("pool %s: cannot reach docker daemon at %s: %w", pc.Name, pc.Docker.Host, err)
		}
	}

	if osType, err := be.OSType(ctx); err == nil && osType != "" && osType != pc.OS {
		return nil, fmt.Errorf("pool %s: daemon at %s is in %q mode, but pool os is %q (wrong container mode)",
			pc.Name, pc.Docker.Host, osType, pc.OS)
	}
	return be, nil
}

func newBackend(pc config.Pool) (backend.Backend, error) {
	if pc.Backend == "qemu" {
		return winvm.NewBackend(winvm.Options{
			Golden: pc.QEMU.Golden, WorkDir: pc.QEMU.WorkDir,
			MemMB: pc.QEMU.MemMB, CPUs: pc.QEMU.CPUs, Accel: pc.QEMU.Accel,
		})
	}
	if pc.Backend == "containerd" {
		return backend.NewContainerdWindows(pc.Containerd.Nerdctl, pc.Containerd.Address, pc.Containerd.Namespace, pc.Containerd.Isolation)
	}
	switch pc.OS {
	case "linux":
		return backend.NewDockerLinux(pc.Docker.Host)
	case "windows":
		return backend.NewDockerWindows(pc.Docker.Host, pc.Docker.Isolation)
	default:
		return nil, nil
	}
}

func maybeInstallWindowsDaemon(pc config.Pool, interactive, installDeps bool, logger *slog.Logger) error {
	guidance := fmt.Errorf("pool %s: %s", pc.Name, winsetup.DaemonHint())
	if !installDeps && !interactive {
		return guidance
	}
	if !installDeps {
		fmt.Printf("Pool %q requires a Windows-container daemon at %s, which is not reachable.\n", pc.Name, pc.Docker.Host)
		fmt.Print("Install it now (requires elevation)? [y/N]: ")
		if !readYes() {
			return guidance
		}
	}
	logger.Info("installing windows container daemon (elevation prompt may appear)")
	if err := winsetup.Install(winsetup.InstallOptions{}); err != nil {
		return fmt.Errorf("install windows daemon: %w", err)
	}
	return nil
}

func readYes() bool {
	var s string
	_, _ = fmt.Scanln(&s)
	s = strings.ToLower(strings.TrimSpace(s))
	return s == "y" || s == "yes"
}

// doctor checks each pool's daemon reachability and container mode.
func doctor(configPath string) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	fmt.Printf("config: %s\nscope=%s owner=%s pools=%d cache=%v\n\n",
		configPath, cfg.GitHub.Scope, cfg.GitHub.Owner, len(cfg.Pools), cfg.Cache.Enabled)

	allOK := true
	for _, pc := range cfg.Pools {
		be, err := newBackend(pc)
		if err != nil {
			fmt.Printf("[%s] backend error: %v\n", pc.Name, err)
			allOK = false
			continue
		}
		if be == nil {
			fmt.Printf("[%s] os=%s UNKNOWN\n", pc.Name, pc.OS)
			allOK = false
			continue
		}
		status := "ok"
		if err := be.Ping(ctx); err != nil {
			status = "UNREACHABLE: " + err.Error()
			if pc.OS == "windows" && (pc.Backend == "" || pc.Backend == "docker") {
				status += "\n        hint: " + winsetup.DaemonHint()
			} else if pc.Backend == "containerd" {
				status += "\n        hint: install containerd+nerdctl (`multirunner install-containerd`)"
			}
			allOK = false
		} else if osType, err := be.OSType(ctx); err != nil {
			status = "UNKNOWN MODE: " + err.Error()
			allOK = false
		} else if osType != pc.OS {
			status = fmt.Sprintf("WRONG MODE: daemon=%s pool=%s", osType, pc.OS)
			allOK = false
		}
		be.Close()
		fmt.Printf("[%s] os=%s host=%s image=%s -> %s\n", pc.Name, pc.OS, pc.Docker.Host, pc.ImageRef(), status)
	}
	if err := warnStarvedRepos(cfg); err != nil {
		allOK = false
	}
	// The git audit is local and quick; it must not inherit whatever the pool
	// pings left of the shared budget.
	auditCtx, cancelAudit := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	warnGitFileConfig(auditCtx, cfg)
	cancelAudit()
	// Each remote phase gets a budget of its own. They share nothing with the
	// pool pings above, and a single hung daemon eating the 20s would otherwise
	// starve them into reporting "could not check" for every repo, which reads
	// as a GitHub problem when it is a local one. The budget also scales with
	// the repo list, since the checks visit repos in bounded waves and a fixed
	// clock would fail a healthy config that merely lists many repos.
	budget := remoteCheckBudget(len(cfg.GitHub.RepoTargets()))
	for _, phase := range []func(context.Context, *config.Config) error{checkActionsEnabled, warnNoSelfHostedWorkflows} {
		phaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), budget)
		err := phase(phaseCtx, cfg)
		cancel()
		if err != nil {
			allOK = false
		}
	}
	if !allOK {
		return fmt.Errorf("preflight found problems")
	}
	fmt.Println("\nall pools ready")
	return nil
}

// checkActionsEnabled flags configured repos that have GitHub Actions switched
// off. Such a repo never queues a job, so multirunner polls it every interval
// forever and the operator sees nothing: no error, no runner, no work. It looks
// identical to a repo that is merely idle, which is what makes it worth calling
// out here. Returns an error if any repo is off, so doctor exits non-zero.
func checkActionsEnabled(ctx context.Context, cfg *config.Config) error {
	if cfg.GitHub.Scope != config.ScopeRepo && cfg.GitHub.Scope != config.ScopeRepos {
		return nil
	}
	var disabled []string
	var incomplete []repoCheckResult
	results := runRepoChecks(ctx, cfg, func(ctx context.Context, c *github.Client) (bool, error) {
		return c.ActionsEnabled(ctx)
	})
	for _, result := range results {
		if result.err != nil {
			fmt.Printf("\n[%s] could not check Actions: %v\n", result.name, result.err)
			incomplete = append(incomplete, result)
		} else if !result.ok {
			disabled = append(disabled, result.name)
		}
	}
	if len(disabled) > 0 {
		fmt.Printf("\nWARNING: Actions is disabled on %d of %d configured repo(s): %s\n"+
			"        These repos can never queue a job, so multirunner polls them for nothing.\n"+
			"        fix: enable Actions under Settings > Actions > General, or remove them from multirunner's GitHub scope.\n",
			len(disabled), len(results), strings.Join(disabled, ", "))
	}
	if len(disabled) > 0 || len(incomplete) > 0 {
		return fmt.Errorf("Actions checks failed: %d disabled, %d incomplete", len(disabled), len(incomplete))
	}
	return nil
}

// warnNoSelfHostedWorkflows flags configured repos where no workflow targets a
// self-hosted runner. Listing a repo here tells multirunner to poll it every
// interval forever, but a repo whose workflows all run on GitHub-hosted runners
// can never hand it a job. The pools just look idle, which is indistinguishable
// from a quiet day.
//
// Detection is a literal search for "self-hosted" under .github/workflows.
// runs-on can legally name custom labels only (runs-on: [Windows, X64]) and
// matrix or expression forms cannot be resolved without evaluating the workflow,
// so a miss here is possible. That is why a repo without a hit only earns a
// note; doctor fails only when the scan itself could not complete.
func warnNoSelfHostedWorkflows(ctx context.Context, cfg *config.Config) error {
	if cfg.GitHub.Scope != config.ScopeRepo && cfg.GitHub.Scope != config.ScopeRepos {
		return nil
	}
	var unused []string
	var incomplete []repoCheckResult
	results := runRepoChecks(ctx, cfg, repoTargetsSelfHosted)
	for _, result := range results {
		if result.err != nil {
			fmt.Printf("\n[%s] could not scan workflows: %v\n", result.name, result.err)
			incomplete = append(incomplete, result)
		} else if !result.ok {
			unused = append(unused, result.name)
		}
	}
	if len(unused) > 0 {
		fmt.Printf("\nNOTE: heuristic scan found no workflow targeting a self-hosted runner in %d of %d configured repo(s): %s\n"+
			"        multirunner polls these every interval but they can never send it a job.\n"+
			"        fix: point a workflow at `runs-on: [self-hosted, ...]`, or remove them from multirunner's GitHub scope.\n"+
			"        (custom-label and matrix runs-on forms are not detected, so verify before removing)\n",
			len(unused), len(results), strings.Join(unused, ", "))
	}
	if len(incomplete) > 0 {
		return fmt.Errorf("workflow scans incomplete for %d repo(s)", len(incomplete))
	}
	return nil
}

// repoTargetsSelfHosted reports whether any workflow file mentions the
// self-hosted label. Stops at the first hit so a repo that uses multirunner
// costs one extra call rather than one per workflow.
func repoTargetsSelfHosted(ctx context.Context, c *github.Client) (bool, error) {
	paths, err := c.RepoFilePaths(ctx)
	if err != nil {
		return false, err
	}
	for _, p := range paths {
		if !strings.HasPrefix(p, ".github/workflows/") {
			continue
		}
		if ext := strings.ToLower(filepath.Ext(p)); ext != ".yml" && ext != ".yaml" {
			continue
		}
		body, err := c.RepoFile(ctx, p)
		if err != nil {
			return false, err
		}
		if strings.Contains(strings.ToLower(string(body)), "self-hosted") {
			return true, nil
		}
	}
	return false, nil
}

// warnGitFileConfig surfaces file-based git configuration that would affect the
// mirror cache's authenticated fetches. Environment-based config is filtered
// out of mirror subprocesses, but the operator's own global or system config
// still applies, and a rewrite of the GitHub URL or a stored Authorization
// header there is almost always unintended for this host. Advisory only: the
// operator owns those files and may have set them on purpose.
func warnGitFileConfig(ctx context.Context, cfg *config.Config) {
	if !cfg.GitCache.Enabled() {
		return
	}
	warnings, err := gitcache.AuditFileConfig(ctx, cfg.GitHub.URL)
	if err != nil {
		fmt.Printf("\ncould not inspect git config: %v\n", err)
		return
	}
	for _, warning := range warnings {
		fmt.Printf("\nWARNING: git config %s\n        mirror fetches inherit file-based git config; review or remove it if unintended\n", warning)
	}
}

// warnStarvedRepos flags the one config shape that silently strands work: pool
// provisioning across more repos than a pool has slots. A repo-scoped runner
// binds to exactly one repo, and a pool slot only re-registers after its runner
// finishes a job, so idle slots never rotate. Repos that never win a slot get no
// runner at all and their jobs queue forever. Autoscale does not have this
// problem because it places runners on the repo that queued the work.
func warnStarvedRepos(cfg *config.Config) error {
	if cfg.GitHub.Scope != config.ScopeRepos || cfg.Provisioning.IsAutoscale() {
		return nil
	}
	repos := len(cfg.GitHub.Repos)
	var starved int
	for _, pc := range cfg.Pools {
		if pc.Size >= repos {
			continue
		}
		fmt.Printf("\n[%s] WARNING: size=%d but repos=%d. At least %d repo(s) will have no %s runner and their jobs will stay queued.\n"+
			"        fix: raise size to %d, or set provisioning: autoscale so runners are placed on the repo that queued the job.\n",
			pc.Name, pc.Size, repos, repos-pc.Size, pc.OS, repos)
		starved++
	}
	if starved > 0 {
		return fmt.Errorf("%d pool(s) cannot cover every configured repo", starved)
	}
	return nil
}

type repoCheckResult struct {
	name string
	ok   bool
	err  error
}

// repoCheckConcurrency caps how many repos a doctor phase inspects at once.
const repoCheckConcurrency = 8

// remoteCheckBudget sizes a doctor phase for the number of repos it visits:
// at least remoteCheckTimeout, and never less than every wave of repos being
// allowed its full per-repo cap.
func remoteCheckBudget(repos int) time.Duration {
	waves := (repos + repoCheckConcurrency - 1) / repoCheckConcurrency
	if budget := time.Duration(waves) * remoteRepoCheckTimeout; budget > remoteCheckTimeout {
		return budget
	}
	return remoteCheckTimeout
}

// tcgReason explains why the QEMU guest fell back to software emulation, so
// the operator can tell a fixable host problem from an inherent one.
func tcgReason(detected bool, goos, goarch string) string {
	switch {
	case !detected:
		return "qemu.accel is set to tcg"
	case goarch != "amd64":
		return "x86-64 hardware acceleration requires an x86-64 host"
	case goos == "linux":
		return "/dev/kvm is missing or not accessible to this user"
	default:
		return "no hardware accelerator is available on this host"
	}
}

func runRepoChecks(
	ctx context.Context,
	cfg *config.Config,
	check func(context.Context, *github.Client) (bool, error),
) []repoCheckResult {
	refs := cfg.GitHub.RepoTargets()
	results := make([]repoCheckResult, len(refs))
	limit := make(chan struct{}, repoCheckConcurrency)
	var wg sync.WaitGroup
	for i, ref := range refs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			name := ref.Owner + "/" + ref.Repo
			results[i].name = name
			select {
			case limit <- struct{}{}:
				defer func() { <-limit }()
			case <-ctx.Done():
				results[i].err = ctx.Err()
				return
			}
			repoCtx, cancel := context.WithTimeout(ctx, remoteRepoCheckTimeout)
			defer cancel()
			repoGH := cfg.GitHub
			repoGH.Scope = config.ScopeRepo
			repoGH.Owner = ref.Owner
			repoGH.Repo = ref.Repo
			client, err := github.New(repoCtx, repoGH, cfg.Auth)
			if err == nil {
				results[i].ok, err = check(repoCtx, client)
			}
			results[i].err = err
		}()
	}
	wg.Wait()
	return results
}

func newLogger(l config.Log) *slog.Logger {
	level := slog.LevelInfo
	switch strings.ToLower(l.Level) {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}
	opts := &slog.HandlerOptions{Level: level}
	var h slog.Handler
	if strings.ToLower(l.Format) == "json" {
		h = slog.NewJSONHandler(os.Stderr, opts)
	} else {
		h = slog.NewTextHandler(os.Stderr, opts)
	}
	return slog.New(h)
}
