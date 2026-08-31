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
ARG DOTNET_VERSION=10.0.400
ARG DOTNET_SHA512=9b8b88590e4da131bfd0da7aa089d0fc04d5418d5f8607ec13d55dc5a17b4399afd54d496c12657fa05c6c6546dc5eab930f26ac6c50f2d3a7712c0fb378c366

SHELL ["powershell", "-NoProfile", "-Command", "$ErrorActionPreference='Stop'; $ProgressPreference='SilentlyContinue';"]

# Backslashes are doubled because Docker's default escape character is `\`, so a
# bare `C:\dotnet` is stored as the drive-relative `C:dotnet` and the SDK lands
# under WORKDIR instead of the intended absolute path.
ENV DOTNET_ROOT=C:\\dotnet
ENV DOTNET_CLI_TELEMETRY_OPTOUT=1
ENV DOTNET_NOLOGO=1
ENV DOTNET_SKIP_FIRST_TIME_EXPERIENCE=1

# Pin the SDK archive and verify Microsoft's published SHA512 before extraction.
RUN $archive = 'C:/dotnet-sdk.zip'; \
    Invoke-WebRequest -Uri ('https://builds.dotnet.microsoft.com/dotnet/Sdk/{0}/dotnet-sdk-{0}-win-x64.zip' -f $env:DOTNET_VERSION) -OutFile $archive; \
    if ((Get-FileHash $archive -Algorithm SHA512).Hash -ne $env:DOTNET_SHA512) { throw 'dotnet SDK checksum mismatch' }; \
    Expand-Archive -Path $archive -DestinationPath $env:DOTNET_ROOT; \
    Remove-Item -Force $archive

RUN $p = [Environment]::GetEnvironmentVariable('PATH','Machine'); \
    [Environment]::SetEnvironmentVariable('PATH', $env:DOTNET_ROOT + ';' + $env:DOTNET_ROOT + '\tools;' + $p, 'Machine')

RUN & ($env:DOTNET_ROOT + '\dotnet.exe') --list-sdks
