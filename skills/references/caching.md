# Actions cache and git cache

Treat cache endpoints as network services and cache tokens as secrets. Review a
redacted config and prove runner reachability before enabling either cache.

## Local Actions cache

`cache.enabled: true` starts the embedded cache when `external_url` is empty.
`mode` is `local-server`, `storage` is `filesystem`, and `path` is the host
storage directory. `listen` is the host bind address. `advertise_url` must be
reachable from inside every runner container or guest, not merely from the
host. Set `external_url` instead when an existing cache service should be used.

Keep `access_token` in an environment reference or omit it so the embedded
server generates one. Never display it. The private path token is the cache's
access gate. Set `skip_token_validation: true` when runner requests can carry
opaque or missing Actions bearer claims, as in `config.example.yaml`. Set it to
`false` only after proving every runner sends parsable cache scopes and a
repository ID. This setting parses claims but does not verify a JWT signature,
so it is not a replacement for the private path token or network restrictions.
Bind only to the required interface, restrict network access, and approve
firewall or service changes separately. Set `max_age_days`, `max_size_gb`, and
`gc_interval_sec` from the storage budget. Don't delete cache data during
assessment or update.

## Git cache

`git_cache.mode: mirror` maintains a host bare mirror and mounts it read-only
into container pools. `dotgit-cache` serves a bundle through the Actions cache
server and works where mounts don't, including QEMU. Both require
`git_cache.path`; `max_age_days` defaults to 30 when enabled.

Automatic mirroring and bundle wiring currently apply only to
`github.scope: repo`. `dotgit-cache` also needs an enabled, runner-reachable
Actions cache URL. For private repositories, the mirror manager currently uses
`auth.pat`; App credentials aren't passed to git fetches. Enabling that fallback
makes PAT authentication take precedence for all GitHub calls, so disclose the
tradeoff and obtain explicit approval.

Run `multirunner doctor --config <path>` to audit runtime reachability, remote
repository checks, and inherited file-based git URL rewrites or authorization
headers. Don't print headers, tokenized URLs, cache tokens, or git credential
output.
