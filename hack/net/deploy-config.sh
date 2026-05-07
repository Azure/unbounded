#!/usr/bin/env bash
# Apply the unbounded-net shared runtime configmap, optionally patching
# LOG_LEVEL and AZURE_TENANT_ID into it.
#
# Behaviour:
#   - Creates the configmap from the rendered manifest when missing.
#   - Does NOT overwrite existing values when LOG_LEVEL/AZURE_TENANT_ID
#     are unset; pass them explicitly to patch.
#
# Required env:
#   NAMESPACE       Kubernetes namespace that hosts the configmap.
#   RENDERED_DIR    Path to deploy/net/rendered (contains 01-configmap.yaml).
#
# Optional env:
#   LOG_LEVEL       When non-empty, patches data.LOG_LEVEL.
#   AZURE_TENANT_ID When non-empty, patches data.AZURE_TENANT_ID.

set -euo pipefail

: "${NAMESPACE:?NAMESPACE is required}"
: "${RENDERED_DIR:?RENDERED_DIR is required}"

CM_NAME="unbounded-net-config"

if ! kubectl get configmap "$CM_NAME" -n "$NAMESPACE" >/dev/null 2>&1; then
    echo "Creating $CM_NAME configmap in namespace $NAMESPACE"
    kubectl apply -f "$RENDERED_DIR/01-configmap.yaml"
fi

if [[ -n "${LOG_LEVEL:-}" ]]; then
    echo "Patching $CM_NAME LOG_LEVEL=$LOG_LEVEL"
    kubectl patch configmap "$CM_NAME" -n "$NAMESPACE" \
        --type merge -p "{\"data\":{\"LOG_LEVEL\":\"$LOG_LEVEL\"}}"
fi

if [[ -n "${AZURE_TENANT_ID:-}" ]]; then
    echo "Patching $CM_NAME AZURE_TENANT_ID=$AZURE_TENANT_ID"
    kubectl patch configmap "$CM_NAME" -n "$NAMESPACE" \
        --type merge -p "{\"data\":{\"AZURE_TENANT_ID\":\"$AZURE_TENANT_ID\"}}"
fi
