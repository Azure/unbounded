# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0

FROM --platform=$BUILDPLATFORM docker.io/library/golang:1.26.6-trixie AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
ARG GIT_COMMIT=unknown
ARG BUILD_TIME=unknown
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath \
    -ldflags "-X github.com/Azure/unbounded/internal/version.Version=${VERSION} -X github.com/Azure/unbounded/internal/version.GitCommit=${GIT_COMMIT} -X github.com/Azure/unbounded/internal/version.BuildTime=${BUILD_TIME}" \
    -o /out/racer-object ./cmd/racer-object

FROM docker.io/library/debian:trixie-slim
ARG VERSION=dev
ARG GIT_COMMIT=unknown
LABEL org.opencontainers.image.title="racer-object" \
      org.opencontainers.image.licenses="Apache-2.0" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${GIT_COMMIT}"
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates \
    && rm -rf /var/lib/apt/lists/*
COPY --from=builder /out/racer-object /usr/local/bin/racer-object
COPY LICENSE NOTICE /usr/share/licenses/racer-object/
# Racer's node-local sockets are root-owned with mode 0600.
USER 0:0
ENTRYPOINT ["/usr/local/bin/racer-object"]
