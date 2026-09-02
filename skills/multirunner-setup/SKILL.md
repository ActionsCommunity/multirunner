---
name: multirunner-setup
description: >-
  **WORKFLOW SKILL**: Set up multirunner on Windows or Linux with a verified
  release and canary. INVOKES: multirunner, GitHub CLI reads, host tools.
  USE FOR: set up multirunner, install multirunner, configure a runner host.
  DO NOT USE FOR: read-only health checks (use multirunner-health), changing
  configured targets (use multirunner-targets), diagnosing an existing failure
  (use multirunner-troubleshoot), upgrades (use multirunner-update).
---

# Set up multirunner

Set up without source or Go. Follow [safety and approvals](../references/safety-and-approvals.md),
[host assessment](../references/host-assessment.md), [verified release](../references/verified-release.md),
[Windows host](../references/windows-host.md), [Linux host](../references/linux-host.md),
[runtimes and toolsets](../references/runtimes-and-toolsets.md),
[caching](../references/caching.md), [authentication](../references/authentication.md),
and [canary verification](../references/canary-verification.md).

## Workflow

1. Ask for target, OS, capacity, tools, and service intent.
2. Assess read-only; propose backend, toolset, and redacted config.
4. Install the verified release. If needed, propose `multirunner install-containerd`,
   `multirunner install-windows-daemon --data-root <path>`, or an approved Linux
   package action.
4. After reboot, reassess incomplete steps.
5. Ask before config backup and write. Preserve other fields, reference secrets
   from the environment, then run doctor.
6. Prefer approved `multirunner connect --repo <owner/repo> --config <path>` or
   `multirunner connect --org <organization> --config <path>`.
7. After doctor passes, ask before `multirunner service install --config <path>` and
   `multirunner service start --config <path>`.
8. Verify service state, then complete canary verification.

## Success criteria

Checksum and doctor pass; service is healthy; the canary completes with
queue-to-start latency reported. Output is secret-free and only approved config
fields changed.
