#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.
#
# kind-up.sh - Create the Orca dev kind cluster idempotently.
#
# Creates a single-cluster, single-purpose kind cluster (1 control
# plane + 3 worker nodes) suitable for installing Orca via
# setup-orca.sh. The 3 worker nodes match Orca's default replica
# count and its required pod-anti-affinity (hostname topology).
#
# Usage: kind-up.sh [flags]
#
#   --name NAME       kind cluster name (default: orca-dev). The
#                     resulting kubectl context is kind-<NAME>.
#   --config PATH     kind cluster config (default: hack/orca/kind-config.yaml)
#
# After this script returns successfully, run:
#
#   ./hack/orca/setup-orca.sh --build --kind-load
#
# to build the orca image locally, side-load it into the kind nodes,
# and install Orca + Azurite + LocalStack into the cluster.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

CLUSTER_NAME="orca-dev"
KIND_CONFIG="${SCRIPT_DIR}/kind-config.yaml"

while [[ $# -gt 0 ]]; do
  case "$1" in
    --name)   CLUSTER_NAME="$2"; shift 2 ;;
    --config) KIND_CONFIG="$2"; shift 2 ;;
    -h|--help)
      awk '
        /^#!/ { next }
        /^#/  { sub(/^# ?/, ""); print; next }
        { exit }
      ' "${BASH_SOURCE[0]}"
      exit 0
      ;;
    *) echo "unknown flag: $1" >&2; exit 1 ;;
  esac
done

command -v kind >/dev/null 2>&1 \
  || { echo "kind is not installed. See https://kind.sigs.k8s.io/docs/user/quick-start/#installation" >&2; exit 1; }

if kind get clusters 2>/dev/null | grep -qx "${CLUSTER_NAME}"; then
  echo ">> Kind cluster '${CLUSTER_NAME}' already exists; skipping creation." >&2
else
  echo ">> Creating kind cluster '${CLUSTER_NAME}' from ${KIND_CONFIG}" >&2
  kind create cluster --name "${CLUSTER_NAME}" --config "${KIND_CONFIG}" --wait 120s
fi

echo ">> Active kubectl context: $(kubectl config current-context)" >&2
echo ">> Next: ./hack/orca/setup-orca.sh --build --kind-load" >&2
