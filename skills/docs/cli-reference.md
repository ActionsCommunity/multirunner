# Multirunner CLI reference

This is the operator-facing reference for the `multirunner` binary. The
installed binary remains authoritative: use `multirunner <command> --help` to
check its version.

## Invocation and shared flags

`multirunner` with no subcommand is the same as `multirunner run`.

| Flag | Where it applies | Meaning |
| --- | --- | --- |
| `--config <path>` | Persistent; accepted by every command | YAML configuration path; defaults to `config.yaml` in the current directory. |
| `-h`, `--help` | Every command | Print command help. |
| `-v`, `--version` | Root command only | Print the binary version. It is not a `run` flag. |
| `--install-deps` | Root invocation and `run` only | Permit automatic installation of a missing Windows Docker daemon; it may elevate. It is not accepted by `doctor`, `service`, or other subcommands. |
| `--dry-run` | Root invocation and `run`; also `connect`, `bake`, every `service` action, `install-windows-daemon`, and `install-containerd` | Validate inputs and print the operation plan without making changes. `bake --dry-run`, and `run --dry-run` with `qemu.bake_iso` configured, still hash the whole ISO. |

Only commands that need configuration load it. `bake`, the Windows installers,
completion generation, and local `detect --path` accept the inherited
`--config` flag but do not read that file. `detect --repo` loads it for GitHub
authentication. `connect` reads/re-writes its YAML target but does not validate
the whole configuration. Service actions resolve the path, while `service
install` stores it for the daemon; they do not parse it at command time.

## Config and `.env` resolution

The loader rejects unknown YAML fields. It treats a value as an environment
reference whenever it begins with `$`, taking the whole remainder (with
optional braces) as the variable name, and only for:

- `auth.pat`
- `auth.private_key_path`
- `webhook.secret`
- `cache.access_token`

An unset referenced variable becomes empty and may then fail validation. Other
configuration fields, including ordinary paths, are passed through as written;
use absolute paths when the working directory is uncertain.

Before parsing configuration, Multirunner reads `.env` files in this order:

1. `<directory containing the config>/.env`
2. `./.env`

An already-exported process environment value wins over both files, and a value
from the first file wins over the second. Blank lines, comments, an optional
`export ` prefix, and matching single/double value quotes are supported.

The CLI makes the `--config` argument absolute for normal orchestrator commands.
`service install` also makes the configuration directory the service working
directory and records `run --config <absolute-path>` in the service definition.

## Public commands

### `run` (and the default command)

```text
multirunner [--config <path>] [--install-deps] [--dry-run]
multirunner run [--config <path>] [--install-deps] [--dry-run]
```

Loads and validates configuration, prepares every backend, starts enabled cache
and metrics services, starts the configured webhook receiver in
`autoscale`/`webhook` mode, and maintains the configured ephemeral runner capacity.
It creates runners and may pull images, create cache/mirror state, start servers,
and perform configured QEMU golden housekeeping.

`--dry-run` loads and validates the config and reports backend preparation,
image pulls, cache/mirror state, listeners, runner registrations, scale-set
updates, and QEMU cleanup or golden-housekeeping decisions without performing
them. It does not contact GitHub or runtimes and is a plan, not a health check;
use the read-only `doctor` command for current-state validation.

`--install-deps` is deliberately narrow despite its broad help text: it only
auto-installs the standalone Windows Docker daemon when an unreachable pool is
`os: windows` with the default/Docker backend. An interactive foreground run
prompts without the flag; a non-interactive run fails with guidance. It does not
install containerd/nerdctl automatically.

### `doctor`

```text
multirunner doctor [--config <path>]
```

