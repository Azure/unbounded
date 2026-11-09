#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0
#
# Synchronized image pull benchmark.
#
# Usage: run.sh <label> <image-ref>
#
# Starts a DaemonSet (gantry-bench/bench-pull) on the target nodes. Each pod
# samples NIC, disk, CPU, memory and gantry/containerd process stats every
# INTERVAL seconds, waits for a shared START_AT epoch, runs "crictl pull", and
# keeps sampling for TAIL_SECONDS. A sampler pod on the origin node records
# origin NIC traffic. Gantry /metrics are snapshotted before and after.
# Raw output goes to tmp/gantry-cluster-bench/<label>/, then the summarizer
# writes summary.md, summary.json and completion.csv there.
#
# Env:
#   NODES=...           node subset: count ("3") or names (comma/space)
#   START_DELAY=120     seconds from DaemonSet creation to START_AT
#   MAX_WAIT=3600       pull timeout per node (seconds)
#   TAIL_SECONDS=15     sampling after the pull returns
#   INTERVAL=1          sampling interval (seconds)
#   NIC=eth0 DISK=nvme0n1
#   FORCE=1             overwrite an existing <label> directory
set -euo pipefail

# shellcheck source-path=SCRIPTDIR
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

label="${1:?usage: run.sh <label> <image-ref>}"
image="${2:?usage: run.sh <label> <image-ref>}"
[[ "${label}" =~ ^[A-Za-z0-9._-]+$ ]] || die "label must match [A-Za-z0-9._-]+"

START_DELAY="${START_DELAY:-120}"
MAX_WAIT="${MAX_WAIT:-3600}"
TAIL_SECONDS="${TAIL_SECONDS:-15}"
INTERVAL="${INTERVAL:-1}"
NIC="${NIC:-eth0}"
DISK="${DISK:-nvme0n1}"

out="${WORK_DIR}/${label}"
if [[ -e "${out}" ]]; then
  [[ "${FORCE:-0}" == 1 ]] || die "${out} exists (set FORCE=1 to overwrite)"
  rm -rf "${out}"
fi
mkdir -p "${out}/pods" "${out}/metrics-pre" "${out}/metrics-post"

PULL_DS=bench-pull
ORIGIN_DS=bench-origin-sampler

cleanup() {
  ds_delete "${PULL_DS}"
  ds_delete "${ORIGIN_DS}"
}
trap cleanup EXIT

mapfile -t nodes < <(target_nodes)
((${#nodes[@]} > 0)) || die "no target nodes"

info="$("${BENCH_DIR}/registry.sh" info "${image}")" || die "cannot inspect ${image} in the origin registry"
read -r image_digest image_bytes <<<"${info}"
log "${label}: ${image} ${image_digest} ${image_bytes} bytes on ${#nodes[@]} nodes"

# _snap_metrics <dir> <pod>
_snap_dir=""
# shellcheck disable=SC2317 # invoked via for_each_parallel
_snap_one() {
  kt 60 get --raw "/api/v1/namespaces/${GANTRY_NS}/pods/$1:9095/proxy/metrics" >"${_snap_dir}/$1.txt" 2>/dev/null ||
    { rm -f "${_snap_dir}/$1.txt"; log "metrics $1 failed"; }
}
snap_metrics() {
  _snap_dir="$1"
  k -n "${GANTRY_NS}" get pods -o name 2>/dev/null | sed 's|^pod/||' | grep -E '^gantry' | for_each_parallel 48 _snap_one || true
  log "metrics snapshot: $(find "$1" -name '*.txt' | wc -l) gantry pods -> $1"
}

for n in "${ORIGIN_DS}" "${PULL_DS}"; do ds_delete "${n}"; done
k -n "${BENCH_NS}" wait --for=delete pod -l "app in (${ORIGIN_DS},${PULL_DS})" --timeout=120s >/dev/null 2>&1 || true

common_env="$(env_entry NIC "${NIC}")
$(env_entry DISK "${DISK}")
$(env_entry INTERVAL "${INTERVAL}")"

ds_apply "${ORIGIN_DS}" sample "${common_env}" "${REGISTRY_NODE}"

start_at=$((EPOCHSECONDS + START_DELAY))
{
  printf 'LABEL=%s\nIMAGE=%s\nIMAGE_DIGEST=%s\nIMAGE_BYTES=%s\n' "${label}" "${image}" "${image_digest}" "${image_bytes}"
  printf 'START_AT=%s\nTAIL_SECONDS=%s\nMAX_WAIT=%s\nINTERVAL=%s\n' "${start_at}" "${TAIL_SECONDS}" "${MAX_WAIT}" "${INTERVAL}"
  printf 'ORIGIN=%s\nREGISTRY_NODE=%s\nTARGET_NODES=%s\n' "$(origin_addr)" "${REGISTRY_NODE}" "${nodes[*]}"
  printf 'GANTRY_IMAGES=%s\n' "$(k -n "${GANTRY_NS}" get ds -o jsonpath='{range .items[*]}{.spec.template.spec.containers[*].image}{" "}{end}' 2>/dev/null || true)"
  printf 'CREATED=%s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)"
} >"${out}/meta.env"

ds_apply "${PULL_DS}" pull "${common_env}
$(env_entry IMAGE "${image}")
$(env_entry START_AT "${start_at}")
$(env_entry TAIL_SECONDS "${TAIL_SECONDS}")
$(env_entry PULL_TIMEOUT "${MAX_WAIT}")" "${nodes[@]}"

ds_wait_running "${PULL_DS}" "${#nodes[@]}" $((START_DELAY - 20)) ||
  log "WARNING: not all pods Running 20s before START_AT; late pods start late (their own start_ns is recorded)"
ds_wait_running "${ORIGIN_DS}" 1 60 || log "WARNING: origin sampler not Running"

snap_metrics "${out}/metrics-pre"
"${BENCH_DIR}/registry.sh" egress >"${out}/origin-pre.txt" || log "origin egress pre failed"
# Pulls must not begin before the baselines exist, or origin traffic and
# metrics are undercounted. Abort rather than record a skewed run.
((EPOCHSECONDS < start_at)) ||
  die "baseline capture finished $((EPOCHSECONDS - start_at))s after START_AT; increase START_DELAY (now ${START_DELAY}) and rerun"
log "START_AT in $((start_at - EPOCHSECONDS))s"

rc=0
ds_wait_done "${PULL_DS}" $((start_at - EPOCHSECONDS + MAX_WAIT + TAIL_SECONDS + 120)) || rc=1

"${BENCH_DIR}/registry.sh" egress >"${out}/origin-post.txt" || log "origin egress post failed"
snap_metrics "${out}/metrics-post"
ds_collect "${PULL_DS}" "${out}/pods"
ds_collect "${ORIGIN_DS}" "${out}/origin-tmp"
mv "${out}/origin-tmp/"*.log "${out}/origin.log" 2>/dev/null || true
rmdir "${out}/origin-tmp" 2>/dev/null || true
cleanup

log "raw results in ${out}"
"$(build_tool)" summarize "${out}" || rc=1
exit "${rc}"
