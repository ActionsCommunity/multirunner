---
name: multirunner-diagnose
description: Monitor or diagnose a configured Multirunner host from bounded read-only evidence. Use for health, capacity, runtime, cache, routing, service, or runner failures; do not apply changes.
---

# Diagnose Multirunner

Use this for routine monitoring or an incident. It gathers evidence and
recommends the smallest next change; it does not repair state. For an approved
repair, hand off to [multirunner-host](../multirunner-host/SKILL.md),
[multirunner-github](../multirunner-github/SKILL.md), or
[multirunner-qemu](../multirunner-qemu/SKILL.md).

## Choose the evidence path

Read [host assessment](../references/host-assessment.md) and
[triage signals](../references/triage-signals.md) (exact log lines, HTTP
statuses, metric names, and `doctor` markers), then the matching
backend/configuration section in [host configuration](../docs/host-configuration.md)
and command behavior in the [CLI reference](../docs/cli-reference.md).

- For cache symptoms, read [caching](../references/caching.md).
- For a target, label, scale-set, or webhook-delivery symptom, use the
  GitHub integration skill and its references.
- For a QEMU pool, read "Monitor and maintain safely" and "Fast failure guide"
  in [QEMU Windows](../docs/qemu-windows.md).
- Read [safety and approvals](../references/safety-and-approvals.md) only
  when proposing a mutation.

## Read-only workflow

1. Capture the symptom, target, provisioning mode, pool, timestamps, redacted
   config, service state, and last known-good state. Use
   `multirunner service <action> --dry-run` for service state; it is read-only.
   A running service does not prove a running orchestrator: look for
   `multirunner: <error>` in the service log first.
2. Run `multirunner doctor --config <path>` and record each failed boundary.
   It is read-only, but cannot prove webhook ingress, cache reachability,
   guest readiness, or org/enterprise workflow routing. For `org`/`enterprise`
   scope it makes no GitHub call, so credentials are unproven. Its pool pings
   share one 20-second budget, so a hung pool can make later pools look
   unreachable.
3. Trace the first failing boundary rather than guessing: target/authentication;
   labels and capacity; service; backend/runtime; image or storage; cache
   route; then runner provisioning. Pool selection depends on the mode: in
   `autoscale` the first YAML-ordered pool carrying every job label
   (case-insensitive) with a free slot wins; in `pool` and `scaleset` modes
   GitHub assigns jobs and multirunner does no matching. Separate webhook
   reachability, signature, delivery, and capacity using the HTTP statuses and
   log lines in triage signals; a label mismatch is logged only at
   `log.level: debug`. Scale-set state has no multirunner read command; use
   read-only GitHub API or `gh` views.
4. Query `/health` and the named `multirunner_*` series only when
   `metrics.listen` is configured. `/health` always answers `200` once
   listening, including after a pool-preparation failure under the service,
   and is not cache, guest, or runner readiness. Zero active
   runners may be normal for autoscale or scale-set mode but is a failure
   signature in `pool` mode. Assess an enabled cache by its own `/health` on
   `cache.listen`.
5. Do not start non-dry-run `run`, install dependencies, pull images, restart
   a service, redeliver a webhook, or dispatch a workflow to diagnose. For a
   QEMU pool, never use real startup as a probe: it first runs golden
   housekeeping (which can rearm or rebuild the golden headlessly), then cleans
   top-level `*.qcow2`, `*.iso`, `*.vars.fd`, and `*.serial.log` files in
   `qemu.work_dir` before pool preflight.

## Report

Mark each pool healthy, degraded, or blocked and distinguish evidence from
unassessed boundaries. State the verified cause or missing evidence, then give
the smallest exact approved remediation and its rollback. Never imply that a
listener, `doctor` pass, `/health` 200, or a `success` job counter proves
end-to-end workflow success; the counter only means the backend wait returned
without error.