Checks configured backend reachability and container OS mode without launching
runners. For `repo` and `repos` scope it also checks whether Actions is enabled
and scans workflow files for a literal `self-hosted` label. The latter is
heuristic: custom-label and matrix forms can be missed. Actions disabled or an
incomplete scan makes `doctor` exit non-zero; a scan that finds no
`self-hosted` label is only a note. For `org` and `enterprise` scope no GitHub
call is made, so credentials are not validated. It also flags undersized fixed
pools for `repos` scope and advisory git-configuration risks when the git cache
is enabled; that git audit never fails preflight. All pool pings share one
20-second budget, so a hung daemon can make later pools report `UNREACHABLE`.

`doctor` is read-only. For QEMU pools it locates the configured binaries and
stats the golden file (no `MR:GOLDEN_OK`, metadata, accelerator, or disk
check) without cleaning `qemu.work_dir`; orphan cleanup occurs only when the
orchestrator starts. It does not hash `qemu.bake_iso`; `run --dry-run` and
real startup do.

### `connect`

```text
multirunner connect --org <org> [options]
multirunner connect --repo <owner/repo> [options]
```

| Flag | Default | Meaning |
| --- | --- | --- |
| `--org <login>` | empty | Create/install an organization-scoped GitHub App. |
| `--repo <owner/repo>` | empty | Create/install a repository-scoped GitHub App. |
| `--name <name>` | `multirunner` | Requested GitHub App name. |
| `--port <port>` | `0` | Loopback browser-flow callback port; `0` chooses one automatically. |
| `--key-out <path>` | config directory + `multirunner-app.private-key.pem` | Private-key output path. |
| `--dry-run` | false | Print the App permissions, callback, target, and local writes without opening a browser or changing GitHub/files. |

The command opens a local browser flow, waits for App creation and installation,
writes the PEM private key with mode `0600`, and rewrites the config's `github`
and `auth` sections. It removes `auth.pat` and writes App ID, installation ID,
and private-key path.

Apply is not transactional: the PEM write happens before the YAML update, so a
YAML failure can leave a new key behind. An `--org` apply updates scope and owner
but does not clear pre-existing `github.repo` or `github.repos`; back up the YAML
and deliberately remove stale target fields before relying on it.

Run the same command with `--dry-run` first. It only validates target syntax and
derives the displayed paths; it does not read or validate the existing config or
key-output path, reserve the callback port, or prove GitHub authorization or
App-name availability.

Current implementation details worth planning around:

- Supply exactly one target. Supplying both `--org` and `--repo` is rejected.
- The App manifest requests `workflow_job`, but its webhook is created inactive.
  The command receives a webhook secret from GitHub but does **not** write
  `webhook.secret`, `webhook.listen`, or a reachable webhook URL. Configure and
  enable autoscale webhooks separately.
- The browser flow does not use `github.url`; it currently defaults to
  GitHub.com. Do not assume `--config` makes this flow GHES-aware.
- Back up the target YAML first. If the existing file cannot be parsed as a
  root mapping, current behavior starts a new mapping and writes that instead.

### `detect`

```text
multirunner detect [--path <checkout>] [--repo <owner/repo>] [--os linux|windows]
```

| Flag | Default | Meaning |
| --- | --- | --- |
| `--path <path>` | `.` | Local checkout to inspect; no network access. |
| `--repo <owner/repo>` | empty | Remote repository to inspect using the config's GitHub auth. Takes precedence over `--path`. |
| `--os <linux|windows>` | `linux` | Target OS for the generated pool block. |

Prints recommendations and a ready-to-paste `pools:` block; it does not edit
the configuration. Workflow routing remains label-based, so apply the suggested
labels in `runs-on` as well as the generated pool configuration.

### `bake`

```text
multirunner bake --iso <Windows-Server.iso> --golden <golden.qcow2> [options]
```

Builds a golden Windows Server Core qcow2 image for a `backend: qemu` pool. It
needs `qemu-system-x86_64`, `qemu-img`, and a Windows Server ISO. A successful
bake verifies the guest's `MR:GOLDEN_OK` serial marker before saving metadata.

