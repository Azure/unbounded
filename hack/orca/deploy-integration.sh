#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.
#
# deploy-integration.sh - Deploy Orca onto an integration cluster with a
# real Azure Blob origin and an in-cluster single-node Garage cachestore
# (PVC-backed).
#
# This is the integration-test deploy path, used by both the
# unbounded-stable (release-upgrade) and unbounded-nightly workflows. It
# is deployment-neutral: it targets whatever cluster the current kube
# context / KUBECONFIG points at. It is intentionally NOT coupled to
# Azurite (the dev emulator): the origin is a real Azure storage account.
# It renders the shippable Orca manifests from deploy/orca and the
# test-only Garage manifest from hack/orca/integration, applies them,
# bootstraps Garage, and waits for the rollout.
#
# Confidential values (Azure account key, Garage S3 keys) come from a
# pre-created Secret named orca-credentials and are injected into Orca
# via envFrom. This script NEVER creates or overwrites that Secret; it
# fails fast if the Secret is missing. The Garage S3 keys are imported
# into Garage from the same Secret by hack/orca/bootstrap-garage.sh.
#
# Non-confidential per-cluster values (Azure account name, container,
# endpoint, origin id) are rendered into the orca-config ConfigMap from
# the flags below.
#
# Usage: deploy-integration.sh [flags]
#
#   --context CTX          kubectl context to target (default: current)
#   --namespace NS         namespace to install into (default: unbounded-kube)
#   --image IMG            orca container image (required, e.g.
#                          ghcr.io/azure/orca:v0.1.14)
#   --azure-account NAME   Azure storage account name (required)
#   --azure-container NAME Azure blob container name (required)
#   --azure-endpoint URL   Azure blob endpoint (optional; blank => the
#                          driver targets https://<account>.blob.core.windows.net)
#   --origin-id ID         origin id (default: azureblob-<account>)
#   --replicas N           number of orca replicas (default: 3)
#   --pvc-size SIZE        Garage data PVC size (default: 100Gi)
#   --storage-class NAME   Garage data PVC StorageClass (default: managed-csi)
#   --log-level LEVEL      orca log level: debug|info|warn|error (default: info)
#   --secret-name NAME     credentials Secret name (default: orca-credentials)
#   -h | --help            show this help

set -euo pipefail

CONTEXT=""
NAMESPACE="unbounded-kube"
ORCA_IMAGE=""
AZURE_ACCOUNT=""
AZURE_CONTAINER=""
AZURE_ENDPOINT=""
ORIGIN_ID=""
REPLICAS=3
PVC_SIZE="100Gi"
STORAGE_CLASS="managed-csi"
LOG_LEVEL="info"
SECRET_NAME="orca-credentials"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"

log() { echo ">> $*" >&2; }
err() { echo "!! $*" >&2; }
die() { err "$@"; exit 1; }

usage() {
  awk '
    /^#!/ { next }
    /^#/  { sub(/^# ?/, ""); print; next }
    { exit }
  ' "${BASH_SOURCE[0]}"
  exit "${1:-0}"
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --context)        CONTEXT="$2"; shift 2 ;;
    --namespace)      NAMESPACE="$2"; shift 2 ;;
    --image)          ORCA_IMAGE="$2"; shift 2 ;;
    --azure-account)  AZURE_ACCOUNT="$2"; shift 2 ;;
    --azure-container) AZURE_CONTAINER="$2"; shift 2 ;;
    --azure-endpoint) AZURE_ENDPOINT="$2"; shift 2 ;;
    --origin-id)      ORIGIN_ID="$2"; shift 2 ;;
    --replicas)       REPLICAS="$2"; shift 2 ;;
    --pvc-size)       PVC_SIZE="$2"; shift 2 ;;
    --storage-class)  STORAGE_CLASS="$2"; shift 2 ;;
    --log-level)      LOG_LEVEL="$2"; shift 2 ;;
    --secret-name)    SECRET_NAME="$2"; shift 2 ;;
    -h|--help)        usage 0 ;;
    *)                err "unknown flag: $1"; usage 1 ;;
  esac
done

[[ -n "${ORCA_IMAGE}" ]]      || die "--image is required"
[[ -n "${AZURE_ACCOUNT}" ]]   || die "--azure-account is required"
[[ -n "${AZURE_CONTAINER}" ]] || die "--azure-container is required"

if [[ -z "${ORIGIN_ID}" ]]; then
  ORIGIN_ID="azureblob-${AZURE_ACCOUNT}"
fi

kubectl_ctx() {
  if [[ -n "${CONTEXT}" ]]; then
    kubectl --context "${CONTEXT}" "$@"
  else
    kubectl "$@"
  fi
}

