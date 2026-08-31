# Linux "native-build" flavor: the build substrate.
# Adds the C/C++ toolchain + python3 that node-gyp, native Python wheels, Rust's
# linker, and most ./configure scripts need. Also the FROM base for node/go/rust.
#
# The -dev packages below mirror what actions/runner-images installs on
# ubuntu-latest, so a job that compiles a native module here behaves the same as
# it does on a GitHub-hosted runner. Without unixodbc-dev, for example, node-gyp
# builds of msnodesqlv8 die on "fatal error: sql.h: No such file or directory".
#
#   docker build -f images/linux/flavors/native-build.Dockerfile \
#     --build-arg PARENT=multirunner/runner-linux:minimal -t multirunner/runner-linux-native-build:dev .
ARG PARENT=gerardsmit/multirunner-runner-linux:minimal
FROM ${PARENT}

USER root
ENV DEBIAN_FRONTEND=noninteractive
RUN apt-get update -y && apt-get install -y --no-install-recommends \
      build-essential cmake ninja-build pkg-config \
      python3 python3-dev python3-pip python3-venv \
      autoconf automake libtool m4 bison flex swig dpkg-dev \
      libssl-dev libicu-dev libsqlite3-dev libyaml-dev \
      unixodbc-dev libpq-dev \
    && rm -rf /var/lib/apt/lists/*

USER runner
