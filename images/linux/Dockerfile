# Minimal Linux ephemeral runner image for multirunner.
# Build context = repo root: docker build -f images/linux/Dockerfile -t multirunner/runner-linux:dev .
#
# Tiers (medium/github-like/custom-baked) override BASE in later milestones; the
# default below is the "minimal" tier built on the .NET runtime deps base.

ARG BASE=mcr.microsoft.com/dotnet/runtime-deps:8.0-noble

FROM ${BASE} AS build
ARG RUNNER_VERSION=2.337.0
# TARGETARCH is supplied automatically by buildx (amd64|arm64); map it to the
# runner's release arch name so multi-arch builds fetch the right tarball.
ARG TARGETARCH
ENV DEBIAN_FRONTEND=noninteractive
RUN apt-get update -y && apt-get install -y --no-install-recommends curl ca-certificates perl \
    && rm -rf /var/lib/apt/lists/*
WORKDIR /actions-runner
RUN case "${TARGETARCH:-amd64}" in \
        amd64) RUNNER_ARCH=x64 ;; \
        arm64) RUNNER_ARCH=arm64 ;; \
        *)     RUNNER_ARCH="${TARGETARCH}" ;; \
    esac \
    && curl -fsSL -o runner.tar.gz \
      "https://github.com/actions/runner/releases/download/v${RUNNER_VERSION}/actions-runner-linux-${RUNNER_ARCH}-${RUNNER_VERSION}.tar.gz" \
    && tar xzf runner.tar.gz \
    && rm runner.tar.gz

# Cache redirect patch, kept as a SIDECAR rather than applied in place.
#
# The runner injects ACTIONS_RESULTS_URL / ACTIONS_CACHE_URL into `uses:` actions
# from the job message, overriding any container env. Renaming those UTF-16LE
# string literals in Runner.Worker.dll (last char L->X) stops that, letting our
# injected ACTIONS_RESULTS_URL reach actions/cache. (See README: self-hosted cache.)
#
# That is only correct when multirunner supplies a replacement, which it does
# only when cache.enabled. Patching in place leaves ACTIONS_RESULTS_URL unset
# whenever the cache is off, and actions/upload-artifact then fails with
# "Unable to get the ACTIONS_RESULTS_URL env variable". entrypoint.sh swaps the
# sidecar in only when a redirect is actually present.
RUN cp bin/Runner.Worker.dll bin/Runner.Worker.dll.mrpatched \
    && perl -0777 -i \
      -pe 's/A\x00C\x00T\x00I\x00O\x00N\x00S\x00_\x00R\x00E\x00S\x00U\x00L\x00T\x00S\x00_\x00U\x00R\x00L\x00/A\x00C\x00T\x00I\x00O\x00N\x00S\x00_\x00R\x00E\x00S\x00U\x00L\x00T\x00S\x00_\x00U\x00R\x00X\x00/g;' \
      -pe 's/A\x00C\x00T\x00I\x00O\x00N\x00S\x00_\x00C\x00A\x00C\x00H\x00E\x00_\x00U\x00R\x00L\x00/A\x00C\x00T\x00I\x00O\x00N\x00S\x00_\x00C\x00A\x00C\x00H\x00E\x00_\x00U\x00R\x00X\x00/g;' \
      bin/Runner.Worker.dll.mrpatched \
    && grep -qa "A.C.T.I.O.N.S._.R.E.S.U.L.T.S._.U.R.X" bin/Runner.Worker.dll.mrpatched \
    && grep -qa "A.C.T.I.O.N.S._.C.A.C.H.E._.U.R.X" bin/Runner.Worker.dll.mrpatched \
    || (echo "patch failed" && exit 1)

FROM ${BASE}
ENV DEBIAN_FRONTEND=noninteractive \
    RUNNER_MANUALLY_TRAP_SIG=1 \
    ACTIONS_RUNNER_PRINT_LOG_TO_STDOUT=1 \
    RUNNER_DISABLE_AUTOUPDATE=1 \
    ImageOS=ubuntu24

RUN apt-get update -y && apt-get install -y --no-install-recommends \
      adduser sudo git curl ca-certificates jq unzip \
    && rm -rf /var/lib/apt/lists/* \
    && adduser --disabled-password --gecos "" --uid 1001 runner \
    && echo "runner ALL=(ALL:ALL) NOPASSWD:ALL" > /etc/sudoers.d/runner

WORKDIR /home/runner
COPY --chown=runner:runner --from=build /actions-runner .
# The runner ships dependency installers; ensure native deps are present.
RUN ./bin/installdependencies.sh && rm -rf /var/lib/apt/lists/*

COPY images/linux/entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh

USER runner
ENTRYPOINT ["/entrypoint.sh"]
