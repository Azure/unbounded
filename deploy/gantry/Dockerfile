# syntax=docker/dockerfile:1.7
#
# Multi-arch container image for the gantry agent.
#
# §Phase 6:
#   - Multi-stage build: golang:alpine for compile, distroless static
#     for runtime. The runtime layer is roughly ~3 MB and contains
#     only the agent binary + CA roots; libp2p Noise (the wire-layer
#     crypto) is statically linked into the binary so no runtime
#     OpenSSL or libsodium is required.
#   - hostNetwork is disabled in the DaemonSet (§7.5); the agent
#     binds 5000/5001/9095/libp2p inside the pod and uses Service /
#     direct pod-IP discovery for peering.
#   - No shell, no package manager - distroless static is the minimal
#     usable base for a static Go binary.
#
# Build args:
#   GO_VERSION       - pinned to match go.mod's `go` directive.
#   GANTRY_VERSION   - value embedded into the binary's version
#                      string via -ldflags. Defaults to "dev".
#   TARGETPLATFORM   - provided by buildx; the build stage cross-
#                      compiles to GOOS/GOARCH derived from it.
#
# Tag policy: deploy/build.sh computes the tag from `git describe`.

ARG GO_VERSION=1.26.3

FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-alpine AS build
ARG TARGETOS
ARG TARGETARCH
ARG GANTRY_VERSION=dev

WORKDIR /src

# Prime the module cache as its own layer so dependency-only changes
# don't trigger a full source rebuild.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    go mod download

COPY . .

# Static, stripped, reproducible-ish build. CGO is disabled so the
# binary works under distroless static; -trimpath strips local paths
# so two builds from the same source produce byte-identical output
# (modulo timestamps embedded by Go's buildinfo).
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
        go build -trimpath \
            -ldflags="-s -w -X main.version=${GANTRY_VERSION}" \
            -o /out/gantry ./cmd/gantry

# Distroless static is the smallest standard base for static Go
# binaries. It includes ca-certificates, /etc/passwd with a `nonroot`
# user, and an empty /tmp; nothing else. No shell, no libc.
FROM gcr.io/distroless/static-debian12:nonroot

LABEL org.opencontainers.image.title="gantry"
LABEL org.opencontainers.image.description="P2P container-image distributor (agent)"
LABEL org.opencontainers.image.licenses="Apache-2.0"
LABEL org.opencontainers.image.source="https://github.com/vpatelsj/gantry"

# Runtime state volume. The DaemonSet mounts a hostPath here for libp2p
# identity persistence; image/blob content lives in containerd's content
# store, not in a Gantry-owned cache directory.
VOLUME ["/var/lib/gantry"]

# Mirror, transfer, libp2p, metrics - see deploy/daemonset.yaml.
# The libp2p listen ports are operator-configurable; the defaults
# below match config.NewDefault().
EXPOSE 5000/tcp
EXPOSE 5001/tcp
EXPOSE 9095/tcp

COPY --from=build /out/gantry /usr/local/bin/gantry

USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/gantry"]
