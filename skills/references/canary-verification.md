# Canary verification

Choose a trusted workflow that already targets the configured labels. Show the
repository, workflow, ref, inputs, and exact dispatch command. Ask immediately before
dispatch.

Record the run creation time and earliest job start time. Report queue-to-start
latency, conclusion, expected labels, and run URL without exposing logs or secrets.
Confirm the ephemeral runner exits and replacement capacity becomes healthy. Finish
with `multirunner doctor --config <config-path>` and `/health` when configured.
