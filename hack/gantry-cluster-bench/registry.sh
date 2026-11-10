#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0
#
# Manage the in-cluster origin registry used by the gantry cluster bench.
#
# Usage: registry.sh <command> [args]
#   up                         deploy the registry on REGISTRY_NODE and wait
#   down                       delete the registry Deployment (data is kept)
#   addr                       print the origin host:port
#   install-tool               copy the gantry-cluster-bench helper into the pod
#   gen <tag> [layers] [size] [repo]
#                              generate and push a synthetic image
#                              (default 4 x 10Gi into bench/g40)
#   info <host:port/repo:tag>  print "<digest> <total-bytes>" of an image
#   delete <tag> [repo]        delete a manifest (then run gc)
#   gc                         registry garbage-collect --delete-untagged
#   ls                         list repositories, tags, and digests
#   df                         show registry disk usage and free space
#   egress                     print "rx_bytes tx_bytes" of the node NIC
set -euo pipefail

# shellcheck source-path=SCRIPTDIR
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

rexec() {
  local t="$1" c="$2"
  shift 2
  kt "${t}" -n "${BENCH_NS}" exec "$(registry_pod)" -c "${c}" -- "$@"
}

cmd_up() {
  k apply -f "${BENCH_DIR}/manifests/namespace.yaml"
  require_registry_node
  sed -e "s/\${REGISTRY_NODE}/${REGISTRY_NODE}/g" -e "s/\${REGISTRY_PORT}/${REGISTRY_PORT}/g" "${BENCH_DIR}/manifests/registry.yaml" | k apply -f -
  k -n "${BENCH_NS}" rollout status deploy/origin-registry --timeout=280s
  log "origin registry ready at $(origin_addr)"
}

cmd_install_tool() {
  local bin pod
  bin="$(build_tool)"
  pod="$(registry_pod)"
  # Copy then rename, so a running gen-image does not block the update.
  k -n "${BENCH_NS}" cp "${bin}" "${pod}:/usr/local/bin/gantry-cluster-bench.new" -c tools
  rexec 60 tools sh -c 'chmod 0755 /usr/local/bin/gantry-cluster-bench.new &&
    mv /usr/local/bin/gantry-cluster-bench.new /usr/local/bin/gantry-cluster-bench' 
}

cmd_gen() {
  local tag="${1:?tag}" layers="${2:-4}" size="${3:-10Gi}" repo="${4:-bench/g40}"
  rexec 7200 tools gantry-cluster-bench gen-image --registry "127.0.0.1:${REGISTRY_PORT}" \
    --repo "${repo}" --tag "${tag}" --layers "${layers}" --layer-size "${size}"
}

cmd_info() {
  local ref="${1:?image ref}" path repo tag
  path="${ref#*/}"
  repo="${path%:*}"
  tag="${path##*:}"
  [[ "${repo}" != "${path}" ]] || die "image ref needs an explicit tag: ${ref}"
  rexec 60 tools gantry-cluster-bench image-info --registry "127.0.0.1:${REGISTRY_PORT}" --repo "${repo}" --tag "${tag}"
}

cmd_delete() {
  local tag="${1:?tag}" repo="${2:-bench/g40}"
  rexec 120 tools gantry-cluster-bench delete-image --registry "127.0.0.1:${REGISTRY_PORT}" --repo "${repo}" --tag "${tag}"
}

cmd_gc() {
  # Do not run while a push is in progress: GC can delete blobs of an
  # upload that has no manifest yet.
  rexec 3600 registry registry garbage-collect --delete-untagged /etc/distribution/config.yml
}

cmd_ls() {
  # shellcheck disable=SC2016 # expanded inside the pod
  rexec 60 tools bash -c '
    set -euo pipefail
    base=/var/lib/registry/docker/registry/v2/repositories
    [ -d "$base" ] || exit 0
    find "$base" -type d -path "*/_manifests/tags/*/current" | sort | while read -r d; do
      repo=${d#"$base"/}; repo=${repo%%/_manifests/*}
      tag=${d%/current}; tag=${tag##*/}
      printf "%s:%s %s\n" "$repo" "$tag" "$(cat "$d/link")"
    done'
}

cmd_df() {
  rexec 120 tools bash -c 'du -sh /var/lib/registry; df -h /var/lib/registry'
}

cmd_egress() {
  # The registry pod is hostNetwork, so this is the node NIC (eth0).
  # shellcheck disable=SC2016 # awk program
  rexec 30 tools awk -v ifc="${NIC:-eth0}" '{sub(":", " ")} $1 == ifc {print $2, $10}' /proc/net/dev
}

case "${1:-}" in
  up) cmd_up ;;
  down) k -n "${BENCH_NS}" delete deploy/origin-registry --ignore-not-found --wait=true ;;
  addr) origin_addr ;;
  install-tool) cmd_install_tool ;;
  gen) shift; cmd_gen "$@" ;;
  info) shift; cmd_info "$@" ;;
  delete) shift; cmd_delete "$@" ;;
  gc) cmd_gc ;;
  ls) cmd_ls ;;
  df) cmd_df ;;
  egress) cmd_egress ;;
  *) sed -n '5,21p' "$0" >&2; exit 2 ;;
esac
