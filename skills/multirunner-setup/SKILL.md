---
name: multirunner-setup
description: Set up a new Multirunner host end to end - verified binary, container runtime or QEMU golden, config, GitHub credentials, doctor, service, and canary. Use for first-time installation; use multirunner-host for changes to an existing host.
---

# Set up Multirunner

Use this for a host that does not yet run Multirunner. Follow the steps in
order; each names the reference to read and the approval it needs. Route
changes to an already-configured host to
[multirunner-host](../multirunner-host/SKILL.md), GitHub-side work beyond the
first credential to [multirunner-github](../multirunner-github/SKILL.md), and
any Windows VM golden bake to [multirunner-qemu](../multirunner-qemu/SKILL.md).
Apply [safety and approvals](../references/safety-and-approvals.md)
throughout: no source build or Go toolchain is needed, and no step writes,
installs, elevates, or registers anything without a separate approval.

## Steps

1. **Capture intent.** Target scope (`repo`, `repos`, `org`, or `enterprise`)
   and owner, host OS and architecture, workloads (Linux containers, Windows
   containers, Windows VMs), capacity per pool, required tools, provisioning
   mode (`pool`, `autoscale`, or `scaleset`), service or foreground, and
   whether an Actions or git cache is wanted. Do not infer any of these.
2. **Assess the host read-only.** Follow
   [host assessment](../references/host-assessment.md) and the OS note
   ([Linux](../references/linux-host.md),
   [Windows](../references/windows-host.md),
   [macOS](../references/macos-host.md)). Report every blocker together
   before proposing anything.
3. **Install the binary.** Follow
   [verified release](../references/verified-release.md): download the
   matching asset and `SHA256SUMS.txt`, verify the checksum (the binary's
   `--version` prints a fixed development string and proves nothing), then
   show the destination and required elevation before placing it on `PATH`.
4. **Provide the runtime.** Pick the backend from
   [runtimes and toolsets](../references/runtimes-and-toolsets.md):
   - Linux containers: a reachable Docker or Podman API. Multirunner has no
     Linux installer; show the exact package action and ask before elevation.
   - Windows containers: run `multirunner install-containerd --dry-run` (or
     `install-windows-daemon --dry-run --data-root <path>`), report the
     planned features, downloads, service, and reboot outcome, then ask before
     the real command. Reassess after any reboot.
   - Windows VMs: hand the golden bake to multirunner-qemu and return with the
     golden path and tool selectors.
5. **Write the config.** Start from the matching file under "Minimal complete
   configurations" in [host configuration](../docs/host-configuration.md)
   (one per provisioning mode and backend, each valid under current
   validation) and look up every other key in its "Complete key index". Use
   `multirunner detect --path <checkout> --os <linux-or-windows>` to
   recommend a container tier. Every non-QEMU pool needs a nonempty
   `docker.host`, including `backend: containerd`; a `scaleset` pool's
   `scale_set` defaults to the pool name and only has to be set to adopt an
   existing scale set. Reference secrets as `${VAR}` and supply them through
   the service environment or a `.env` file next to the config; never inline
   values. Show the redacted file and ask before writing it. Back up any
   existing file first.
6. **Add credentials.** Follow
   [authentication](../references/authentication.md): preview
   `multirunner connect --repo <owner/repo> --own-app --config <path> --dry-run
   --non-interactive` or the `--org` form, review the manifest it prints, then
   run it after approval. Off a terminal, connect requires the model to be
   stated: `--own-app` creates a dedicated App (and is the only option for a
   repository target), `--device` prints a device code and must not be used
   where the output is logged. Write the config (step 5) first so connect can derive
   the permissions from the chosen provisioning mode. It creates and installs a
   GitHub App, writes the PEM, and rewrites the config at mode 0600 (check the
   service account can still read it). Leave the webhook alone here: without
   `--webhook-url` the App registers an inactive placeholder hook and no
   events, which step 9 completes. Use a PAT only for a scope `connect` cannot
   create (including any `enterprise` target, which GitHub Apps cannot serve at
   all), a private git cache, or an approved existing deployment.
7. **Preflight.** Run `multirunner doctor --config <path>` and clear every
   failed boundary. Then run `multirunner run --config <path> --dry-run` and
   review the printed plan and warnings. Neither proves webhook ingress, cache
   reachability, guest boot, or workflow routing; see
   [triage signals](../references/triage-signals.md) for what `doctor`
   does and does not check.
8. **Enable optional caches** only after the core path works, following
   [caching](../references/caching.md). Use a host name, not an IP
   literal, in `advertise_url`; bind listeners privately.
9. **Wire autoscale or scale sets.** For `autoscale`, the webhook receiver,
   secret, and public TLS boundary are a separate approved change through
   multirunner-github. If connect ran without `--webhook-url`, the App still
   needs its real hook URL and the `workflow_job` event before it can deliver. For
   `scaleset`, the first start creates or updates the GitHub scale set.
10. **Install the service.** Run `multirunner service install --config <abs
    path> --dry-run`, then `service install`, then `service start --dry-run`
    and `service start`, each after approval. The service records the absolute
    config path and runs from the config directory, so its `.env` and PEM must
    be readable there. Confirm state with `service start --dry-run` or the OS
    service tool, and look for `multirunner: <error>` in the service log,
    because a running service can hold a failed orchestrator.
11. **Prove it end to end.** Follow
    [canary verification](../references/canary-verification.md) with an
    approved trusted workflow. Report queue-to-start latency, the run URL,
    and that the ephemeral runner exited and capacity recovered.

## Success criteria

Checksum verified, `doctor` clean, `run --dry-run` plan reviewed, service
running with the orchestrator alive, canary completed. Only approved config
fields were written, and no secret, private key, JIT configuration, or cache
token appeared in chat or logs. List every boundary the canary did not cover.
