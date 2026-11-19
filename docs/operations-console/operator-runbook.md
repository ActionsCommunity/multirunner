---
title: Multirunner Operations Console Operator Runbook
created: 2026-10-10
updated: 2026-10-21
status: active
type: feature
tags: [operations-console, operations, recovery, updates]
---

# Multirunner Operations Console Operator Runbook

Use this runbook for one loopback-only Multirunner installation. Replace paths
with the installed host's configuration, database, binary, and rollback paths.
Never bypass a failed trust, integrity, compatibility, or health check.

## Pair and open the console

1. Confirm `history.enabled: true`, a local database path, and a loopback-only
   `history.listen` value.
2. Start Multirunner, then run:

   ```powershell
   multirunner console --config C:\multirunner\config.yaml
   ```

3. Open the fixed loopback URL shown by the command and enter the one-time
   pairing token displayed in the initiating terminal. Use `--no-browser` only
   when opening that URL manually on the same host.
4. Confirm the browser exchanges the token through the pairing form, clears
   the token input, captures the short-lived origin proof, and loads
   authenticated `/api/v1/system` and `/api/v1/session`. The token must never
   appear in the browser URL, launcher arguments, logs, or browser storage.

## Protect the console secret

`service install` creates the protected console secret when history is enabled.
`service start` requires the existing secret and fails rather than silently
replacing it. Keep the database directory restricted to the operator and
service identity. The daemon is the sole owner of pairing and session
authentication state; the CLI never reads or rotates the secret. On Windows,
install the service as the operator who will launch the console; installation
records that exact SID for the protected pairing pipe.

Windows service installation/start must be run once from an elevated terminal
as that same operator. It records an immutable protected HKLM owner binding for
the canonical database directory; LocalSystem authenticates the binding before
reading the operator marker or console secret. Existing conflicting or
incompletely written bindings fail closed and require administrator repair;
they are never silently replaced.

For a history-enabled Windows service using device-flow OAuth authentication,
the active, explicitly configured
`auth.token_path` is also validated as that elevated operator, including the
existing token's protected permissions and parent-directory owners. Its
directory receives a separate protected binding to the **same** operator SID.
The service bootstraps that binding before constructing GitHub clients and
reading or refreshing the token. An external token directory does not require
`console.operator`. Only files directly in each bound directory receive this
owner trust, not its ancestors' files, siblings, or subdirectories.

No additional binding is created without an active explicit OAuth token path
(for example, when PAT or installation-App authentication is selected). There
is no automatic trust for implicit home/profile credential locations, GitHub
CLI token stores, or arbitrary paths discovered later. Configure the actual
OAuth sidecar path and repeat elevated service install/start as the original
operator. `auth.token_path` must resolve to the same location for that operator
and LocalSystem; profile-dependent environment references are not remapped from
the service's profile back to the operator's profile. These Windows service
bindings require local-drive, plain paths;
UNC, device paths, and symlink/redirected OAuth token paths are unsupported.
Foreground operator authentication is unchanged.

## Configure history ingestion

REST enrichment/backfill is limited to explicitly configured repository targets
(`owner/repository`). Organization and enterprise targets are not enumerated
into repositories. Their workflow/job history needs reachable GitHub
`workflow_job` webhooks; local runner lifecycle history alone does not contain
the workflow's jobs and steps. Missing webhook deliveries in these scopes
cannot be repaired by repository REST synchronization.

Keep `history.listen` loopback-only. Configure the separate `webhook.listen`
endpoint to receive the applicable GitHub deliveries, and configure the same
nonempty `webhook.secret` in GitHub and Multirunner. Signatures are mandatory,
including behind a tunnel. An old unsigned listener must be migrated to a
shared secret before startup, or disabled by removing `webhook.listen`.

Search results for jobs and steps open their parent run inspection. Runner
session links inspect retained lifecycle history (including previous host
epochs); command links inspect the selected command alongside the audit list.

## Migrate and enable capabilities

1. Preserve the current binary and create a verified WAL-aware database backup.
2. Stage the new binary and record its SHA-256, version, and commit.
3. Start it against a copy of the production database first. Migration must
   verify the contiguous migration ledger, immutable migration checksums,
   available disk, database integrity, and supported schema range.
