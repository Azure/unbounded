#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.
#
# kind-create.sh - create the Orca dev Kind cluster idempotently.
set -euo pipefail

CLUSTER_NAME=${CLUSTER_NAME:?CLUSTER_NAME must be set}
KIND_CONFIG=${KIND_CONFIG:?KIND_CONFIG must be set}

if ! command -v kind >/dev/null 2>&1; then
  echo "kind is not installed. See https://kind.sigs.k8s.io/docs/user/quick-start/#installation" >&2
  exit 1
fi

if kind get clusters 2>/dev/null | grep -qx "${CLUSTER_NAME}"; then
  echo "Kind cluster '${CLUSTER_NAME}' already exists; skipping creation."
  exit 0
fi

echo "Creating Kind cluster '${CLUSTER_NAME}' from ${KIND_CONFIG} ..."
kind create cluster --name "${CLUSTER_NAME}" --config "${KIND_CONFIG}" --wait 120s

echo "Cluster ready. Current context:"
kubectl config current-context
