# Actions cache and git cache

Treat cache endpoints as network services and cache tokens as secrets. Review a
redacted config and prove runner reachability before enabling either cache.

## Local Actions cache

`cache.enabled: true` starts the embedded cache only when `mode` is not `off`
and `external_url` is empty. `mode` is normally `local-server`, `storage` is
currently only `filesystem`, and `path` is the host storage directory. Any other
storage value, including `minio`, fails embedded-cache startup.
`listen` is the host bind address. `advertise_url` must be reachable from inside
every runner container or guest, not merely from the host. Multirunner maps only
a host name in the advertised URL to the backend's host alias: Docker adds an
extra-hosts entry pointing it at `host-gateway`, containerd rewrites it to the
Windows NAT gateway, and QEMU rewrites it to `10.0.2.2`. An IP literal such as
`127.0.0.1` is passed through unchanged and resolves to the container or guest
itself. Set `external_url` instead when an existing cache
service should be used. The embedded server exposes an unauthenticated
`GET /health` on `cache.listen`.

Keep `access_token` in an environment reference or omit it so the embedded
server generates one. It must be URL-path-safe (no `/`, `?`, `#`, or escaping).
For the embedded server, `advertise_url` is the untokenized base URL: Multirunner
appends `/_mr/<token>`. The private path token is the cache's access gate. Set
`skip_token_validation: true` when runner requests can carry
opaque or missing Actions bearer claims, as in `config.example.yaml`. Set it to
`false` only after proving every runner sends parsable cache scopes and a
repository ID. This setting parses claims but does not verify a JWT signature,
so it is not a replacement for the private path token or network restrictions.
Bind only to the required interface, restrict network access, and approve
firewall or service changes separately. Set `max_age_days`, `max_size_gb`, and
`gc_interval_sec` from the storage budget. Don't delete cache data during
assessment or update.

For an external standalone cache server, set an explicit persistent access token
there and set `cache.external_url` to its runner-reachable URL including
`/_mr/<token>`. The standalone `cacheserver --advertise` flag is informational:
it does not connect that service to Multirunner. External mode does not start
the embedded cache, so test its own health and data path separately. Current
startup logging records `cache.external_url` verbatim, including its path token:
restrict those logs and rotate the token if it is exposed. Treat any changed
retention, size cap, GC interval, or mirror age as an approved cleanup
operation, because a later scheduled GC/sweep can evict existing data. The
embedded cache's first GC tick is after gc_interval_sec.

## Git cache

`git_cache.mode: mirror` maintains a host bare mirror and mounts it read-only
into container pools. On a QEMU pool the mount is silently dropped and nothing
warns. `dotgit-cache` serves a bundle through the Actions cache server and works
where mounts don't, including QEMU. Both require `git_cache.path`;
`max_age_days` defaults to 30 when enabled.

Automatic mirroring and bundle wiring currently apply only to
`github.scope: repo`. `dotgit-cache` also needs the embedded, runner-reachable
Actions cache server: an `external_url` does not expose multirunner's
git-bundle endpoint. For private repositories, the mirror manager currently uses
`auth.pat`; App credentials aren't passed to git fetches. With App-only auth
against a private repository every fetch fails at warning level and startup
continues without a mirror. Enabling the PAT fallback makes PAT authentication
take precedence for all GitHub calls, so disclose the tradeoff and obtain
explicit approval.

Run `multirunner doctor --config <path>` to audit runtime reachability, remote
repository checks, and, when `git_cache` is enabled, inherited file-based git
URL rewrites or authorization headers. That git audit is advisory and never
fails preflight. `doctor` does not test the cache listener, tokenized URL, or
runner/guest cache route. Don't print headers, tokenized URLs, cache tokens, or
git credential output.
