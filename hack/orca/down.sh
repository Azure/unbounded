#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.
#
# down.sh - delete the Orca dev Kind cluster.
set -euo pipefail

CLUSTER_NAME=${CLUSTER_NAME:?CLUSTER_NAME must be set}

if ! command -v kind >/dev/null 2>&1; then
  echo "kind is not installed; nothing to do." >&2
  exit 0
fi

if ! kind get clusters 2>/dev/null | grep -qx "${CLUSTER_NAME}"; then
  echo "No Kind cluster named '${CLUSTER_NAME}'; nothing to delete."
  exit 0
fi

echo "Deleting Kind cluster '${CLUSTER_NAME}' ..."
kind delete cluster --name "${CLUSTER_NAME}"
