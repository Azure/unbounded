#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0
#
# Build gantry from a source tree, package it like images/gantry/Containerfile
# (ubuntu:noble, /unbounded/bin/gantry, ENTRYPOINT gantry agent), and push it
# to the in-cluster origin registry through kubectl port-forward. Docker
# treats localhost registries as insecure, so plain HTTP works without
# daemon configuration. Prints the in-cluster image reference.
#
# Usage: build-gantry-image.sh <source-tree> <tag>
#   e.g. build-gantry-image.sh ../.. dev-1
# Env:   LOCAL_PORT=15005   local end of the port-forward
set -euo pipefail

# shellcheck source-path=SCRIPTDIR
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

src="$(cd "${1:?usage: build-gantry-image.sh <source-tree> <tag>}" && pwd)"
tag="${2:?usage: build-gantry-image.sh <source-tree> <tag>}"
LOCAL_PORT="${LOCAL_PORT:-15005}"

ctx="${WORK_DIR}/gantry-image"
rm -rf "${ctx}"
mkdir -p "${ctx}"

log "building gantry from ${src}"
(cd "${src}" && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 GOTOOLCHAIN="${GOTOOLCHAIN:-go1.26.10}" \
  timeout -s TERM 600 go build -trimpath -ldflags "-s -w" -o "${ctx}/gantry" ./cmd/gantry)
cp "${src}/LICENSE" "${src}/NOTICE" "${ctx}/"

cat >"${ctx}/Containerfile" <<'EOF2'
FROM ubuntu:noble
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates \
    && apt-get clean && rm -rf /var/lib/apt/lists/*
RUN mkdir -p /unbounded/bin
COPY gantry /unbounded/bin/gantry
COPY LICENSE NOTICE /unbounded/licenses/
ENV PATH="/unbounded/bin:${PATH}"
WORKDIR /unbounded
EXPOSE 5000 5001 9095
ENTRYPOINT ["/unbounded/bin/gantry"]
CMD ["agent"]
EOF2

local_ref="localhost:${LOCAL_PORT}/gantry:${tag}"
timeout -s TERM 900 docker build -q -f "${ctx}/Containerfile" -t "${local_ref}" "${ctx}" >/dev/null

pod="$(registry_pod)"
[[ -n "${pod}" ]] || die "origin registry pod not running (registry.sh up)"
kubectl -n "${BENCH_NS}" port-forward "pod/${pod}" "${LOCAL_PORT}:${REGISTRY_PORT}" >/dev/null 2>&1 &
pf=$!
trap 'kill "${pf}" 2>/dev/null || true' EXIT
for _ in $(seq 1 30); do
  curl -fsS "http://localhost:${LOCAL_PORT}/v2/" >/dev/null 2>&1 && break
  sleep 1
done

timeout -s TERM 900 docker push -q "${local_ref}" >&2
digest="$(curl -fsSI -H 'Accept: application/vnd.docker.distribution.manifest.v2+json' \
  -H 'Accept: application/vnd.oci.image.manifest.v1+json' \
  "http://localhost:${LOCAL_PORT}/v2/gantry/manifests/${tag}" | awk -F': ' 'tolower($1) == "docker-content-digest" {print $2}' | tr -d '\r')"
docker rmi "${local_ref}" >/dev/null 2>&1 || true

log "pushed gantry:${tag} ${digest}"
printf '%s/gantry:%s\n' "$(origin_addr)" "${tag}"