| Flag | Default | Meaning |
| --- | --- | --- |
| `--iso <path>` | required | Windows Server ISO. |
| `--iso-sha256 <hex>` | empty | Expected ISO SHA-256; rejects a mismatch. |
| `--golden <path>` | required | Golden qcow2 output path. |
| `--disk-gb <n>` | `40` | Golden disk size in GB. |
| `--mem-mb <n>` | `4096` | Installer VM memory in MB. |
| `--cpus <n>` | `2` | Installer VM vCPUs. |
| `--accel <kvm, whpx, hvf, tcg>` | auto | QEMU accelerator. |
| `--runner-version <version>` | `2.337.0` in the current release | Actions runner version to bake. |
| `--runner-sha256 <hex>` | empty | Required when `--runner-version` is not the built-in version. |
| `--tools <selectors>` | empty | Comma-separated/repeatable `dotnet[:major]`, `node[:major]`, `go`, and `buildtools[:line]` selectors. |
| `--licensed` | false | Mark the image as key/KMS-licensed; skips evaluation-license housekeeping. |
| `--vnc <host:port>` | `127.0.0.1:0` | Raw VNC listener; port `0` selects a free port, otherwise the port must be at least 5900 and differ from `--vnc-web`. Empty disables it when `--vnc-web` is also empty. |
| `--vnc-web <host:port>` | `127.0.0.1:8090` | Serve an embedded noVNC page for the install; the port cannot be `0`; empty disables it. |
| `--dry-run` | false | Validate and hash inputs, then print artifact, executable, VM, and VNC plans without creating files or starting QEMU. |
| `--prepare-only` | false | Prepare artifacts and print QEMU arguments, without launching QEMU. |

Use `--dry-run` before approval. The bake deadline is fixed: 45 minutes, at
least 75 minutes once any .NET/Node/Go tool is selected, plus 90 minutes per
Build Tools line; there is no override flag, and QEMU is killed on expiry with
the serial tail in the error. `--prepare-only` is not a dry run: it runs
`qemu-img create`, which overwrites an existing golden in place, removes the
existing `<golden>.serial.log`, creates `<golden>.autounattend.iso` (containing
the plaintext guest Administrator password; only a full bake deletes it), and,
with detected
UEFI firmware, creates `<golden>.vars.fd`; it can also stage verified downloads
in a temporary directory. It starts neither QEMU nor a VNC/noVNC listener, so
the printed endpoints are not live; it merely skips starting the QEMU installer
process.

With `--vnc-web` enabled, its host also controls QEMU's raw VNC and WebSocket
bind address. A normal bake starts the listener(s) and prints their dynamic
addresses and web URL. `--prepare-only` only prints a prospective plan; port-0
choices are transient, and no viewer is running until QEMU is started manually
(with `_vmview` for the browser page). The WebSocket is noVNC transport, not a
web page. The generated arguments do not set a VNC password and the embedded
viewer has no authentication layer; keep the loopback defaults or firewall the
listeners. `--vnc-web ""` disables both the viewer and default VNC; an explicit
`--vnc` enables raw VNC alone. Use the same tool selectors under `qemu.tools`
when automatic QEMU golden rebuilding is desired.

### `install-windows-daemon`

```text
multirunner install-windows-daemon [--data-root <path>] [--dry-run]
```

Windows only. Without `--dry-run`, it elevates through UAC, enables the Windows
Containers feature (plus Hyper-V on client editions, where Hyper-V isolation is
written into `daemon.json`), and installs a standalone Moby daemon in
Windows-container mode at
`npipe:////./pipe/docker_engine_windows`. Downloads are not checksum-verified;
only the downloaded `dockerd` version string is compared. A reboot may be
required.
An apply may create the `docker-users` group and add the invoking user; a
sign-out/in may be needed before that membership takes effect.

| Flag | Default | Meaning |
| --- | --- | --- |
| `--data-root <path>` | `%ProgramData%\multirunner\docker\data` | Image/container store; point it at an existing store to retain images. `run --install-deps` always uses the default. |
| `--dry-run` | false | Inspect current feature, service, and reboot state and print the planned changes without elevation or mutation. |

