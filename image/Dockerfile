# All stages share these pinned multi-architecture inputs. Update alongside
# image/README.md; snapshot apt still verifies Debian's signed Release files.
ARG DEBIAN=debian:bookworm-slim@sha256:7ff6ba31527b9e336162fa061746c366c4b2032f4d2cff2078bea43f78efe3f8
ARG GO=golang:1.26.3-bookworm@sha256:386d475a660466863d9f8c766fec64d7fdad3edac2c6a05020c09534d71edb4b

FROM --platform=$BUILDPLATFORM ${GO} AS golang

FROM golang AS agent
ARG TARGETARCH
ARG IMAGE_VERSION
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/boxpool-agent ./cmd/boxpool-agent
COPY internal/agent ./internal/agent
RUN CGO_ENABLED=0 GOOS=linux GOARCH=$TARGETARCH go build -trimpath -buildvcs=false \
    -ldflags="-s -w -X main.version=$IMAGE_VERSION" -o /boxpool-agent ./cmd/boxpool-agent

FROM --platform=$BUILDPLATFORM ${DEBIAN} AS tools
COPY --from=golang /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY image/debian.sources /etc/apt/sources.list.d/debian.sources
RUN apt-get -o APT::Update::Error-Mode=any update \
    && apt-get install -y --no-install-recommends ca-certificates curl xz-utils \
       build-essential flex bison libssl-dev libelf-dev bc e2fsprogs zstd \
       gcc-x86-64-linux-gnu gcc-aarch64-linux-gnu \
    && rm -rf /var/lib/apt/lists/*

# The official Node.js build for the target, checked against image/versions.txt.
FROM tools AS node
ARG TARGETARCH
COPY image/versions.txt /versions.txt
RUN . /versions.txt \
    && case $TARGETARCH in amd64) arch=x64 sha=$NODE_SHA256_amd64 ;; arm64) arch=arm64 sha=$NODE_SHA256_arm64 ;; esac \
    && curl -fsSL --retry 3 -o /node.tar.xz "https://nodejs.org/dist/v$NODE_VERSION/node-v$NODE_VERSION-linux-$arch.tar.xz" \
    && echo "$sha  /node.tar.xz" | sha256sum -c - \
    && mkdir -p /node/share/doc/node \
    && tar -xJf /node.tar.xz -C /node --strip-components=1 --no-same-owner \
    && mv /node/LICENSE /node/share/doc/node/ && rm /node/CHANGELOG.md /node/README.md

FROM ${DEBIAN} AS userland
COPY --from=golang /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY image/debian.sources /etc/apt/sources.list.d/debian.sources
RUN apt-get -o APT::Update::Error-Mode=any update \
    && apt-get install -y --no-install-recommends tini ca-certificates mount procps \
    && rm -rf /var/lib/apt/lists/* \
    && mkdir -p /dev/pts /dev/shm /proc /sys /run/boxpool /tmp /var/tmp /root \
    && chmod 1777 /tmp /var/tmp
COPY --from=node /node/ /usr/local/
# Exact versions from image/versions.txt; npm picks each CLI's native binary
# for the target CPU from its optional dependencies.
COPY image/versions.txt /tmp/versions.txt
RUN . /tmp/versions.txt \
    && npm install -g --no-fund --no-audit --no-update-notifier --cache /tmp/npm-cache \
       "@anthropic-ai/claude-code@$CLAUDE_CODE_VERSION" "@openai/codex@$CODEX_VERSION" \
       "@google/gemini-cli@$GEMINI_CLI_VERSION" \
    && rm -rf /tmp/npm-cache /tmp/versions.txt /root/.npm
# Declared after the slow layers: a new version must not reinstall the agents.
ARG IMAGE_VERSION
ARG IMAGE_REVISION
RUN printf '%s\n' "$IMAGE_VERSION" > /etc/boxpool-image
COPY --from=agent /boxpool-agent /usr/local/bin/boxpool-agent
COPY --chmod=0755 image/init /sbin/init
ENV PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
WORKDIR /root
LABEL org.opencontainers.image.source="https://github.com/Sukhendu2002/boxpool" \
      org.opencontainers.image.version=$IMAGE_VERSION \
      org.opencontainers.image.revision=$IMAGE_REVISION
# Docker supplies its own mounts; no VM setup or privileged container needed.
# Replacing CMD supports `cat /etc/boxpool-image` or a custom agent command.
ENTRYPOINT ["/usr/bin/tini", "-g", "--"]
CMD ["/usr/local/bin/boxpool-agent", "--listen", "unix:/run/boxpool/agent.sock"]

FROM tools AS kernel
ARG TARGETARCH
ARG JOBS=4
WORKDIR /build
COPY image/kernel ./config
COPY image/build-kernel.sh ./build-kernel.sh
RUN ./build-kernel.sh "$TARGETARCH" "$JOBS"

FROM tools AS filesystem
ARG TARGETARCH
ARG IMAGE_VERSION
ARG IMAGE_REVISION
COPY --from=userland / /rootfs/
# mke2fs runs as root inside BuildKit, preserving uid/gid/modes, symlinks and
# hardlinks without a loop mount, host sudo, or a privileged build/container.
RUN mkdir /out && truncate -s 2G /out/rootfs-$TARGETARCH.ext4 \
    && mke2fs -q -t ext4 -F -L boxpool -m 0 -d /rootfs /out/rootfs-$TARGETARCH.ext4 \
    && e2fsck -fn /out/rootfs-$TARGETARCH.ext4 \
    && zstd -T2 -10 --rm /out/rootfs-$TARGETARCH.ext4 \
    && printf 'version=%s\nrevision=%s\narch=%s\n' "$IMAGE_VERSION" "$IMAGE_REVISION" "$TARGETARCH" > /out/image-$TARGETARCH.txt

FROM tools AS bundle
ARG TARGETARCH
COPY --from=kernel /out/ /out/
COPY --from=filesystem /out/ /out/
RUN cd /out && sha256sum * > SHA256SUMS-$TARGETARCH

FROM scratch AS artifacts
COPY --from=bundle /out/ /
