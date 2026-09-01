# Windows "buildtools" flavor: a manifest-selected Visual Studio Build Tools
# release line on top of the minimal runner image. Gives MSVC, the Windows SDK,
# CMake and MSBuild for native C/C++ and .NET compile/test jobs — without the IDE
# (the IDE needs a golden VM, see `multirunner bake`, not a container).
#
# Build on a Windows-container daemon matching the host (ltsc2025):
#   docker build -f images/windows/flavors/buildtools.Dockerfile \
#     --build-arg PARENT=multirunner/runner-windows:dev \
#     --build-arg BUILDTOOLS_LINE=18 -t multirunner/runner-windows-buildtools:dev .
ARG PARENT=gerardsmit/multirunner-runner-windows:minimal
FROM ${PARENT}
ARG BUILDTOOLS_LINE

SHELL ["powershell", "-NoProfile", "-Command", "$ErrorActionPreference='Stop'; $ProgressPreference='SilentlyContinue';"]
COPY images/versions.json C:/image-versions.json
COPY images/windows/buildtools-smoke C:/buildtools-smoke

# vs_buildtools.exe returns 3010 (success, reboot required) on a clean install;
# treat 0 and 3010 as success and anything else as failure. Compile a real C#
# project with MSBuild so missing managed build dependencies fail the image build.
RUN $manifest = (Get-Content C:/image-versions.json -Raw | ConvertFrom-Json).buildtools; \
    $line = $env:BUILDTOOLS_LINE; \
    if ([string]::IsNullOrWhiteSpace($line)) { $line = $manifest.default_line }; \
    $property = $manifest.lines.PSObject.Properties[$line]; \
    if ($null -eq $property) { throw "unknown Build Tools release line: $line" }; \
    $v = $property.Value; \
    Invoke-WebRequest -Uri $v.bootstrapper_url -OutFile C:/vs_buildtools.exe; \
    if ((Get-FileHash C:/vs_buildtools.exe -Algorithm SHA256).Hash -ne $v.bootstrapper_sha256) { throw 'Build Tools bootstrapper checksum mismatch' }; \
    Invoke-WebRequest -Uri $v.channel_url -OutFile C:/vs.channel; \
    if ((Get-FileHash C:/vs.channel -Algorithm SHA256).Hash -ne $v.channel_sha256) { throw 'Build Tools channel checksum mismatch' }; \
    $p = Start-Process -FilePath C:/vs_buildtools.exe -Wait -PassThru -ArgumentList \
      '--quiet','--wait','--norestart','--nocache', \
      '--installPath','C:\BuildTools', \
      '--channelUri','file:///C:/vs.channel', \
      '--installChannelUri','file:///C:/vs.channel','--noUpdateInstaller', \
      '--add','Microsoft.VisualStudio.Workload.VCTools', \
      '--add','Microsoft.VisualStudio.Component.VC.Tools.x86.x64', \
      '--add','Microsoft.VisualStudio.Component.Windows11SDK.26100', \
      '--add','Microsoft.VisualStudio.Component.VC.CMake.Project', \
      '--add','Microsoft.Net.Component.4.8.SDK', \
      '--add','Microsoft.Net.Component.4.8.TargetingPack', \
      '--add','Microsoft.VisualStudio.Component.Roslyn.Compiler', \
      '--includeRecommended'; \
    if ($p.ExitCode -ne 0 -and $p.ExitCode -ne 3010) { throw "vs_buildtools failed: $($p.ExitCode)" }; \
    & C:/BuildTools/MSBuild/Current/Bin/MSBuild.exe C:/buildtools-smoke/BuildToolsSmoke.csproj /nologo /restore:false /p:Configuration=Release; \
    if ($LASTEXITCODE -ne 0) { throw "MSBuild smoke project failed: $LASTEXITCODE" }; \
    Remove-Item -Recurse -Force C:/buildtools-smoke; \
    Remove-Item -Force C:/vs_buildtools.exe, C:/vs.channel, C:/image-versions.json; \
    if ([string]::IsNullOrWhiteSpace($env:TEMP)) { throw 'TEMP is empty; refusing cleanup' }; \
    $tempPath = [IO.Path]::GetFullPath($env:TEMP).TrimEnd('\'); \
    $allowedTempPaths = @('C:\Users\ContainerAdministrator\AppData\Local\Temp', 'C:\Windows\Temp'); \
    if ($tempPath -notin $allowedTempPaths) { throw ("unexpected TEMP path; refusing cleanup: $tempPath") }; \
    Get-ChildItem -LiteralPath $tempPath -Force | Remove-Item -Recurse -Force

# vswhere.exe is installed by the VS Installer bootstrapper (not the workload) at
# the fixed path C:\Program Files (x86)\Microsoft Visual Studio\Installer\vswhere.exe,
# so microsoft/setup-msbuild and ilammy/msvc-dev-cmd locate the toolchain here.
# Jobs run `VsDevCmd.bat` from VSBUILDTOOLS or resolve MSVC/MSBuild via vswhere.
# Backslash is doubled because Docker's default escape character is `\`; a bare
# `C:\BuildTools` is stored as the drive-relative `C:BuildTools`.
ENV VSBUILDTOOLS=C:\\BuildTools
