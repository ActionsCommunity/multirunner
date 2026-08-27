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

SHELL ["powershell", "-NoProfile", "-Command", "$ErrorActionPreference='Stop'; $ProgressPreference='SilentlyContinue';"]

WORKDIR C:/actions-runner
RUN Invoke-WebRequest -Uri ('https://github.com/actions/runner/releases/download/v{0}/actions-runner-win-x64-{0}.zip' -f $env:RUNNER_VERSION) -OutFile runner.zip; \
    Expand-Archive -Path runner.zip -DestinationPath .; \
    Remove-Item runner.zip

# MinGit for actions/checkout.
RUN Invoke-WebRequest -Uri ('https://github.com/git-for-windows/git/releases/download/v{0}.windows.1/MinGit-{0}-64-bit.zip' -f $env:GIT_VERSION) -OutFile git.zip; \
    Expand-Archive -Path git.zip -DestinationPath C:/MinGit; \
    Remove-Item git.zip

# Cache redirect patch (same as Linux image).
COPY images/windows/patch-runner.ps1 C:/patch-runner.ps1
RUN C:/patch-runner.ps1 -Dll C:/actions-runner/bin/Runner.Worker.dll

ENV RUNNER_MANUALLY_TRAP_SIG=1 \
    ACTIONS_RUNNER_PRINT_LOG_TO_STDOUT=1 \
    RUNNER_DISABLE_AUTOUPDATE=1

# Prepend MinGit to PATH (keep the existing system PATH).
RUN $p = [Environment]::GetEnvironmentVariable('PATH','Machine'); \
    [Environment]::SetEnvironmentVariable('PATH', 'C:\MinGit\cmd;C:\MinGit\bin;' + $p, 'Machine')

COPY images/windows/entrypoint.ps1 C:/entrypoint.ps1
ENTRYPOINT ["powershell", "-NoProfile", "-ExecutionPolicy", "Bypass", "-File", "C:/entrypoint.ps1"]
