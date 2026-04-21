#!/usr/bin/env bash
# Apply the unbounded-net node DaemonSet manifests and ensure the
# workload rolls to pick up new images even when the manifest hash is
# unchanged.
#
# Required env:
#   NAMESPACE       Kubernetes namespace.
#   RENDERED_DIR    Path to deploy/net/rendered (contains node/*.yaml).
#
# Optional env:
#   ROLLOUT_TIMEOUT kubectl rollout timeout (default: 120s).

set -euo pipefail

: "${NAMESPACE:?NAMESPACE is required}"
: "${RENDERED_DIR:?RENDERED_DIR is required}"
ROLLOUT_TIMEOUT="${ROLLOUT_TIMEOUT:-120s}"

NODE_DIR="$RENDERED_DIR/node"

node_gen_before=$(kubectl get daemonset/unbounded-net-node -n "$NAMESPACE" \
    -o jsonpath='{.metadata.generation}' 2>/dev/null || echo "0")

kubectl apply -f "$NODE_DIR/01-serviceaccount.yaml"
kubectl apply -f "$NODE_DIR/02-rbac.yaml"
kubectl apply -f "$NODE_DIR/03-daemonset.yaml"

node_gen_after=$(kubectl get daemonset/unbounded-net-node -n "$NAMESPACE" \
    -o jsonpath='{.metadata.generation}')

if [[ "$node_gen_before" == "$node_gen_after" ]]; then
    echo "Node manifest unchanged, restarting to pick up new image..."
    kubectl rollout restart daemonset/unbounded-net-node -n "$NAMESPACE"
fi

echo "Waiting for node rollout to complete..."
kubectl rollout status daemonset/unbounded-net-node -n "$NAMESPACE" \
    --timeout="$ROLLOUT_TIMEOUT"
