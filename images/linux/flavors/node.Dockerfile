# Linux "node" flavor: Node.js + corepack (npm/pnpm/yarn).
# Inherits gcc/make/python3 from native-build, so node-gyp native modules build.
#
# Node is laid down in the hosted tool cache layout instead of a plain prefix so
# `actions/setup-node` resolves it from cache rather than downloading a copy on
# every job. images/versions.json is the source of truth for versions,
# the default, and vendor digests; other versions still download.
#
# Corepack is installed from its own pinned npm tarball: Node stopped
# distributing it in the release archive from Node 25 onwards. npm is launched
# through a `/usr/bin/env node` shebang, so that install prepends the cached
# Node bin directory to PATH — the /usr/local/bin symlinks do not exist yet.
#
#   docker build -f images/linux/flavors/node.Dockerfile \
#     --build-arg PARENT=multirunner/runner-linux-native-build:dev -t multirunner/runner-linux-node:dev .
ARG PARENT=gerardsmit/multirunner-runner-linux:native-build
FROM ${PARENT}

USER root
ENV DEBIAN_FRONTEND=noninteractive \
    AGENT_TOOLSDIRECTORY=/opt/hostedtoolcache \
    RUNNER_TOOL_CACHE=/opt/hostedtoolcache
COPY images/versions.json /tmp/image-versions.json

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
    && default_major="$(jq -er '.node.default_major | tostring' /tmp/image-versions.json)" \
    && default_version="$(jq -er --arg major "$default_major" '.node.releases[$major].version' /tmp/image-versions.json)" \
    && test -n "$default_version" || { echo "missing default Node version"; exit 1; } \
    && majors="$(jq -er '.node.releases | keys[]' /tmp/image-versions.json)" \
    && test -n "$majors" || { echo "missing Node versions"; exit 1; } \
    && for major in $majors; do \
         ver="$(jq -er --arg major "$major" '.node.releases[$major].version' /tmp/image-versions.json)" \
         && archive="node-v${ver}-linux-${node_arch}.tar.xz" \
         && dest="/opt/hostedtoolcache/node/${ver}/${node_arch}" \
         && mkdir -p "$dest" \
         && curl -fsSLO "https://nodejs.org/dist/v${ver}/${archive}" \
         && checksum_key="linux_${node_arch}_sha256" \
         && checksum="$(jq -er --arg major "$major" --arg key "$checksum_key" '.node.releases[$major][$key]' /tmp/image-versions.json)" \
         && test -n "$checksum" \
         && echo "${checksum}  ${archive}" | sha256sum --check - \
         && tar -xJf "$archive" -C "$dest" --strip-components=1 \
         && rm "$archive" \
         && test -x "$dest/bin/node" \
         && touch "/opt/hostedtoolcache/node/${ver}/${node_arch}.complete" \
         || { echo "Node ${ver} install failed"; exit 1; }; \
       done \
    && dir="/opt/hostedtoolcache/node/${default_version}/${node_arch}" \
    && corepack_url="$(jq -er '.node.corepack.url' /tmp/image-versions.json)" \
    && corepack_sha512="$(jq -er '.node.corepack.sha512' /tmp/image-versions.json)" \
    && curl -fsSL -o /tmp/corepack.tgz "$corepack_url" \
    && echo "${corepack_sha512}  /tmp/corepack.tgz" | sha512sum --check - \
    && PATH="$dir/bin:$PATH" "$dir/bin/npm" install -g --no-audit --no-fund --no-update-notifier --cache /tmp/npm-cache /tmp/corepack.tgz \
    && rm -rf /tmp/corepack.tgz /tmp/npm-cache \
    && ln -sf "$dir/bin/node" /usr/local/bin/node \
    && ln -sf "$dir/bin/npm" /usr/local/bin/npm \
    && ln -sf "$dir/bin/npx" /usr/local/bin/npx \
    && ln -sf "$dir/bin/corepack" /usr/local/bin/corepack \
    && corepack enable --install-directory /usr/local/bin \
    && node --version && npm --version \
    && rm /tmp/image-versions.json \
    && chown -R runner:runner /opt/hostedtoolcache

USER runner
