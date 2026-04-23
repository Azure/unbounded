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

kubectl apply -f deploy/inventory/common/01-namespace.yaml
sed "s/{{ SSL_MODE }}/disable/" deploy/inventory/common/02-config.yaml | kubectl apply -f -

# Skip re-creating the pg-creds secret if it already exists.
if kubectl get secret pg-creds -n inventory-collector &>/dev/null; then
  echo "Secret 'pg-creds' already exists, skipping creation"
else
  echo "Creating secret 'pg-creds'..."
  PG_PASSWORD="$(head -c 32 /dev/urandom | base64 | tr -d '/+=' | head -c 32)"
  PG_PASSWORD_B64="$(echo -n "${PG_PASSWORD}" | base64)"
  sed "s/{{ PASSWORD }}/${PG_PASSWORD_B64}/" deploy/inventory/common/03-secret.yaml | sed "s/DB_PASSWORD/POSTGRES_PASSWORD/" | kubectl apply -f -
fi

echo "Done."
