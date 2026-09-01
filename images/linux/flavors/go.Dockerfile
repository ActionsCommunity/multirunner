# Linux "go" flavor: Go toolchain on the native-build substrate (cgo works via
# the inherited gcc; pure-Go builds ignore it).
#
#   docker build -f images/linux/flavors/go.Dockerfile \
#     --build-arg PARENT=multirunner/runner-linux-native-build:dev -t multirunner/runner-linux-go:dev .
ARG PARENT=gerardsmit/multirunner-runner-linux:native-build
FROM ${PARENT}
# TARGETARCH (amd64|arm64) is supplied by buildx; it matches Go's release arch name.
ARG TARGETARCH

USER root
ENV PATH=/usr/local/go/bin:${PATH}
COPY images/versions.json /tmp/image-versions.json
RUN GO_VERSION="$(jq -er '.go.version' /tmp/image-versions.json)" \
    && ARCH="${TARGETARCH:-amd64}" \
    && case "$ARCH" in \
         amd64|arm64) ;; \
         *) echo "unsupported Go architecture: $ARCH" && exit 1 ;; \
       esac \
    && GO_SHA256="$(jq -er --arg key "linux_${ARCH}_sha256" '.go[$key]' /tmp/image-versions.json)" \
    && curl -fsSL "https://go.dev/dl/go${GO_VERSION}.linux-${ARCH}.tar.gz" -o /tmp/go.tar.gz \
    && echo "${GO_SHA256}  /tmp/go.tar.gz" | sha256sum --check - \
    && tar -C /usr/local -xzf /tmp/go.tar.gz \
    && rm /tmp/go.tar.gz /tmp/image-versions.json

USER runner
RUN go version
