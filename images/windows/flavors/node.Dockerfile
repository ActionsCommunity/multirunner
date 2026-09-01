# Windows "node" flavor: every pinned active Node.js LTS + corepack
# (npm/pnpm/yarn) on top of the minimal runner image. The manifest default is
# exposed on PATH; actions/setup-node can select every cached declared major.
#
# Node is laid down in the hosted tool cache layout instead of a plain directory
# so `actions/setup-node` resolves it from cache rather than downloading a copy
# on every job.
#
# Build on a Windows-container daemon matching the host (ltsc2025):
#   docker --host npipe:////./pipe/docker_engine_windows build \
#     -f images/windows/flavors/node.Dockerfile \
#     --build-arg PARENT=multirunner/runner-windows:pwsh \
#     -t multirunner/runner-windows:node .
ARG PARENT=gerardsmit/multirunner-runner-windows:minimal
FROM ${PARENT}

SHELL ["powershell", "-NoProfile", "-Command", "$ErrorActionPreference='Stop'; $ProgressPreference='SilentlyContinue';"]
COPY images/versions.json C:/image-versions.json

# actions/setup-node looks for <tool-cache>/node/<version>/<arch> and treats the
# sibling `<arch>.complete` marker as proof the entry is fully written. The
# runner reads the cache root from these two variables, so both are set.
# Backslashes are doubled because Docker's default escape character is `\`, so
# `C:\hostedtoolcache\windows` would otherwise be stored as `C:hostedtoolcachewindows`.
ENV AGENT_TOOLSDIRECTORY=C:\\hostedtoolcache\\windows
ENV RUNNER_TOOL_CACHE=C:\\hostedtoolcache\\windows

RUN $manifest = Get-Content C:/image-versions.json -Raw | ConvertFrom-Json; \
    $defaultMajor = [string]$manifest.node.default_major; \
    $defaultRelease = $manifest.node.releases.PSObject.Properties[$defaultMajor].Value; \
    if (-not $defaultRelease) { throw 'missing default Node release' }; \
    foreach ($property in $manifest.node.releases.PSObject.Properties) { \
      $major = $property.Name; \
      $release = $property.Value; \
      $ver = $release.version; \
      $dest = 'C:\hostedtoolcache\windows\node\' + $ver + '\x64'; \
      $archive = 'node-v' + $ver + '-win-x64.zip'; \
      $zip = 'C:/node-' + $major + '.zip'; \
      $temp = 'C:/nodetmp-' + $major; \
      Invoke-WebRequest -Uri ('https://nodejs.org/dist/v{0}/{1}' -f $ver, $archive) -OutFile $zip; \
      if ((Get-FileHash $zip -Algorithm SHA256).Hash -ne $release.windows_x64_sha256) { throw ('Node {0} archive checksum mismatch' -f $major) }; \
      Expand-Archive -Path $zip -DestinationPath $temp; \
      New-Item -ItemType Directory -Force -Path $dest | Out-Null; \
      Copy-Item -Path ($temp + '/node-v' + $ver + '-win-x64/*') -Destination $dest -Recurse -Force; \
      New-Item -ItemType File -Force -Path ($dest + '.complete') | Out-Null; \
      Remove-Item -Force $zip; \
      Remove-Item -Recurse -Force $temp; \
    }; \
    $defaultDest = 'C:\hostedtoolcache\windows\node\' + $defaultRelease.version + '\x64'; \
    $p = [Environment]::GetEnvironmentVariable('PATH','Machine'); \
    [Environment]::SetEnvironmentVariable('PATH', $defaultDest + ';' + $defaultDest + '\node_modules\npm\bin;' + $p, 'Machine'); \
    & ($defaultDest + '\corepack.cmd') enable --install-directory $defaultDest; \
    & ($defaultDest + '\node.exe') --version; \
    Remove-Item -Force C:/image-versions.json
