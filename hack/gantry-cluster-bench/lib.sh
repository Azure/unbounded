#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0
#
# Shared settings and helpers for hack/gantry-cluster-bench. Source it.

# shellcheck disable=SC2034 # variables are consumed by the sourcing scripts

BENCH_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${BENCH_DIR}/../.." && pwd)"

# Uses the ambient KUBECONFIG / current context. Every setting below can be
# overridden from the environment; see README.md.
BENCH_NS="${BENCH_NS:-gantry-bench}"
# Namespace of the operator-managed gantry and racer DaemonSets.
GANTRY_NS="${GANTRY_NS:-unbounded-system}"
# Node that hosts the origin registry. It should not be a pull target.
REGISTRY_NODE="${REGISTRY_NODE:-}"
REGISTRY_PORT="${REGISTRY_PORT:-5005}"
# Label selector for pull target nodes. Empty selects every node.
TARGET_SELECTOR="${TARGET_SELECTOR:-}"
# Image used by the bench pods; pre-pull it on every node.
TOOLS_IMAGE="${TOOLS_IMAGE:-docker.io/library/ubuntu:24.04}"
WORK_DIR="${WORK_DIR:-${REPO_ROOT}/tmp/gantry-cluster-bench}"
KTIMEOUT="${KTIMEOUT:-300}"

log() { printf '%s %s\n' "$(date -u +%H:%M:%S)" "$*" >&2; }
die() { log "ERROR: $*"; exit 1; }

# k runs kubectl with a hard timeout (SIGTERM, then SIGKILL 10s later).
k() { timeout -s TERM -k 10 "${KTIMEOUT}" kubectl "$@"; }

# kt runs kubectl with an explicit timeout in seconds.
kt() { local t="$1"; shift; timeout -s TERM -k 10 "${t}" kubectl "$@"; }

node_ip() {
  k get node "$1" -o jsonpath='{.status.addresses[?(@.type=="InternalIP")].address}'
}

# origin_addr prints host:port of the origin registry.
origin_addr() {
  if [[ -n "${ORIGIN:-}" ]]; then
    printf '%s\n' "${ORIGIN}"
    return
  fi

  local ip
  require_registry_node
  ip="$(node_ip "${REGISTRY_NODE}")"
  [[ -n "${ip}" ]] || die "cannot resolve IP of ${REGISTRY_NODE}"
  printf '%s:%s\n' "${ip}" "${REGISTRY_PORT}"
}

require_registry_node() {
  [[ -n "${REGISTRY_NODE}" ]] || die "set REGISTRY_NODE to the node that hosts the origin registry"
}

# gantry_pods prints the names of the gantry agent pods.
gantry_pods() {
  k -n "${GANTRY_NS}" get pods -l app.kubernetes.io/name=gantry,app.kubernetes.io/component=agent \
    -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}'
}

registry_pod() {
  k -n "${BENCH_NS}" get pod -l app=origin-registry --field-selector=status.phase=Running \
    -o jsonpath='{.items[0].metadata.name}'
}

# target_nodes prints Ready nodes matching TARGET_SELECTOR, optionally
# filtered by NODES (space or comma separated names, or a count "N" meaning
# the first N Ready nodes).
target_nodes() {
  local all
  local sel=()
  [[ -z "${TARGET_SELECTOR}" ]] || sel=(-l "${TARGET_SELECTOR}")
  all="$(k get nodes "${sel[@]}" -o jsonpath='{range .items[*]}{.metadata.name}{" "}{range .status.conditions[?(@.type=="Ready")]}{.status}{end}{"\n"}{end}' |
    awk '$2 == "True" {print $1}' | sort)"

  if [[ -z "${NODES:-}" ]]; then
    printf '%s\n' "${all}"
  elif [[ "${NODES}" =~ ^[0-9]+$ ]]; then
    printf '%s\n' "${all}" | head -n "${NODES}"
  else
    local n
    for n in ${NODES//,/ }; do
      grep -qx -- "${n}" <<<"${all}" || die "node ${n} is not a Ready target node (TARGET_SELECTOR=${TARGET_SELECTOR})"
      printf '%s\n' "${n}"
    done
  fi
}

