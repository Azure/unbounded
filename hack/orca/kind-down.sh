#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0
#
# kind-down.sh - Delete the Orca dev kind cluster.
#
# Idempotent. Exits 0 if the cluster does not exist.
#
# Usage: kind-down.sh [flags]
#
#   --name NAME   kind cluster name (default: orca-dev)

set -euo pipefail

CLUSTER_NAME="orca-dev"

while [[ $# -gt 0 ]]; do
  case "$1" in
    --name) CLUSTER_NAME="$2"; shift 2 ;;
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

if ! command -v kind >/dev/null 2>&1; then
  echo ">> kind is not installed; nothing to do." >&2
  exit 0
fi

if ! kind get clusters 2>/dev/null | grep -qx "${CLUSTER_NAME}"; then
  echo ">> No kind cluster named '${CLUSTER_NAME}'; nothing to delete." >&2
  exit 0
fi

echo ">> Deleting kind cluster '${CLUSTER_NAME}'" >&2
kind delete cluster --name "${CLUSTER_NAME}"
