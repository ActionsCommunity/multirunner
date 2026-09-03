---
name: multirunner-qemu
description: Safely bake, configure, monitor, and debug Multirunner Windows QEMU runner pools. Use for QEMU goldens, Windows VM runners, noVNC, VNC, and QEMU VM failures; not Windows container pools.
---

# Operate Multirunner QEMU Windows pools

Use this only for Windows VM runners, goldens, bakes, QEMU startup, VNC/noVNC,
or VM failures, not Windows containers. Read the relevant section of the
[QEMU Windows reference](../docs/qemu-windows.md) before choosing a command:
host requirements/bake/configure for a golden change, the noVNC/VNC sections
for a viewer, and "Monitor and maintain safely" plus "Fast failure guide" for
an incident. Read the `bake` section of the
[CLI reference](../docs/cli-reference.md) for the authoritative flag table,
[host configuration](../docs/host-configuration.md) for surrounding pool
fields, the QEMU section of [triage signals](../references/triage-signals.md)
for log lines, [caching](../references/caching.md) only for `dotgit-cache`,
and [safety and approvals](../references/safety-and-approvals.md) before a
mutation.

## Non-negotiable boundaries

- `doctor` is read-only and does not clean `qemu.work_dir`. A non-dry-run
  `run`, or installed-service `start`/`restart`, first runs golden
  housekeeping (a headless rearm or rebuild is possible), then removes
  top-level `*.qcow2`, `*.iso`, `*.vars.fd`, and `*.serial.log` files from
  `work_dir` before pool preflight. Use a dedicated, idle, disposable work
  directory per pool (the default is shared); never use real startup as a
  probe.
- Treat the golden qcow2, `<golden>.meta.json`, `<golden>.serial.log`, and
  `<golden>.vars.fd` as one rollback asset and copy them elsewhere before any
  bake: `bake` and `--prepare-only` overwrite the qcow2 and delete the serial
  log as their first step but never remove `<golden>.meta.json`. A failed
  first bake leaves a qcow2 without metadata; a failed re-bake leaves a broken
  qcow2 beside the previous, still-valid metadata. Either passes `doctor` and
  boots. A rearm boots the golden
  writable with no overlay and overwrites its serial log, and its success is
  not verified. Golden rearm/rebuild is a separate approved write.
- `bake --dry-run` validates its plan (and hashes the ISO). `--prepare-only`
  writes artifacts, including `<golden>.autounattend.iso` containing the
  plaintext guest Administrator password, and starts neither QEMU nor
  VNC/noVNC; delete that ISO when finished. Viewer endpoints exist only while
  the bake's QEMU process runs; they are not persistent and ports must not be
  assumed. `--vnc-web` cannot use port 0, `--vnc` needs port 0 or at least
  5900, and the two ports must differ.
- VNC/noVNC has no authentication or TLS, and the bake CLI uses the same
  fixed guest Administrator password for every golden with no flag to change
  it; it persists into every job VM. Keep console access private and handle
  screens, serial logs, and endpoints as sensitive.
- A service stop/restart can interrupt VM jobs and waits at most 20 seconds.
  Drain capacity first; use runtime VNC/QMP (`MULTIRUNNER_VM_VNC`, fixed
  ports) only for an approved single-slot debug pool. During a bake the QMP
  socket is held by the bake itself, so a concurrent `_screenshot` may stall.

## Workflow

1. Record the redacted pool, golden sidecar set, work directory, accelerator,
   disk/RAM capacity, tools/runner selectors, cache route, and active VM state.
   `doctor` only locates the QEMU binaries and stats the golden; it reports no
   disk, RAM, accelerator, or `MR:GOLDEN_OK` state. No read-only command
   reports the accelerator except `bake --dry-run`; a TCG fallback is warned
   only at real startup, and WHPX/HVF are never probed.
2. For a bake or pool change, present the exact ISO/checksum, expanded tool
   selectors, disk and startup effects, rollback asset, and VNC exposure. Bare
   `node` bakes every manifest major, bare `dotnet` every bakeable channel the
   manifest targets at QEMU, and bare `buildtools` the manifest default line;
   the fixed bake timeout is 45 minutes, at
   least 75 with any .NET/Node/Go, plus 90 per Build Tools line, with no
   override and QEMU killed on expiry. Run the exact dry-run, then obtain
   approval before bake, config write, rearm/rebuild, listener exposure, or
   service action.
3. For a completed bake, require `MR:GOLDEN_OK` in `<golden>.serial.log`
   written by this bake; `<golden>.meta.json` alone proves nothing after a
   re-bake because the old file survives. Record the endpoints the bake printed
   only as evidence of where it listened, since they are gone once it exits.
   For `dotgit-cache`, verify the documented embedded-cache and repo-scope
   requirements, and use a host name (not an IP literal) in `advertise_url`
   because only host names are rewritten to `10.0.2.2` for the guest.
4. After an approved startup/change, verify only the changed boundary and an
   approved canary. A job VM's serial log is normally empty (only the bake
   guest writes to COM1) and the in-guest log dies with the overlay, so use a
   single-slot debug pool for guest-side evidence. Report golden inputs,
   acceleration, capacity, cache route, and any untested safety boundary.