### `install-containerd`

```text
multirunner install-containerd [--dry-run]
```

Windows only. Without `--dry-run`, it elevates to install containerd, runhcs,
nerdctl, and Windows CNI plugins, enabling both the Containers and Hyper-V
features unconditionally. Downloads are not checksum-verified, and binaries
that already exist are skipped rather than upgraded, while the containerd
configuration, machine PATH, and service are rewritten every run. It registers
containerd at `\\.\pipe\containerd-containerd`. The installer sets no
isolation; `containerd.isolation` decides at launch (`auto` picks process on
Windows Server and Hyper-V on client editions). `--dry-run` inspects current
feature, service, and reboot state and prints the planned changes without
elevation or mutation. An apply may require a reboot.

An apply takes over the generic `containerd` service: it overwrites its
configuration, changes machine PATH, and stops/re-registers that service. Do not
use it as a probe or apply it over a shared containerd deployment without an
approved backup/migration plan.

Run the installer with `--dry-run` before requesting approval. Windows restricts
optional-feature inspection to elevated processes, so a non-elevated dry run may
report those feature states as unknown; it still reports service and pending
reboot state, conditional feature actions, and whether applying may require a
reboot. Running the installer without `--dry-run` remains the explicit apply
operation and triggers UAC. The commands do not prompt separately for Y/N, which
keeps scripted use deterministic.

### `service`

```text
multirunner service [--dry-run] <install|uninstall|start|stop|restart> [--config <path>]
```

Supported service managers are Windows SCM, systemd on Debian/Ubuntu, and macOS
launchd. All actions require administrator/root privileges.

`--dry-run` queries service state and reports the selected action, resolved
configuration path, and effect without changing the service. It does not require
elevation. Place it before or after the action; for example,
`multirunner service restart --dry-run --config <path>`.

| Action | Effect |
| --- | --- |
| `install` | Creates the `multirunner` service and stores the resolved config path as `run --config <path>`. |
| `uninstall` | Removes that service definition. |
| `start` | Starts the existing service. |
| `stop` | Stops the existing service. |
| `restart` | Restarts the existing service. |

`--config` is material to `install`; the other actions accept it because it is
global but do not load the file. The installed definition does not include
`--install-deps`.

### `completion`

```text
multirunner completion <bash|zsh|fish|powershell> [--no-descriptions]
```

Writes a completion script to standard output. Each shell command accepts only
`--no-descriptions` in addition to help.

```sh
source <(multirunner completion bash)
source <(multirunner completion zsh)
multirunner completion fish | source
multirunner completion powershell | Out-String | Invoke-Expression
```

### `help`

```text
multirunner help [command]
```

Prints built-in help for the root command or a subcommand. `--help` is the
equivalent flag form; neither operation loads configuration.

## Hidden QEMU developer helpers

These commands are intentionally omitted from normal help. They are diagnostic
tools, not a stable operator interface; their inherited `--config` flag is
unused.

| Command | Flags and defaults | Effect |
| --- | --- | --- |
| `multirunner _bootkeys` | `--qmp 127.0.0.1:4445`, `--presses 15` | Sends repeated Enter keypresses through QMP. Despite an old source comment, it does not issue a VM reset. |
| `multirunner _screenshot` | `--qmp 127.0.0.1:4445`, `--out shot.png` | Uses QMP `screendump` to write a PNG. |
| `multirunner _vmview` | `--http 127.0.0.1:8090`, `--ws-port 5701` | Serves the embedded noVNC client; it does not start QEMU or proxy the WebSocket. |
| `multirunner _jitiso` | `--jit <blob>`, `--out jit.iso` | Writes a JIT-config ISO. Treat the blob and ISO as sensitive. |

