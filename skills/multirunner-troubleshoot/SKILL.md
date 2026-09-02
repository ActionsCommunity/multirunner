---
name: multirunner-troubleshoot
description: >-
  **ANALYSIS SKILL**: Diagnose multirunner runtime, image, authentication, queue,
  and service failures from bounded, secret-safe evidence. INVOKES: multirunner
  doctor, service and runtime tools, HTTP health checks, GitHub CLI read commands.
  USE FOR: troubleshoot multirunner, jobs stuck queued, runtime unreachable, wrong
  container mode, service crash loop. DO NOT USE FOR: initial setup
  (use multirunner-setup), routine health checks (use multirunner-health), target
  changes (use multirunner-targets), upgrades (use multirunner-update).
---

# Troubleshoot multirunner

Follow [safety and approvals](../references/safety-and-approvals.md) and
[host assessment](../references/host-assessment.md). Use the applicable
[Windows host](../references/windows-host.md) or [Linux host](../references/linux-host.md)
checks, plus [runtimes and toolsets](../references/runtimes-and-toolsets.md),
[caching](../references/caching.md), and [authentication](../references/authentication.md).
Diagnosis stays read-only until remediation is approved.

## Workflow

1. Capture symptom, target, pool, time, and last healthy state.
2. Run doctor and inspect at most 200 redacted service log lines.
3. Trace the first broken boundary:

| Symptom | Read-only checks |
|---|---|
| Runtime unreachable or wrong container mode | Service, endpoint, runtime OS, host resources |
| DNS or image | Host and runtime DNS, OS build, digest, isolation, architecture |
| Authentication | Auth shape and file permissions without reading values |
| Queued jobs | Scope, labels, Actions setting, `runs-on`, capacity |
| Crash loop | Exit state, config, bounded logs, runtime dependencies |

4. Correlate `/health`, bounded metrics, and GitHub run timestamps.
5. State a verified cause or the missing evidence. Don't guess.
6. Propose one minimal correction and request required approvals.
7. Rerun the failed check, doctor, and `/health`. A canary needs separate approval.
