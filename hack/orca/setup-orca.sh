#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.
#
# setup-orca.sh - Install Orca (with Azurite origin + LocalStack S3
# cachestore) into a Kubernetes cluster.
#
# This is the single coherent entrypoint for developer Orca installs.
# It works against any cluster reachable via kubectl: kind, AKS, EKS,
# k3d, plain kubeadm. Defaults match the standard dev shape: azureblob
# origin pointing at an in-cluster Azurite emulator, S3 cachestore
# pointing at an in-cluster LocalStack. After this script returns,
# `bin/orcadev <verb>` can drive the install with no extra flags
# (orcadev auto-opens port-forwards as needed).
#
# Usage: setup-orca.sh [flags]
#
#   --context CTX        kubectl context to target (default: current)
#   --namespace NS       namespace to install into (default: unbounded-kube)
#   --origin DRIVER      azureblob | awss3 (default: azureblob)
#   --image IMG          orca container image (default: ghcr.io/azure/orca:dev)
#   --build              build the orca image locally with `make image-orca-local`
#                        before applying (typical when paired with --kind-load)
#   --kind-load          side-load the image into the target's kind cluster
#                        before applying (target context must be kind-*)
#   --log-level LEVEL    orca log level: debug, info, warn, error (default: info)
#   --replicas N         number of orca replicas (default: 3, matches the
#                        worker-node count in hack/orca/kind-config.yaml)
#   --no-wait            apply manifests and exit without waiting for
#                        emulators / orca to reach Ready
#   --uninstall          delete everything this script created and exit
#
# Real-Azure mode (advanced): set AZURE_STORAGE_ACCOUNT, AZURE_STORAGE_KEY,
# and AZURE_CONTAINER in the environment before invoking. Endpoint is
# computed as https://<account>.blob.core.windows.net/. The in-cluster
# Azurite + LocalStack are still deployed in this mode but Orca ignores
# them and talks to real Azure for origin (cachestore stays on the
# in-cluster LocalStack).
#
# Examples:
#
#   # I have no cluster - bring up kind + install (see also kind-up.sh)
#   ./hack/orca/kind-up.sh
#   ./hack/orca/setup-orca.sh --build --kind-load
#
#   # I have an AKS cluster, image is already in a registry the cluster can pull
#   ./hack/orca/setup-orca.sh \
#       --context my-aks \
#       --image my-registry.io/orca:dev
#
#   # Switch an existing install to awss3 origin
#   ./hack/orca/setup-orca.sh --origin awss3
#
#   # Tear everything down
#   ./hack/orca/setup-orca.sh --uninstall

set -euo pipefail

# -----------------------------------------------------------------------------
# Defaults

CONTEXT=""
NAMESPACE="unbounded-kube"
ORIGIN_DRIVER="azureblob"
ORCA_IMAGE="ghcr.io/azure/orca:dev"
DO_BUILD=0
DO_KIND_LOAD=0
LOG_LEVEL="info"
REPLICAS=3
DO_WAIT=1
DO_UNINSTALL=0

# Computed paths.
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"

# Well-known Azurite dev key. Public Microsoft-documented constant,
# not a secret. Used when no real Azure account is configured.
AZURITE_DEV_KEY="Eby8vdM02xNOcqFlqUwJPLlmEtlCDXJ1OUzFT50uSRZ6IFsuFq2UVErCz4I6tq/K1SZFPTOtr/KBHBeksoGMGw=="

# -----------------------------------------------------------------------------
# Helpers

log()  { echo ">> $*" >&2; }
err()  { echo "!! $*" >&2; }
die()  { err "$@"; exit 1; }

usage() {
  # Print the comment block from the top of this file (everything
  # between the leading comment block and the first blank
  # post-comment line), then exit.
  awk '
    /^#!/ { next }
    /^#/  { sub(/^# ?/, ""); print; next }
    { exit }
  ' "${BASH_SOURCE[0]}"
  exit "${1:-0}"
}

# kubectl_ctx runs kubectl with --context only when CONTEXT is non-empty,
# matching kubectl's "current context" default behaviour.
kubectl_ctx() {
  if [[ -n "${CONTEXT}" ]]; then
    kubectl --context "${CONTEXT}" "$@"
  else
    kubectl "$@"
  fi
}

# is_kind_context returns 0 if CONTEXT (or the current context when
# CONTEXT is empty) names a kind cluster. Kind contexts are always
# prefixed "kind-". We never check via `kind get clusters` because
# that requires the kind binary; the prefix check is sufficient and
# fast.
is_kind_context() {
  local ctx="${CONTEXT}"
  if [[ -z "${ctx}" ]]; then
    ctx="$(kubectl config current-context 2>/dev/null || true)"
  fi
  [[ "${ctx}" == kind-* ]]
}

