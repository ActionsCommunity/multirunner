# Linux "rust" flavor: rustup stable + musl target, with Node inherited from the
# node flavor (napi-rs / wasm-pack / Tauri all need Rust + Node in one job).
#
#   docker build -f images/linux/flavors/rust.Dockerfile \
#     --build-arg PARENT=multirunner/runner-linux-node:dev -t multirunner/runner-linux-rust:dev .
ARG PARENT=gerardsmit/multirunner-runner-linux:node
FROM ${PARENT}
ARG TARGETARCH

USER root
ENV DEBIAN_FRONTEND=noninteractive
RUN apt-get update -y && apt-get install -y --no-install-recommends musl-tools musl-dev \
    && rm -rf /var/lib/apt/lists/*

# System-wide rustup so the runner user (and any job) can add components/targets.
ENV RUSTUP_HOME=/usr/local/rustup \
    CARGO_HOME=/usr/local/cargo
ENV PATH=${CARGO_HOME}/bin:${PATH}
COPY images/versions.json /tmp/image-versions.json
RUN RUST_VERSION="$(jq -er '.rust.version' /tmp/image-versions.json)" \
    && case "${TARGETARCH:-amd64}" in \
      amd64) rustup_arch=x86_64; digest_key=rustup_x64_sha256 ;; \
      arm64) rustup_arch=aarch64; digest_key=rustup_arm64_sha256 ;; \
      *) echo "unsupported rustup architecture: ${TARGETARCH}" && exit 1 ;; \
    esac \
    && rustup_sha="$(jq -er --arg key "$digest_key" '.rust[$key]' /tmp/image-versions.json)" \
    && curl -fsSL "https://static.rust-lang.org/rustup/dist/${rustup_arch}-unknown-linux-gnu/rustup-init" -o /tmp/rustup-init \
    && echo "${rustup_sha}  /tmp/rustup-init" | sha256sum --check - \
    && chmod +x /tmp/rustup-init \
    && /tmp/rustup-init -y --no-modify-path --profile minimal --default-toolchain "${RUST_VERSION}" \
    && rm /tmp/rustup-init /tmp/image-versions.json \
    && rustup target add x86_64-unknown-linux-musl \
    && rustup component add clippy rustfmt \
    && chmod -R a+rwX "${RUSTUP_HOME}" "${CARGO_HOME}"

USER runner
RUN rustc --version && cargo --version
