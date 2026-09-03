# Authentication and validation

Prefer a GitHub App because it uses short-lived installation tokens and isn't
tied to a person. Assess the target and existing redacted auth shape before any
change.

## GitHub App

Preview one of the supported browser flows before approval:

```text
multirunner connect --repo <owner/repo> --config <path> --dry-run
multirunner connect --org <organization> --config <path> --dry-run
```

After approval, repeat the selected command without `--dry-run`.

The command writes `app_id`, `installation_id`, and a private key path while
removing `auth.pat`. It never prints generated credentials. It does not persist
the captured webhook secret or activate the App webhook; configure an autoscale
webhook separately if it is needed. Don't
read or relay the private key or secret-bearing API responses. The current
connect command doesn't create `repos` or `enterprise` scoped Apps. For
`scope: repos`, every repository used with App auth must belong to one App
installation account.

Back up the YAML first: connect writes the PEM (default
`<config-dir>/multirunner-app.private-key.pem`, or `--key-out`) before
updating YAML, so it is not transactional. It then rewrites the whole config
file at mode 0600, replaces a file that does not parse as a YAML mapping with a
fresh document, and sets `github.url` to GitHub.com when absent. Each browser
step waits up to 5 minutes; on timeout the App may already exist on GitHub with
nothing written locally. An organization target also leaves pre-existing
`github.repo` and `github.repos` fields in place and no later check warns about
them; remove stale fields deliberately.

## Explicit PAT fallback

Use a PAT only when the owner requests a scope that connect cannot create, a
private git cache needs it, or an existing approved deployment requires it.
Request only the classic scope needed by the configured target: `repo` for
repository runners (including every entry of `scope: repos`, which is the only
option when the listed repositories span more than one account), `admin:org`
for organization runners, or `manage_runners:enterprise` for enterprise
runners. Don't require unrelated
scopes. Store it as `auth.pat: "${GITHUB_PAT}"` and supply the value through the
service environment or a restrictively permissioned environment file. Never
paste, echo, inspect, or log the value. A configured PAT takes precedence over
App authentication.

## Read-only validation

`gh auth status --hostname <host>` safely reports the GitHub CLI account and
authentication state without printing the token. It doesn't validate the
credentials in multirunner's config. If the owner separately authorizes use of
the CLI identity, `gh api --method GET repos/<owner>/<repo> --silent` is a
read-only repository reachability check.

`multirunner doctor --config <path>` validates the auth shape and, for `repo`
and `repos`, performs bounded read-only checks using the configured multirunner
credentials: Actions disabled is a hard failure, while a missing `self-hosted`
workflow is only a note unless the scan was incomplete. For `org` and
`enterprise` scope it makes no GitHub call, so invalid credentials pass. Redact
output before sharing it. Validate private-key file existence and restrictive
permissions without reading its contents.
