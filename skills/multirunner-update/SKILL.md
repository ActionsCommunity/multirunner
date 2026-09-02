---
name: multirunner-update
description: >-
  **WORKFLOW SKILL**: Upgrade multirunner with checksums, rollback, validation,
  and an approved canary. INVOKES: release API reads, checksum, doctor, runtime
  and service tools, GitHub CLI reads and approved dispatch. USE FOR: update
  multirunner, upgrade runner host, refresh images. DO NOT USE FOR: installation
  (use multirunner-setup), health-only checks (use multirunner-health), target changes
  (use multirunner-targets), unrelated runtime upgrades.
---

# Update multirunner

Follow [safety and approvals](../references/safety-and-approvals.md),
[host assessment](../references/host-assessment.md), [verified release](../references/verified-release.md),
the applicable [Windows host](../references/windows-host.md) or
[Linux host](../references/linux-host.md) checks,
[runtimes and toolsets](../references/runtimes-and-toolsets.md),
[caching](../references/caching.md),
[authentication](../references/authentication.md), and
[canary verification](../references/canary-verification.md).

## Workflow

1. Record version, service, runtimes, images, disk, and doctor baseline.
2. Compare with the latest stable release and read its notes.
3. Verify the staged release and its reported version.
4. Present service actions, image pulls, compatibility diff, canary, and rollback.
   Resolve existing health failures first.
5. Ask before stopping the service and creating a restrictive rollback copy.
6. Ask before replacing the binary and before each configured image pull. Don't prune.
7. Run the new binary's doctor against the unchanged config before restart.
8. On failure, show rollback and ask before restoring. Don't restore automatically.
9. Ask before service start or restart. Verify service, doctor, `/health`, runtime,
   capacity, and reprovision errors.
10. Complete the approved canary procedure. Retain rollback assets.

## Success criteria

Checksum, service, doctor, `/health`, and canary pass; rollback remains available;
configuration is unchanged or matches the exact approved diff.