# nodes_yaml_list prints the target nodes as an inline YAML list.
nodes_yaml_list() {
  local first=1 n
  printf '['
  for n in "$@"; do
    if ((first)); then first=0; else printf ', '; fi
    printf '"%s"' "${n}"
  done
  printf ']'
}

# build_tool builds the static gantry-cluster-bench helper and prints its path.
build_tool() {
  local out="${WORK_DIR}/bin/gantry-cluster-bench"
  mkdir -p "$(dirname "${out}")"
  (cd "${REPO_ROOT}" && CGO_ENABLED=0 GOTOOLCHAIN="${GOTOOLCHAIN:-go1.26.10}" \
    timeout -s TERM 300 go build -trimpath -o "${out}" ./hack/cmd/gantry-cluster-bench) >&2
  printf '%s\n' "${out}"
}

# for_each_parallel <max> <func> reads items from stdin and runs
# "<func> <item>" with at most <max> jobs in flight.
for_each_parallel() {
  local max="$1" fn="$2" item
  while read -r item; do
    [[ -n "${item}" ]] || continue
    while (($(jobs -rp | wc -l) >= max)); do wait -n || true; done
    "${fn}" "${item}" &
  done
  wait || true
}

# ds_apply <name> <mode> <env-yaml> <node>... creates the node.sh ConfigMap
# and a DaemonSet running node.sh in MODE <mode> on exactly the given nodes.
# <env-yaml> is extra container env entries, already indented by 12 spaces.
# render_manifest <file>: print a bench manifest with ${BENCH_NS},
# ${REGISTRY_NODE} and ${REGISTRY_PORT} substituted.
render_manifest() {
  sed -e "s|\${BENCH_NS}|${BENCH_NS}|g" \
    -e "s|\${REGISTRY_NODE}|${REGISTRY_NODE:-}|g" \
    -e "s|\${REGISTRY_PORT}|${REGISTRY_PORT:-}|g" "$1"
}

