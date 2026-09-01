# Windows "node" flavor: the default pinned Node.js LTS + corepack
# (npm/pnpm/yarn) on top of the minimal runner image. Mirrors the default entry
# from images/versions.json.
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
    $major = [string]$manifest.node.default_major; \
    $release = $manifest.node.releases.PSObject.Properties[$major].Value; \
    if (-not $release) { throw 'missing default Node release' }; \
    $ver = $release.version; \
    $dest = 'C:\hostedtoolcache\windows\node\' + $ver + '\x64'; \
    $archive = 'node-v' + $ver + '-win-x64.zip'; \
    Invoke-WebRequest -Uri ('https://nodejs.org/dist/v{0}/{1}' -f $ver, $archive) -OutFile C:/node.zip; \
    if ((Get-FileHash C:/node.zip -Algorithm SHA256).Hash -ne $release.windows_x64_sha256) { throw 'Node archive checksum mismatch' }; \
    Expand-Archive -Path C:/node.zip -DestinationPath C:/nodetmp; \
    New-Item -ItemType Directory -Force -Path $dest | Out-Null; \
    Copy-Item -Path ('C:/nodetmp/node-v{0}-win-x64/*' -f $ver) -Destination $dest -Recurse -Force; \
    New-Item -ItemType File -Force -Path ('C:\hostedtoolcache\windows\node\{0}\x64.complete' -f $ver) | Out-Null; \
    $p = [Environment]::GetEnvironmentVariable('PATH','Machine'); \
    [Environment]::SetEnvironmentVariable('PATH', $dest + ';' + $dest + '\node_modules\npm\bin;' + $p, 'Machine'); \
    & ($dest + '\corepack.cmd') enable --install-directory $dest; \
    & ($dest + '\node.exe') --version; \
    Remove-Item -Force C:/node.zip, C:/image-versions.json; \
    Remove-Item -Recurse -Force C:/nodetmp
