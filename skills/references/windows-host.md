# Windows host

Use this reference after the shared host assessment.

## Supported choices

| Workload | Backend | When to select it |
|---|---|---|
| Windows containers | `containerd` | Preferred Windows-container path on current Windows builds. It uses containerd, runhcs, nerdctl, and CNI. |
| Windows containers | `docker` or omitted | Use an existing Windows Docker daemon. A local named pipe may use `isolation: auto`; remote daemons require `process` or `hyperv`. |
| Linux containers | `docker` or omitted | Point `docker.host` at a reachable Docker-compatible Linux daemon, such as a WSL2 engine. Doctor must report daemon OS `linux`. |
| Windows virtual machines | `qemu` | Use a licensed Windows Server ISO and an x86-64 QEMU guest when containers don't meet workload requirements. |

Windows Server normally supports process isolation. Windows client editions need
Hyper-V isolation. `auto` detects this only for a local Windows daemon.

## Read-only assessment

Check the Windows edition, architecture, free disk and memory, virtualization
support, service state, and the configured runtime endpoint. Locate `docker`,
`nerdctl.exe`, or `qemu-system-x86_64` without installing anything. Run
`multirunner doctor --config <path>` only after reviewing the redacted config.

Use `multirunner install-containerd` or
`multirunner install-windows-daemon --data-root <path>` only after separate
approval for elevation, downloads, Windows features, services, and a possible
reboot. Reassess after reboot before continuing.

For health and troubleshooting, compare the pool `os`, backend, endpoint,
isolation, host edition, and image OS. Don't switch container modes or services
as a diagnostic shortcut.