ds_apply() {
  local name="$1" mode="$2" envy="$3"
  shift 3
  (($# > 0)) || die "ds_apply: no nodes"

  render_manifest "${BENCH_DIR}/manifests/namespace.yaml" | k apply -f - >/dev/null
  k -n "${BENCH_NS}" create configmap bench-node-script --from-file=node.sh="${BENCH_DIR}/node.sh" \
    --dry-run=client -o yaml | k apply -f - >/dev/null

  k apply -f - >/dev/null <<EOF2
apiVersion: apps/v1
kind: DaemonSet
metadata:
  name: ${name}
  namespace: ${BENCH_NS}
  labels:
    app: ${name}
    gantry-cluster-bench: "true"
spec:
  selector:
    matchLabels:
      app: ${name}
  updateStrategy:
    type: OnDelete
  template:
    metadata:
      labels:
        app: ${name}
        gantry-cluster-bench: "true"
    spec:
      hostNetwork: true
      hostPID: true
      dnsPolicy: ClusterFirstWithHostNet
      terminationGracePeriodSeconds: 2
      tolerations: []
      affinity:
        nodeAffinity:
          requiredDuringSchedulingIgnoredDuringExecution:
            nodeSelectorTerms:
              - matchExpressions:
                  - key: kubernetes.io/hostname
                    operator: In
                    values: $(nodes_yaml_list "$@")
      containers:
        - name: node
          image: ${TOOLS_IMAGE}
          imagePullPolicy: IfNotPresent
          command: ["bash", "/script/node.sh"]
          securityContext:
            privileged: true
          env:
            - name: NODE_NAME
              valueFrom:
                fieldRef:
                  fieldPath: spec.nodeName
            - name: MODE
              value: "${mode}"
${envy}
          resources:
            requests:
              cpu: 10m
              memory: 32Mi
            limits:
              cpu: "1"
              memory: 256Mi
          volumeMounts:
            - name: script
              mountPath: /script
      volumes:
        - name: script
          configMap:
            name: bench-node-script
EOF2
}

# env_entry <name> <value> prints one env list entry at DaemonSet indentation.
env_entry() {
  printf '            - name: %s\n              value: "%s"\n' "$1" "$2"
}

# ds_pods <name> prints "pod node" lines.
ds_pods() {
  k -n "${BENCH_NS}" get pods -l "app=$1" \
    -o jsonpath='{range .items[*]}{.metadata.name}{" "}{.spec.nodeName}{"\n"}{end}'
}

# ds_wait_scheduled <name> <count> <timeout> waits until <count> pods are Running.
ds_wait_running() {
  local name="$1" want="$2" deadline=$((SECONDS + $3)) running
  while :; do
    running="$(k -n "${BENCH_NS}" get pods -l "app=${name}" --field-selector=status.phase=Running -o name | wc -l)"
    ((running >= want)) && return 0
    ((SECONDS < deadline)) || { log "${name}: only ${running}/${want} pods Running"; return 1; }
    sleep 3
  done
}

_pod_done() {
  local pod="${1%% *}" node="${1##* }"
  if kt 60 -n "${BENCH_NS}" logs --tail=2 "${pod}" 2>/dev/null | grep -q '^DONE '; then
    printf '%s\n' "${node}" >>"${_DONE_FILE}"
  fi
}

# ds_wait_done <name> <timeout> polls pod logs until every pod printed DONE.
ds_wait_done() {
  local name="$1" deadline=$((SECONDS + $2)) total ndone
  _DONE_FILE="$(mktemp -p "${WORK_DIR}" done.XXXXXX)"
  while :; do
    : >"${_DONE_FILE}"
    total="$(ds_pods "${name}" | grep -c . || true)"
    ds_pods "${name}" | for_each_parallel 48 _pod_done
    ndone="$(sort -u "${_DONE_FILE}" | grep -c . || true)"
    log "${name}: ${ndone}/${total} done"
    if ((total > 0 && ndone >= total)); then rm -f "${_DONE_FILE}"; return 0; fi
    if ((SECONDS >= deadline)); then rm -f "${_DONE_FILE}"; log "${name}: timed out"; return 1; fi
    sleep "${POLL_SECONDS:-15}"
  done
}

_collect_one() {
  local pod="${1%% *}" node="${1##* }"
  kt 120 -n "${BENCH_NS}" logs "${pod}" >"${_COLLECT_DIR}/${node}.log" 2>&1 ||
    log "logs ${pod} (${node}) failed"
}

# ds_collect <name> <dir> saves each pod's log as <dir>/<node>.log.
ds_collect() {
  _COLLECT_DIR="$2"
  mkdir -p "${_COLLECT_DIR}"
  ds_pods "$1" | for_each_parallel 48 _collect_one
}

ds_delete() {
  k -n "${BENCH_NS}" delete ds "$1" --ignore-not-found --wait=false >/dev/null
}

# ds_oneshot <name> <mode> <env-yaml> runs node.sh on all target nodes, waits
# for DONE, saves logs under WORK_DIR/<name>-<ts>/, and deletes the DaemonSet.
ds_oneshot() {
  local name="$1" mode="$2" envy="$3" out nodes
  mapfile -t nodes < <(target_nodes)
  ((${#nodes[@]} > 0)) || die "no target nodes"
  out="${WORK_DIR}/${name}-$(date -u +%Y%m%dT%H%M%SZ)"
  mkdir -p "${out}"
  log "${name}: ${#nodes[@]} nodes, logs in ${out}"
  ds_delete "${name}"
  k -n "${BENCH_NS}" wait --for=delete pod -l "app=${name}" --timeout=120s >/dev/null 2>&1 || true
  ds_apply "${name}" "${mode}" "${envy}" "${nodes[@]}"
  local rc=0
  ds_wait_running "${name}" "${#nodes[@]}" 300 || rc=1
  ds_wait_done "${name}" "${ONESHOT_TIMEOUT:-900}" || rc=1
  ds_collect "${name}" "${out}"
  ds_delete "${name}"
  printf '%s\n' "${out}"
  return "${rc}"
}
