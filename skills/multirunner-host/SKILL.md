---
name: multirunner-host
description: Tune or upgrade an existing Multirunner host and its container pools. Use for approved host configuration and lifecycle changes; use multirunner-setup for a new host and multirunner-diagnose for read-only investigation.
---

# Operate a Multirunner host

Use this for pool/runtime/capacity/cache/observability changes or a binary or
configured-image update on a host that already runs Multirunner. Route a
first-time installation to [multirunner-setup](../multirunner-setup/SKILL.md).
Route GitHub scopes, App credentials,
scale sets, and `webhook`/autoscale fields to
[multirunner-github](../multirunner-github/SKILL.md). Route any Windows VM
golden, bake, or QEMU-pool lifecycle work to
[multirunner-qemu](../multirunner-qemu/SKILL.md).

## Load only the relevant guidance

Start with [host assessment](../references/host-assessment.md), the matching
OS note ([Linux](../references/linux-host.md),
[Windows](../references/windows-host.md), or
[macOS](../references/macos-host.md)), and the intended command in the
[CLI reference](../docs/cli-reference.md). Then choose the matching source
of truth:

| Need | Read |
|---|---|
| Pool, backend, image, capacity, metrics, logs, or provisioning fields | The relevant section of [host configuration](../docs/host-configuration.md) |
| Any key's type, default, or validation rule | "Complete key index" in [host configuration](../docs/host-configuration.md) |
| A whole valid starting file per mode or backend | "Minimal complete configurations" in [host configuration](../docs/host-configuration.md) |
| Per-pool `size`, `runner_group_id`, `max_consecutive_failures`, or `tool_cache` (inert unless `mode: shared-volume` and `volume` are both set) | "Common pool fields" and "Tool cache and Docker socket" in [host configuration](../docs/host-configuration.md) |
| Container tier or host runtime choice | [Runtimes and toolsets](../references/runtimes-and-toolsets.md) |
| Actions or Git cache | [Caching](../references/caching.md) |
| Binary release or configured-image update | [Verified release](../references/verified-release.md) |
| Reading service, `doctor`, metric, or log output | [Triage signals](../references/triage-signals.md) |
| A proposed write or external mutation | [Safety and approvals](../references/safety-and-approvals.md) |
| Approved end-to-end validation | [Canary verification](../references/canary-verification.md) |

## Operating rules

- Establish a redacted baseline: config, service identity/state, runtime
  reachability, capacity, and current errors. Run `doctor` only when a valid
  config exists. It is read-only preflight, not proof of a cache route,
  webhook ingress, guest boot, or workflow success.
- Use `run --dry-run` and `service <action> --dry-run` only to plan startup
  effects. Neither is a health check. `run --dry-run` reports a cache plan
  whenever `cache.enabled` is true and `external_url` is empty, but startup
  also requires `mode` not `off`. With `qemu.bake_iso` set, a dry run hashes
  the whole ISO and can take minutes. Do not use non-dry-run `run`,
  installers, image pulls, or service actions as probes.
- Know the real startup order before any service action: golden housekeeping
  for every QEMU pool (possible headless rearm or rebuild), then pool by pool
  in YAML order, cleanup of top-level `*.qcow2`, `*.iso`, `*.vars.fd`, and
  `*.serial.log` files in that pool's `qemu.work_dir` followed by its
  reachability preflight. Pool preparation
  is all-or-nothing: one unreachable backend keeps every pool down. The
  default `work_dir` (OS temp plus `multirunner-vm`) is shared by every QEMU
  pool that omits it, so set one per pool.
- A service stop/restart cancels the orchestrator and waits at most 20 seconds,
  so drain capacity first.
- The Windows installers download binaries without checksum verification,
  enable Hyper-V as well as Containers where applicable, and
  `install-containerd` never upgrades binaries that already exist while still
  rewriting containerd config, PATH, and the service. `run --install-deps`
  always uses the default data root. Present those effects before approval.
- Keep secrets in their configured environment/file references. Never expose
  values, private keys, JIT configuration, or cache path tokens.

## Change lifecycle

1. State the requested outcome and smallest redacted diff. Include capacity
   arithmetic, runtime/image or release effect, network exposure, GitHub-side
   effect, startup/service action, rollback asset, and canary boundary. Call
   out silent no-ops: `git_cache.mode: mirror` never reaches a QEMU pool and
   nothing warns, and App-only auth against a private repository only logs a
   mirror warning.
2. Obtain approval immediately before each write or external mutation. Keep a
   rollback reference; do not prune images, caches, mirrors, or prior assets as
   part of the change.
3. Apply only the approved change. Re-run `doctor` and the relevant dry-run
   plan before a service action. Stop and present rollback options if either
   fails.
4. After the approved service action, verify the changed boundary and run only
   an approved trusted canary. Report untested boundaries explicitly; a
   `success` job counter only means the backend wait returned without error.
