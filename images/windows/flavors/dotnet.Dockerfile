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

SHELL ["powershell", "-NoProfile", "-Command", "$ErrorActionPreference='Stop'; $ProgressPreference='SilentlyContinue';"]
COPY images/versions.json C:/image-versions.json

# Backslashes are doubled because Docker's default escape character is `\`, so a
# bare `C:\dotnet` is stored as the drive-relative `C:dotnet` and the SDK lands
# under WORKDIR instead of the intended absolute path.
ENV DOTNET_ROOT=C:\\dotnet
ENV DOTNET_CLI_TELEMETRY_OPTOUT=1
ENV DOTNET_NOLOGO=1
ENV DOTNET_SKIP_FIRST_TIME_EXPERIENCE=1

# Install every SDK assigned to the Windows container target and verify each
# archive against Microsoft's published SHA512 before extraction.
RUN $manifest = Get-Content C:/image-versions.json -Raw | ConvertFrom-Json; \
    $channels = @($manifest.dotnet.channels.PSObject.Properties | Where-Object { $_.Value.targets -contains 'windows-container' }); \
    if ($channels.Count -eq 0) { throw 'no .NET channel targets the Windows container' }; \
    foreach ($channel in $channels) { \
      $release = $channel.Value; \
      $archive = 'C:/dotnet-sdk-' + $channel.Name + '.zip'; \
      Invoke-WebRequest -Uri ('https://builds.dotnet.microsoft.com/dotnet/Sdk/{0}/dotnet-sdk-{0}-win-x64.zip' -f $release.version) -OutFile $archive; \
      if ((Get-FileHash $archive -Algorithm SHA512).Hash -ne $release.windows_x64_sha512) { throw ('dotnet SDK checksum mismatch for channel ' + $channel.Name) }; \
      Expand-Archive -Path $archive -DestinationPath $env:DOTNET_ROOT; \
      Remove-Item -Force $archive; \
    }; \
    Remove-Item -Force C:/image-versions.json

RUN $p = [Environment]::GetEnvironmentVariable('PATH','Machine'); \
    [Environment]::SetEnvironmentVariable('PATH', $env:DOTNET_ROOT + ';' + $env:DOTNET_ROOT + '\tools;' + $p, 'Machine')

RUN & ($env:DOTNET_ROOT + '\dotnet.exe') --list-sdks
