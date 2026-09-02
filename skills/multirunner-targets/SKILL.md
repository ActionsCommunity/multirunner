---
name: multirunner-targets
description: >-
  **WORKFLOW SKILL**: Manage repository, repository-list, organization, and
  enterprise targets with approved config changes and read-only validation.
  INVOKES: doctor, connect, file and service tools, GitHub CLI reads.
  USE FOR: add, remove, or validate a
  multirunner target; change repository or organization.
  DO NOT USE FOR: first host setup (use multirunner-setup), health-only checks (use
  multirunner-health), failure diagnosis (use multirunner-troubleshoot), upgrades
  (use multirunner-update).
---

# Manage multirunner targets

Follow [safety and approvals](../references/safety-and-approvals.md) and
[authentication](../references/authentication.md). When target labels or pool
coverage change, use the applicable [Windows host](../references/windows-host.md)
or [Linux host](../references/linux-host.md) guidance and
[runtimes and toolsets](../references/runtimes-and-toolsets.md), then use
[canary verification](../references/canary-verification.md).

## Workflow

1. Locate the config, run doctor, and show redacted targets.
2. Verify targets with GitHub reads. Don't guess names or labels.
3. Build the smallest valid diff:

| Intent | Config shape |
|---|---|
| One repository | `github.scope: repo`, `owner`, and `repo` |
| Several repositories | `github.scope: repos`, optional default `owner`, and `repos` |
| Organization | `github.scope: org` and `owner` |
| Enterprise | `github.scope: enterprise` and `owner` |

4. Reject duplicates. App repository lists use one installation account. Fixed
   pools must cover every repository or use approved autoscaling.
5. Show the diff and untouched fields. Ask before backup and write.
6. Run doctor. Don't restore automatically after failure.
7. Prefer `connect` when repo or org authentication must change.
8. Explain effects, ask before restart, then run the approved canary.

## Success criteria

Doctor passes; authentication supports the scope; only the exact
approved fields changed; no target or registration was removed implicitly.
