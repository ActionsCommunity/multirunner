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
and [canary verification](../references/canary-verification.md).

## Workflow

1. Ask for target, OS, concurrency, tools, config, and service intent.
2. Assess read-only. Use Docker or Podman for Linux, containerd or the standalone
   daemon for Windows containers, and QEMU for licensed Windows guests.
3. Propose backend, image tier, and redacted config diff.
4. Install the verified release. If needed, propose `multirunner install-containerd`,
   `multirunner install-windows-daemon --data-root <path>`, or an approved Linux
   package action.
5. After a reboot, reassess and resume from the first incomplete step.
6. Ask before config backup and write. Preserve unrelated fields, use environment
   references for secrets, then run doctor.
7. Prefer approved `multirunner connect --repo <owner/repo> --config <path>` or
   `multirunner connect --org <organization> --config <path>`.
8. After doctor passes, ask before `multirunner service install --config <path>` and
   `multirunner service start --config <path>`.
9. Verify service state, then complete canary verification.

## Success criteria

Checksum, doctor, service, and canary pass. Report queue latency, expose no
credential, and change configuration only through the approved diff.
