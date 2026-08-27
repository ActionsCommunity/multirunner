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

# vs_buildtools.exe returns 3010 (success, reboot required) on a clean install;
# treat 0 and 3010 as success and anything else as failure.
RUN Invoke-WebRequest -Uri https://aka.ms/vs/17/release/vs_buildtools.exe -OutFile C:/vs_buildtools.exe; \
    $p = Start-Process -FilePath C:/vs_buildtools.exe -Wait -PassThru -ArgumentList \
      '--quiet','--wait','--norestart','--nocache', \
      '--installPath','C:\BuildTools', \
      '--add','Microsoft.VisualStudio.Workload.VCTools', \
      '--add','Microsoft.VisualStudio.Component.VC.Tools.x86.x64', \
      '--add','Microsoft.VisualStudio.Component.Windows11SDK.26100', \
      '--add','Microsoft.VisualStudio.Component.VC.CMake.Project', \
      '--includeRecommended'; \
    if ($p.ExitCode -ne 0 -and $p.ExitCode -ne 3010) { throw \"vs_buildtools failed: $($p.ExitCode)\" }; \
    Remove-Item -Force C:/vs_buildtools.exe; \
    Remove-Item -Recurse -Force $env:TEMP/*

# vswhere.exe is installed by the VS Installer bootstrapper (not the workload) at
# the fixed path C:\Program Files (x86)\Microsoft Visual Studio\Installer\vswhere.exe,
# so microsoft/setup-msbuild and ilammy/msvc-dev-cmd locate the toolchain here.
# Jobs run `VsDevCmd.bat` from VSBUILDTOOLS or resolve MSVC/MSBuild via vswhere.
# Backslash is doubled because Docker's default escape character is `\`; a bare
# `C:\BuildTools` is stored as the drive-relative `C:BuildTools`.
ENV VSBUILDTOOLS=C:\\BuildTools
