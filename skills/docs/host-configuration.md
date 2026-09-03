# Host configuration reference

This is the current runtime contract for `config.yaml`. It focuses on settings
that select, size, and connect runner hosts. Use `multirunner doctor --config
config.yaml` and `multirunner run --config config.yaml --dry-run` before
starting a changed configuration.

## Configuration loading, defaults, and secrets

YAML fields are strict: an unknown field is rejected. Defaults are applied after
parsing, then required cross-field checks run.

| Setting | Required value / default |
|---|---|
| `github.url` | `https://github.com` when omitted; set the GHES base URL otherwise. |
| `github.scope` | Required: `repo`, `repos`, `org`, or `enterprise`. `repo` needs `owner` + `repo`; `repos` needs a nonempty `repos` list; `org`/`enterprise` need `owner`. |
| `auth` | Set `pat`, or all of `app_id`, `installation_id`, and `private_key_path`. A nonempty PAT takes precedence over App credentials. |
| `provisioning` | `pool` by default; also `autoscale`, compatibility alias `webhook`, or `scaleset`. |
| `webhook` | Autoscale listener, signature secret, path, and polling controls; see [Webhook](#webhook). |
| `metrics` | Optional Prometheus and liveness listener; see [Metrics and provisioning](#metrics-and-provisioning). |
| `cache`, `git_cache` | Optional Actions and source-cache services; see [Cache](#cache) and [Git cache](#git-cache). |
| `pools` | One or more backend/capacity definitions; see [Common pool fields](#common-pool-fields). |
| `log.level`, `log.format` | `info`, `text`. |

Use environment references for secrets rather than literal values:

```yaml
auth:
  pat: "${GITHUB_PAT}"
cache:
  access_token: "${MULTIRUNNER_CACHE_ACCESS_TOKEN}"
webhook:
  secret: "${GITHUB_WEBHOOK_SECRET}"
```

Only `auth.pat`, `auth.private_key_path`, `webhook.secret`, and
`cache.access_token` expand `$NAME` or `${NAME}`. A missing referenced variable
becomes empty. Expansion is whole-value only: a value that does not begin with
`$` is used literally, and a value such as `${NAME}-suffix` is treated as one
variable name rather than an interpolation, so it resolves to empty.

The loader reads `<config-dir>/.env`, then `./.env`; an already exported
environment variable wins over both, and the first file wins over the second.
Both files accept blank lines, `#` comments, an optional `export ` prefix, and
matching surrounding single or double quotes around the value. Restrict access
to both files and never put JIT configuration or tokens in logs.

## GitHub target and authentication

| Field | Effect / constraint |
|---|---|
| `github.url` | GitHub.com by default. For GHES, use the GHES web base URL, not a repository URL or its `/api/v3` endpoint. |
| `github.scope` | Required: `repo`, `repos`, `org`, or `enterprise`. |
| `github.owner` | Required for `repo`, `org`, and `enterprise`; for `repos`, it supplies the owner for short repository entries. |
| `github.repo` | Required only for `scope: repo`; it is ignored for `scope: repos`, where leaving it set raises a startup/`--dry-run` warning. |
| `github.repos` | Required nonempty list for `scope: repos`. Each entry is `repo` (uses `owner`) or `owner/repo`; duplicates are rejected case-insensitively. An entry may contain at most one `/`, must have nonempty owner and repo parts, and must contain no whitespace. Entries and `github.owner` are whitespace-trimmed before validation. |
| `auth.pat` | A nonempty PAT takes precedence over App credentials. Use it only when its scope is required. |
| `auth.app_id`, `auth.installation_id`, `auth.private_key_path` | All three are required when no PAT is set. The service identity must be able to read the PEM path without exposing its contents. |

With App authentication, every `github.repos` entry must belong to one
installation account. `multirunner connect` supports only repo or organization
targets; it updates the selected scope/owner and App auth, removes a PAT, writes
a private key, and is not a GHES-aware setup flow. It does not clear stale
`github.repo`/`github.repos` fields for an organization target, configure or
activate a webhook, or update YAML transactionally. See the
[CLI reference](cli-reference.md#connect) and
[authentication reference](../references/authentication.md) before using it.

## Common pool fields

Every `pools[]` entry needs a unique `name` and `os: linux` or `os: windows`.

| Field | Effect / default |
|---|---|
| `backend` | Omit or use `docker` for the Docker-compatible backend; `containerd` is the Windows runhcs backend; `qemu` is the Windows VM backend. Only the latter two are special values; do not rely on an unknown value falling through to Docker behavior. |
| `size` | Concurrent slots: warm capacity in `pool`, active-per-pool cap in `autoscale`, and maximum concurrent runners in `scaleset`. `0` becomes `1`; lower values are rejected. Account for RAM, disk, and image/overlay capacity per slot. A fixed `repos` pool needs enough slots to cover its repositories. |
| `labels` | Runner routing labels; set labels that workflows actually request. Autoscale picks the first YAML-order pool with all requested labels and spare capacity, so put broad fallback pools last. |
| `runner_group_id` | Numeric GitHub runner group for `pool`/`autoscale`; `0` becomes `1`. It is not used by scale sets. |
| `work_folder` | Runner work directory; `_work` by default. |
| `name_prefix` | Prefix for `pool`/`autoscale` ephemeral runner names; `multirunner` by default. Scale sets use their own `mr-scaleset-*` names. |
| `max_consecutive_failures` | `pool`/`autoscale` failure-log threshold; `0` becomes `5`. It is not a circuit breaker: slots continue exponential retry. A negative value is accepted and makes the extra "hit max consecutive failures" message log from the first failure. Scale-set runners do not use it. |
| `scale_set`, `runner_group` | Scale-set-only fields. `scale_set` is required and unique per pool; empty `runner_group` selects GitHub's default group. Startup can create or update this remote GitHub state, including labels. |
| `image`, `image_tier` | Container image selection for the Docker and containerd backends; see [Images and tiers](#images-and-tiers). Ignored by `backend: qemu`. |
| `docker`, `containerd`, `qemu` | Backend blocks. Only the block matching `backend` is read at launch, except `docker.host`, which is validated for every non-QEMU pool. |
| `tool_cache` | See [Tool cache and Docker socket](#tool-cache-and-docker-socket). |

`docker.host` is required for every non-QEMU pool by current validation. It is
the endpoint used by Docker-compatible backends, with one containerd exception
called out below.

## Images and tiers

`image` wins over `image_tier`. Without `image`, `minimal` (also the default)
uses the published `gerardsmit/multirunner-runner-<os>:latest` image. Known tiers
are pulled automatically:

| Pool OS | Published `image_tier` values |
|---|---|
| Linux | `minimal`, `native-build`, `node`, `dotnet`, `rust`, `go` |
| Windows containers | `minimal`, `node`, `dotnet`, `buildtools`, `buildtools:<manifest-line>` |
| QEMU Windows VM | None; bake tools into the golden with `qemu.tools`. |

The unqualified Windows `buildtools` tier follows the current manifest default;
the available `buildtools:<line>` values come from the release manifest
embedded in the binary; an unknown line is rejected.

An unknown but syntactically valid tier resolves to
`multirunner/runner-<os>-<tier>:dev`. The CLI does **not** build it. Build and
preload that exact image (or make it pullable) before use. Unknown tiers must be
lowercase letters/digits separated by `.`, `_`, or `-`, and at most 64
characters; otherwise configuration loading fails.

## Linux Docker or Podman

Use the Docker-compatible API endpoint. There is no separate `podman` backend
configuration; leave `backend` omitted or set it to `docker`.

```yaml
pools:
  - name: linux-node
    os: linux
    size: 2
    image_tier: node
    labels: [self-hosted, linux, x64, node]
    docker:
      host: "unix:///var/run/docker.sock"
```

For Podman, enable its API service and point the same field at its compatible
socket, for example `unix:///run/user/<uid>/podman/podman.sock`. Do not expose a
daemon TCP listener beyond a protected local network. `docker.isolation` is not
used by Linux containers.

The Docker backend maps a configured cache hostname to Docker's `host-gateway`,
which lets an embedded cache be reached from a Linux container. See
[Cache](#cache) for listener and advertisement rules.

## Windows Docker / standalone Moby

The Docker-compatible Windows backend requires a Windows-container daemon. The
bundled installer enables the Containers feature (and Hyper-V on client
editions), then creates a standalone Moby service and the dedicated pipe:

```powershell
multirunner install-windows-daemon --dry-run
multirunner install-windows-daemon
```

Review the dry-run feature, service, data-root, and reboot plan before approving
the elevated install.

```yaml
pools:
  - name: windows-moby
    os: windows
    backend: docker                 # optional; Docker is the default
    size: 2
    image_tier: dotnet
    labels: [self-hosted, windows, x64, dotnet]
    docker:
      host: "npipe:////./pipe/docker_engine_windows"
      isolation: auto
```

`docker.isolation` accepts `process`, `hyperv`, or `auto`/empty. Automatic
selection works only with a verified-local Windows named pipe: it uses `process`
on Windows Server and `hyperv` on Windows client editions. For a remote/TCP
daemon or a non-Windows controller, choose `process` or `hyperv` explicitly.

`docker.windows_dind` is currently **unused**. Do not rely on its advertised
`off`, `host-pipe`, or `hyperv` values.

`install-windows-daemon` may create the `docker-users` group and add the
invoking user. Require a sign-out/in before assuming the new membership works.

## Windows containerd / runhcs / nerdctl

This is the Windows-native containerd path. Install the supported runtime on a
Windows host, then reboot if the command enables Windows features:

```powershell
multirunner install-containerd --dry-run
multirunner install-containerd
```

Review the dry-run feature, service, download, and reboot plan before approving
the elevated install.

```yaml
pools:
  - name: windows-containerd
    os: windows
    backend: containerd
    size: 2
    image_tier: node
    labels: [self-hosted, windows, x64, node]
    docker:
      host: "required-but-ignored" # current validation workaround
    containerd:
      address: '\\.\pipe\containerd-containerd'
      namespace: multirunner
      isolation: auto
      # nerdctl: 'C:\Program Files\containerd\bin\nerdctl.exe'
```

| `containerd` field | Effect / default |
|---|---|
| `address` | containerd pipe; `\\.\pipe\containerd-containerd` by default. |
| `nerdctl` | Path to `nerdctl.exe`; empty looks it up on `PATH`. |
| `namespace` | `multirunner` by default. |
| `isolation` | `process`, `hyperv`, or `auto`/empty; automatic mode is process on Server and Hyper-V on client. |

**Current implementation constraint:** validation still requires a nonempty
`docker.host` for `backend: containerd`, but the containerd backend ignores it.
Keep the clearly labelled placeholder until that validation is removed. Set
`os: windows`; runhcs reports a Windows container OS regardless of the field.

**Installer boundary:** `install-containerd` takes over the generic
`containerd` service: its apply operation overwrites its configuration, changes
machine PATH, and stops/re-registers that service. Do not use it as a probe or
apply it on a shared containerd host without an approved backup/migration plan.

For cache URLs using a hostname, nerdctl cannot add a host alias on Windows.
multirunner instead rewrites the URL to the Windows NAT gateway when it launches
the container.

## QEMU Windows VM

QEMU is a **Windows guest backend by design**. Configure `os: windows`; the
schema does not currently reject another OS value, but it is unsupported. It
needs `qemu-system-x86_64` and `qemu-img` on `PATH`, a pre-baked qcow2 golden,
and enough disk for one copy-on-write overlay per active slot. It needs no
`docker.host`.

```yaml
pools:
  - name: windows-vm
    os: windows
    backend: qemu
    size: 1
    labels: [self-hosted, windows, x64, vm]
    qemu:
      golden: /var/lib/multirunner/windows-server.qcow2
      work_dir: /var/lib/multirunner/qemu-work
      mem_mb: 4096
      cpus: 2
      accel: ""                    # automatic host-compatible acceleration
      bake_iso: /srv/isos/WindowsServer.iso
      bake_iso_sha256: <64-hex-sha256>
      tools: [dotnet, "node:24", "buildtools:17"]
```

| `qemu` field | Effect / default |
|---|---|
| `golden` | Required path to the baked qcow2 image. |
| `work_dir` | Per-job overlays, JIT ISOs, NVRAM copies, and serial logs; defaults to the OS temp directory plus `multirunner-vm`, which every QEMU pool that omits it shares. Set a dedicated directory per pool and never store a golden in it. `doctor` never cleans it, but a non-dry-run `run` or installed-service start/restart removes matching top-level `*.qcow2`, `*.iso`, `*.vars.fd`, and `*.serial.log` files after golden housekeeping and before the reachability preflight. |
| `mem_mb`, `cpus` | Per-VM resources; values at or below zero use 4096 MB and 2 vCPUs. |
| `accel` | Empty detects KVM (Linux amd64 with usable `/dev/kvm`), WHPX (Windows amd64), HVF (macOS amd64), otherwise TCG. Explicit values are passed to QEMU. |
| `bake_iso`, `bake_iso_sha256` | ISO for automatic rebuilds; the optional 64-hex digest rejects unexpected media. The ISO content is fingerprinted whenever configured. |
| `runner_version`, `runner_sha256` | Runner used for (re)bakes. Empty uses the embedded release-manifest version and digest. A custom version requires its SHA-256 for `bake`, or when configured `bake_iso` makes startup evaluate rebuild inputs; `doctor` does not enforce it for an existing golden alone. |
| `licensed` | Records that a real key/KMS is configured; it skips evaluation-license housekeeping, but does not activate Windows. |
| `tools` | Valid selectors: `dotnet[:major]`, `node[:major]`, `go`, `buildtools[:line]`. Bare `node` expands to every manifest major, bare `dotnet` to every bakeable channel the manifest targets at QEMU, and bare `buildtools` to the manifest default line. They are validated at config load and should match the bake command. |

`image` and `image_tier` are **ignored** for QEMU (a startup warning names the
pool). The backend also silently ignores all launch mounts: `tool_cache`,
Docker-socket/DinD, and `git_cache.mode: mirror` do not reach the VM and
nothing warns. Bake toolchains into the golden;
use `git_cache.mode: dotgit-cache` rather than mount-based mirroring when its
other requirements are met.

With `bake_iso` configured, startup computes the golden fingerprint and can
rebuild a stale golden. Housekeeping runs once at orchestrator startup, not
periodically; drain active VMs before starting a service that could rearm or
rebuild a golden. Never keep a manually launched VM, golden, or forensic
artifact in `work_dir`. See [QEMU Windows runner reference](qemu-windows.md)
for baking, VNC/noVNC viewing, serial logs, and licensing; on macOS also read
[macOS host notes](../references/macos-host.md).

## Tool cache and Docker socket

For container backends, add the named-volume configuration beneath the relevant
pool; it is mounted only when both `mode: shared-volume` and `volume` are set:

```yaml
pools:
  - name: linux-node
    # ... os, labels, and docker configuration ...
    tool_cache:
      mode: shared-volume
      volume: mr-toolcache-linux
      readonly: true
```

| `tool_cache` field | Effect |
|---|---|
| `mode` | Only `shared-volume` mounts the configured named volume; `off` or empty has no effect. |
| `volume` | Required named volume. Empty has no effect. |
| `readonly` | `false` by default; set `true` to prevent jobs from changing the shared cache. |

The target is `/opt/hostedtoolcache` for Linux and
`C:\hostedtoolcache\windows` for Windows. QEMU ignores this mount.

`docker.enable_dind: true` mounts `/var/run/docker.sock` into the runner at the
same path. It is host-Docker socket passthrough, not a nested daemon, and grants
jobs substantial control of that daemon. Keep it disabled unless the job trust
boundary explicitly allows it. It is primarily a Linux socket setting; it does
not make Windows DinD work.

## Cache

The embedded cache runs on the multirunner host when `cache.enabled: true`,
`cache.mode` is not `off`, and `cache.external_url` is empty.

```yaml
cache:
  enabled: true
  mode: local-server
  storage: filesystem
  path: /var/lib/multirunner/cache
  listen: "0.0.0.0:3000"           # firewall to runner networks only
  advertise_url: "http://host.docker.internal:3000"
  access_token: "${MULTIRUNNER_CACHE_ACCESS_TOKEN}"
  skip_token_validation: false
  max_age_days: 7
  max_size_gb: 50
  gc_interval_sec: 3600
```

| Field | Effect / default when enabled |
|---|---|
| `enabled` | `false` by default. `true` is still not enough to start the embedded server when `mode: off` or `external_url` is set. |
| `mode` | Empty becomes `local-server`; `off` prevents cache startup. Other values are not validated; do not use them. |
| `storage` | Empty becomes `filesystem`. The embedded server accepts only `filesystem`; any other value, including `minio`, fails startup. |
| `path` | Filesystem root for `cache.db` and blobs. It has no default; set an absolute writable host path rather than using the service working directory. |
| `listen` | `0.0.0.0:3000`; bind/firewall it deliberately. |
| `advertise_url` | Required for runner redirection; must be reachable from inside every runner. For the embedded server, use its untokenized base URL: Multirunner appends `/_mr/<token>`. Empty leaves runners on the upstream cache. |
| `external_url` | Uses an existing reachable cache instead of starting the embedded server. It still requires `enabled: true` and a mode other than `off`. For `cacheserver`, use the runner-reachable URL including `/_mr/<token>`. Current startup logging records it verbatim, including that path token; restrict service logs and rotate it if exposed. |
| `access_token` | Optional URL-path-safe token (no `/`, `?`, `#`, or escaping); the embedded server generates one on each start if omitted. It is not applied to `external_url`; set the matching token on the external service and include it in that URL. |
| `skip_token_validation` | `false` by default. `true` accepts missing/opaque Actions bearer tokens, while the path token still gates access. |
| `upstream` | Catch-all upstream; defaults to GitHub's results receiver. |
| `max_age_days`, `max_size_gb`, `gc_interval_sec` | Defaults: 7 days, unlimited size (`0` or lower), and 3600 seconds. Negative age disables age expiry; negative interval disables GC. |

Container Docker backends map the advertised hostname to their host gateway;
containerd rewrites it to the Windows NAT gateway; QEMU rewrites it to
`10.0.2.2`. The host listener must accept the resulting traffic. Metrics has no
built-in authentication; cache API access is path-token gated and can
additionally validate Actions bearer tokens, but the embedded cache's /health
endpoint is unauthenticated. Prefer loopback, a private network, or a firewall.

The embedded cache exposes its own `/health`; it is separate from
Multirunner metrics health. An external cache has its own health and retention
model. Changing embedded cache retention, size, or GC settings can cause a
later scheduled GC/sweep to delete existing data; the embedded cache's first
GC tick is after gc_interval_sec. Treat that as a cleanup decision.

## Git cache

```yaml
git_cache:
  mode: mirror                    # mirror | dotgit-cache | off
  path: /var/lib/multirunner/gitmirror
  max_age_days: 30
```

It is active only with `mode: mirror` or `dotgit-cache` and a nonempty `path`.
The current implementation initializes and refreshes mirrors only for
`github.scope: repo`; other scopes log a warning. `0` defaults to 30 days and a
negative `max_age_days` disables pruning.

`mirror` bind-mounts the host bare repository into container runners, so it is
not usable by QEMU. `dotgit-cache` serves a bundle through the configured cache
server and can seed QEMU Windows jobs, but requires all of the following:

- `github.scope: repo`;
- an enabled, runner-reachable embedded cache; and
- a nonempty `cache.advertise_url` so the bundle URL can be injected; and
- no `cache.external_url`, because an external cache does not expose
  Multirunner's git-bundle endpoint.

For private repositories, mirror refresh currently uses `auth.pat`; App
credentials are not supplied to Git fetches. That PAT then takes precedence for
all GitHub API calls as well. Mirror expiry can remove stale data after startup,
so review retention changes as a cleanup action.

## Logging

| Field | Effect / default |
|---|---|
| `log.level` | `info` by default. `debug`, `warn`, and `error` select those levels; any other nonempty value falls back to info. |
| `log.format` | `text` by default. Only `json` selects JSON output; any other value produces text. |

Use debug only for a bounded investigation and redact exported logs.

## Webhook

The `webhook` block is used only with `provisioning: autoscale` or its
compatibility alias `webhook`. It receives GitHub App `workflow_job` queued
events and feeds the autoscaler.

Only a delivery whose `action` is `queued` can launch capacity. Other
`workflow_job` actions return success without launching a runner. Under
`scope: repo` or `repos`, queued events for an empty or unmanaged repository
are ignored. Under `scope: org` or `enterprise` there is no repository filter:
any queued delivery that passes the signature check can launch a runner, so a
nonempty `secret` is the only gate.

| Field | Effect / default |
|---|---|
| `listen` | Empty disables the receiver. A nonempty address starts a plain-HTTP listener when autoscale runs. |
| `path` | `/webhook` when omitted for autoscale; set the same path in the external GitHub App webhook URL. It must begin with `/` and is not config-validated: a value with no `/` panics the orchestrator when the receiver registers, and a value such as `hooks/webhook` registers as a host-scoped route that never matches. |
| `secret` | GitHub HMAC-SHA256 secret. Keep it in an environment reference. Empty only logs a startup warning and then accepts unsigned events; it must not be exposed publicly. |
| `poll_interval_sec` | `0` becomes 300 seconds. A negative value disables polling. Polling works only for `repo` and `repos`; `org` and `enterprise` need a reachable signed webhook or a fixed pool. |

Terminate TLS and apply network restrictions outside Multirunner; the receiver
does not provide TLS. `doctor` does not prove public ingress, webhook
activation, signature verification, or end-to-end runner readiness. `connect`
does not persist this block or activate the App webhook; configure both
separately.

## Metrics and provisioning

Metrics are independent of the cache:

```yaml
metrics:
  listen: "127.0.0.1:9090"
```

An empty `metrics.listen` disables them. A configured listener exposes unauthenticated
`/metrics` and `/health`; proxy or firewall it if monitoring is remote.
`/health` always returns `200 ok` once listening. The listener starts after
GitHub client, golden housekeeping, and cache setup but before pool
preparation, so under the service it does not prove cache, backend, guest,
target/authentication, runner readiness, or even a successful startup. The
series are
`multirunner_runners_active{pool}`, `multirunner_jobs_total{pool,result}`
(`result` is `success` or `error`), and
`multirunner_reprovision_errors_total{pool}`; a `success` count means the
backend wait did not error, so a non-zero container exit still counts as
success and it does not mean GitHub marked the workflow job successful.

`provisioning` changes how the configured host capacity is consumed:

| Mode | Host-facing consequence |
|---|---|
| `pool` | Default; keeps `size` JIT runners per pool registered and idle. |
| `autoscale` / `webhook` | Launches on queued demand; polling defaults to 300 seconds only for `repo`/`repos`. Webhook delivery, when enabled, must be reachable and signed. |
| `scaleset` | GitHub drives demand. It supports `repo`, `org`, and `enterprise`, not `repos`; each pool needs a distinct `scale_set`. `size` remains the maximum concurrent runners. Startup creates, reuses, or updates remote scale-set labels and `runner_group` state. |

For a scale set with empty `labels`, GitHub receives the scale-set name as
the label. Treat a scale-set start/restart as a GitHub mutation and retain the
prior remote labels and runner group for rollback.

## Configuration lifecycle tools

The [CLI reference](cli-reference.md) is the canonical flag catalog. These are
the host-configuration commands to use at each lifecycle boundary:

| Command | Configuration role |
|---|---|
| `multirunner doctor --config <path>` | Read-only preflight for configured backends and repository-scoped checks; it does not prove cache, webhook, or guest readiness. |
| `multirunner run --config <path> --dry-run` | Loads the config and prints startup effects without starting services or runners. Run without `--dry-run` only after approval. |
| `multirunner detect` | Recommends a container image tier and pool block without editing the configuration. |
| `multirunner connect --dry-run` / `connect` | Preview or create App authentication and target changes; the apply command writes a key and YAML. |
| `multirunner service <action> --dry-run` | Preview service installation or control; `install` records the resolved config path. |
| `multirunner bake --dry-run` | Validate QEMU bake inputs and planned writes. Use [QEMU Windows](qemu-windows.md) for actual bake, VNC, and golden lifecycle guidance. |
| `multirunner install-windows-daemon --dry-run` / `install-containerd --dry-run` | Preview Windows runtime feature, download, service, and reboot effects before elevation. |
| `cacheserver` | Run an external cache. Give it a persistent `--access-token`, then set `cache.external_url` to the matching tokenized URL. |

`help` and `completion` do not configure a runner host. Hidden QEMU helpers
are diagnostic interfaces, not normal host configuration.

## Current implementation constraints

| Constraint | Operator action |
|---|---|
| `backend: containerd` requires nonempty `docker.host` | Supply a labelled placeholder; it is ignored by containerd. |
| `cache.storage: minio` | Do not configure it; filesystem is the only supported storage. |
| `docker.windows_dind` | Treat as inert; no Windows DinD mode is implemented. |
| QEMU `image`, `image_tier`, and mounts | Bake tools; use `dotgit-cache` for VM checkout acceleration. |
| Unknown `image_tier` | Build/preload the generated local `:dev` image yourself. |
| QEMU pool OS | Use `os: windows`; QEMU is a Windows backend by intent. |

## Minimal complete configurations

Each block below is a whole `config.yaml` that passes current validation. Swap
`auth.pat` for the `app_id`/`installation_id`/`private_key_path` trio where App
authentication is required.

### `provisioning: pool` — Linux Docker

```yaml
github:
  scope: repo
  owner: my-org
  repo: my-repo

auth:
  pat: "${GITHUB_PAT}"

provisioning: pool

pools:
  - name: linux
    os: linux
    size: 2
    labels: [self-hosted, linux, x64]
    docker:
      host: "unix:///var/run/docker.sock"
```

### `provisioning: autoscale` — Windows Docker (standalone Moby)

Polling is outbound-only and works for `repo`/`repos`. `webhook.listen` is
optional in the schema but required in practice for `org`/`enterprise`, which
cannot poll queued jobs.

```yaml
github:
  scope: org
  owner: my-org

auth:
  pat: "${GITHUB_PAT}"

provisioning: autoscale

webhook:
  listen: "0.0.0.0:8080"
  path: /webhook
  secret: "${GITHUB_WEBHOOK_SECRET}"
  poll_interval_sec: 300

pools:
  - name: windows
    os: windows
    size: 2
    image_tier: dotnet
    labels: [self-hosted, windows, x64, dotnet]
    docker:
      host: "npipe:////./pipe/docker_engine_windows"
      isolation: auto
```

### `provisioning: scaleset` — Windows containerd

`scaleset` supports `repo`, `org`, and `enterprise` but not `repos`. Every pool
needs its own unique `scale_set`. `docker.host` is still required by validation
even though the containerd backend ignores it.

```yaml
github:
  scope: org
  owner: my-org

auth:
  pat: "${GITHUB_PAT}"

provisioning: scaleset

pools:
  - name: windows-containerd
    os: windows
    backend: containerd
    size: 3
    image_tier: node
    labels: [self-hosted, windows, x64, node]
    scale_set: multirunner-windows
    runner_group: "Default"
    docker:
      host: "required-but-ignored" # current validation workaround
    containerd:
      address: '\\.\pipe\containerd-containerd'
      namespace: multirunner
      isolation: auto
```

### QEMU Windows VM on a Linux host

`backend: qemu` requires `qemu.golden` and takes no `docker.host`.

```yaml
github:
  scope: repo
  owner: my-org
  repo: my-repo

auth:
  pat: "${GITHUB_PAT}"

provisioning: pool

pools:
  - name: windows-vm
    os: windows
    backend: qemu
    size: 1
    labels: [self-hosted, windows, x64, vm]
    qemu:
      golden: /var/lib/multirunner/golden-servercore.qcow2
      work_dir: /var/lib/multirunner/vm
      mem_mb: 4096
      cpus: 2
```

## Complete key index

Every key the loader accepts, with its YAML type, default, and constraint. Keys
not listed here are rejected: YAML parsing is strict. "when enabled" means the
default is applied only while the owning feature is switched on.

| Key | Type | Default / constraint |
|---|---|---|
| `github.url` | string | `https://github.com`. GHES web base URL, not `/api/v3`. |
| `github.scope` | string | Required, one of `repo`, `repos`, `org`, `enterprise`. |
| `github.owner` | string | Trimmed. Required for `repo`/`org`/`enterprise`; for `repos` required unless every entry is `owner/repo`. |
| `github.repo` | string | Required for `scope: repo`. Ignored, with a warning, for `scope: repos`. |
| `github.repos` | list of string | Required nonempty for `scope: repos`. Entry is `repo` or `owner/repo`; trimmed; no whitespace; at most one `/`; no case-insensitive duplicates; with App auth all entries must share one owner. |
| `auth.pat` | string | Empty. `${VAR}` expanded. Nonempty wins over App credentials. |
| `auth.app_id` | integer | `0`. Required together with `installation_id` and `private_key_path` when no PAT is set. |
| `auth.installation_id` | integer | `0`. Required with `app_id`. |
| `auth.private_key_path` | string | Empty. `${VAR}` expanded. Required with `app_id`. |
| `provisioning` | string | `pool`. Also `autoscale`, `webhook` (alias for autoscale), and `scaleset`. Any other value is rejected. |
| `cache.enabled` | bool | `false`. |
| `cache.mode` | string | `local-server` when enabled. `off` prevents startup; other values are not validated. |
| `cache.storage` | string | `filesystem` when enabled. Any other nonempty value fails cache startup. |
| `cache.path` | string | No default. Root for `cache.db` and `blobs/`; set an absolute writable path. |
| `cache.listen` | string | `0.0.0.0:3000` when enabled. |
| `cache.advertise_url` | string | Empty, which leaves runners on the upstream cache. Trailing `/` is trimmed and `/_mr/<token>` is appended. |
| `cache.external_url` | string | Empty. Nonempty uses an existing cache instead of starting the embedded server; it still needs `enabled: true` and a `mode` other than `off`. |
| `cache.access_token` | string | Empty generates one per start. `${VAR}` expanded. Must be URL-path-safe (no `/`, `?`, `#`, and no character needing percent-escaping) or cache startup fails. Not applied to `external_url`. |
| `cache.skip_token_validation` | bool | `false`. `true` accepts opaque or missing Actions bearer tokens; the path token still gates access. |
| `cache.upstream` | string | `https://results-receiver.actions.githubusercontent.com` when enabled. Must parse as a URL. Empty disables the catch-all proxy. |
| `cache.max_age_days` | integer | `7` when enabled. `0` or lower after defaulting disables age expiry. |
| `cache.max_size_gb` | integer | `0` = unlimited. |
| `cache.gc_interval_sec` | integer | `3600` when enabled. `0` or lower after defaulting disables GC. |
| `git_cache.mode` | string | Empty (off). `mirror` or `dotgit-cache` enable it, and only together with a nonempty `path`. |
| `git_cache.path` | string | No default. Required nonempty for either enabled mode. |
| `git_cache.max_age_days` | integer | `30` when enabled. `0` or lower never prunes. |
| `webhook.listen` | string | Empty disables the receiver. Plain HTTP; autoscale modes only. |
| `webhook.path` | string | `/webhook` when provisioning is `autoscale`/`webhook`; not defaulted in other modes. Must begin with `/`; not config-validated. |
| `webhook.secret` | string | Empty accepts unsigned events after a startup warning. `${VAR}` expanded. |
| `webhook.poll_interval_sec` | integer | `300` when provisioning is `autoscale`/`webhook`. Negative disables polling. Polling only works for `repo`/`repos`. |
| `metrics.listen` | string | Empty disables `/metrics` and `/health`. No authentication. |
| `log.level` | string | `info`. `debug`, `warn`, `error` select those levels; any other value falls back to info. |
| `log.format` | string | `text`. Only `json` selects JSON output. |
| `pools` | list | At least one entry is required. |
| `pools[].name` | string | Required, unique across pools. |
| `pools[].os` | string | Required, `linux` or `windows`. |
| `pools[].backend` | string | Empty = `docker`. Also `containerd` (Windows) and `qemu` (Windows VM). Any other value falls through to Docker behavior; do not rely on that. |
| `pools[].size` | integer | `0` becomes `1`; negative is rejected. |
| `pools[].image` | string | Empty. Wins over `image_tier`. Ignored by `qemu`. |
| `pools[].image_tier` | string | `minimal`. A published flavor pulls its tag; an unknown but syntactically valid tier resolves to a local `multirunner/runner-<os>-<tier>:dev` that is never built for you. An unknown tier must be `[a-z0-9]` groups separated by `.`, `_`, or `-`, and at most 64 characters, otherwise config load fails. Not validated when `image` is set or `backend: qemu`. |
| `pools[].labels` | list of string | Empty. Routing labels. A scale set with empty labels receives its scale-set name as the label. |
| `pools[].runner_group_id` | integer | `0` becomes `1`. Used by `pool`/`autoscale`; scale sets resolve the group from `runner_group` instead. |
| `pools[].work_folder` | string | `_work`. |
| `pools[].name_prefix` | string | `multirunner`. Runner names become `<prefix>-<os>-<random>`. Not used by scale sets. |
| `pools[].max_consecutive_failures` | integer | `0` becomes `5`. Log threshold only; retries never stop. |
| `pools[].scale_set` | string | Empty. Required, and unique per pool, when `provisioning: scaleset`; ignored in other modes. |
| `pools[].runner_group` | string | Empty selects GitHub's default group. Scale-set mode only. |
| `pools[].docker.host` | string | Required nonempty for every pool whose `backend` is not `qemu`, including `containerd`, which ignores the value. |
| `pools[].docker.enable_dind` | bool | `false`. `true` mounts `/var/run/docker.sock` into the runner at the same path. Linux container backends. |
| `pools[].docker.isolation` | string | Empty/`auto`, `process`, or `hyperv`. Windows Docker backend only. `auto` requires a verified-local `npipe://` host and otherwise fails backend construction. Unused on Linux. |
| `pools[].docker.windows_dind` | string | Parsed but never read. Inert. |
| `pools[].tool_cache.mode` | string | Empty or `off` = no mount. Only `shared-volume` mounts, and only with a nonempty `volume`. |
| `pools[].tool_cache.volume` | string | Empty = no mount. Named volume. |
| `pools[].tool_cache.readonly` | bool | `false`. |
| `pools[].containerd.address` | string | `\\.\pipe\containerd-containerd`. |
| `pools[].containerd.nerdctl` | string | Empty looks up `nerdctl.exe` on `PATH`; backend construction fails when it is not found. |
| `pools[].containerd.namespace` | string | `multirunner`. |
| `pools[].containerd.isolation` | string | Empty/`auto` = process on Windows Server, Hyper-V on client. Also `process` or `hyperv`. |
| `pools[].qemu.golden` | string | Required for `backend: qemu`. Path to the baked qcow2. |
| `pools[].qemu.work_dir` | string | Empty = `<OS temp dir>/multirunner-vm`, shared by every QEMU pool that omits it. Use a dedicated directory per pool. |
| `pools[].qemu.mem_mb` | integer | `0` or lower becomes `4096`. |
| `pools[].qemu.cpus` | integer | `0` or lower becomes `2`. |
| `pools[].qemu.accel` | string | Empty autodetects (`kvm`/`whpx`/`hvf`, otherwise `tcg`). Any other value is passed to QEMU as written; a resolved `tcg` logs a startup warning. |
| `pools[].qemu.bake_iso` | string | Empty disables automatic rebuilds. Nonempty makes startup and `run --dry-run` hash the whole ISO. |
| `pools[].qemu.bake_iso_sha256` | string | Empty. 64 hexadecimal characters; enforced on the bake path, not at config load. |
| `pools[].qemu.runner_version` | string | Empty uses the embedded manifest version (`2.337.0` in the current release). |
| `pools[].qemu.runner_sha256` | string | Empty. Required when `runner_version` is non-default; enforced on the bake path, not at config load. |
| `pools[].qemu.licensed` | bool | `false`. `true` skips evaluation-license housekeeping; it does not activate Windows. |
| `pools[].qemu.tools` | list of string | Empty. Selectors `dotnet[:major]`, `node[:major]`, `go`, `buildtools[:line]`; validated at config load, so an unknown selector fails startup. |
