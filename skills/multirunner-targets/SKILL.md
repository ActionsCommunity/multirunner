---
name: multirunner-targets
description: >-
  **WORKFLOW SKILL**: Add, remove, or validate multirunner repository, repository-list,
  organization, and enterprise targets through approval-gated config changes and
  read-only validation. INVOKES: multirunner doctor, multirunner connect, file tools,
  OS service tools, GitHub CLI read commands. USE FOR: add multirunner target, remove
  runner target, validate runner target, change multirunner repository or organization.
  DO NOT USE FOR: first host setup (use multirunner-setup), health-only checks (use
  multirunner-health), failure diagnosis (use multirunner-troubleshoot), upgrades
  (use multirunner-update).
---

# Manage multirunner targets

Follow [safety and approvals](../references/safety-and-approvals.md) and
[canary verification](../references/canary-verification.md). Change only approved
target fields.

## Workflow

1. Locate the active config, run doctor, and show current targets with secrets redacted.
2. Verify requested targets with GitHub read commands. Don't guess names or labels.
3. Build the smallest valid diff:

| Intent | Config shape |
|---|---|
| One repository | `github.scope: repo`, `owner`, and `repo` |
| Several repositories | `github.scope: repos`, optional default `owner`, and `repos` |
| Organization | `github.scope: org` and `owner` |
| Enterprise | `github.scope: enterprise` and `owner` |

4. Reject duplicates. App-authenticated repository lists must use one installation
   account. Fixed pools must cover repository count or use approved autoscaling.
5. Show the redacted diff and untouched fields. Ask before backup and write.
6. Run doctor. Don't restore automatically after failure.
7. Prefer an approved `connect` flow when repo or org authentication must change.
8. Explain registration effects, then ask before service restart.
9. If requested, perform the approved canary procedure.

## Success criteria

Doctor passes, authentication supports the scope, only approved fields changed,
and no target or registration was removed implicitly.
