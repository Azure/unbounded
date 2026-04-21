#!/usr/bin/env bash
# Apply the unbounded-net controller manifests and ensure the workload
# rolls to pick up new images even when the manifest hash is unchanged.
#
# Also removes legacy resources from earlier releases (webhook service
# and several validating admission policies) that are no longer rendered
# but may still exist in long-lived clusters.
#
# Required env:
#   NAMESPACE       Kubernetes namespace.
#   RENDERED_DIR    Path to deploy/net/rendered (contains controller/*.yaml).
#
# Optional env:
#   ROLLOUT_TIMEOUT kubectl rollout timeout (default: 120s).

set -euo pipefail

: "${NAMESPACE:?NAMESPACE is required}"
: "${RENDERED_DIR:?RENDERED_DIR is required}"
ROLLOUT_TIMEOUT="${ROLLOUT_TIMEOUT:-120s}"

CTRL_DIR="$RENDERED_DIR/controller"

ctrl_gen_before=$(kubectl get deployment/unbounded-net-controller -n "$NAMESPACE" \
    -o jsonpath='{.metadata.generation}' 2>/dev/null || echo "0")

kubectl apply -f "$CTRL_DIR/01-serviceaccount.yaml"
kubectl apply -f "$CTRL_DIR/02-rbac.yaml"
kubectl apply -f "$CTRL_DIR/04-service.yaml"

# Legacy resources retired in prior releases; remove if present.
kubectl delete service unbounded-net-webhook -n "$NAMESPACE" --ignore-not-found
for vap in \
    unbounded-net-webhook-field-restriction \
    unbounded-net-csr-restriction \
    unbounded-net-csr-approval-restriction; do
    kubectl delete validatingadmissionpolicy "$vap" --ignore-not-found
    kubectl delete validatingadmissionpolicybinding "$vap" --ignore-not-found
done

kubectl apply -f "$CTRL_DIR/06-validatingwebhook.yaml"
kubectl apply -f "$CTRL_DIR/07-apiservice.yaml"
kubectl apply -f "$CTRL_DIR/08-mutatingwebhook.yaml"
kubectl apply -f "$CTRL_DIR/09-vap.yaml"
kubectl apply -f "$CTRL_DIR/10-status-viewer.yaml"
kubectl apply -f "$CTRL_DIR/03-deployment.yaml"

ctrl_gen_after=$(kubectl get deployment/unbounded-net-controller -n "$NAMESPACE" \
    -o jsonpath='{.metadata.generation}')

if [[ "$ctrl_gen_before" == "$ctrl_gen_after" ]]; then
    echo "Controller manifest unchanged, restarting to pick up new image..."
    kubectl rollout restart deployment/unbounded-net-controller -n "$NAMESPACE"
fi

echo "Waiting for controller rollout to complete..."
kubectl rollout status deployment/unbounded-net-controller -n "$NAMESPACE" \
    --timeout="$ROLLOUT_TIMEOUT"
