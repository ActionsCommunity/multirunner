# macOS host notes

Read this only when the Multirunner host is macOS.

- The native service manager is launchd. The installed service must be able to
  read the chosen config, its adjacent or working-directory .env file, and the
  App private-key path. Check those paths and ownership without reading secret
  values before installing or restarting it.
- Linux runner pools need an existing Docker-compatible API. A macOS host is
  not a Windows containerd or Windows Docker-container host; use a Windows host
  for those pools.
- Windows QEMU runners use an x86-64 guest. Intel macOS selects HVF when the
  host and virtualization support allow it. Apple Silicon and other ARM hosts
  use TCG x86 emulation, which is supported but substantially slower,
  particularly for a golden bake. Do not promise Intel-like capacity or bake
  time on an ARM Mac.
- A read-only doctor check validates configured boundaries, not a complete
  guest boot, cache route, workflow compatibility, or service-account
  environment. Use an approved trusted canary for end-to-end proof.

Keep service controls, cache and metrics listeners, VNC, and credentials on
private operator-controlled boundaries. A foreground shell's environment is not
necessarily the launchd service environment.
