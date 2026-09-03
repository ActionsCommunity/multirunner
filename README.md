# multirunner

**Run many GitHub Actions self-hosted runners in parallel on one machine — Linux, Windows, or both at once.**

A GitHub Actions self-hosted runner executes **one job at a time**. To run jobs in
parallel you normally stand up several runners by hand and babysit them.
multirunner does it for you: it keeps a pool of **fresh, throwaway runners**
ready, hands each one a single job, then tears it down and replaces it with a
clean one. It also bundles a **local Actions cache** and a **local git mirror**
so your jobs stop re-downloading the same gigabytes from GitHub on every run.

One small binary. One config file. No Kubernetes, no control plane.

---

## Why

- **Real parallelism** — N isolated runners on one host, each on its own fresh
  registration. Queue 10 jobs, run 10 at once.
- **Clean every time** — each job gets a pristine, ephemeral environment (a
  container or a VM). No state leaks between jobs.
- **Fast** — a self-hosted Actions cache keeps `actions/cache` on your host, and
  a git mirror makes `actions/checkout` fetch only the new commits instead of
  full-cloning every time.
- **Cheap to run** — no public endpoint required; runners dial out to GitHub, so
  it works behind NAT out of the box.

## What it can do

| Capability | Details |
|---|---|
| **Linux runners** | Docker **or** Podman (docker-compatible API). |
| **Windows runners (containers)** | Native Windows containers via **containerd + runhcs** — no Docker Desktop. |
| **Windows runners in QEMU VMs** | Run Windows from a baked golden image on Linux, Windows, or macOS. |
| **Mix Linux + Windows** | Multiple pools, different OSes, one orchestrator. |
| **Self-hosted Actions cache** | Built-in v2 cache server — `actions/cache` stays on your host. |
| **Git mirror cache** | Local bare mirror; `actions/checkout` fetches only the delta. |
| **Autoscaling** | Keep N warm, scale on demand (polling or webhook), or let GitHub drive capacity through runner scale sets. |
| **Runs as a service** | Windows SCM, Linux systemd, macOS launchd. |
| **Metrics** | Prometheus endpoint + health check. |
| **Housekeeping** | Cache + mirror garbage collection, automatic. |

The Linux Docker backend is validated end-to-end against a real GitHub repo,
including real toolchains (`actions/checkout`, `actions/setup-dotnet`,
`dotnet build`) and `actions/cache` save/restore against the local server.
Windows container images receive build and smoke checks in CI. Integration jobs
for containerd and QEMU are included for suitable self-hosted infrastructure.

---

## How it works

In `pool` and `autoscale` modes, multirunner:

1. Calls GitHub's `generate-jitconfig` (repo / repos / org / enterprise scope) for a
   single-use registration.
2. Launches a clean runner, either a container or a VM, that runs the **stock GitHub
   runner** with that JIT config.
3. The runner takes exactly one job, then deregisters itself (ephemeral).
4. multirunner notices it exited and immediately starts a fresh one.

In `scaleset` mode, GitHub's scale-set session supplies the JIT config and desired
runner count instead. Every mode runs the official `actions/runner` binary unchanged,
so jobs behave exactly as on GitHub-hosted runners.

---

## Install

Download a binary for your OS/arch from the
[Releases](../../releases) page, or build from source:

```sh
go install github.com/GerardSmit/multirunner/cmd/multirunner@latest
```

Prebuilt binaries are published for **Linux, Windows, and macOS**, each in
**x64 and ARM64**.

### Agent plugins

This repository includes shared agent skills and plugin manifests for Copilot
CLI, Claude Code, and Codex. Install the Copilot CLI plugin directly from
GitHub:

```sh
copilot plugin install ActionsCommunity/multirunner
copilot plugin list
```

For plugin development, install from a local checkout:

```sh
copilot plugin install .
```

Reinstall after changing the checkout because Copilot CLI caches installed
plugin contents.

