---
name: multirunner-github
description: Configure or diagnose Multirunner GitHub targets, credentials, labels, scale sets, and workflow_job webhooks. Use for GitHub integration rather than local runtime or QEMU lifecycle work.
---

# Operate Multirunner GitHub integration

Use this for GitHub scope, App/PAT credentials, repository lists, labels,
runner groups, scale sets, and webhook autoscaling. Route local host/runtime
work to [multirunner-host](../multirunner-host/SKILL.md) and Windows VM golden
work to [multirunner-qemu](../multirunner-qemu/SKILL.md). For a broad or
unknown incident, start with [multirunner-diagnose](../multirunner-diagnose/SKILL.md).

## Load the relevant source of truth

Read "GitHub target and authentication" in
[host configuration](../docs/host-configuration.md), the matching
`connect`, `doctor`, or `run` command section of the
[CLI reference](../docs/cli-reference.md), and
[authentication](../references/authentication.md).

| Need | Read |
|---|---|
| Pool labels, autoscale order, or scale-set behavior | "Common pool fields" and "Metrics and provisioning" in [host configuration](../docs/host-configuration.md) |
| `workflow_job` receiver | "Webhook" in [host configuration](../docs/host-configuration.md) |
| Delivery, routing, or polling symptoms | Autoscale section of [triage signals](../references/triage-signals.md) |
| A GitHub/configuration mutation | [Safety and approvals](../references/safety-and-approvals.md) |
| Approved routing proof | [Canary verification](../references/canary-verification.md) |

## Operating rules

- Establish the exact scope, owner, repositories, App installation, labels,
  runner group, provisioning mode, and receiver boundary before changing
  anything. Do not infer any of them.
- `connect` supports one GitHub.com `--repo owner/repo` or `--org` target
  (plus `--name`, `--port`, `--key-out`, `--dry-run`). It writes the PEM
  (mode 0600) before the YAML, then rewrites the whole config file at mode
  0600, so it is not transactional and a service account may lose read
  access. It replaces an unparsable config with a fresh document, pins
  `github.url` to GitHub.com when absent, leaves stale `repo`/`repos` fields
  on an org target (nothing ever warns about them), and does not finish
  webhook setup. Each browser step times out after 5 minutes; a timeout can
  leave the App created on GitHub with nothing written locally. It creates a
  new App; it cannot rotate an existing App key or installation. Preserve the
  old credential/App until an approved cutover passes validation. Back up the
  YAML first; never expose credentials.
- A webhook path must begin with `/`; it is not config-validated. A value with
  no `/` at all panics the orchestrator when the receiver registers (after
  pools are prepared), and a value like `hooks/webhook` registers as a
  host-scoped route that never matches. An empty `webhook.secret` only logs a warning and
  then accepts unsigned events. Under `org`/`enterprise` scope there is no
  repository filter, so the secret is the only gate on who can launch runners.
  Use a nonempty secret reference and an approved TLS proxy/tunnel for public
  delivery. Do not expose raw webhook, cache, metrics, VNC, or QMP listeners
  publicly.
- Autoscale label matching: the pool must carry every job label,
  case-insensitive; a job with no labels matches any pool; pools are tried in
  YAML order and the first with a free slot wins. `pool` and `scaleset` modes
  do no local matching. Polling is a no-op for `org`/`enterprise`, and
  `poll_interval_sec: -1` with empty `webhook.listen` leaves autoscale
  silently inert.
- Scale sets need per-pool `scale_set` (required, unique) and optional
  `runner_group` (resolved by name; empty is GitHub's default group);
  `pool`/`autoscale` use numeric `runner_group_id` instead. Scale-set start
  creates, reuses, or updates the GitHub scale set (labels,
  runner group by name, auto-update disabled); runner names ignore
  `name_prefix`; shutdown deregisters running runners. `scaleset` with
  `scope: repos` passes config validation and fails only at start. Treat these
  GitHub-side effects separately from local capacity.

## Change and verify

1. Capture the redacted baseline and use read-only GitHub views plus `doctor`
   to assess the intended target. `doctor` does not prove ingress, custom-label
   routing, cache reachability, or org/enterprise workflow routing, and for
   `org`/`enterprise` scope it makes no GitHub call at all.
2. Present the smallest config/App/proxy change together with labels, capacity,
   remote effect, rollback, and canary plan. Obtain approval separately before
   config/secret writes, App or webhook edits, scale-set effects, restart, or
   canary dispatch.
3. After the approved change, use bounded redacted logs, the webhook HTTP
   statuses, and GitHub delivery or scale-set state to verify the changed
   boundary. Only an approved trusted canary proves end-to-end routing.