Port defaults require care: a bake uses QMP `127.0.0.1:4455`; its raw VNC and
noVNC WebSocket ports are selected dynamically and printed. Runtime VM debugging via
`MULTIRUNNER_VM_VNC="0.0.0.0:2,websocket=5702"` uses QMP
`127.0.0.1:4457`. The screenshot/key helpers default to `4445`, so pass
`--qmp` explicitly for bake or runtime-debug VMs. For the runtime example, use
`_vmview --ws-port 5702` and ensure the browser can reach that WebSocket port.

## Companion binaries

### `cacheserver`

`cacheserver` runs the self-hosted Actions cache separately from the
orchestrator, for example on a shared cache host.

| Flag | Default | Meaning |
| --- | --- | --- |
| `--listen <host:port>` | `0.0.0.0:3000` | HTTP listen address. |
| `--path <path>` | `/data` | Directory for `cache.db` and blobs. |
| `--advertise <URL>` | empty | Advertised base URL; informational to this standalone process. |
| `--access-token <token>` | generated | URL-path-safe private cache token. |
| `--upstream <URL>` | `https://results-receiver.actions.githubusercontent.com` | Catch-all proxy upstream. |
| `--skip-token-validation` | `true` | Accept opaque/missing Actions bearer tokens. It does not remove the private path-token check. |

This process writes its storage path. For an external Multirunner cache, choose
a persistent explicit `--access-token` and set `cache.external_url` to the
matching URL including `/_mr/<token>`; `--advertise` alone does not attach the
server to an instance. Its default listener is network-wide, so use a firewall,
private network, or a deliberate reverse proxy.

## Environment variables

Multirunner reads no environment variable for its own configuration; the
configuration file is the interface. These variables are read by specific code
paths.

| Variable | Read by | Effect |
| --- | --- | --- |
| Any name referenced as `${NAME}` / `$NAME` in `auth.pat`, `auth.private_key_path`, `webhook.secret`, or `cache.access_token` | Config loader, for every command that loads configuration | Supplies the secret. Unset becomes empty, which usually then fails validation. Also satisfied from `<config-dir>/.env` or `./.env`. |
| `MULTIRUNNER_VM_VNC` | `run` / orchestrator, QEMU runtime VMs | When nonempty it is passed to QEMU as `-vnc <value>` and additionally opens QMP on `127.0.0.1:4457`; otherwise the VM runs with `-display none`. Example: `0.0.0.0:2,websocket=5702`. Debug only: it exposes an unauthenticated console. |
| `GIT_CONFIG_COUNT`, `GIT_CONFIG_KEY_<n>`, `GIT_CONFIG_VALUE_<n>` | Git mirror subprocesses when `git_cache` is enabled | Inherited indexed git config is stripped and selectively rebuilt. An unparsable or negative count is ignored with a warning, an oversized count is truncated, an inherited `http.*.extraheader` carrying `Authorization:` is dropped, and while a PAT is in use inherited `url.*.insteadOf` / `pushInsteadOf` rewrites are dropped. |
| `GIT_TERMINAL_PROMPT` | Git mirror subprocesses | Forced to `0` by Multirunner; a value set on the host is overridden. |
| `ProgramData` | Windows installers | Read by Multirunner only for the installer status and log file paths. The default data root `%ProgramData%\multirunner\docker\data` is a literal that PowerShell expands, not Multirunner. |

`GITHUB_PAT`, `GITHUB_WEBHOOK_SECRET`, and `MULTIRUNNER_CACHE_ACCESS_TOKEN` are
not special names: they are only the conventional names used by the shipped
example configuration's `${...}` references.

## Exit codes

The binary uses two exit codes.

| Code | Meaning |
| --- | --- |
| `0` | The command completed. For `doctor`, every check passed. For `run`, the orchestrator shut down cleanly after a signal. |
| `1` | Any error returned by the command: unknown flag or argument (every Multirunner command rejects positional arguments; only the generated `help` command takes one), config read/parse/validation failure, `doctor` finding a problem (`preflight found problems`), a backend or GitHub failure during startup, a failed bake, or a failed service/installer action. |

