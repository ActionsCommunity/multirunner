# Safety and approvals

Apply these rules to every multirunner skill:

- Assess read-only state before proposing a mutation.
- Show the exact command, local path, GitHub target, workflow, ref, inputs, config
  diff, and expected effects that apply to the requested action.
- Ask immediately before elevation, package or runtime installation, file writes,
  image pulls, service changes, GitHub App creation or installation, runner
  registration, workflow dispatch, cleanup, target removal, or other removals.
- Treat each service or GitHub change as a separate approval.
- Never print, paste, transmit, summarize, or store PATs, private keys, PEM content,
  JIT configuration, authorization headers, webhook secrets, cache access tokens,
  service credentials, or sensitive process environments.
- Redact sensitive fields before showing config or logs. Bound log collection.
- Preserve existing configuration and comments outside the exact approved diff.
- Don't restore, rotate, prune, deregister, or remove anything automatically.
- Keep `docker.enable_dind: false`. A Docker socket mount needs separate approval
  after explaining that jobs would control the host daemon.
- Don't use `multirunner run --install-deps` during assessment or diagnosis because
  it can elevate and mutate the host.

`multirunner connect` is preferred over PAT authentication. It creates and installs
a GitHub App and writes a restricted private key. Show its target and command before
approval, but don't relay its raw output because it can contain a webhook secret.
The current command accepts `--repo owner/repo` or `--org organization`. Don't invent
repository-list or enterprise flags.
