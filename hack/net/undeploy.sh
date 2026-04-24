#!/usr/bin/env bash
# Remove all unbounded-net resources from a cluster in dependency order
# (workloads first, then RBAC, then CRDs, then namespace).
#
# Required env:
#   NAMESPACE       Kubernetes namespace.
#   RENDERED_DIR    Path to deploy/net/rendered.
#   CRD_DIR         Path to deploy/net/crd (raw CRD manifests).

set -euo pipefail

: "${NAMESPACE:?NAMESPACE is required}"
: "${RENDERED_DIR:?RENDERED_DIR is required}"
: "${CRD_DIR:?CRD_DIR is required}"

CTRL_DIR="$RENDERED_DIR/controller"
NODE_DIR="$RENDERED_DIR/node"

# Workloads first so admission webhooks aren't torn down before the pods
# they admit are stopped.
kubectl delete -f "$NODE_DIR/03-daemonset.yaml"          --ignore-not-found
kubectl delete -f "$NODE_DIR/02-rbac.yaml"               --ignore-not-found
kubectl delete -f "$NODE_DIR/01-serviceaccount.yaml"     --ignore-not-found

kubectl delete -f "$CTRL_DIR/09-vap.yaml"                --ignore-not-found
kubectl delete -f "$CTRL_DIR/08-mutatingwebhook.yaml"    --ignore-not-found
kubectl delete -f "$CTRL_DIR/07-apiservice.yaml"         --ignore-not-found
kubectl delete -f "$CTRL_DIR/06-validatingwebhook.yaml"  --ignore-not-found
kubectl delete -f "$CTRL_DIR/04-service.yaml"            --ignore-not-found
kubectl delete -f "$CTRL_DIR/03-deployment.yaml"         --ignore-not-found
kubectl delete -f "$CTRL_DIR/02-rbac.yaml"               --ignore-not-found
kubectl delete -f "$CTRL_DIR/01-serviceaccount.yaml"     --ignore-not-found

kubectl delete -f "$CRD_DIR/" --ignore-not-found

# Legacy APIService names retained for backwards compatibility cleanup.
kubectl delete apiservice status.net.unbounded-cloud.io          --ignore-not-found
kubectl delete apiservice v1alpha1.status.net.unbounded-cloud.io --ignore-not-found

if [[ "$NAMESPACE" != "kube-system" ]]; then
    kubectl delete -f "$RENDERED_DIR/00-namespace.yaml" --ignore-not-found
fi
