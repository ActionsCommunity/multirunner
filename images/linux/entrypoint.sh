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

runner_as_user=()
runner_command=(./run.sh --jitconfig "${JIT_CONFIG}")
docker_socket=/var/run/docker.sock
if [ -e "${docker_socket}" ]; then
  if [ ! -S "${docker_socket}" ]; then
    echo "ERROR: ${docker_socket} is mounted but is not a Unix socket" >&2
    exit 1
  fi

  socket_gid=$(stat -c '%g' "${docker_socket}")
  socket_group=$(getent group "${socket_gid}" | cut -d: -f1 || true)
  if [ -z "${socket_group}" ]; then
    socket_group="multirunner-docker-${socket_gid}"
    sudo groupadd --gid "${socket_gid}" "${socket_group}"
  fi
  sudo usermod --append --groups "${socket_group}" runner

  runner_as_user=(sudo --preserve-env --set-home --user runner -- env "PATH=${PATH}")
  runner_command=("${runner_as_user[@]}" ./run.sh --jitconfig "${JIT_CONFIG}")
  if ! "${runner_as_user[@]}" test -r "${docker_socket}" 2>/dev/null; then
    echo "ERROR: runner cannot read ${docker_socket} after group mapping" >&2
    exit 1
  fi
  if ! "${runner_as_user[@]}" test -w "${docker_socket}" 2>/dev/null; then
    echo "ERROR: runner cannot write ${docker_socket} after group mapping" >&2
    exit 1
  fi
fi

"${runner_command[@]}" &
runner_pid=$!
wait "${runner_pid}"
