# Windows "node" flavor: Node.js 22 LTS + corepack (npm/pnpm/yarn) on top of the
# minimal runner image. Mirrors images/linux/flavors/node.Dockerfile.
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
ARG NODE_VERSION=22.23.1

SHELL ["powershell", "-NoProfile", "-Command", "$ErrorActionPreference='Stop'; $ProgressPreference='SilentlyContinue';"]

# actions/setup-node looks for <tool-cache>/node/<version>/<arch> and treats the
# sibling `<arch>.complete` marker as proof the entry is fully written. The
# runner reads the cache root from these two variables, so both are set.
# Backslashes are doubled because Docker's default escape character is `\`, so
# `C:\hostedtoolcache\windows` would otherwise be stored as `C:hostedtoolcachewindows`.
ENV AGENT_TOOLSDIRECTORY=C:\\hostedtoolcache\\windows
ENV RUNNER_TOOL_CACHE=C:\\hostedtoolcache\\windows

RUN $ver = $env:NODE_VERSION; \
    $dest = 'C:\hostedtoolcache\windows\node\' + $ver + '\x64'; \
    Invoke-WebRequest -Uri ('https://nodejs.org/dist/v{0}/node-v{0}-win-x64.zip' -f $ver) -OutFile C:/node.zip; \
    Expand-Archive -Path C:/node.zip -DestinationPath C:/nodetmp; \
    New-Item -ItemType Directory -Force -Path $dest | Out-Null; \
    Copy-Item -Path ('C:/nodetmp/node-v{0}-win-x64/*' -f $ver) -Destination $dest -Recurse -Force; \
    New-Item -ItemType File -Force -Path ('C:\hostedtoolcache\windows\node\{0}\x64.complete' -f $ver) | Out-Null; \
    Remove-Item -Force C:/node.zip; \
    Remove-Item -Recurse -Force C:/nodetmp

# Put node/npm on PATH for steps that call them directly without setup-node.
RUN $dir = 'C:\hostedtoolcache\windows\node\' + $env:NODE_VERSION + '\x64'; \
    $p = [Environment]::GetEnvironmentVariable('PATH','Machine'); \
    [Environment]::SetEnvironmentVariable('PATH', $dir + ';' + $dir + '\node_modules\npm\bin;' + $p, 'Machine')

# corepack ships with Node 22 and provides the pnpm/yarn shims that
# pnpm/action-setup and `pnpm exec` rely on.
RUN $dir = 'C:\hostedtoolcache\windows\node\' + $env:NODE_VERSION + '\x64'; \
    & ($dir + '\corepack.cmd') enable --install-directory $dir; \
    & ($dir + '\node.exe') --version
