# Windows host

Use this reference after the shared host assessment.

## Supported choices

| Workload | Backend | When to select it |
|---|---|---|
| Windows containers | `containerd` | The README's documented native path without Docker Desktop. It uses containerd, runhcs, nerdctl, and CNI; isolation is chosen at launch by `containerd.isolation`, not by the installer. |
| Windows containers | `docker` or omitted | Use an existing Windows Docker daemon. A local named pipe may use `isolation: auto`; remote daemons require `process` or `hyperv`. |
| Linux containers | `docker` or omitted | Point `docker.host` at a reachable Docker-compatible Linux daemon, such as a WSL2 engine. Doctor must report daemon OS `linux`. |
| Windows virtual machines | `qemu` | Use appropriately licensed Windows Server media, including supported evaluation media, and an x86-64 QEMU guest when containers don't meet workload requirements. |
| Windows host process | `host` | Use only for trusted, narrowly allowlisted jobs that must access the current user's local drives, shells, credentials, or signed-in tools. There is no container or VM isolation. |

Windows Server normally supports process isolation. Windows client editions need
Hyper-V isolation. `auto` detects this only for a local Windows daemon.

## Read-only assessment

Check the Windows edition, architecture, free disk and memory, virtualization
support, service state, and the configured runtime endpoint. Locate `docker`,
`nerdctl.exe`, or `qemu-system-x86_64` without installing anything. Run
`multirunner doctor --config <path>` only after reviewing the redacted config.
For a host pool, also inspect the service/current-user identity, workflow
authorization tuple, resolved runner/state directories, free disk, and whether
another pool uses either directory case-insensitively. `doctor` does not create
the directories, download the runner, or recover an interrupted process.

Run `multirunner install-containerd --dry-run` or
`multirunner install-windows-daemon --dry-run --data-root <path>` first. Review
the reported feature, service, download, data-root, and reboot plan. Windows may
hide optional-feature state from a non-elevated dry run; preserve that uncertainty
rather than guessing. Run the same command without `--dry-run` only after separate
approval for elevation and the reported changes. Reassess after reboot.

Disclose these installer facts before approval: neither installer verifies a
download checksum (the daemon installer only compares the downloaded
`dockerd` version string); `install-containerd` enables both Containers and
Hyper-V unconditionally, and the daemon installer adds Hyper-V on client
editions; elevation runs PowerShell with `-ExecutionPolicy Bypass
-EncodedCommand`, which endpoint protection may flag; and `run --install-deps`
always uses the default data root, so pass `--data-root` through the explicit
installer when an existing image store must be kept.

The Windows daemon installer may create the `docker-users` group and add the
invoking user; require a sign-out/in before treating that membership as active.

`install-containerd` is not a harmless probe or additive package install: it
overwrites the generic `containerd` configuration, changes machine PATH, and
stops/re-registers that service. It skips any binary that already exists, so
re-running it never upgrades containerd, nerdctl, or CNI; upgrade those
deliberately. Do not run it over an existing shared containerd deployment
without an approved backup/migration plan. The standalone Windows
daemon installer is separate, but still reconfigures its own Multirunner daemon
service.

For `backend: containerd`, set `containerd.address`, `containerd.nerdctl`,
`containerd.namespace`, and `containerd.isolation` as needed. Current config
validation also requires a nonempty `docker.host` even though the containerd
backend ignores it; retain a clearly labelled placeholder until that constraint
is removed.

For `backend: host`, use `os: windows` and `size: 1`, remove image, Docker,
container, and active tool-cache settings, and bind the pool to an exact
repository/workflow/event/actor/branch tuple under `provisioning: autoscale`.
The explicit `host.allow_unrestricted` escape hatch removes that routing gate
without adding isolation. The default Windows service identity is LocalSystem;
host startup refuses it unless `host.allow_local_system: true`, so configure
the service to run as the intended user instead. A real start may download the
verified runner and recover a recorded interrupted process; preview authority
with `run --dry-run` first.

For health and troubleshooting, compare the pool `os`, backend, endpoint,
isolation, host edition, and image OS. Don't switch container modes or services
as a diagnostic shortcut.
