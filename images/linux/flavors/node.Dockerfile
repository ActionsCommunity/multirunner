# Linux "node" flavor: Node.js + corepack (npm/pnpm/yarn).
# Inherits gcc/make/python3 from native-build, so node-gyp native modules build.
#
# Node is laid down in the hosted tool cache layout instead of a plain prefix so
# `actions/setup-node` resolves it from cache rather than downloading a copy on
# every job. NODE_VERSIONS lists every version to seed; jobs pinning any of them
# get a cache hit, and anything else still falls back to a download.
#
#   docker build -f images/linux/flavors/node.Dockerfile \
#     --build-arg PARENT=multirunner/runner-linux-native-build:dev -t multirunner/runner-linux-node:dev .
ARG PARENT=gerardsmit/multirunner-runner-linux:native-build
FROM ${PARENT}
# Kept in sync with the major versions the configured repos pin via setup-node.
ARG NODE_VERSIONS="20.20.0 22.23.1 24.13.0"
ARG NODE_DEFAULT=22.23.1

USER root
ENV DEBIAN_FRONTEND=noninteractive \
    AGENT_TOOLSDIRECTORY=/opt/hostedtoolcache \
    RUNNER_TOOL_CACHE=/opt/hostedtoolcache

# actions/setup-node looks for <tool-cache>/node/<version>/<arch> and treats the
# sibling `<arch>.complete` marker as proof the entry is fully written, so the
# marker is only written once the extract has succeeded.
RUN apt-get update -y && apt-get install -y --no-install-recommends ca-certificates curl xz-utils \
    && rm -rf /var/lib/apt/lists/* \
    && arch="$(dpkg --print-architecture)" \
    && case "$arch" in \
         amd64) node_arch=x64 ;; \
         arm64) node_arch=arm64 ;; \
         *) echo "unsupported arch: $arch" && exit 1 ;; \
       esac \
    && for ver in ${NODE_VERSIONS}; do \
         dest="/opt/hostedtoolcache/node/${ver}/${node_arch}"; \
         mkdir -p "$dest"; \
         curl -fsSL "https://nodejs.org/dist/v${ver}/node-v${ver}-linux-${node_arch}.tar.xz" \
           | tar -xJ -C "$dest" --strip-components=1; \
         test -x "$dest/bin/node"; \
         touch "/opt/hostedtoolcache/node/${ver}/${node_arch}.complete"; \
       done \
    && chown -R runner:runner /opt/hostedtoolcache

# Put the default Node on PATH for steps that call node/npm directly without
# going through setup-node.
ENV NODE_DEFAULT=${NODE_DEFAULT}
RUN arch="$(dpkg --print-architecture)" \
    && case "$arch" in amd64) node_arch=x64 ;; arm64) node_arch=arm64 ;; esac \
    && dir="/opt/hostedtoolcache/node/${NODE_DEFAULT}/${node_arch}" \
    && ln -sf "$dir/bin/node" /usr/local/bin/node \
    && ln -sf "$dir/bin/npm" /usr/local/bin/npm \
    && ln -sf "$dir/bin/npx" /usr/local/bin/npx \
    && ln -sf "$dir/bin/corepack" /usr/local/bin/corepack \
    && corepack enable --install-directory /usr/local/bin \
    && node --version && npm --version

USER runner