# kind_cluster_name strips the "kind-" prefix from the active context
# so we can pass it to `kind load image-archive --name`.
kind_cluster_name() {
  local ctx="${CONTEXT}"
  if [[ -z "${ctx}" ]]; then
    ctx="$(kubectl config current-context 2>/dev/null || true)"
  fi
  echo "${ctx#kind-}"
}

# -----------------------------------------------------------------------------
# Flag parsing

while [[ $# -gt 0 ]]; do
  case "$1" in
    --context)    CONTEXT="$2"; shift 2 ;;
    --namespace)  NAMESPACE="$2"; shift 2 ;;
    --origin)     ORIGIN_DRIVER="$2"; shift 2 ;;
    --image)      ORCA_IMAGE="$2"; shift 2 ;;
    --build)      DO_BUILD=1; shift ;;
    --kind-load)  DO_KIND_LOAD=1; shift ;;
    --log-level)  LOG_LEVEL="$2"; shift 2 ;;
    --replicas)   REPLICAS="$2"; shift 2 ;;
    --no-wait)    DO_WAIT=0; shift ;;
    --uninstall)  DO_UNINSTALL=1; shift ;;
    -h|--help)    usage 0 ;;
    *)            err "unknown flag: $1"; usage 1 ;;
  esac
done

case "${ORIGIN_DRIVER}" in
  azureblob|awss3) ;;
  *) die "--origin must be azureblob or awss3 (got ${ORIGIN_DRIVER})" ;;
esac

if [[ "${DO_KIND_LOAD}" == "1" ]] && ! is_kind_context; then
  die "--kind-load requires a kind context; current context is not kind-* (use --context or switch with kubectl config use-context)"
fi

# -----------------------------------------------------------------------------
# Uninstall path: short-circuit before any rendering / waiting.

if [[ "${DO_UNINSTALL}" == "1" ]]; then
  log "Uninstalling orca + dev emulators from namespace ${NAMESPACE}"
  # Use --ignore-not-found so this is idempotent against partial
  # installs. We delete the namespace last to take everything in
  # the namespace with it; the explicit per-kind deletes above are
  # belt-and-suspenders so RBAC clusterRoleBindings (none today,
  # but future-proof) get cleaned up too.
  kubectl_ctx -n "${NAMESPACE}" delete deployment orca azurite localstack --ignore-not-found
  kubectl_ctx -n "${NAMESPACE}" delete service orca orca-peers azurite localstack --ignore-not-found
  kubectl_ctx -n "${NAMESPACE}" delete configmap orca-config localstack-init-buckets --ignore-not-found
  kubectl_ctx -n "${NAMESPACE}" delete secret orca-credentials --ignore-not-found
  kubectl_ctx -n "${NAMESPACE}" delete serviceaccount orca --ignore-not-found
  kubectl_ctx delete namespace "${NAMESPACE}" --ignore-not-found
  log "Uninstall complete."
  exit 0
fi

# -----------------------------------------------------------------------------
# Optional: build + side-load the image (kind only).

if [[ "${DO_BUILD}" == "1" ]]; then
  log "Building orca image (${ORCA_IMAGE}) via 'make image-orca-local'"
  ( cd "${REPO_ROOT}" && make image-orca-local ORCA_IMAGE="${ORCA_IMAGE}" )
fi

if [[ "${DO_KIND_LOAD}" == "1" ]]; then
  command -v kind >/dev/null 2>&1 || die "kind is not on PATH; install kind first"
  command -v podman >/dev/null 2>&1 || command -v docker >/dev/null 2>&1 \
    || die "neither podman nor docker is on PATH; cannot save the image archive"
  engine="$(command -v podman || command -v docker)"

  cluster="$(kind_cluster_name)"
  log "Side-loading ${ORCA_IMAGE} into kind cluster '${cluster}' via ${engine##*/}"

  tmpdir="$(mktemp -d)"
  trap 'rm -rf "${tmpdir}"' EXIT
  archive="${tmpdir}/orca.tar"
  "${engine}" save -o "${archive}" "${ORCA_IMAGE}"
  kind load image-archive "${archive}" --name "${cluster}"
fi

# -----------------------------------------------------------------------------
# Determine origin/credentials shape.

