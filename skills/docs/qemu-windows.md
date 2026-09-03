# QEMU Windows runner reference

Use the QEMU backend for Windows workloads that need a full VM, full Visual
Studio, or a tool combination unavailable in the Windows container images. It
boots an x86-64 Windows Server Core golden image; every job gets a disposable
copy-on-write disk and a JIT-configuration ISO, then powers off. The
[CLI reference](cli-reference.md#bake) holds the authoritative `bake` flag
table and [host configuration](host-configuration.md) the surrounding pool
fields.

## Host requirements

Install these on the machine that runs `multirunner`:

- `qemu-system-x86_64` and `qemu-img` on `PATH`.
- A Windows Server ISO, enough local disk for the golden and concurrent overlays,
  and a valid Windows licence or evaluation plan.
- OVMF/edk2 firmware where available. multirunner finds common firmware paths
  automatically; without it, it falls back to BIOS.

Acceleration is chosen when `qemu.accel` is empty:

| Host | Accelerator | Notes |
|---|---|---|
| Linux `amd64` | KVM | Requires the process to open `/dev/kvm`. |
| Windows `amd64` | WHPX | Enable host virtualization first. |
| macOS `amd64` | HVF | Intel Macs only. |
| ARM host or unsupported host | TCG | Supported x86 emulation; much slower, especially for bakes. |

There is no QEMU installer command. Install the QEMU system emulator, image
utility, and firmware with the host's package manager, then verify the two
binaries before creating a golden.

## Bake a golden image

The bake writes a qcow2 disk, unattended-install ISO, serial log, metadata, and
usually a UEFI variables sidecar. It is a host write and can download/install
toolchains inside the guest.

```sh
multirunner bake \
  --iso /srv/isos/WindowsServer.iso \
  --iso-sha256 <64-lowercase-hex> \
  --golden /var/lib/multirunner/windows-server.qcow2 \
  --disk-gb 80 \
  --mem-mb 4096 \
  --cpus 2 \
  --tools dotnet,node:24,buildtools:17
```

Run that exact command with `--dry-run` first. It validates and hashes the ISO
and reports artifact paths, QEMU binaries, VM resources, and VNC endpoints
without creating files, allocating ports, downloading tools, or starting QEMU.
After approval, repeat it without `--dry-run`.

Defaults are 40 GB, 4096 MB, two vCPUs, automatic acceleration, and the
embedded runner version. The bake deadline is fixed at 45 minutes, at least 75
with any .NET/Node/Go tool, plus 90 per Build Tools line; each Build Tools line
is a separate multi-GB installation, so raise `--disk-gb` accordingly. Valid
selectors are `dotnet[:major]`, `node[:major]`, `go`, and `buildtools[:line]`;
bare `node` bakes every manifest major, bare `dotnet` every bakeable channel
the manifest targets at QEMU, and bare `buildtools` the manifest default line.
A non-default `--runner-version` requires `--runner-sha256`. Both `bake` and
`--prepare-only` overwrite an existing golden and delete its serial log first
but leave the old `<golden>.meta.json` in place, so copy the golden and its
sidecars elsewhere before re-baking.

`--prepare-only` is useful for inspecting the generated QEMU invocation, but it
still creates the golden disk and unattended ISO. It starts neither QEMU nor the
VNC/noVNC viewer; any printed VNC or viewer address is only a prospective manual
launch plan. Use `--dry-run` for a preview.

A successful bake ends with `MR:GOLDEN_OK` in
`<golden>.serial.log`. Do not use a golden missing that marker. Preserve these
sidecars with the golden:

```text
windows-server.qcow2
windows-server.qcow2.meta.json
windows-server.qcow2.serial.log
windows-server.qcow2.vars.fd       # when UEFI is found
```

## Configure a QEMU pool

QEMU is a Windows backend by design. It does not need `docker.host`.

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
      accel: ""                     # auto: kvm, whpx, hvf, or tcg
      bake_iso: /srv/isos/WindowsServer.iso
      bake_iso_sha256: <64-lowercase-hex>
      tools: [dotnet, "node:24", "buildtools:17"]
```

| Field | Default / effect |
|---|---|
| `golden` | Required qcow2 path. |
| `work_dir` | Per-job overlays, JIT ISOs, NVRAM copies, and temporary serial logs. Default: the OS temp directory plus `multirunner-vm`. Use a dedicated directory. |
| `mem_mb`, `cpus` | Per-job VM resources; default 4096 MB and 2 CPUs. |
| `accel` | Empty selects the host-compatible accelerator; explicit values are `kvm`, `whpx`, `hvf`, or `tcg`. |
| `bake_iso`, `bake_iso_sha256` | Enables automatic rebuild when housekeeping decides it is needed; the digest also rejects unexpected installation media. |
| `runner_version`, `runner_sha256` | Baked runner version. A non-default version needs its SHA-256 when baking or when configured `bake_iso` makes startup evaluate rebuild inputs; an existing golden without `bake_iso` is not rejected by `doctor` for this alone. |
| `licensed` | Set only for an activated key/KMS guest; disables evaluation-license housekeeping. |
| `tools` | Toolchains expected in the golden. Keep this aligned with the bake command. |

`image` and `image_tier` are ignored by QEMU with a startup warning.
`tool_cache`, Docker socket/DinD, and bind-mounted `git_cache.mode: mirror` are
likewise unavailable to the VM, silently.
Bake tools with `qemu.tools`; for checkout acceleration, use
`git_cache.mode: dotgit-cache` with an enabled, runner-reachable cache and
`github.scope: repo`.

The VM uses QEMU user-mode networking with an e1000 NIC. It has no bridge or
inbound-network YAML setting. multirunner rewrites a host name in the embedded
cache `advertise_url` to `10.0.2.2`, so the cache must be reachable on the QEMU
host. An IP literal such as `127.0.0.1` is not rewritten and resolves to the
guest itself; use a host name.

## Watch a bake with noVNC

Bake viewing is enabled by default:

```sh
multirunner bake --iso /srv/isos/WindowsServer.iso \
  --golden /var/lib/multirunner/windows-server.qcow2 \
  --vnc-web 127.0.0.1:8090
```

Open `http://127.0.0.1:8090` while the bake runs. Set `--vnc-web ""` to disable
the viewer and the default VNC listener. To expose raw VNC without the browser
viewer, also pass `--vnc <host:port>` explicitly.

`--vnc` accepts a raw VNC `host:port`; port `0` selects a free port and is the
default (`127.0.0.1:0`). When `--vnc-web` is enabled, its host governs all three
listeners: HTTP, raw VNC, and QEMU's WebSocket. The raw VNC and WebSocket ports
are allocated dynamically and printed at bake startup, together with the web
URL. The WebSocket address is the noVNC transport, not a page to open directly.

VNC, WebSocket, and the embedded viewer have no TLS, password, or authentication;
the safe defaults bind to loopback. The current bake CLI also has no
Administrator-password override and bakes a fixed guest credential, so treat
console access as sensitive. Keep the host firewall closed and use loopback/SSH
port forwarding or a protected network if remote viewing is necessary. Bake QMP
remains fixed at `127.0.0.1:4455`.

## Watch a running job VM (debug only)

Normal job VMs are headless. Set `MULTIRUNNER_VM_VNC` before starting the
orchestrator to ask QEMU for a VNC/WebSocket listener, then run the hidden
viewer helper in another terminal:

```powershell
$env:MULTIRUNNER_VM_VNC = "127.0.0.1:2,websocket=5702"
multirunner run --config C:\multirunner\config.yaml

# Separate terminal while a VM is running
multirunner _vmview --http 127.0.0.1:8091 --ws-port 5702
```

Open `http://127.0.0.1:8091`. The example uses raw VNC TCP 5902, WebSocket 5702,
and runtime QMP `127.0.0.1:4457`. These are debug-only, fixed process-wide ports:
run a debug pool at `size: 1`; concurrent QEMU VMs will collide. Do not expose
the endpoint beyond a protected loopback/network boundary.

`_screenshot` and `_bootkeys` default to QMP port 4445, which matches neither
mode. Supply the explicit address when using them:

```sh
# During a bake
multirunner _screenshot --qmp 127.0.0.1:4455 --out bake.png

# During a debug job VM
multirunner _screenshot --qmp 127.0.0.1:4457 --out job.png
```

During a bake the QMP socket accepts one client and the bake itself holds it
until the install media is ejected (up to 20 minutes), so an early
`_screenshot` may stall against its 15-second deadline. These hidden helpers
and `MULTIRUNNER_VM_VNC` (which also applies to housekeeping rearm boots) are
diagnostic interfaces, not a multi-VM monitoring service. Never show a JIT ISO,
a `<golden>.autounattend.iso`, or their contents.

## Monitor and maintain safely

- A job's serial log is `<work_dir>/<runner>.qcow2.serial.log`. It remains
  after a clean exit but is normally empty: only the bake guest writes to COM1.
  The runtime guest logs to `C:\mr-startup.log` inside the overlay, which is
  deleted when QEMU exits, so guest-side evidence needs a single-slot debug
  pool with `MULTIRUNNER_VM_VNC`. A later real startup removes the serial log
  during cleanup; treat it as potentially sensitive.
- `multirunner doctor` locates the QEMU binaries and stats the golden file. It
  does not check `MR:GOLDEN_OK`, `<golden>.meta.json`, guest boot, firmware,
  acceleration, free disk, or VNC. A failed first bake leaves a qcow2 without
  metadata that housekeeping skips at debug level; a failed re-bake leaves a
  broken qcow2 beside the previous metadata, which housekeeping treats as
  baked. `doctor` reports both as ok and the pool boots them. `doctor` is
  read-only and does not clean `work_dir`.
- The selected accelerator is reported read-only only by `bake --dry-run`; a
  real startup warns when it falls back to TCG, but WHPX and HVF are never
  probed, so a host without virtualization fails at QEMU launch instead.
- A non-dry-run `multirunner run`, or an installed-service `start`/`restart`,
  runs golden housekeeping first, then removes top-level `*.qcow2`, `*.iso`,
  `*.vars.fd`, and `*.serial.log` files from `work_dir` (errors ignored),
  then runs the reachability preflight. Do not start it while a job VM or
  manual inspection may use that directory.
- Housekeeping checks each golden once at startup. An evaluation guest with
  fewer than 14 days left is rearmed if possible; changed tools or exhausted
  rearms trigger a rebuild only when `bake_iso` is configured. A rearm boots
  the golden writable with no overlay, overwrites `<golden>.serial.log`, and
  is recorded as applied whenever QEMU exits cleanly; its `slmgr /rearm`
  result is not verified. Drain VMs before starting a service that may do
  this. Automatic rebuilds are headless, and the check is not periodic.
- Windows licensing remains the operator's responsibility. `licensed: true`
  only tells multirunner to skip evaluation handling; it does not activate
  Windows.

## Fast failure guide

| Symptom | Check first |
|---|---|
| `qemu-system-x86_64` or `qemu-img` missing | PATH and host package installation. |
| Slow/hung guest | The startup TCG warning, `bake --dry-run` `accel=`, host virtualization, free RAM/disk, then a single-slot debug VM console (the job serial log is empty). |
| Bake exits without a usable golden | Tail `<golden>.serial.log`; require `MR:GOLDEN_OK` from this bake. A timeout error carries the serial tail. |
| Golden boots but jobs fail | Check `<golden>.serial.log` for `MR:GOLDEN_OK`; a failed or interrupted bake still passes `doctor`, and a stale `.meta.json` survives a failed re-bake. |
| noVNC page cannot connect | Printed viewer/WebSocket addresses, firewall, and only one active VM. |
| Cache unavailable in a VM | Cache listener, a host name (not an IP literal) in `advertise_url`, and reachability at `10.0.2.2`; use `dotgit-cache` rather than mount-based cache features. |
