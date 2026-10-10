---
title: Multirunner Operations Console Operator Runbook
created: 2026-10-10
updated: 2026-10-10
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
   multirunner console open --config C:\multirunner\config.yaml
   ```

3. Open the fixed loopback URL shown by the command and enter the one-time
   pairing token displayed in the initiating terminal. Use `--no-browser` only
   when opening that URL manually on the same host.
4. Confirm the browser exchanges the token through the pairing form, clears
   the token input, captures the short-lived origin proof, and loads
   authenticated `/api/v1/system` and `/api/v1/session`. The token must never
   appear in the browser URL, launcher arguments, logs, or browser storage.

## Create and rotate the console secret

`service install` creates the protected console secret when history is enabled.
`service start` requires the existing secret and fails rather than silently
replacing it. Keep the database directory restricted to the operator and
service identity.

To invalidate every browser session:

1. Stop the Multirunner service.
2. Confirm no restore or update handoff is staged or activating.
3. Run:

   ```powershell
   multirunner console rotate-secret --config C:\multirunner\config.yaml
   ```

4. Restart the service and pair again with `console open`.

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
