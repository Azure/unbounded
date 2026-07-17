#!/usr/bin/env bash
set -euo pipefail

CLUSTER_NAME="inventorydev"

# Create the kind cluster if it doesn't already exist.
if kind get clusters 2>/dev/null | grep -q "^${CLUSTER_NAME}$"; then
  echo "kind cluster '${CLUSTER_NAME}' already exists"
else
  echo "Creating kind cluster '${CLUSTER_NAME}'..."
  kind create cluster --name "${CLUSTER_NAME}"
fi

# Switch the active kubectl context to the kind cluster.
kubectl config use-context "kind-${CLUSTER_NAME}"
kubectl cluster-info

NAMESPACE="${INVENTORY_NAMESPACE:-unbounded-system}"
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
RENDER_DIR="$(mktemp -d)"
trap 'rm -rf "${RENDER_DIR}"' EXIT

# Render the namespace and database ConfigMap (the secret is rendered
# separately below only when it does not already exist, so we never
# overwrite a previously generated password).
go run "${REPO_ROOT}/hack/cmd/render-manifests" \
  --templates-dir "${REPO_ROOT}/deploy/inventory" \
  --output-dir "${RENDER_DIR}" \
  --set Namespace="${NAMESPACE}" \
  --set SSLMode=disable \
  --set Password=cGxhY2Vob2xkZXI=

kubectl apply -f "${RENDER_DIR}/common/01-namespace.yaml"
kubectl apply -f "${RENDER_DIR}/common/02-config.yaml"

# Skip re-creating the pg-creds secret if it already exists.
if kubectl get secret pg-creds -n "${NAMESPACE}" &>/dev/null; then
  echo "Secret 'pg-creds' already exists, skipping creation"
else
  echo "Creating secret 'pg-creds'..."
  PG_PASSWORD="$(head -c 32 /dev/urandom | base64 | tr -d '/+=' | head -c 32)"
  PG_PASSWORD_B64="$(echo -n "${PG_PASSWORD}" | base64)"
  go run "${REPO_ROOT}/hack/cmd/render-manifests" \
    --templates-dir "${REPO_ROOT}/deploy/inventory" \
    --output-dir "${RENDER_DIR}" \
    --set Namespace="${NAMESPACE}" \
    --set SSLMode=disable \
    --set Password="${PG_PASSWORD_B64}"
  kubectl apply -f "${RENDER_DIR}/common/03-secret.yaml"
fi

echo "Done."
