# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

# Combined Dockerfile for unbounded-net-controller and unbounded-net-node.
# Builds natively on each target architecture (cross-compilation not
# attempted because CGO is enabled).

# =============================================================================
# Build stage - builds natively on each target architecture.
# Upstream Go image on Debian bookworm. GO_VERSION is parsed from go.mod
# (major.minor) in CI.
# =============================================================================
ARG GO_VERSION=1.26
FROM docker.io/library/golang:${GO_VERSION}-bookworm AS builder

# Standard Go images set GOTOOLCHAIN=auto by default, which lets `go` switch
# to the toolchain version required by go.mod if it is newer than the image.

# Install build dependencies (Go is provided by the base image).
# bpftool is copied into the runtime image since Ubuntu 24.04 no longer
# packages it as a standalone binary.
RUN apt-get update \
    && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
        ca-certificates git make curl tar \
        binutils gcc libc6-dev linux-libc-dev \
        pkg-config libcap-dev libssl-dev \
        clang lld libbpf-dev bpftool \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /src

# Platform args (provided by buildx, stable values)
ARG TARGETOS
ARG TARGETARCH

# Software version args
ARG CNI_PLUGINS_VERSION=v1.9.1

# Pre-downloaded build assets from repository resources/
COPY resources/ /resources/

# Download CNI plugins for target architecture (cached unless version changes)
RUN mkdir -p /cni-plugins && \
    file="/resources/cni-plugins-${TARGETOS}-${TARGETARCH}-${CNI_PLUGINS_VERSION}.tgz"; \
    if [ ! -s "$file" ]; then \
        echo "Missing CNI plugins archive: $file"; \
        exit 1; \
    fi; \
    tar -xz -C /cni-plugins -f "$file"

# Copy go mod files first for better caching
COPY go.mod go.sum ./

# Download dependencies (cached in /go/pkg/mod, shared across architectures)
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

# Build ntttcp for network performance testing (retry up to 3 times for transient network errors)
RUN for i in 1 2 3; do \
        git clone --depth 1 https://github.com/microsoft/ntttcp-for-linux.git /tmp/ntttcp-for-linux && break; \
        echo "git clone attempt $i failed, retrying in 10s..."; \
        rm -rf /tmp/ntttcp-for-linux; \
        [ "$i" = "3" ] && exit 1; \
        sleep 10; \
    done \
    && cd /tmp/ntttcp-for-linux/src \
    && make \
    && cp ntttcp /usr/local/bin/ntttcp \
    && rm -rf /tmp/ntttcp-for-linux

# Copy source code (this layer changes frequently)
COPY . .

# Copy frontend dist files from the build output in the context
COPY internal/net/html/dist/ /src/internal/net/html/dist/

# Compile eBPF programs (target bpf is architecture-independent)
RUN clang -O2 -g -target bpf \
    -D__TARGET_ARCH_${TARGETARCH} \
    -I/usr/include \
    -c bpf/unbounded_encap.c \
    -o internal/net/ebpf/unbounded_encap_bpfel.o

# Build all binaries with CGO enabled (needed for runtime deps such as
# sqlite). Cache mounts persist the Go module cache and per-arch build
# cache across builds.
ARG VERSION=dev
ARG GIT_COMMIT=unknown
ARG BUILD_TIME=unknown
ENV STAMP_LDFLAGS="-s -w \
    -X github.com/Azure/unbounded/internal/version.Version=${VERSION} \
    -X github.com/Azure/unbounded/internal/version.GitCommit=${GIT_COMMIT} \
    -X github.com/Azure/unbounded/internal/version.BuildTime=${BUILD_TIME}"
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build,id=go-build-${TARGETARCH} \
    CGO_ENABLED=1 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build \
    -ldflags="${STAMP_LDFLAGS}" \
    -o /unbounded-net-controller \
    ./cmd/unbounded-net-controller && \
    CGO_ENABLED=1 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build \
    -ldflags="${STAMP_LDFLAGS}" \
    -o /unbounded-net-node \
    ./cmd/unbounded-net-node && \
    CGO_ENABLED=1 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build \
    -ldflags="${STAMP_LDFLAGS}" \
    -o /unping \
    ./cmd/unping && \
    CGO_ENABLED=1 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build \
    -ldflags="${STAMP_LDFLAGS}" \
    -o /unroute \
    ./cmd/unroute

# =============================================================================
# Shared runtime base image - contains all packages needed by both images.
# This is the only stage that runs under QEMU for arm64.
# =============================================================================
FROM docker.io/library/ubuntu:24.04 AS runtime-base

# Install all packages needed by both controller and node in a single apt call.
# - ca-certificates: TLS support (both)
# - passwd: shadow-utils equivalent for groupadd/useradd (both)
# - wireguard-tools: WireGuard support (node)
# - iproute2: ip command (node)
# - iptables: iptables/ip6tables for masquerade rules (node)
# - util-linux: nsenter for host namespace sysctl setup (node init)
# - hostname: hostname binary
# - bpftool: eBPF map/program inspection (node)
RUN apt-get update \
    && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
        ca-certificates passwd iproute2 iptables util-linux hostname \
        wget tcpdump iputils-ping \
        libatomic1 libcap2 openssl vim \
        procps less libsqlite3-0 tar gzip jq python3 dnsutils \
        wireguard-tools \
    && rm -rf /var/lib/apt/lists/*

# Copy bpftool from the build image (Ubuntu 24.04 does not package it
# as a standalone binary).
COPY --from=builder /usr/sbin/bpftool /usr/sbin/bpftool

# Ensure /sbin and /usr/sbin are in PATH for ip command
ENV PATH="/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
ENV PS1='[\u@\h \W]\$ '

# =============================================================================
# Controller runtime image
# =============================================================================
FROM runtime-base AS controller

LABEL org.opencontainers.image.source="https://github.com/project-unbounded/unbounded-net"
LABEL org.opencontainers.image.description="unbounded-net controller"
LABEL org.opencontainers.image.licenses="MIT"

# Create non-root user and set entrypoint (cached, runs under QEMU once)
RUN groupadd -r controller && useradd -r -g controller controller

USER controller
ENTRYPOINT ["/usr/local/bin/unbounded-net-controller"]

# Copy binary from builder last (changes frequently, but COPY is fast)
COPY --from=builder /unbounded-net-controller /usr/local/bin/unbounded-net-controller
COPY --from=builder /unping /usr/local/bin/unping
COPY --from=builder /unroute /usr/local/bin/unroute

# =============================================================================
# Node runtime image
# =============================================================================
FROM runtime-base AS node

LABEL org.opencontainers.image.source="https://github.com/project-unbounded/unbounded-net"
LABEL org.opencontainers.image.description="unbounded-net node agent"
LABEL org.opencontainers.image.licenses="MIT"

# Set entrypoint
ENTRYPOINT ["/usr/local/bin/unbounded-net-node"]

# Copy init script for installing CNI plugins
COPY images/net-node/node-agent-init.sh /usr/local/bin/node-agent-init.sh
RUN chmod 0755 /usr/local/bin/node-agent-init.sh

# Copy CNI plugins first (rarely changes, cached)
COPY --from=builder /cni-plugins/ /opt/cni/bin/

# Copy ntttcp for network performance testing
COPY --from=builder /usr/local/bin/ntttcp /usr/local/bin/ntttcp

# Copy binary from builder last (changes frequently)
COPY --from=builder /unbounded-net-node /usr/local/bin/unbounded-net-node
COPY --from=builder /unping /usr/local/bin/unping
COPY --from=builder /unroute /usr/local/bin/unroute