The Claude Code and Codex manifests are `.claude-plugin/plugin.json` and
`.codex-plugin/plugin.json`; all three manifests declare the shared `skills/`
directory. Everything a skill links to lives under it (`skills/references/`
and `skills/docs/`), so the plugin stays self-contained however it is
packaged.

Ask Copilot to use one of these five focused skill routers. Each loads only the
reference material needed for its mode:

| Skill | Purpose |
|---|---|
| `multirunner-setup` | Set up a new host end to end: verified binary, runtime, config, credentials, service, and canary |
| `multirunner-host` | Tune or upgrade an existing host and its container pools with a planned, approved lifecycle |
| `multirunner-diagnose` | Run bounded read-only health checks and diagnose service, runtime, cache, routing, and runner failures |
| `multirunner-github` | Configure or diagnose GitHub targets, credentials, labels, scale sets, and `workflow_job` webhooks |
| `multirunner-qemu` | Bake, inspect, monitor, and safely debug Windows QEMU runner pools |

The setup and host skills use published binaries and `SHA256SUMS.txt`; they do
not require a source build or Go toolchain. The former narrow
skills are intentionally replaced by these routers so agents load less repeated
guidance. Every skill preserves existing configuration and asks before
elevation, package installation, service changes, GitHub writes, workflow
dispatch, or removal. Secret values and JIT configuration must never be shown
in chat or logs.

The same packaged skills also have native manifests for
[Claude Code](.claude-plugin/plugin.json) and [Codex](.codex-plugin/plugin.json).
For local Claude Code development, run `claude --plugin-dir .`. Codex packages
the root `skills/` directory through its `.codex-plugin/plugin.json`; use a
workspace or local marketplace when installing it for Codex. The root
[`plugin.json`](plugin.json) remains the Copilot CLI manifest.

### Operator references

- [CLI reference](skills/docs/cli-reference.md)
- [Host configuration reference](skills/docs/host-configuration.md)
- [QEMU Windows guide](skills/docs/qemu-windows.md)

---

## Quick start (Linux)

1. **Write a tiny config** (`config.yaml`):

   ```yaml
   github:
     scope: repo
     owner: my-user
     repo: my-repo
   auth:
     pat: "${GITHUB_PAT}"        # export GITHUB_PAT=... (or use `connect`, below)
   pools:
     - name: linux
       os: linux
       size: 3
       labels: [self-hosted, linux, x64]
       docker:
         host: "tcp://127.0.0.1:2375"   # your Docker/Podman endpoint
   ```

2. **Run it:**

   ```sh
   export GITHUB_PAT=...            # or put GITHUB_PAT=... in a .env file
   multirunner run --config config.yaml --dry-run
   multirunner run --config config.yaml
   ```

That's it. The runner image is pulled automatically (no build step), and your
runners appear under **Settings → Actions → Runners**. Push a workflow with
`runs-on: [self-hosted, linux, x64]` and watch them pick up jobs.

