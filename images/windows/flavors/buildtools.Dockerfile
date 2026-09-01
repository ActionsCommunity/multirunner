# Windows "buildtools" flavor: Visual Studio 2022 Build Tools (VCTools workload)
# on top of the minimal runner image. Gives MSVC v143, the Windows SDK, CMake and
# MSBuild for native C/C++ and .NET compile/test jobs — without the full VS IDE
# (the IDE needs a golden VM, see `multirunner bake`, not a container).
#
# Build on a Windows-container daemon matching the host (ltsc2025):
#   docker build -f images/windows/flavors/buildtools.Dockerfile \
#     --build-arg PARENT=multirunner/runner-windows:dev -t multirunner/runner-windows-buildtools:dev .
ARG PARENT=gerardsmit/multirunner-runner-windows:minimal
FROM ${PARENT}

SHELL ["powershell", "-NoProfile", "-Command", "$ErrorActionPreference='Stop'; $ProgressPreference='SilentlyContinue';"]
COPY images/versions.json C:/image-versions.json

# vs_buildtools.exe returns 3010 (success, reboot required) on a clean install;
# treat 0 and 3010 as success and anything else as failure.
RUN $v = (Get-Content C:/image-versions.json -Raw | ConvertFrom-Json).buildtools; \
    Invoke-WebRequest -Uri $v.bootstrapper_url -OutFile C:/vs_buildtools.exe; \
    if ((Get-FileHash C:/vs_buildtools.exe -Algorithm SHA256).Hash -ne $v.bootstrapper_sha256) { throw 'Build Tools bootstrapper checksum mismatch' }; \
    $p = Start-Process -FilePath C:/vs_buildtools.exe -Wait -PassThru -ArgumentList \
      '--quiet','--wait','--norestart','--nocache', \
      '--installPath','C:\BuildTools', \
      '--add','Microsoft.VisualStudio.Workload.VCTools', \
      '--add','Microsoft.VisualStudio.Component.VC.Tools.x86.x64', \
      '--add','Microsoft.VisualStudio.Component.Windows11SDK.26100', \
      '--add','Microsoft.VisualStudio.Component.VC.CMake.Project', \
      '--includeRecommended'; \
    if ($p.ExitCode -ne 0 -and $p.ExitCode -ne 3010) { throw \"vs_buildtools failed: $($p.ExitCode)\" }; \
    Remove-Item -Force C:/vs_buildtools.exe, C:/image-versions.json; \
    if ([string]::IsNullOrWhiteSpace($env:TEMP)) { throw 'TEMP is empty; refusing cleanup' }; \
    $tempPath = [IO.Path]::GetFullPath($env:TEMP).TrimEnd('\'); \
    $allowedTempPaths = @('C:\Users\ContainerAdministrator\AppData\Local\Temp', 'C:\Windows\Temp'); \
    if ($tempPath -notin $allowedTempPaths) { throw (\"unexpected TEMP path; refusing cleanup: $tempPath\") }; \
    Get-ChildItem -LiteralPath $tempPath -Force | Remove-Item -Recurse -Force

# vswhere.exe is installed by the VS Installer bootstrapper (not the workload) at
# the fixed path C:\Program Files (x86)\Microsoft Visual Studio\Installer\vswhere.exe,
# so microsoft/setup-msbuild and ilammy/msvc-dev-cmd locate the toolchain here.
# Jobs run `VsDevCmd.bat` from VSBUILDTOOLS or resolve MSVC/MSBuild via vswhere.
# Backslash is doubled because Docker's default escape character is `\`; a bare
# `C:\BuildTools` is stored as the drive-relative `C:BuildTools`.
ENV VSBUILDTOOLS=C:\\BuildTools
