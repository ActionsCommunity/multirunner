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
# Supported LTS lines, pinned to current security patches.
ARG NODE_VERSIONS="22.23.2 24.20.0"
ARG NODE_DEFAULT=22.23.2

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
         case "${ver}-${node_arch}" in \
           22.23.2-x64) sha=d60acfe00a2932254bb0ad20e01b0d74397a0875595de719654b214f4b03f307 ;; \
           22.23.2-arm64) sha=fff4078c5def658577f92c88db7db3bc0072924bfb93fe52c1e744a54e94abb8 ;; \
           24.20.0-x64) sha=2f2c0da162318f0de47665410c7c8c2ed3d36c8f3105de4bbc61176c70a7cbf2 ;; \
           24.20.0-arm64) sha=5f4ddab610c1ab2016b3c227cebdbf6d9495161487e4739c7b90090595f465f7 ;; \
           *) echo "missing checksum for ${ver}-${node_arch}" && exit 1 ;; \
         esac; \
         dest="/opt/hostedtoolcache/node/${ver}/${node_arch}"; \
         mkdir -p "$dest"; \
         archive="node-v${ver}-linux-${node_arch}.tar.xz"; \
         curl -fsSLO "https://nodejs.org/dist/v${ver}/${archive}"; \
         echo "${sha}  ${archive}" | sha256sum --check -; \
         tar -xJf "$archive" -C "$dest" --strip-components=1; \
         rm "$archive"; \
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