4. Roll back with the pre-migration backup. Do not attempt reverse migrations.
5. Enable privileged capabilities only after their targeted security,
   interruption, recovery, observability, and platform tests pass. A compiled
   capability is not automatically approved for production use.

## Back up and restore

### Drain without blocking controls

Drain pauses new provisioning and durably waits for existing sessions; it does
not hold the command executor while runners finish. Resume or Cancel drain can
interrupt the wait, and individual Terminate/Recycle controls remain available
for the same pool. Warm idle runners do not finish without a job: verify that
they are idle, then explicitly terminate them if you need an empty pool. Drain
never assumes an active session is idle or kills a possibly busy job for you.
If its deadline expires, the wait ends as `interrupted`; the pool stays paused
and existing jobs continue. Resume explicitly when it is safe.

One daemon owns recovery and command execution for each history database through
an OS-held `.runtime.lock` sidecar. Keep that file in place; deleting it can
split the lock identity. The OS releases ownership when the process exits, so
a crashed daemon does not leave a permanent lock. Startup closes incomplete
sessions from the previous runtime as interrupted before provisioning begins.

### Reconcile an unknown command outcome

Open the command from Audit (including its search/deep link). Inspect the
runtime, artifact metadata, and command evidence first. If the command has an
unknown outcome, enter a reason and the displayed `reconcile <command ID>`
phrase, then choose **Reconcile outcome**. A recent local console pairing is
required. This records the operator, reason, state, and fencing token before
requesting a fresh authoritative reconciliation; it never executes the effect
again. The domain remains fenced while reconciling and remains blocked if the
adapter still cannot prove whether the effect completed. Follow **Open current
command state** to see the result. Stale submissions are rejected; reload the
command before trying again. Do not edit the database or delete the uncertain
command to bypass its fence.

- Create backups through the console command path. Backups use SQLite online
  backup semantics, include checksum/schema/build metadata, and are verified
  before being reported as usable.
- Before restore, drain active work and preserve the current database and
  binary as rollback artifacts.
- Stage and validate the restore copy before activation. Require checksum,
  integrity, foreign-key, schema, migration, host-policy, and disk checks.
- Resume provisioning only after service, API, database, event, and runner
  health pass. If activation or verification fails, restore the preserved
  database and record the failure.

## Inspect and stage signed updates

1. Configure an HTTPS metadata URL and the approved threshold-signed root,
   target key IDs, repository, workflow, and builder identity.
2. Without a trusted root, use inspection and non-mutating preflight only.
   Download-for-execution and apply remain blocked.
3. Verify root rotation, timestamp, snapshot, targets, expiry, monotonic
   versions, artifact hash/length, revocation policy, release tag/commit,
   provenance, SBOM, platform, API, UI, and database compatibility.
4. Before apply, create a verified backup and drain active work.
5. Keep the staged artifact and previous binary on a restricted local path.

## Deploy on Windows and handle UAC

1. Wait for active runners to drain and stop new intake.
2. Record the staged binary hash and preserve the installed binary/database.
3. Start the approved elevated replacement. UAC cancellation means deployment
   is blocked; it is not a successful or partially verified release.
4. Replace atomically through the service-manager path. Do not overwrite the
   running executable from the worker process.
5. Retain the elevation transcript, artifact hashes, backup metadata, and
   verification results.

## Verify health

After migration, restore, or update, verify:

- the service is running the expected binary version, commit, and hash;
- the UI, API version, embedded UI digest, and database schema are compatible;
- anonymous console/API requests are rejected;
- a new one-time pairing token creates an authenticated session;
- `/api/v1/system`, `/api/v1/events`, health, metrics, and history queries work;
- event replay advances without gaps and no command remains unexpectedly
  claimed, running, reconciling, or unknown;
- runner provisioning and active-job accounting remain healthy.

## Roll back

Rollback immediately when replacement, startup, migration, API/UI
compatibility, event health, or runner health verification fails:

1. Stop the failed service instance.
2. Restore the known-good binary.
3. Restore the compatible pre-migration/pre-update database when required.
4. Restart and repeat the full health verification.
5. Keep failure evidence and rollback artifacts until the incident is resolved.

Do not delete the prior binary, database backup, metadata, or verification
evidence during the same maintenance window.
