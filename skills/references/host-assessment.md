# Host assessment

Detect Windows or Linux and `amd64` or `arm64`, then inspect without elevation:

- Installed multirunner version, binary path, config path, service state, and free disk.
- Docker or Podman reachability, server OS, endpoint, and required images.
- On Windows, containerd, nerdctl, Containers, Hyper-V, and pending reboot state.
- On Linux, systemd and Docker or Podman service state.
- QEMU executable, acceleration, golden image, and disk only for requested Windows VMs.
- Config target scope, pools, backends, labels, capacity, provisioning, and metrics.

Run `multirunner doctor --config <config-path>` when a config exists. Use
`multirunner detect --path <checkout> --os <linux-or-windows>` for local workload
detection. Report all blockers together.

Use `Get-Service multirunner` on Windows or
`systemctl status multirunner --no-pager` on Linux. When metrics are enabled, request
only `/health` and relevant bounded Prometheus series. Use GitHub read commands for
recent runs. Collect no more than 100 routine or 200 diagnostic service log lines.
