#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0
#
# Switch the operator-managed gantry to a bench mode and wait until every
# agent pod runs in that mode.
#
# Usage: deploy.sh <default|racer|adapter> [gantry-image-ref]
#   default  gantry p2p mode (no ClusterCache/gantry)
#   racer    Racer mode, manifests/clustercache-racer.yaml
#   adapter  Racer mode, manifests/clustercache-adapter.yaml
#
# The unbounded operator must already be installed with gantry enabled on the
# Site of the target nodes (the default). This script:
#   1. adds the origin registry to ConfigMap/gantry-config (the original is
#      saved once to WORK_DIR/gantry-config.orig.yaml for undeploy.sh),
#   2. writes the bench key of ConfigMap/unbounded-component-overrides that
#      raises the gantry agent limits and, if given, overrides its image,
#   3. creates, replaces, or deletes ClusterCache/gantry for the mode,
#   4. waits for the gantry DaemonSet to roll out in the requested mode.
#
# Env: GANTRY_CPU_LIMIT=32 GANTRY_MEMORY_LIMIT=48Gi  agent limits
#      GANTRY_SET="peer_fetch_timeout=15m transfer_max_concurrent_serves=10"
#                  space separated top-level gantry config overrides
#      DEPLOY_TIMEOUT=900  seconds to wait for the rollout
set -euo pipefail

# shellcheck source-path=SCRIPTDIR
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

mode="${1:?usage: deploy.sh <default|racer|adapter> [image-ref]}"
image="${2:-}"
case "${mode}" in
  default) cache_manifest="" ;;
  racer) cache_manifest="${BENCH_DIR}/manifests/clustercache-racer.yaml" ;;
  adapter) cache_manifest="${BENCH_DIR}/manifests/clustercache-adapter.yaml" ;;
  *) die "unknown mode ${mode} (want default, racer, or adapter)" ;;
esac

GANTRY_CPU_LIMIT="${GANTRY_CPU_LIMIT:-32}"
GANTRY_MEMORY_LIMIT="${GANTRY_MEMORY_LIMIT:-48Gi}"
GANTRY_SET="${GANTRY_SET:-peer_fetch_timeout=15m transfer_max_concurrent_serves=10}"
DEPLOY_TIMEOUT="${DEPLOY_TIMEOUT:-900}"

mkdir -p "${WORK_DIR}"
origin="$(origin_addr)"
tool="$(build_tool)"

# 1. Origin registry in gantry-config. The operator creates this ConfigMap
# once and never overwrites it.
orig="${WORK_DIR}/gantry-config.orig.yaml"
current="$(k -n "${GANTRY_NS}" get configmap gantry-config -o jsonpath='{.data.config\.yaml}')" ||
  die "ConfigMap ${GANTRY_NS}/gantry-config not found; is gantry enabled in the operator?"
[[ -e "${orig}" ]] || printf '%s\n' "${current}" >"${orig}"

sets=()
for s in ${GANTRY_SET}; do sets+=(--set "${s}"); done
printf '%s\n' "${current}" | "${tool}" gantry-config --origin "${origin}" "${sets[@]}" >"${WORK_DIR}/gantry-config.yaml"
k -n "${GANTRY_NS}" create configmap gantry-config --from-file=config.yaml="${WORK_DIR}/gantry-config.yaml" \
  --dry-run=client -o yaml | k apply -f - >/dev/null
log "gantry-config lists origin ${origin}"

# 2. Workload override: limits and optional image.
image_line=""
[[ -z "${image}" ]] || image_line="                    image: ${image}
                    imagePullPolicy: Always"
cat >"${WORK_DIR}/overrides.yaml" <<EOF2
apiVersion: overrides.unbounded-cloud.io/v1alpha1
overrides:
  - component: gantry
    kind: DaemonSet
    patch:
      spec:
        template:
          spec:
            containers:
              - name: gantry
${image_line}
                resources:
                  limits:
                    cpu: "${GANTRY_CPU_LIMIT}"
                    memory: ${GANTRY_MEMORY_LIMIT}
EOF2
if ! k -n "${GANTRY_NS}" get configmap unbounded-component-overrides >/dev/null 2>&1; then
  k -n "${GANTRY_NS}" create configmap unbounded-component-overrides >/dev/null
fi
k -n "${GANTRY_NS}" create configmap unbounded-component-overrides \
  --from-file=gantry-cluster-bench.yaml="${WORK_DIR}/overrides.yaml" --dry-run=client -o json |
  k -n "${GANTRY_NS}" patch configmap unbounded-component-overrides --type merge --patch-file /dev/stdin >/dev/null

# 3. ClusterCache/gantry selects the gantry backend.
if [[ -z "${cache_manifest}" ]]; then
  k delete clustercache gantry --ignore-not-found --wait=true --timeout=280s >/dev/null
  want_racer=""
else
  # Persistence may not be changeable in place, so recreate on a change.
  if k get clustercache gantry >/dev/null 2>&1 &&
    ! k diff -f "${cache_manifest}" >/dev/null 2>&1; then
    log "replacing ClusterCache/gantry"
    k delete clustercache gantry --wait=true --timeout=280s >/dev/null
  fi
  k apply -f "${cache_manifest}" >/dev/null
  want_racer="true"
fi
printf '%s\n' "${mode}" >"${WORK_DIR}/mode"

# 4. Wait until the operator rendered the DaemonSet for this mode and it rolled out.
deadline=$((SECONDS + DEPLOY_TIMEOUT))
jp='{.spec.template.spec.containers[?(@.name=="gantry")].env[?(@.name=="GANTRY_RACER_ENABLED")].value}'
while :; do
  got="$(k -n "${GANTRY_NS}" get ds gantry -o jsonpath="${jp}" 2>/dev/null || true)"
  img="$(k -n "${GANTRY_NS}" get ds gantry -o jsonpath='{.spec.template.spec.containers[?(@.name=="gantry")].image}' 2>/dev/null || true)"
  if [[ "${got}" == "${want_racer}" && (-z "${image}" || "${img}" == "${image}") ]]; then
    break
  fi
  ((SECONDS < deadline)) || die "gantry DaemonSet not in mode ${mode} (GANTRY_RACER_ENABLED='${got}', image ${img})"
  sleep 5
done
if [[ -n "${want_racer}" ]]; then
  kt "${DEPLOY_TIMEOUT}" -n "${GANTRY_NS}" rollout status ds/racer --timeout="$((DEPLOY_TIMEOUT - 10))s"
fi
kt "${DEPLOY_TIMEOUT}" -n "${GANTRY_NS}" rollout status ds/gantry --timeout="$((DEPLOY_TIMEOUT - 10))s"

want="$(target_nodes | grep -c . || true)"
ready="$(k -n "${GANTRY_NS}" get ds gantry -o jsonpath='{.status.numberReady}')"
log "gantry mode ${mode} ready on ${ready} nodes (target ${want})"
((ready >= want)) || die "gantry not Ready on all target nodes"