# Real-Azure mode is opted into via env vars; otherwise everything
# falls back to the in-cluster Azurite emulator with the well-known
# dev key.
real_azure=0
if [[ "${ORIGIN_DRIVER}" == "azureblob" && -n "${AZURE_STORAGE_ACCOUNT:-}" ]]; then
  real_azure=1
  [[ -n "${AZURE_STORAGE_KEY:-}" ]] || die "AZURE_STORAGE_KEY must be set when AZURE_STORAGE_ACCOUNT is set"
  [[ -n "${AZURE_CONTAINER:-}" ]]   || die "AZURE_CONTAINER must be set when AZURE_STORAGE_ACCOUNT is set"
fi

if [[ "${ORIGIN_DRIVER}" == "azureblob" ]]; then
  if [[ "${real_azure}" == "1" ]]; then
    azure_account="${AZURE_STORAGE_ACCOUNT}"
    azure_container="${AZURE_CONTAINER}"
    azure_endpoint=""  # leave blank; azureblob driver targets *.blob.core.windows.net
    azure_key="${AZURE_STORAGE_KEY}"
    origin_id="azureblob-${AZURE_STORAGE_ACCOUNT}"
  else
    azure_account="devstoreaccount1"
    azure_container="orca-test"
    azure_endpoint="http://azurite.${NAMESPACE}.svc.cluster.local:10000/devstoreaccount1/"
    azure_key="${AZURITE_DEV_KEY}"
    origin_id="azureblob-azurite"
  fi
else
  azure_account=""
  azure_container=""
  azure_endpoint=""
  # Still ship the Azurite dev key in the Secret so a later origin
  # switch via `setup-orca.sh --origin azureblob` works without
  # re-running this script's Secret step.
  azure_key="${AZURITE_DEV_KEY}"
  origin_id="awss3-localstack"
fi

# -----------------------------------------------------------------------------
# Render manifests to a temp dir.

rendered="$(mktemp -d)"
trap 'rm -rf "${rendered}"' EXIT
rendered_orca="${rendered}/orca"
rendered_dev="${rendered}/dev"
mkdir -p "${rendered_orca}" "${rendered_dev}"

log "Rendering orca manifests"
( cd "${REPO_ROOT}" && go run ./hack/cmd/render-manifests \
    --templates-dir "${REPO_ROOT}/deploy/orca" \
    --output-dir "${rendered_orca}" \
    --set "Namespace=${NAMESPACE}" \
    --set "Image=${ORCA_IMAGE}" \
    --set "ImagePullPolicy=IfNotPresent" \
    --set "TargetReplicas=${REPLICAS}" \
    --set "OriginID=${origin_id}" \
    --set "OriginDriver=${ORIGIN_DRIVER}" \
    --set "AzureAccount=${azure_account}" \
    --set "AzureContainer=${azure_container}" \
    --set "AzureEndpoint=${azure_endpoint}" \
    --set "OriginAWSS3Endpoint=http://localstack.${NAMESPACE}.svc.cluster.local:4566" \
    --set "OriginAWSS3Region=us-east-1" \
    --set "OriginAWSS3Bucket=orca-origin" \
    --set "OriginAWSS3UsePathStyle=true" \
    --set "CachestoreBucket=orca-cache" \
    --set "CachestoreEndpoint=http://localstack.${NAMESPACE}.svc.cluster.local:4566" \
    --set "CachestoreRegion=us-east-1" \
    --set "ClusterService=orca-peers.${NAMESPACE}.svc.cluster.local" \
    --set "ServerAuthEnabled=false" \
    --set "InternalTLSEnabled=false" \
    --set "LogLevel=${LOG_LEVEL}" \
)

log "Rendering dev emulator manifests (Azurite + LocalStack)"
( cd "${REPO_ROOT}" && go run ./hack/cmd/render-manifests \
    --templates-dir "${REPO_ROOT}/deploy/orca/dev" \
    --output-dir "${rendered_dev}" \
    --set "Namespace=${NAMESPACE}" \
    --set "CachestoreBucket=orca-cache" \
    --set "OriginBucket=orca-origin" \
    --set "AzuriteContainer=orca-test" \
    --set "AzuriteNodePort=30100" \
    --set "LocalstackNodePort=30200" \
)

# -----------------------------------------------------------------------------
# Apply.

log "Applying namespace + emulator manifests to context $(kubectl_ctx config current-context)"

# Namespace first so subsequent applies have somewhere to land.
kubectl_ctx apply -f "${rendered_orca}/01-namespace.yaml"

