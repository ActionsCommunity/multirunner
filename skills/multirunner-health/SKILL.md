---
name: multirunner-health
description: >-
  **ANALYSIS SKILL**: Inspect multirunner service state, runtimes, images, targets,
  metrics, queue latency, and recent bounded errors without changing the host or
  GitHub. INVOKES: multirunner doctor, OS service tools, runtime CLIs, HTTP health
  checks, GitHub CLI read commands. USE FOR: check multirunner health, inspect runner
  host, runner queue latency, multirunner status. DO NOT USE FOR: setup (use
  multirunner-setup), config or target changes (use multirunner-targets), remediation
  (use multirunner-troubleshoot), upgrades (use multirunner-update).
---

# Check multirunner health

Follow [safety and approvals](../references/safety-and-approvals.md) and
[host assessment](../references/host-assessment.md). Use the applicable
[Windows host](../references/windows-host.md) or [Linux host](../references/linux-host.md)
checks, plus [runtimes and toolsets](../references/runtimes-and-toolsets.md),
[caching](../references/caching.md), and [authentication](../references/authentication.md).
This skill is strictly read-only.

## Report

1. Report service state, version, and config path.
2. Report each pool as healthy, degraded, or blocked with runtime reachability,
   container mode, images, labels, and capacity.
3. Report target coverage, `/health`, bounded metrics, and recent errors.
4. Calculate recent queue-to-start from run creation to earliest job start using
   GitHub read commands. Don't dispatch a workflow.
5. Cite exact evidence for failures. Route remediation to
   `multirunner-troubleshoot` without performing it.

## Success criteria

Every target and pool was assessed. Output is bounded and secret-free. No local
or GitHub state changed.
