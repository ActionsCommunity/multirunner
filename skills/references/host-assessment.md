# Host assessment

Detect Windows, Linux, or macOS and `amd64` or `arm64`, then inspect without elevation:

- Installed multirunner version, binary path, config path, service state, and free disk.
- Docker or Podman reachability, server OS, endpoint, and required images.
- On Windows, containerd, nerdctl, Containers, Hyper-V, and pending reboot state.
- On Linux, systemd and Docker or Podman service state.
- On macOS, launchd and the Docker-compatible API; read [macOS host notes](macos-host.md).
- QEMU executable, acceleration, golden image, and disk only for requested Windows VMs.
- Config target scope, pools, backends, labels, capacity, provisioning, and metrics.

Run `multirunner doctor --config <config-path>` when a config exists. It is a
read-only preflight, including for QEMU pools, and does not clean
`qemu.work_dir`; see [triage signals](triage-signals.md) for its markers and
blind spots. Run cleanup or remediation only through a separate, explicit
command after reviewing its targets and obtaining approval. Use
`multirunner detect --path <checkout> --os <linux-or-windows>` for
local workload detection. Report all blockers together.

Use `multirunner service start --dry-run` (read-only; prints current service
state and the resolved config path without starting anything), or
`Get-Service multirunner` on Windows,
`systemctl status multirunner --no-pager` on Linux, or
`launchctl print system/multirunner` on macOS. A running service can still
hold a failed orchestrator; check the log for `multirunner: <error>`. When
metrics are enabled, request only `/health` and the three `multirunner_*`
series named in triage signals. `/health` always answers 200; it is not cache,
startup, or runner readiness. Use GitHub read commands for
recent runs. Collect no more than 100 routine or 200 diagnostic service log lines.
