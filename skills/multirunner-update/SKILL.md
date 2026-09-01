---
name: multirunner-update
description: >-
  **WORKFLOW SKILL**: Safely upgrade multirunner binaries and runner images using
  stable releases, SHA256SUMS.txt verification, rollback preparation, conformance
  checks, and an approved canary. INVOKES: release API read commands, checksum tools,
  multirunner doctor, runtime CLIs, OS service tools, GitHub CLI read and approved
  dispatch commands. USE FOR: update multirunner, upgrade runner host, refresh
  multirunner images, verify multirunner upgrade. DO NOT USE FOR: first installation
  (use multirunner-setup), health-only checks (use multirunner-health), target changes
  (use multirunner-targets), unrelated runtime upgrades.
---

# Update multirunner

Follow [safety and approvals](../references/safety-and-approvals.md),
[host assessment](../references/host-assessment.md), [verified release](../references/verified-release.md),
and [canary verification](../references/canary-verification.md).

## Workflow

1. Record installed version, paths, service, runtimes, images, disk, and doctor baseline.
2. Compare with the latest stable release and read its notes.
3. Verify the staged release and its reported version.
4. Present versions, paths, service actions, image pulls, compatibility diff, canary,
   and rollback. Resolve existing health failures first.
5. Ask before stopping the service and creating a restrictive rollback copy.
6. Ask before replacing the binary and before each configured image pull. Don't prune.
7. Run the new binary's doctor against the unchanged config before restart.
8. On failure, show rollback and ask before restoring. Don't restore automatically.
9. Ask before service start or restart. Verify service, doctor, `/health`, runtime,
   capacity, and reprovision errors.
10. Complete the approved canary procedure. Retain rollback assets.

## Success criteria

Checksum and doctor pass, rollback remains available, configuration changes only
through an approved compatibility diff, and the canary reports queue latency.
