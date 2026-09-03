# Triage signals

Exact strings, endpoints, and semantics an operator can grep or query while
diagnosing. All come from the current source; redact values before sharing.

## Process and service

- A running service is not proof of a running orchestrator. The service wrapper
  keeps running after an orchestrator startup error; that error is printed once
  to stderr as `multirunner: <error>`. Check the service log for that prefix
  first.
- `multirunner service <install|uninstall|start|stop|restart> --dry-run` is
  read-only (there is no `status` action). It prints the platform, service
  name, current state (`running`, `stopped`, or `unknown/not installed`), the
  resolved config path, and the command it would run.
- Service stop and restart cancel the orchestrator and wait at most 20 seconds.
  In-flight jobs beyond that window are interrupted.
- Pool preparation is all-or-nothing: one unreachable backend aborts startup for
  every pool; the log ends with that pool's error.
- `run --dry-run` and startup print config warnings such as
  `github.repo is ignored when scope=repos` and
  `pool "<name>": backend=qemu ignores image/image_tier`.

## `doctor`

- Per-pool markers: `backend error:`, `UNREACHABLE:`, `UNKNOWN MODE:`,
  `WRONG MODE: daemon=<os> pool=<os>`, `os=<x> UNKNOWN`; success ends with
  `all pools ready`; failure exits non-zero with `preflight found problems`.
- All pool pings share one 20-second budget. One hung daemon can make later
  pools report `UNREACHABLE` spuriously; re-run with the hung pool isolated.
- For `repo`/`repos` scope, Actions disabled is a hard failure; no `self-hosted`
  workflow found is a note only, failing only when the scan was incomplete.
- For `org`/`enterprise` scope, `doctor` performs no GitHub call at all: bad
  credentials pass.
- The git-configuration audit runs only when `git_cache` is enabled and is
  advisory; it never fails preflight.
- For a QEMU pool it only locates `qemu-system-x86_64`/`qemu-img` and stats the
  golden file. It does not read `MR:GOLDEN_OK`, `<golden>.meta.json`, the NVRAM
  sidecar, work-directory writability, free disk, or the accelerator.
- With `qemu.bake_iso` configured, `run --dry-run` and real startup hash the
  whole ISO (`doctor` does not); a multi-GB read can take minutes and a digest
  mismatch fails the command.

## Metrics and health

- Served only when `metrics.listen` is set: `GET /metrics` and `GET /health`,
  unauthenticated.
- `/health` always returns `200 ok` once listening. The listener starts after
  GitHub client setup, golden housekeeping, and cache startup but before pool
  preparation, so under the service wrapper a pool-preparation failure leaves
  `/health` serving `200` with no orchestrator. Health `200` plus zero runners
  in `provisioning: pool` is a failed-startup signature, not idle capacity; no
  listener at all points at an earlier startup failure.
- Series: `multirunner_runners_active{pool}` (gauge),
  `multirunner_jobs_total{pool,result}` with `result` `success` or `error`, and
  `multirunner_reprovision_errors_total{pool}`. `success` means the backend wait
  returned without error; a non-zero container exit still counts as success.
- The embedded Actions cache has its own unauthenticated `GET /health` on
  `cache.listen`.

## Autoscale and webhook

- Startup: `webhook listening addr=...` proves bind, not ingress.
  `webhook listener has no secret; signatures will not be verified` means every
  unsigned body is accepted.
- HTTP responses: `401 invalid signature` (log `rejected webhook with bad
  signature`), `400 bad payload`, `200` for `ping` and for non-`queued`
  `workflow_job` actions, `204` for any other event type. Bodies over 1 MiB are
  truncated: with a secret they fail the signature check (`401`), without one
  they fail JSON parsing (`400`).
- Only `action: queued` launches; the log line is `workflow_job queued` with
  repository and labels.
- Delivered but nothing launched: `WARN ignoring queued job for unmanaged
  repository` applies only to `repo`/`repos` scope. Label mismatch or every
  matching pool full logs `queued job: no matching pool with spare capacity` at
  DEBUG level only; set `log.level: debug` to see it. Success is
  `INFO scaling up pool=<name> target=<n>`.
- Under `org`/`enterprise` scope there is no repository filter: any delivery
  that passes the signature check can launch a runner, so the secret is the
  only gate.
- Label matching (autoscale only): the pool must carry every job label,
  comparison is case-insensitive, and a job with no labels matches every pool.
  Pools are tried in YAML order; the first with a free slot wins. In `pool` and
  `scaleset` modes GitHub assigns jobs; multirunner does no matching.
- Polling: `WARN poll queued jobs failed` with
  `queued-job polling failed for N of M repos: ...` distinguishes one repo's
  permission problem from a total outage. Polling is a no-op for
  `org`/`enterprise`; the startup warning about that fires only when
  `webhook.listen` is empty. `poll_interval_sec: -1` with no `webhook.listen`
  leaves autoscale silently inert.

## Pool mode and runners

- `ERROR slot failed; backing off` carries `index`, `consecutive_failures`, and
  `delay`; `ERROR slot hit max consecutive failures; still retrying` means the
  slot never gives up, so a permanently broken slot is a quiet loop.
  `ERROR pool stopped` cancels the whole manager.
- Runner lifecycle: `runner launched`, `runner exited exit_code=<n>`,
  `deregister runner failed`, and `runner still busy after failed kill; keeping
  registration`, which explains orphaned GitHub registrations.

## Cache and git cache

- Startup: `cache server listening addr=... advertise=<redacted>`,
  `cache redirect enabled`, and `WARN cache enabled but advertise_url is empty;
  runners will not be redirected`.
- GC: `cache gc evicted entries count=<n>` or `cache gc failed`; the first tick
  is one `gc_interval_sec` after start.
- `WARN initial git mirror failed; continuing`: startup does not fail on a
  mirror error, including App-only auth against a private repository.
  `git mirror refresh failed` repeats every 5 minutes; the mirror sweep runs
  every 6 hours. `WARN git cache configured but scope is not repo` means no
  mirror is wired at all.

## QEMU

- `WARN QEMU Windows guest is using software CPU emulation; <reason>` appears
  only at a real startup and only when the accelerator resolves to `tcg`. WHPX
  and HVF are never probed, so a Windows or macOS host without virtualization
  fails at QEMU launch instead of warning.
- Housekeeping lines before pool preparation: `rearming golden eval license`,
  `rebuilding golden`, and `golden needs rebuild but no bake ISO configured;
  run: multirunner bake`. A golden without `<golden>.meta.json` is skipped at
  DEBUG level as `golden not baked yet` and still boots. A failed re-bake keeps
  the previous `.meta.json`, so housekeeping treats the broken qcow2 as baked;
  only `MR:GOLDEN_OK` in `<golden>.serial.log` proves the current disk.
- A job VM's `<work_dir>/<runner>.qcow2.serial.log` is normally empty: only the
  bake guest writes to COM1. The runtime guest logs to `C:\mr-startup.log`
  inside the overlay, which is deleted when QEMU exits.