> `${VAR}` config references resolve from the environment, and from a `.env` file
> (the config's directory, then the working dir) — so `GITHUB_PAT=ghp_…` in `.env`
> is enough; real environment variables take precedence.

> **No PAT?** Preview with `multirunner connect --repo owner/name --config
> config.yaml --dry-run`; repeat without `--dry-run` to create and install a
> GitHub App and write its credentials.

`config.example.yaml` documents every option (cache, autoscaling, tiers, …) when
you want to go further.

### Serving multiple repositories

Use `scope: repos` to serve several repositories without granting organization
runner access:

```yaml
github:
  scope: repos
  owner: my-org
  repos:
    - api
    - web
    - another-owner/tools
auth:
  pat: "${GITHUB_PAT}"
```

Short entries inherit `github.owner`; explicit `owner/repo` entries can span
accounts when PAT authentication is used. A GitHub App installation belongs to
one account, so App-authenticated lists must all use that installation account.
The App also needs repository Administration write and Contents read permissions.
Apps created by the current `multirunner connect --repo ...` flow request both.
For an older App, add Contents read under the App's repository permissions and
approve the updated permission on its installation.

In fixed pool mode, every pool needs at least as many slots as configured
repositories so each repository receives a warm runner. `multirunner doctor`
rejects undersized pools. Autoscale mode instead registers capacity directly on
the repository with queued work.

---

## Backends

`pools[].backend` and `pools[].docker.host` select how a pool runs.

### Linux containers (Docker / Podman)

```yaml
pools:
  - name: linux-pool
    os: linux
    size: 3
    labels: [self-hosted, linux, x64]
    docker:
      host: "tcp://127.0.0.1:2375"            # Docker (WSL2)
      # host: "npipe:////./pipe/podman-machine-default"   # Podman on Windows
```

The image defaults to the published `gerardsmit/multirunner-runner-linux:latest`
and is pulled automatically. Set `image:` only to use your own, or pick a
prebuilt **flavor** with `image_tier:` (see below).

### Runner image flavors

The default `minimal` image is just the runner + git. For common toolchains, set
`image_tier:` to a prebuilt flavor — published as tags on
`gerardsmit/multirunner-runner-<os>` and pulled automatically:

| `image_tier`   | OS      | Includes                                                        |
|----------------|---------|----------------------------------------------------------------|
| `minimal` (default) | both | runner + git + jq + unzip (Linux) / MinGit (Windows)      |
| `native-build` | linux   | gcc/g++/make, cmake, ninja, pkg-config, python3 (+dev)         |
| `node`         | both    | + Node LTS + corepack (npm/pnpm/yarn); node-gyp works on Linux |
| `dotnet`       | both    | + .NET SDK (**Node included** for ASP.NET SPA builds)          |
| `rust`         | linux   | + rustup stable + musl target (**Node included** for napi-rs)  |
| `go`           | linux   | + Go toolchain                                                 |
| `buildtools`   | windows | + default/current VS Build Tools line (currently 18)          |
| `buildtools:17` | windows | + Visual Studio 2022 Build Tools 17                           |
| `buildtools:18` | windows | + Visual Studio 2026 Build Tools 18                           |

Flavor layering differs by OS. Linux uses `dotnet`/`rust` ⊃ `node` ⊃
`native-build` and `go` ⊃ `native-build`. Windows uses `dotnet` ⊃ `node`, while
`buildtools` is a separate branch. Each Build Tools major is a separate image;
the unqualified flavor aliases the `default_line` in `images/versions.json`.
The Node flavor caches every declared LTS major on both operating
systems; the manifest `default_major` is exposed on `PATH`, and
`actions/setup-node` selects another cached major without downloading it.
Corepack is pinned as its own entry in `images/versions.json` and installed from
the npm registry tarball, because Node no longer bundles it from Node 25 onwards.
The SDK channels assigned to Linux, Windows
containers, and QEMU are declared in `images/versions.json`; Windows SDK
archives include the WindowsDesktop packs. Windows jobs needing Node/.NET plus
native MSVC tooling therefore need a custom image derived from `buildtools`, or a
[QEMU golden VM](#windows-runners-with-qemu). Full Visual Studio
(IDE) also requires a golden VM. A custom `image_tier:` you build yourself
resolves to a local `multirunner/runner-<os>-<tier>:dev` tag.

The `update-image-versions` workflow checks all published image flavors weekly
and opens one consolidated maintenance PR when anything changes. The complete
inventory is in `images/versions.json`: base-image digests, runner and toolchain
versions, vendor checksums, and support/EOL dates. `native-build` has no separate
upstream pin; its rolling Ubuntu packages are refreshed and validated by the
weekly image build even when no maintenance PR is needed.

Node's `supported-lts` track keeps every LTS major until its end-of-life,
including majors that have moved to Maintenance LTS. Node's official release
schedule is used to add a major automatically on the day it becomes LTS; non-LTS
and future-LTS releases are ignored, and `default_major`
remains an explicit choice. The .NET releases index is used to discover new
active or maintenance LTS/STS channels. Because upstream cannot decide which
multirunner image should carry a .NET channel, discoveries are written to
`dotnet.unassigned_supported_channels` and fail the image policy check until a
maintainer moves each channel under `dotnet.channels` and assigns any combination
of `linux`, `windows-container`, and `qemu-windows` targets. Preview and EOL .NET
channels are ignored. A declared Node or .NET line that has passed its EOL date
is still refreshed — upstream simply stops publishing patches — but the updater
annotates the run with a warning and the image policy check fails, so removing
support remains an explicit decision.

Dockerfiles and QEMU golden bakes consume the JSON directly; the only generated
duplicates are the two Docker `ARG BASE` defaults, which must exist before a
Dockerfile can copy the manifest. Run `go run ./cmd/update-image-versions` to
perform the same refresh locally. The updater fills every version, EOL date, and
checksum; adding a Node or .NET line requires no Dockerfile or QEMU template
changes.

**Routing is label-based:** give each flavor pool a distinct label and select it
from a workflow with `runs-on`:

```yaml
pools:
  - name: linux-dotnet
    os: linux
    image_tier: dotnet
    labels: [self-hosted, linux, dotnet]
  - name: linux-node
    os: linux
    image_tier: node
    labels: [self-hosted, linux, node]
```

```yaml
# in the consuming repo's workflow
jobs:
  build:
    runs-on: [self-hosted, linux, dotnet]
```

Not sure which flavors a repo needs? **`multirunner detect`** scans it and prints a
ready-to-paste `pools:` block plus the images to pull:

```sh
multirunner detect --path .                 # scan a local checkout
multirunner detect --repo octo/hello        # scan a remote repo via the GitHub API
```

### Windows containers (containerd, no Docker Desktop)

multirunner drives **containerd + runhcs** directly through `nerdctl`, so Windows
containers run on the OS Host Compute Service with no Docker Desktop:

```powershell
# Read-only host inspection and installation plan.
multirunner install-containerd --dry-run

# Elevates once: installs containerd + runhcs + nerdctl + CNI, enables the
# Containers (and, on Windows client, Hyper-V) features.
multirunner install-containerd
```

```yaml
pools:
  - name: windows-pool
    os: windows
    backend: containerd
    size: 2
    image: "multirunner/runner-windows:dev"
    labels: [self-hosted, windows, x64]
    docker:
      host: "required-but-ignored" # required by current validation
    containerd:
      isolation: auto      # auto: process on Server, hyperv on Windows client
```

`multirunner doctor` reports daemon reachability and catches mismatches (e.g. a
Linux daemon assigned to a Windows pool). With `scope: repo` or `scope: repos`
it also checks the input side: whether GitHub Actions is enabled, and whether a
heuristic scan finds `self-hosted` in a workflow. The workflow scan is advisory
because custom-label and matrix expressions may not contain that literal;
authentication, permission, timeout, and truncated-tree failures still make
doctor exit non-zero instead of reporting an incomplete preflight as ready.

### Windows runners with QEMU

Run Windows runners as **VMs** with QEMU. Bake a golden **Server Core** image
once; each job boots a clean copy-on-write overlay, reads its JIT config from an
attached ISO, runs one job, and powers off. Acceleration is selected from the
host automatically: KVM on x86-64 Linux, WHPX on x86-64 Windows, and HVF on
Intel macOS. The guest is x86-64, so ARM hosts—including Apple Silicon—use TCG
software emulation. That path is supported but substantially slower than native
hardware virtualization, especially while baking the golden image.

```sh
multirunner bake --iso WinServer2022Eval.iso --golden /var/lib/multirunner/golden.qcow2
# bake toolchains into the golden (the VM equivalent of container flavors):
multirunner bake --iso WinServer2022Eval.iso --golden golden.qcow2 --tools dotnet,node,buildtools
# exact, combinable selectors are also supported (two Build Tools lines are two
# full Visual Studio installs, so raise --disk-gb well above the 40 GB default):
multirunner bake --iso WinServer2022Eval.iso --golden golden.qcow2 --disk-gb 120 --tools dotnet:10,buildtools:17,buildtools:18
# optionally reject an ISO that does not match the licensed media you selected:
multirunner bake --iso WinServer2022.iso --iso-sha256 <sha256> --golden golden.qcow2
```

The QEMU backend boots the baked golden image and **ignores `image`/`image_tier`**
(those only apply to container backends — multirunner warns if you set them on a
qemu pool). To give a VM runner toolchains, bake them in with `--tools`
(`dotnet` = the SDK channels carrying the `qemu-windows` target in
`images/versions.json`, minus any that have reached EOL; `dotnet:8`, `dotnet:9`,
and `dotnet:10` select exact majors and fail if that major is no longer
supported; `node` = every declared LTS major while `node:22` and `node:24` select
exact majors; `buildtools` selects the manifest `default_line`; `buildtools:17`
and `buildtools:18` are exact and may be combined — each line is a full Visual
Studio install, so combining them needs a larger `--disk-gb` than the 40 GB
default and roughly doubles the bake's install budget).
List the same tools under `qemu.tools` so a managed rebuild reuses them.
With `qemu.bake_iso` configured, a later orchestrator start detects tool-fingerprint
drift and can rebuild; without it, Multirunner reports that rebuilding is unavailable.
Invalid selectors are rejected at config load.
Built-in bakes pin and verify the runner, MinGit, Node, Go, .NET SDKs, and Visual
Studio Build Tools. The Windows ISO content and every selected payload identity
are part of the golden fingerprint. Downloads are checked before execution or
extraction, and the consolidated image-version workflow refreshes these shared
pins alongside the container images.
When selecting a different `--runner-version`, also provide its archive digest
with `--runner-sha256`.

```yaml
pools:
  - name: win-vm
    os: windows
    backend: qemu
    size: 1
    qemu:
      golden: /var/lib/multirunner/golden.qcow2
      work_dir: /var/lib/multirunner/run
      mem_mb: 4096
      cpus: 2
      accel: ""            # auto: x64 kvm/whpx/hvf; ARM uses x86 TCG emulation
      bake_iso: /var/lib/multirunner/WinServer2022.iso
      bake_iso_sha256: "<sha256>"   # optional expected digest; ISO is always fingerprinted
      tools: [dotnet, "node:24", "buildtools:17", "buildtools:18"]
```

`multirunner doctor` is read-only and never cleans `qemu.work_dir`. A real
`multirunner run`, or an installed-service `start`/`restart`, attempts cleanup
before QEMU/golden preflight: dedicate the directory and keep manual VMs,
forensic artifacts, and active job VMs out of it.

Highlights:

- **Bake serves a live noVNC viewer** — watch the unattended install in your
  browser. QEMU VNC binds to the viewer host and uses printed dynamic ports;
  `--vnc-web ""` disables it. The golden ships with **git** and the
  runner preinstalled, so jobs are ready to build immediately. The console is
  unauthenticated and the current bake CLI uses a fixed guest Administrator
  credential; keep it on a private operator boundary.
- **Verified completion** — the bake only ships a golden after it sees the
  `MR:GOLDEN_OK` serial marker; otherwise it fails loudly.
- **Licensing** — a Windows guest needs its own license. Server eval = 180 days,
  `slmgr /rearm`-able ~5–6× (~3 years), or supply a key/KMS.
- **Golden housekeeping** — at orchestrator startup, multirunner can rearm an
  evaluation golden in place, or attempt a configured rebuild for tool drift or
  exhausted rearms. A real key/KMS skips time-based evaluation handling, not
  tool-fingerprint checks.

---

## Self-hosted Actions cache

Keep `actions/cache` traffic on your host instead of round-tripping to GitHub's
Azure backend:

```yaml
cache:
  enabled: true
  mode: local-server
  advertise_url: "http://host.docker.internal:3000"   # reachable from runners
  access_token: "${MULTIRUNNER_CACHE_ACCESS_TOKEN}"   # optional; embedded server generates one
  max_age_days: 7        # evict entries unused this long
  max_size_gb: 0         # 0 = unlimited; otherwise LRU-evict to fit
```

multirunner runs an embedded Go server implementing the v2 twirp `CacheService`
plus the Azure block-upload data plane, stores blobs locally, and injects
`ACTIONS_RESULTS_URL` / `ACTIONS_CACHE_URL` / `ACTIONS_CACHE_SERVICE_V2=true` into
every runner. The runner image includes a small patch so the redirect reaches
`uses:` actions (not just `run:` steps). The patch ships as a sidecar copy and is
swapped in at container start only when a redirect is actually injected, so with
the cache off the stock runner keeps its own `ACTIONS_RESULTS_URL` and
`actions/upload-artifact` still works. Stale entries are garbage-collected
automatically.

The embedded cache adds a private `/_mr/<token>` path segment to the URL it
injects into runners. For a standalone external cache server, start it with
`--access-token ...` and put the matching token path in `cache.external_url`.

> **Podman on Windows:** runners reach the host bridge as
> `host.containers.internal` (the Podman VM, `10.88.0.1`), not the Windows host.
> Run the cache as a published container and set
> `cache.external_url: http://host.containers.internal:3000/_mr/<token>`, using
> the matching persistent standalone-cache token.

---

## Git mirror cache

Avoid full-cloning big repos on every job. multirunner keeps a host-side bare
mirror per repo and updates it in the background:

```yaml
git_cache:
  mode: mirror           # mirror | dotgit-cache | off
  path: /var/lib/multirunner/gitmirror
  max_age_days: 30       # remove mirrors unused this long
```

- **`mirror`** — mounts the bare mirror read-only into the runner; `checkout`
  uses it as a reference and fetches only the tip delta.
- **`dotgit-cache`** — serves the mirror as a git bundle over the cache server; a
  job-started hook seeds the workspace from it. Works where bind-mounts can't
  (the QEMU VM), so VM jobs get the same fast checkout. The per-job token still
  fetches the delta from GitHub, so private-repo auth is unaffected.

---

## Autoscaling

```yaml
provisioning: pool       # pool | autoscale | scaleset
```

- **`pool`** (default) — keep N runners warm per pool. Zero inbound; works behind
  NAT with no extra setup.
- **`autoscale`** — launch runners on demand up to each pool's `size`:
  - **Polling** (outbound, NAT-safe) — multirunner polls GitHub for queued work
    and scales up.
  - **Webhook** (low-latency) — set `webhook.listen` to receive `workflow_job`
    events (needs a reachable URL; use a tunnel like smee.io / cloudflared).
    A nonempty `webhook.secret` verifies signatures; an empty one accepts
    unsigned events and is unsafe on a public listener.
- **`scaleset`** — let GitHub decide. A [runner scale set][scaleset] holds a
  long-poll session open and reports the desired runner count, which is the same
  mechanism actions-runner-controller uses.

### Runner scale sets

In the other two modes multirunner decides when to launch. In `scaleset` mode it
does not: GitHub reports how many runners the scale set should have and
multirunner provisions to that number. That buys three things:

- Assignments arrive in seconds, with no poll interval to tune.
- No inbound endpoint, so it stays NAT-safe like `pool`.
- Demand is the queue depth GitHub computes for the scale set, rather than one
  inferred from job events. The host also advertises real capacity, so GitHub
  knows when it is saturated instead of having requests dropped.

Runners still come from the same backends, because the session hands back the
same encoded JIT config used by the other provisioning modes. Docker,
containerd, Podman and QEMU all work unchanged.

```yaml
provisioning: scaleset

github:
  scope: repo          # repo | org | enterprise (a scale set binds to one target)
  owner: my-org
  repo: my-repo

pools:
  - name: linux-pool
    os: linux
    size: 4            # also the maximum capacity advertised to GitHub
    scale_set: multirunner-linux
    runner_group: Default
    labels: [self-hosted, linux, x64]

  - name: windows-pool
    os: windows
    size: 2
    scale_set: multirunner-windows
    labels: [self-hosted, windows, x64]
```

Each pool needs its own `scale_set`, because a scale set carries one label set
and therefore one runner OS. Names are reused across restarts, so restarting
multirunner does not churn registrations. Drain or wait for active jobs before a
stop/restart, because it cancels orchestration and can interrupt runner work.
Target a scale set from a workflow the same way as any other runner:

```yaml
jobs:
  build:
    runs-on: [self-hosted, linux, x64]
```

[scaleset]: https://github.com/actions/scaleset

---

## Run as a service

multirunner installs itself as a native OS service (Windows SCM, Linux systemd,
macOS launchd):

```sh
sudo multirunner service install --dry-run --config /etc/multirunner/config.yaml
sudo multirunner service install --config /etc/multirunner/config.yaml   # Linux/macOS
sudo multirunner service start --dry-run
sudo multirunner service start
```

```powershell
multirunner service install --dry-run --config C:\multirunner\config.yaml
multirunner service install --config C:\multirunner\config.yaml          # Windows (elevated)
multirunner service start --dry-run
multirunner service start
```

`service uninstall` removes it.

---

## Metrics & health

Set `metrics.listen` (e.g. `127.0.0.1:9090`) to expose:

- `GET /metrics` — Prometheus: `multirunner_runners_active{pool}`,
  `multirunner_jobs_total{pool,result}`, `multirunner_reprovision_errors_total{pool}`.
- `GET /health` — liveness.

---

## CLI

Built with cobra — `multirunner <command> --help` for details; `--config` is global.
The mutating `multirunner` commands that support a plan expose `--dry-run`;
the companion `cacheserver` does not. `update-image-versions` is a repository
maintenance tool run by CI, not an operator command.
Hidden `_` developer helpers are excluded.

```text
multirunner [run]                   run the orchestrator (default)
multirunner connect --org <org>     create + install a GitHub App, write auth to config
multirunner doctor                  check daemons + container mode, no runners
multirunner detect                  scan a repo, recommend image flavors + pools
multirunner bake                    build a golden Windows VM image (qemu backend)
multirunner install-containerd      install the Windows-container stack (elevates)
multirunner install-windows-daemon  install the Windows Docker daemon (elevates)
multirunner service ...             install | uninstall | start | stop | restart
multirunner completion <shell>      shell completion script
```

---

## Build from source

Pure Go (CGO-free), so it cross-compiles to every target:

```sh
go build ./cmd/multirunner
# cross-compile, e.g.:
GOOS=linux   GOARCH=arm64 go build -o multirunner       ./cmd/multirunner
GOOS=windows GOARCH=amd64 go build -o multirunner.exe    ./cmd/multirunner
GOOS=darwin  GOARCH=arm64 go build -o multirunner        ./cmd/multirunner
```

## Layout

```text
cmd/multirunner    orchestrator + CLI
cmd/cacheserver    standalone cache server
internal/config    config schema + loader
internal/github    JIT client (repo/org/enterprise, PAT/App)
internal/backend   container backends (Docker/Podman, containerd Windows)
internal/winvm     QEMU Windows-VM backend + golden bake
internal/pool      per-OS ephemeral pool + reprovision loop
internal/scaleset  runner scale-set sessions + lifecycle management
internal/cache     self-hosted v2 cache server
internal/gitcache  host git mirror manager
images/            runner + cacheserver Dockerfiles
```

## Tests

```sh
go test ./...
```
