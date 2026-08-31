#!/usr/bin/env bash
# Ephemeral runner entrypoint: start the runner with an injected JIT config.
# The runner takes exactly one job (ephemeral) then exits; multirunner detects
# the exit and provisions a replacement.
set -euo pipefail

if [ -z "${JIT_CONFIG:-}" ]; then
  echo "ERROR: JIT_CONFIG env var is required" >&2
  exit 1
fi

runner_pid=""
cleanup() {
  if [ -n "${runner_pid}" ]; then
    # RUNNER_MANUALLY_TRAP_SIG=1 lets the runner handle the signal itself.
    kill -TERM "${runner_pid}" 2>/dev/null || true
    wait "${runner_pid}" 2>/dev/null || true
  fi
}
trap cleanup TERM INT

# The image ships the stock Runner.Worker.dll plus a patched sidecar that stops
# the runner from overriding ACTIONS_RESULTS_URL / ACTIONS_CACHE_URL. Swap it in
# only when multirunner injected a cache redirect; otherwise stock behaviour must
# win so actions/upload-artifact and actions/cache still reach GitHub.
if [ -n "${ACTIONS_RESULTS_URL:-}" ] && [ -f bin/Runner.Worker.dll.mrpatched ]; then
  cp -f bin/Runner.Worker.dll.mrpatched bin/Runner.Worker.dll
fi

./run.sh --jitconfig "${JIT_CONFIG}" &
runner_pid=$!
wait "${runner_pid}"
