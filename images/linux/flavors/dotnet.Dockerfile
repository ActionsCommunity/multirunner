# Linux "dotnet" flavor: .NET SDK 8 + 9, with Node inherited from the node flavor.
# Covers ASP.NET Core + JS SPA builds (mcr dotnet/sdk ships no Node; this does).
#
#   docker build -f images/linux/flavors/dotnet.Dockerfile \
#     --build-arg PARENT=multirunner/runner-linux-node:dev -t multirunner/runner-linux-dotnet:dev .
ARG PARENT=gerardsmit/multirunner-runner-linux:node
FROM ${PARENT}
ARG DOTNET8_VERSION=8.0.424
ARG DOTNET9_VERSION=9.0.317
ARG DOTNET8_X64_SHA512=6503fd9f464d5e3a4f43a881d2b74afc6a2c46ceda74d027f1565b7239f4b3ec884857c03c0dcd49eb52f384d5ae1fa5aaf135f0a6aabc5518103aceed643c74
ARG DOTNET8_ARM64_SHA512=bb19b6779ad93d146055583d644ef269bb42501f6c7fdef51e14026cde9d5fd726d370de098a8d8504867fb24bfcb5ab88cc22bec812461aede334de1aacf7b6
ARG DOTNET9_X64_SHA512=145bf69dcb88c4b905feb531cfdd7894a75fc875d2a030e958a13d1fb1131521c8cebd8a8a6e0fbd1a433ebae9cde86356b6adad07b1ad81efb92b36ff8a3333
ARG DOTNET9_ARM64_SHA512=fdf30fe705c91304d890115e955f738055f8c0885ea9891e7df1153321120fa2c38b6ae4dd132f871cb8facc0d1fabbd2b25ddd53d0a5b4293aa85d296e3b98d

USER root
ENV DOTNET_ROOT=/usr/local/dotnet \
    DOTNET_CLI_TELEMETRY_OPTOUT=1 \
    DOTNET_NOLOGO=1 \
    DOTNET_SKIP_FIRST_TIME_EXPERIENCE=1
ENV PATH=${DOTNET_ROOT}:${DOTNET_ROOT}/tools:${PATH}
# Pin the exact SDK archives and verify the SHA512 values from Microsoft's
# release metadata before extracting them.
RUN arch="$(dpkg --print-architecture)" \
    && mkdir -p "${DOTNET_ROOT}" \
    && case "$arch" in \
         amd64) dotnet_arch=x64; sha8="${DOTNET8_X64_SHA512}"; sha9="${DOTNET9_X64_SHA512}" ;; \
         arm64) dotnet_arch=arm64; sha8="${DOTNET8_ARM64_SHA512}"; sha9="${DOTNET9_ARM64_SHA512}" ;; \
         *) echo "unsupported arch: $arch" && exit 1 ;; \
       esac \
    && for spec in "${DOTNET8_VERSION}:${sha8}" "${DOTNET9_VERSION}:${sha9}"; do \
         version="${spec%%:*}"; sha="${spec#*:}"; \
         archive="dotnet-sdk-${version}-linux-${dotnet_arch}.tar.gz"; \
         curl -fsSLo "/tmp/${archive}" "https://builds.dotnet.microsoft.com/dotnet/Sdk/${version}/${archive}"; \
         echo "${sha}  /tmp/${archive}" | sha512sum --check -; \
         tar -xzf "/tmp/${archive}" -C "${DOTNET_ROOT}"; \
         rm "/tmp/${archive}"; \
       done \
    && chmod -R a+rX "${DOTNET_ROOT}"

USER runner
RUN dotnet --list-sdks
