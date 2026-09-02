---
name: multirunner-setup
description: >-
  **WORKFLOW SKILL**: Set up multirunner on Windows or Linux with a verified binary,
  GitHub App, service, and canary. INVOKES: multirunner, GitHub CLI read
  commands, OS and runtime tools. USE FOR: set up multirunner, install multirunner,
  configure a runner host, onboard a multirunner host.
  DO NOT USE FOR: read-only health checks (use multirunner-health), changing
  configured targets (use multirunner-targets), diagnosing an existing failure
  (use multirunner-troubleshoot), upgrades (use multirunner-update).
---

# Set up multirunner

Set up without source code or Go. Follow [safety and approvals](../references/safety-and-approvals.md),
[host assessment](../references/host-assessment.md), [verified release](../references/verified-release.md),
[Windows host](../references/windows-host.md), [Linux host](../references/linux-host.md),
[runtimes and toolsets](../references/runtimes-and-toolsets.md),
[caching](../references/caching.md), [authentication](../references/authentication.md),
and [canary verification](../references/canary-verification.md).

## Workflow

1. Ask for target, OS, concurrency, tools, config, and service intent.
2. Assess read-only, then propose the backend, toolset, and redacted config diff.
4. Install the verified release. If needed, propose `multirunner install-containerd`,
   `multirunner install-windows-daemon --data-root <path>`, or an approved Linux
   package action.
4. After a reboot, reassess and resume from the first incomplete step.
5. Ask before config backup and write. Preserve unrelated fields, use environment
   references for secrets, then run doctor.
6. Prefer approved `multirunner connect --repo <owner/repo> --config <path>` or
   `multirunner connect --org <organization> --config <path>`.
7. After doctor passes, ask before `multirunner service install --config <path>` and
   `multirunner service start --config <path>`.
8. Verify service state, then complete canary verification.