There is no distinct code per failure class; parse the message, not the code.
`--dry-run` variants still exit `1` when their inputs are invalid, so they are
usable as a validation gate in scripts.

## Command and flag index

Every command the binary registers, with its own flags. `--config` is
persistent and therefore accepted everywhere; `--help` likewise.

| Command | Own flags (default) | Loads config |
| --- | --- | --- |
| `multirunner` (root, = `run`) | `--install-deps` (false), `--dry-run` (false), `--version`/`-v` (prints the fixed string `0.1.0-dev`; not a release identifier) | yes |
| `run` | `--install-deps` (false), `--dry-run` (false) | yes |
| `doctor` | none | yes |
| `connect` | `--org` (empty), `--repo` (empty), `--name` (`multirunner`), `--port` (`0`), `--key-out` (empty => `<config dir>/multirunner-app.private-key.pem`), `--dry-run` (false) | no (rewrites the YAML target sections only) |
| `detect` | `--path` (`.`), `--repo` (empty), `--os` (`linux`) | only with `--repo` |
| `bake` | `--iso`, `--iso-sha256`, `--golden`, `--disk-gb` (`40`), `--mem-mb` (`4096`), `--cpus` (`2`), `--accel` (empty=auto), `--runner-version` (`2.337.0`), `--runner-sha256`, `--tools` (empty), `--licensed` (false), `--vnc` (`127.0.0.1:0`), `--vnc-web` (`127.0.0.1:8090`), `--dry-run` (false), `--prepare-only` (false) | no |
| `install-windows-daemon` | `--data-root` (empty => `%ProgramData%\multirunner\docker\data`), `--dry-run` (false) | no |
| `install-containerd` | `--dry-run` (false) | no |
| `service` | `--dry-run` (false, persistent to its actions) | no |
| `service install\|uninstall\|start\|stop\|restart` | inherits `--dry-run` | no |
| `completion bash\|zsh\|fish\|powershell` | `--no-descriptions` (false) | no |
| `help [command]` | none | no |
| `_bootkeys` (hidden) | `--qmp` (`127.0.0.1:4445`), `--presses` (`15`) | no |
| `_screenshot` (hidden) | `--qmp` (`127.0.0.1:4445`), `--out` (`shot.png`) | no |
| `_vmview` (hidden) | `--http` (`127.0.0.1:8090`), `--ws-port` (`5701`) | no |
| `_jitiso` (hidden) | `--jit` (empty), `--out` (`jit.iso`) | no |

`--os` on `detect` accepts only `linux` or `windows`; anything else is rejected
before the scan. `connect` requires exactly one of `--org` or `--repo`, and
`--repo` must be `owner/repo`. Every Multirunner command rejects positional
arguments; only the generated `help` command takes one.

The `--dry-run` variables on the root command and on `run` are the same
setting, so `multirunner --dry-run` and `multirunner run --dry-run` behave
identically; the same is true of `--install-deps`.

## Operational checklist

- `doctor` is read-only. Before a non-dry-run `run` or installed-service
  start/restart, ensure each QEMU work directory is dedicated and idle: startup
  runs golden housekeeping (possible headless rearm/rebuild) first, then, pool
  by pool in YAML order, top-level orphan-artifact cleanup in that pool's
  `work_dir` followed by its reachability preflight. One unreachable pool
  aborts startup for every pool.
- Treat `run`, `connect`, both Windows installers, `service` actions, `bake`,
  and the companion binaries as state-changing operations.
- Keep private keys, PATs, webhook secrets, cache path tokens, JIT blobs, and
  VNC endpoints out of terminal history, logs, and untrusted networks.
- In autoscale mode, configure a reachable and signed webhook separately after
  `connect`; polling remains the outbound-only fallback for repository scope.