# Cleanup stack for rendered tempdirs.
cleanup_paths=()
cleanup() {
  local p
  for ((i=${#cleanup_paths[@]}-1; i>=0; i--)); do
    p="${cleanup_paths[$i]}"
    [[ -n "${p}" && -e "${p}" ]] && rm -rf "${p}"
  done
}
trap cleanup EXIT

# -----------------------------------------------------------------------------
# Preflight: the credentials Secret must already exist. We never create
# or overwrite it; it is operator-owned.

log "Checking for credentials Secret ${SECRET_NAME} in namespace ${NAMESPACE}"
if ! kubectl_ctx -n "${NAMESPACE}" get secret "${SECRET_NAME}" >/dev/null 2>&1; then
  die "Secret ${SECRET_NAME} not found in namespace ${NAMESPACE}. Pre-create it with ORCA_AZUREBLOB_ACCOUNT_KEY, ORCA_CACHESTORE_S3_ACCESS_KEY, ORCA_CACHESTORE_S3_SECRET_KEY before deploying."
fi

for key in ORCA_AZUREBLOB_ACCOUNT_KEY ORCA_CACHESTORE_S3_ACCESS_KEY ORCA_CACHESTORE_S3_SECRET_KEY; do
  val="$(kubectl_ctx -n "${NAMESPACE}" get secret "${SECRET_NAME}" -o "jsonpath={.data.${key}}" 2>/dev/null || true)"
  [[ -n "${val}" ]] || die "Secret ${SECRET_NAME} is missing required key ${key}"
done

# -----------------------------------------------------------------------------
# Render manifests.

CACHESTORE_ENDPOINT="http://garage.${NAMESPACE}.svc.cluster.local:3900"
CLUSTER_SERVICE="orca-peers.${NAMESPACE}.svc.cluster.local"

rendered="$(mktemp -d)"
cleanup_paths+=("${rendered}")
rendered_orca="${rendered}/orca"
rendered_garage="${rendered}/garage"
mkdir -p "${rendered_orca}" "${rendered_garage}"

log "Rendering orca manifests (image ${ORCA_IMAGE}, origin azureblob account ${AZURE_ACCOUNT})"
( cd "${REPO_ROOT}" && go run ./hack/cmd/render-manifests \
    --templates-dir "${REPO_ROOT}/deploy/orca" \
    --output-dir "${rendered_orca}" \
    --set "Namespace=${NAMESPACE}" \
    --set "Image=${ORCA_IMAGE}" \
    --set "ImagePullPolicy=IfNotPresent" \
    --set "TargetReplicas=${REPLICAS}" \
    --set "RequireAntiAffinity=false" \
    --set "OriginID=${ORIGIN_ID}" \
    --set "OriginDriver=azureblob" \
    --set "AzureAccount=${AZURE_ACCOUNT}" \
    --set "AzureContainer=${AZURE_CONTAINER}" \
    --set "AzureEndpoint=${AZURE_ENDPOINT}" \
    --set "CachestoreBucket=orca-cache" \
    --set "CachestoreEndpoint=${CACHESTORE_ENDPOINT}" \
    --set "CachestoreRegion=us-east-1" \
    --set "ClusterService=${CLUSTER_SERVICE}" \
    --set "ServerAuthEnabled=false" \
    --set "InternalTLSEnabled=false" \
    --set "LogLevel=${LOG_LEVEL}" \
)

log "Rendering Garage manifest (PVC ${PVC_SIZE} on ${STORAGE_CLASS})"
( cd "${REPO_ROOT}" && go run ./hack/cmd/render-manifests \
    --templates-dir "${REPO_ROOT}/hack/orca/integration" \
    --output-dir "${rendered_garage}" \
    --set "Namespace=${NAMESPACE}" \
    --set "CachestoreRegion=us-east-1" \
    --set "GarageStorage=${PVC_SIZE}" \
    --set "GarageStorageClass=${STORAGE_CLASS}" \
)

# -----------------------------------------------------------------------------
# Apply.

log "Applying namespace + Garage to context $(kubectl_ctx config current-context)"
kubectl_ctx apply -f "${rendered_orca}/01-namespace.yaml"
kubectl_ctx apply -f "${rendered_garage}/garage.yaml"

# Bootstrap Garage (layout, key import from Secret, cachestore bucket)
# before bringing Orca up: Orca's readiness probe fails until the
# cachestore bucket exists.
log "Bootstrapping Garage"
bootstrap_args=(--namespace "${NAMESPACE}" --secret-name "${SECRET_NAME}")
if [[ -n "${CONTEXT}" ]]; then
  bootstrap_args=(--context "${CONTEXT}" "${bootstrap_args[@]}")
fi
"${SCRIPT_DIR}/bootstrap-garage.sh" "${bootstrap_args[@]}"

# Service before Deployment: the headless orca-peers Service must exist
# (with its DNS A-records) before the pods start so the initial
# cluster.refresh sees the full peer set.
log "Applying orca RBAC, ConfigMap, Service, Deployment"
kubectl_ctx apply -f "${rendered_orca}/02-rbac.yaml"
kubectl_ctx apply -f "${rendered_orca}/03-config.yaml"
kubectl_ctx apply -f "${rendered_orca}/05-service.yaml"
kubectl_ctx apply -f "${rendered_orca}/04-deployment.yaml"

log "Waiting for orca (${REPLICAS} replicas) to be Ready"
kubectl_ctx -n "${NAMESPACE}" rollout status deployment/orca --timeout=300s

log "Orca deployed to namespace ${NAMESPACE} (context: $(kubectl_ctx config current-context))."
log "Origin: azureblob account ${AZURE_ACCOUNT}, container ${AZURE_CONTAINER}. Cachestore: Garage (${CACHESTORE_ENDPOINT})."
