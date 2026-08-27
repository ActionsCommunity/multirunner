# Windows ephemeral runner image for multirunner.
# IMPORTANT: build this on a Windows-container daemon (the standalone dockerd from
# setup/install-windows-daemon.ps1), NOT on Podman/WSL/Linux.
#   docker --host npipe:////./pipe/docker_engine_windows build \
#     -f images/windows/Dockerfile -t multirunner/runner-windows:dev .
#
# BASE must match the host build for process isolation: ltsc2025 (build 26100) on
# Windows 11 24H2 / Server 2025. Use ltsc2022 only with Hyper-V isolation.
ARG BASE=mcr.microsoft.com/windows/servercore:ltsc2025

FROM ${BASE}
ARG RUNNER_VERSION=2.337.0
ARG GIT_VERSION=2.47.1
ARG POWERSHELL_VERSION=7.6.4

SHELL ["powershell", "-NoProfile", "-Command", "$ErrorActionPreference='Stop'; $ProgressPreference='SilentlyContinue';"]

WORKDIR C:/actions-runner
RUN Invoke-WebRequest -Uri ('https://github.com/actions/runner/releases/download/v{0}/actions-runner-win-x64-{0}.zip' -f $env:RUNNER_VERSION) -OutFile runner.zip; \
    Expand-Archive -Path runner.zip -DestinationPath .; \
    Remove-Item runner.zip

# MinGit for actions/checkout.
RUN Invoke-WebRequest -Uri ('https://github.com/git-for-windows/git/releases/download/v{0}.windows.1/MinGit-{0}-64-bit.zip' -f $env:GIT_VERSION) -OutFile git.zip; \
    Expand-Archive -Path git.zip -DestinationPath C:/MinGit; \
    Remove-Item git.zip

# PowerShell 7. GitHub Actions defaults `run:` steps on Windows to `pwsh`, and
# GitHub-hosted windows runners ship it, so workflows routinely declare
# `shell: pwsh`. Without it those steps fail with "pwsh: command not found".
RUN Invoke-WebRequest -Uri ('https://github.com/PowerShell/PowerShell/releases/download/v{0}/PowerShell-{0}-win-x64.zip' -f $env:POWERSHELL_VERSION) -OutFile pwsh.zip; \
    Expand-Archive -Path pwsh.zip -DestinationPath C:/PowerShell; \
    Remove-Item pwsh.zip

# Cache redirect patch, kept as a SIDECAR rather than applied in place.
#
# The patch stops the runner from overriding ACTIONS_RESULTS_URL /
# ACTIONS_CACHE_URL, which is only correct when multirunner injects replacements
# (cache.enabled). Applying it unconditionally leaves ACTIONS_RESULTS_URL unset
# whenever the cache is off, and actions/upload-artifact then fails with
# "Unable to get the ACTIONS_RESULTS_URL env variable". entrypoint.ps1 swaps the
# sidecar in only when a redirect is actually present.
COPY images/windows/patch-runner.ps1 C:/patch-runner.ps1
RUN Copy-Item C:/actions-runner/bin/Runner.Worker.dll C:/actions-runner/bin/Runner.Worker.dll.mrpatched; \
    C:/patch-runner.ps1 -Dll C:/actions-runner/bin/Runner.Worker.dll.mrpatched

ENV RUNNER_MANUALLY_TRAP_SIG=1 \
    ACTIONS_RUNNER_PRINT_LOG_TO_STDOUT=1 \
    RUNNER_DISABLE_AUTOUPDATE=1

# Prepend MinGit and PowerShell 7 to PATH (keep the existing system PATH).
RUN $p = [Environment]::GetEnvironmentVariable('PATH','Machine'); \
    [Environment]::SetEnvironmentVariable('PATH', 'C:\MinGit\cmd;C:\MinGit\bin;C:\PowerShell;' + $p, 'Machine')

COPY images/windows/entrypoint.ps1 C:/entrypoint.ps1
ENTRYPOINT ["powershell", "-NoProfile", "-ExecutionPolicy", "Bypass", "-File", "C:/entrypoint.ps1"]
