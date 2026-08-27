# Windows "dotnet" flavor: .NET SDK 10 on top of the node flavor, so .NET repos
# that also build a JS front end work in one image. Mirrors
# images/linux/flavors/dotnet.Dockerfile, which chains onto node the same way.
#
# The Windows SDK archive carries the WindowsDesktop targeting and runtime packs,
# so WPF/WinForms projects (UseWPF/UseWindowsForms) compile without a separate
# install. Native C/C++ still needs the buildtools flavor.
#
# Build on a Windows-container daemon matching the host (ltsc2025):
#   docker --host npipe:////./pipe/docker_engine_windows build \
#     -f images/windows/flavors/dotnet.Dockerfile \
#     --build-arg PARENT=multirunner/runner-windows:node \
#     -t multirunner/runner-windows:dotnet .
ARG PARENT=gerardsmit/multirunner-runner-windows:node
FROM ${PARENT}
ARG DOTNET_CHANNEL=10.0

SHELL ["powershell", "-NoProfile", "-Command", "$ErrorActionPreference='Stop'; $ProgressPreference='SilentlyContinue';"]

# Backslashes are doubled because Docker's default escape character is `\`, so a
# bare `C:\dotnet` is stored as the drive-relative `C:dotnet` and the SDK lands
# under WORKDIR instead of the intended absolute path.
ENV DOTNET_ROOT=C:\\dotnet
ENV DOTNET_CLI_TELEMETRY_OPTOUT=1
ENV DOTNET_NOLOGO=1
ENV DOTNET_SKIP_FIRST_TIME_EXPERIENCE=1

# dotnet-install.ps1 is the supported scriptable installer and resolves the
# newest patch on the channel, so the image does not pin a patch that goes stale.
RUN Invoke-WebRequest -Uri https://dot.net/v1/dotnet-install.ps1 -OutFile C:/dotnet-install.ps1; \
    & C:/dotnet-install.ps1 -Channel $env:DOTNET_CHANNEL -InstallDir $env:DOTNET_ROOT; \
    Remove-Item -Force C:/dotnet-install.ps1

RUN $p = [Environment]::GetEnvironmentVariable('PATH','Machine'); \
    [Environment]::SetEnvironmentVariable('PATH', $env:DOTNET_ROOT + ';' + $env:DOTNET_ROOT + '\tools;' + $p, 'Machine')

RUN & ($env:DOTNET_ROOT + '\dotnet.exe') --list-sdks