# Emulators next; orca's startup probe will fail until the cachestore
# bucket and Azurite container exist, so we deploy these first and
# wait for their init hooks to settle before bringing orca up.
kubectl_ctx apply -f "${rendered_dev}/01-localstack.yaml"
kubectl_ctx apply -f "${rendered_dev}/03-azurite.yaml"

if [[ "${DO_WAIT}" == "1" ]]; then
  log "Waiting for LocalStack to be Ready"
  kubectl_ctx -n "${NAMESPACE}" rollout status deployment/localstack --timeout=120s

  log "Waiting for LocalStack init-hook to create cachestore bucket"
  ok=0
  for _ in $(seq 1 30); do
    if kubectl_ctx -n "${NAMESPACE}" exec deploy/localstack -- \
        awslocal s3api head-bucket --bucket orca-cache >/dev/null 2>&1; then
      ok=1; break
    fi
    sleep 2
  done
  [[ "${ok}" == "1" ]] || die "LocalStack init-hook did not create orca-cache within 60s"

  log "Waiting for Azurite to be Ready"
  kubectl_ctx -n "${NAMESPACE}" rollout status deployment/azurite --timeout=180s

  log "Waiting for Azurite container-ensurer sidecar to create the orca-test container"
  ok=0
  for _ in $(seq 1 30); do
    if kubectl_ctx -n "${NAMESPACE}" exec deploy/azurite -c container-ensurer -- \
        az storage container exists --name orca-test --query exists -o tsv 2>/dev/null \
        | grep -qi true; then
      ok=1; break
    fi
    sleep 2
  done
  [[ "${ok}" == "1" ]] || die "Azurite container-ensurer did not create orca-test within 60s"
fi

# -----------------------------------------------------------------------------
# orca-credentials Secret. Create-or-update in one pass so this is
# idempotent against re-runs that change credentials.

log "Creating/updating orca-credentials Secret"
kubectl_ctx -n "${NAMESPACE}" create secret generic orca-credentials \
  --from-literal=ORCA_CACHESTORE_S3_ACCESS_KEY=test \
  --from-literal=ORCA_CACHESTORE_S3_SECRET_KEY=test \
  --from-literal=ORCA_AWSS3_ACCESS_KEY=test \
  --from-literal=ORCA_AWSS3_SECRET_KEY=test \
  --from-literal=ORCA_AZUREBLOB_ACCOUNT_KEY="${azure_key}" \
  --dry-run=client -o yaml \
  | kubectl_ctx apply -f -

# -----------------------------------------------------------------------------
# Apply orca RBAC, ConfigMap, Service, Deployment.

log "Applying orca manifests"
kubectl_ctx apply -f "${rendered_orca}/02-rbac.yaml"
kubectl_ctx apply -f "${rendered_orca}/03-config.yaml"
# Service before Deployment: the headless orca-peers Service must
# exist (with its DNS A-records) before the pods start so the initial
# cluster.refresh sees the full peer set instead of bootstrapping
# into the self-only fallback.
kubectl_ctx apply -f "${rendered_orca}/05-service.yaml"
kubectl_ctx apply -f "${rendered_orca}/04-deployment.yaml"

if [[ "${DO_WAIT}" == "1" ]]; then
  log "Waiting for orca (${REPLICAS} replicas) to be Ready"
  kubectl_ctx -n "${NAMESPACE}" rollout status deployment/orca --timeout=180s
fi

# -----------------------------------------------------------------------------
# Done. Print the next-step commands operators care about.

cat >&2 <<EOF

Orca is installed in namespace ${NAMESPACE} (context: $(kubectl_ctx config current-context)).
Origin: ${ORIGIN_DRIVER}$( [[ "${real_azure}" == "1" ]] && echo " (real Azure account: ${AZURE_STORAGE_ACCOUNT})" || echo " (in-cluster emulator)" ).

Next steps:
  bin/orcadev upload --generate --count 5 --size 10MiB   # seed synthetic blobs
  bin/orcadev roundtrip --file /tmp/test.bin             # verify SHA-256 roundtrip
  bin/orcadev scenario cold-warm                         # canned end-to-end scenario
  bin/orcadev bench --key synth1 --duration 30s          # parallel-GET benchmark

orcadev auto-port-forwards svc/orca, svc/azurite, svc/localstack as
needed, so no separate \`kubectl port-forward\` is required. If you
want a stable foreground forward (for ad-hoc curl), run:
  kubectl${CONTEXT:+ --context ${CONTEXT}} -n ${NAMESPACE} port-forward svc/orca 8443:8443
EOF
