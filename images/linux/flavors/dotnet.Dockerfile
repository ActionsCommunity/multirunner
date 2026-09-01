# Linux "dotnet" flavor: .NET SDK 8 + 9, with Node inherited from the node flavor.
# Covers ASP.NET Core + JS SPA builds (mcr dotnet/sdk ships no Node; this does).
#
#   docker build -f images/linux/flavors/dotnet.Dockerfile \
#     --build-arg PARENT=multirunner/runner-linux-node:dev -t multirunner/runner-linux-dotnet:dev .
ARG PARENT=gerardsmit/multirunner-runner-linux:node
FROM ${PARENT}

USER root
ENV DOTNET_ROOT=/usr/local/dotnet \
    DOTNET_CLI_TELEMETRY_OPTOUT=1 \
    DOTNET_NOLOGO=1 \
    DOTNET_SKIP_FIRST_TIME_EXPERIENCE=1
ENV PATH=${DOTNET_ROOT}:${DOTNET_ROOT}/tools:${PATH}
COPY images/versions.json /tmp/image-versions.json
# Pin the exact SDK archives and verify the SHA512 values from Microsoft's
# release metadata before extracting them. Channels are extracted oldest first
# because every SDK shares one DOTNET_ROOT: the last-extracted `dotnet` host and
# hostfxr win, and only the newest host can run every installed SDK.
RUN arch="$(dpkg --print-architecture)" \
    && mkdir -p "${DOTNET_ROOT}" \
    && case "$arch" in \
         amd64) dotnet_arch=x64 ;; \
         arm64) dotnet_arch=arm64 ;; \
         *) echo "unsupported arch: $arch" && exit 1 ;; \
       esac \
    && channels="$(jq -er '.dotnet.channels | to_entries | map(select(.value.targets | index("linux"))) | sort_by(.key | split(".") | map(tonumber)) | .[].key' /tmp/image-versions.json)" \
    && for channel in $channels; do \
         version="$(jq -er --arg channel "$channel" '.dotnet.channels[$channel].version' /tmp/image-versions.json)" \
         && sha="$(jq -er --arg channel "$channel" --arg key "linux_${dotnet_arch}_sha512" '.dotnet.channels[$channel][$key]' /tmp/image-versions.json)" \
         && archive="dotnet-sdk-${version}-linux-${dotnet_arch}.tar.gz" \
         && curl -fsSLo "/tmp/${archive}" "https://builds.dotnet.microsoft.com/dotnet/Sdk/${version}/${archive}" \
         && echo "${sha}  /tmp/${archive}" | sha512sum --check - \
         && tar -xzf "/tmp/${archive}" -C "${DOTNET_ROOT}" \
         && rm "/tmp/${archive}" \
         || { echo ".NET SDK ${channel} install failed"; exit 1; }; \
       done \
    && rm /tmp/image-versions.json \
    && chmod -R a+rX "${DOTNET_ROOT}"

USER runner
RUN dotnet --list-sdks
