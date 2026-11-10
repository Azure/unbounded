#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0
#
# Undo deploy.sh: delete ClusterCache/gantry, drop the bench workload
# override, and restore the saved ConfigMap/gantry-config. Gantry stays
# installed (the operator owns it) and returns to default p2p mode.
#
# Env: KEEP_CLUSTERCACHE=1  leave ClusterCache/gantry in place
set -euo pipefail

# shellcheck source-path=SCRIPTDIR
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

if [[ "${KEEP_CLUSTERCACHE:-0}" != 1 ]]; then
  k delete clustercache gantry --ignore-not-found --wait=true --timeout=280s || true
fi

k -n "${GANTRY_NS}" patch configmap unbounded-component-overrides --type json \
  -p '[{"op":"remove","path":"/data/gantry-cluster-bench.yaml"}]' >/dev/null 2>&1 || true

orig="${WORK_DIR}/gantry-config.orig.yaml"
if [[ -e "${orig}" ]]; then
  k -n "${GANTRY_NS}" create configmap gantry-config --from-file=config.yaml="${orig}" \
    --dry-run=client -o yaml | k apply -f - >/dev/null
  rm -f "${orig}"
  log "restored gantry-config"
fi
rm -f "${WORK_DIR}/mode"

kt 900 -n "${GANTRY_NS}" rollout status ds/gantry --timeout=880s || true
