# Authentication and validation

Prefer a GitHub App because it uses short-lived installation tokens and isn't
tied to a person. Assess the target and existing redacted auth shape before any
change.

## GitHub App

`connect` offers two credential models, differing in who owns the credential:

- **Shared app (default).** Authorizes the public "Multirunner Connect" App via
  the device flow: connect prints a user code, the operator enters it at
  `https://github.com/login/device`, and multirunner stores the resulting user
  access token. Nothing is created, there is no browser callback, and it works
  over SSH. The credential belongs to the authorizing user and stops working
  when they lose access to the org. The App is published by the multirunner
  project, so its publisher can mint installation tokens for every account that
  installs it; `--own-app` avoids that trust relationship. There are two such
  Apps, chosen by the target: `multirunner-connect`
  (`organization_self_hosted_runners: write`) for `--org`, and
  `multirunner-connect-personal` (`administration: write` + `metadata: read`) for
  `--repo`. Both exist on github.com only.
- **Own app (`--own-app`).** Creates a dedicated GitHub App in the org/account
  through the manifest browser flow. The credential is an org-owned installation
  token that keeps working after any individual leaves, but the App is the
  owner's to manage. Choose this when the credential must outlive a person.

On a terminal without `--own-app`, connect asks which model to use (defaulting to
the shared app). Off a terminal (`--non-interactive`, or redirected/null stdin)
connect fails unless the model is stated: pass `--own-app`, or `--device` to
accept a printed code — never where the output is logged, since whoever reads the
code can authorize their own account into the host. Preview either flow before
approval:

```text
multirunner connect --org <organization> --config <path> --dry-run --non-interactive
multirunner connect --org <organization> --config <path> --dry-run --own-app --non-interactive
```

### Shared-app device flow (default)

The dry run describes the device flow without contacting GitHub. Apply it by
repeating without `--dry-run`: enter the printed code at the verification URL and
authorize the App. Only enter a code you requested yourself in this session — the
device flow has no redirect, so a code someone else sends you is a phishing lever;
restrict it to CLIs and headless hosts, which is exactly this case.

The App must be installed on the target org before the token can manage its
runners. A first-time user is authorized but not yet installed
(`GET /user/installations` returns nothing), so connect prints the install URL
(`https://github.com/apps/multirunner-connect/installations/new`) and, on a
terminal, waits for the installation to appear; off a terminal it exits non-zero
so scripts fail fast. A `--org` naming a personal account is refused: only an
organization installation carries `organization_self_hosted_runners`. A `--repo`
target has no such requirement: its App is installed wherever the repo lives. On success connect writes `auth.client_id` and
`auth.token_path`, removing `auth.pat` and any installation-App keys. The user
access and refresh tokens live in a JSON sidecar next to the config at mode 0600
(`multirunner-user-token.json`); they rotate on refresh and are never inlined
into the YAML. multirunner refreshes the access token automatically before it
expires; a failed or expired refresh reports that you must re-run
`multirunner connect`, and never prints a token. Device auth drives runner scale
sets: it follows the normal `provisioning: scaleset` default (for non-`repos`
scopes) as well as `pool`, because a refreshing transport keeps the user access
token fresh for the life of the long-poll session. Enterprise scope and `scope:
repos` remain unavailable to device auth. Repository scope works through
`multirunner-connect-personal`, but that App only registers runners: repo
autoscale polling (`actions: read`) and doctor's workflow scan
(`contents: read`) need `--own-app`, and connect says so after a repo connect.
A config whose `github.url` names a host other than github.com is refused,
because both shared Apps, their client ids and their device endpoints only exist
on github.com.

### Own-app manifest flow (`--own-app`)

`--dry-run` prints the exact App manifest that would be POSTed, so review the
requested permissions and events before approving. After approval, repeat with
`--own-app` and without `--dry-run`. Pass `--non-interactive` whenever no person
is at the terminal: without it, connect prompts for anything the flags and config
do not already determine. Prompts never appear when stdin is not a terminal, so
the command cannot hang unattended. The manifest-only flags (`--webhook-url`,
`--detect`, `--name`, `--key-out`, `--port`) apply only here; on the device path
they are rejected with a pointer to `--own-app`.

Connect derives the manifest from what the host will actually do rather than
requesting a fixed set. Scope selects the runner-admin permission
(`administration: write` for a repository, `organization_self_hosted_runners:
write` for an organization); `actions: read` is added when the host polls the
job queue or subscribes to `workflow_job`; repository scope also carries
`contents: read` because doctor's workflow scan reads `.github/workflows`.
Use `--detect` to add `contents: read` on an organization target.

`--webhook-url <public-https-url>` registers an active hook and subscribes to
`workflow_job`. Connect asks for one only when the config already selects
autoscale; `scaleset` (the default) and `pool` receive no deliveries and are
never asked. Without it, connect registers an inactive placeholder hook and
no events: GitHub rejects a manifest whose hook URL is not publicly reachable
even when the hook is inactive, so a loopback or private address is refused
before the browser opens. To enable webhook mode afterwards, set the real URL in
the App settings and add the `workflow_job` event, which requires
`actions: read`. Scale-set provisioning needs none of this.

The command writes `app_id`, `installation_id`, and a private key path while
removing `auth.pat`. It never prints generated credentials. GitHub returns the
App's webhook secret exactly once, so connect writes it beside the private key
at mode 0600 and reports the path; it is not inlined into the YAML. Reference it
as `webhook.secret: "${VAR}"` and supply the value through the service
environment. Don't read or relay the private key, the webhook secret, or
secret-bearing API responses.

The current connect command doesn't create `repos` scoped Apps. An
`enterprise` scoped App is not merely unimplemented: GitHub grants Apps no
enterprise self-hosted-runner permission, so an installation token is rejected
by every enterprise runner endpoint. `scope: enterprise` with App credentials is
refused at config validation and must use `auth.pat` with a classic PAT carrying
`manage_runners:enterprise`. For `scope: repos`, every repository used with App
auth must belong to one App installation account.

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

`multirunner doctor --config <path>` validates the auth shape and performs
bounded read-only checks using the configured multirunner credentials. For
`repo` and `repos`, Actions disabled is a hard failure, while a missing
`self-hosted` workflow is only a note unless the scan was incomplete. For `org`
and `enterprise`, doctor lists one runner to prove the credential reaches the
runner-admin API for that scope; a wrong token, a token missing `admin:org` or
`manage_runners:enterprise`, and an unknown org login or enterprise slug are
reported apart, because their fixes differ. An empty runner list is a pass.

That check proves only reachability of the runner-admin API. It does not prove
label routing, webhook ingress, cache reachability, runner-group placement, or
that any workflow targets a self-hosted runner. Redact output before sharing it.
Validate private-key file existence and restrictive permissions without reading
its contents.
