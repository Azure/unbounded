#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0
#
# Install or upgrade gantry from deploy/gantry/chart for the bench and wait
# for the agent DaemonSet to be Ready on every target node.
#
# Usage: deploy.sh <values-file> <image-ref> [extra helm args...]
#   ORIGIN_ADDR in the values file is replaced by the origin host:port.
set -euo pipefail

# shellcheck source-path=SCRIPTDIR
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

values="${1:?usage: deploy.sh <values-file> <image-ref> [helm args]}"
image="${2:?usage: deploy.sh <values-file> <image-ref> [helm args]}"
shift 2

origin="$(origin_addr)"
mkdir -p "${WORK_DIR}"
rendered="${WORK_DIR}/values-rendered.yaml"
sed "s/ORIGIN_ADDR/${origin}/g" "${values}" >"${rendered}"

want="$(target_nodes | grep -c .)"
log "deploying gantry ${image} (${values}) to ${GANTRY_NS}, expecting ${want} pods"

timeout -s TERM -k 10 600 helm upgrade --install gantry "${REPO_ROOT}/deploy/gantry/chart" \
  --namespace "${GANTRY_NS}" --create-namespace \
  -f "${rendered}" --set image.reference="${image}" --set image.pullPolicy=Always \
  --timeout 9m "$@"

kt 900 -n "${GANTRY_NS}" rollout status ds/gantry --timeout=880s
ready="$(k -n "${GANTRY_NS}" get ds gantry -o jsonpath='{.status.numberReady}')"
log "gantry ready on ${ready} nodes (target ${want})"
((ready >= want)) || die "gantry not Ready on all target nodes"
