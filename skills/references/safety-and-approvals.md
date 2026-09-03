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
- Before a mutation with a `--dry-run` mode, run it and report its plan. This
  covers `run`, `connect`, `bake`, all `service` actions, and the bundled
  installers. `cacheserver` has no dry-run; state its effects and request
  approval instead. A dry run is local planning and
  does not replace `doctor`, remote validation, approval, or a canary. It can
  still be slow: `bake --dry-run`, and `run --dry-run` with `qemu.bake_iso`
  configured, hash the whole ISO.
- Before a bundled Windows runtime install, run its non-elevated `--dry-run`,
  report the planned features, downloads, paths, service actions, and reboot
  outcome or uncertainty, then request approval for the real command.
- `doctor` is read-only, including for QEMU pools, and never cleans
  `qemu.work_dir`. A non-dry-run orchestration start first runs golden
  housekeeping (which can rearm or rebuild a golden headlessly), then removes
  top-level `*.qcow2`, `*.iso`, `*.vars.fd`, and `*.serial.log` files from each
  pool's `work_dir`, then runs the reachability preflight. Run cleanup or
  remediation only through a separate, explicit command after reviewing its
  targets and obtaining approval. Treat a golden rearm or rebuild as a separate
  write and drain capacity first.
- A service stop or restart waits at most 20 seconds before returning; jobs
  still running after that are interrupted.

`multirunner connect` is preferred over PAT authentication. Preview it with
`--dry-run`; applying it creates and installs a GitHub App, writes a restricted
private key, and rewrites the whole config file at mode 0600. Show its target
and command before approval. Its success output omits generated secrets; never
inspect or relay secret-bearing API responses. The current command accepts
`--repo owner/repo` or `--org organization`, plus `--name`, `--port`,
`--key-out`, and `--dry-run`. Don't invent repository-list or enterprise flags.
