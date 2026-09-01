---
name: multirunner-troubleshoot
description: >-
  **ANALYSIS SKILL**: Diagnose multirunner runtime, container mode, DNS, image,
  authentication, queue, and service crash failures using bounded, secret-safe
  evidence before proposing remediation. INVOKES: multirunner doctor, OS service
  tools, runtime CLIs, HTTP health checks, GitHub CLI read commands. USE FOR:
  troubleshoot multirunner, jobs stuck queued, runner runtime unreachable, wrong
  container mode, multirunner service crash loop. DO NOT USE FOR: initial setup
  (use multirunner-setup), routine health checks (use multirunner-health), target
  changes (use multirunner-targets), upgrades (use multirunner-update).
---

# Troubleshoot multirunner

Follow [safety and approvals](../references/safety-and-approvals.md) and
[host assessment](../references/host-assessment.md). Diagnosis stays read-only
until remediation is approved.

## Workflow

1. Capture symptom, target, labels, pool, time, and last healthy state.
2. Run doctor and inspect at most 200 redacted service log lines.
3. Trace the first broken boundary:

| Symptom | Read-only checks |
|---|---|
| Runtime unreachable or wrong container mode | Service, endpoint, runtime OS, host resources |
| DNS or image | Host and runtime DNS, OS build, digest, isolation, architecture |
| Authentication | Auth shape and file permissions without reading values |
| Queued jobs | Scope, labels, Actions setting, `runs-on`, capacity |
| Crash loop | Exit state, config, bounded logs, runtime dependencies |

4. Correlate `/health`, bounded metrics, and recent GitHub run timestamps.
5. State a verified cause or the missing evidence. Don't guess.
6. Propose one minimal correction with exact effects and request required approvals.
7. Rerun the failed check, doctor, and `/health`. Canary dispatch is separately approved.

## Success criteria

Evidence proves the cause and repaired boundary. Output is bounded and secret-free.
Doctor passes, and no mutation occurred before approval.
