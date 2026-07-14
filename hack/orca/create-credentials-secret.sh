#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.
#
# create-credentials-secret.sh - Create (or update) the orca-credentials
# Secret on an integration cluster.
#
# The Secret holds the three confidential values Orca needs:
#   - ORCA_AZUREBLOB_ACCOUNT_KEY     real Azure storage account key (you provide)
#   - ORCA_CACHESTORE_S3_ACCESS_KEY  Garage S3 access key id (generated here)
#   - ORCA_CACHESTORE_S3_SECRET_KEY  Garage S3 secret key   (generated here)
#
# The Garage S3 keys are the single source of truth: hack/orca/deploy-integration.sh
# imports them into Garage (via bootstrap-garage.sh) and injects them into
# Orca via envFrom. This script generates fresh ones in the format Garage
# requires (access id = "GK" + 12 hex bytes; secret = 32 hex bytes) unless
# you pass your own.
#
# Idempotent: re-running replaces the Secret in place (create-or-update).
#
# Usage: create-credentials-secret.sh [flags]
#
#   --azure-account-key KEY   Azure storage account key (required; or set
#                             env ORCA_AZUREBLOB_ACCOUNT_KEY)
#   --context CTX             kubectl context to target (default: current)
#   --namespace NS            namespace (default: unbounded-system)
#   --secret-name NAME        Secret name (default: orca-credentials)
#   --access-key ID           Garage access key id (default: generated)
#   --secret-key SECRET       Garage secret key (default: generated)
#   -h | --help               show this help

set -euo pipefail

CONTEXT=""
NAMESPACE="unbounded-system"
SECRET_NAME="orca-credentials"
AZURE_ACCOUNT_KEY="${ORCA_AZUREBLOB_ACCOUNT_KEY:-}"
ACCESS_KEY=""
SECRET_KEY=""

log() { echo ">> $*" >&2; }
die() { echo "!! $*" >&2; exit 1; }

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
    --azure-account-key) AZURE_ACCOUNT_KEY="$2"; shift 2 ;;
    --context)           CONTEXT="$2"; shift 2 ;;
    --namespace)         NAMESPACE="$2"; shift 2 ;;
    --secret-name)       SECRET_NAME="$2"; shift 2 ;;
    --access-key)        ACCESS_KEY="$2"; shift 2 ;;
    --secret-key)        SECRET_KEY="$2"; shift 2 ;;
    -h|--help)           usage 0 ;;
    *)                   echo "!! unknown flag: $1" >&2; usage 1 ;;
  esac
done

[[ -n "${AZURE_ACCOUNT_KEY}" ]] || die "--azure-account-key (or env ORCA_AZUREBLOB_ACCOUNT_KEY) is required"
command -v openssl >/dev/null 2>&1 || die "openssl is required to generate Garage keys"

# Generate Garage keys in the required format if not supplied.
[[ -n "${ACCESS_KEY}" ]] || ACCESS_KEY="GK$(openssl rand -hex 12)"
[[ -n "${SECRET_KEY}" ]] || SECRET_KEY="$(openssl rand -hex 32)"

# Sanity-check the access key format (Garage rejects others on import).
[[ "${ACCESS_KEY}" =~ ^GK[0-9a-f]{24}$ ]] \
  || die "access key '${ACCESS_KEY}' is not 'GK' + 24 hex chars (12 bytes)"

kubectl_ctx() {
  if [[ -n "${CONTEXT}" ]]; then
    kubectl --context "${CONTEXT}" "$@"
  else
    kubectl "$@"
  fi
}

log "Creating/updating Secret ${SECRET_NAME} in namespace ${NAMESPACE}"

# Build the Secret client-side and apply, so the apply is a create-or-update
# and the values never reach the API server as command arguments.
kubectl_ctx -n "${NAMESPACE}" create secret generic "${SECRET_NAME}" \
  --from-literal=ORCA_AZUREBLOB_ACCOUNT_KEY="${AZURE_ACCOUNT_KEY}" \
  --from-literal=ORCA_CACHESTORE_S3_ACCESS_KEY="${ACCESS_KEY}" \
  --from-literal=ORCA_CACHESTORE_S3_SECRET_KEY="${SECRET_KEY}" \
  --dry-run=client -o yaml \
  | kubectl_ctx label --local -f - --dry-run=client -o yaml \
      app.kubernetes.io/name=orca \
  | kubectl_ctx apply -f -

cat >&2 <<EOF

Secret ${SECRET_NAME} applied to namespace ${NAMESPACE}.

Garage S3 credentials are recorded only in the Secret and are imported
into Garage automatically by hack/orca/deploy-integration.sh.
EOF
