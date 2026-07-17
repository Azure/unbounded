#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.
#
# bootstrap-garage.sh - Initialize a freshly deployed single-node Garage
# so it can serve as Orca's cachestore on an integration cluster.
#
# Garage starts completely empty and refuses all S3 operations until it
# has (1) a cluster layout, (2) an access key, and (3) a bucket. This
# script performs those three one-time, idempotent steps via
# `kubectl exec deploy/garage -- /garage ...` (the Garage image is a
# near-scratch image with no shell, so each step is a direct binary
# exec; the conditional/idempotent logic runs here).
#
# The S3 access/secret keys are READ FROM the orca-credentials Secret
# (keys ORCA_CACHESTORE_S3_ACCESS_KEY / ORCA_CACHESTORE_S3_SECRET_KEY)
# and imported into Garage, so the operator-provided Secret stays the
# single source of truth shared by Garage and Orca.
#
# With the persistent PVC the layout/key/bucket survive pod restarts, so
# this only needs to run at install time; re-running it is safe.
#
# Usage: bootstrap-garage.sh [flags]
#
#   --context CTX        kubectl context to target (default: current)
#   --namespace NS       namespace Garage is deployed in (default: unbounded-system)
#   --secret-name NAME   Secret holding the cachestore S3 keys (default: orca-credentials)
#   --bucket NAME        cachestore bucket to create (default: orca-cache)
#   --key-name NAME      Garage key name to import under (default: orca)
#   --capacity CAP       layout capacity for the single node (default: 100G)
#   --zone ZONE          layout zone name (default: dc1)
#   -h | --help          show this help

set -euo pipefail

CONTEXT=""
NAMESPACE="unbounded-system"
SECRET_NAME="orca-credentials"
BUCKET="orca-cache"
KEY_NAME="orca"
CAPACITY="100G"
ZONE="dc1"

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
    --context)     CONTEXT="$2"; shift 2 ;;
    --namespace)   NAMESPACE="$2"; shift 2 ;;
    --secret-name) SECRET_NAME="$2"; shift 2 ;;
    --bucket)      BUCKET="$2"; shift 2 ;;
    --key-name)    KEY_NAME="$2"; shift 2 ;;
    --capacity)    CAPACITY="$2"; shift 2 ;;
    --zone)        ZONE="$2"; shift 2 ;;
    -h|--help)     usage 0 ;;
    *)             err "unknown flag: $1"; usage 1 ;;
  esac
done

kubectl_ctx() {
  if [[ -n "${CONTEXT}" ]]; then
    kubectl --context "${CONTEXT}" "$@"
  else
    kubectl "$@"
  fi
}

# gexec runs a Garage CLI subcommand inside the running Garage pod.
gexec() { kubectl_ctx -n "${NAMESPACE}" exec deploy/garage -- /garage -c /etc/garage.toml "$@"; }

# secret_value reads a single key out of the credentials Secret and
# base64-decodes it. Fails if the Secret or key is missing.
secret_value() {
  local key="$1" val
  val="$(kubectl_ctx -n "${NAMESPACE}" get secret "${SECRET_NAME}" \
    -o "jsonpath={.data.${key}}" 2>/dev/null || true)"
  [[ -n "${val}" ]] || die "Secret ${SECRET_NAME} is missing key ${key} (namespace ${NAMESPACE})"
  printf '%s' "${val}" | base64 -d
}

log "Waiting for Garage to be Ready (namespace ${NAMESPACE})"
kubectl_ctx -n "${NAMESPACE}" rollout status deployment/garage --timeout=180s

# Wait for the node's RPC to answer before issuing layout commands.
ok=0
for _ in $(seq 1 30); do
  if gexec status >/dev/null 2>&1; then ok=1; break; fi
  sleep 2
done
[[ "${ok}" == "1" ]] || die "Garage node did not become ready within 60s"

log "Bootstrapping Garage (layout, cachestore key, ${BUCKET} bucket)"

# Assign + apply a single-node layout once (idempotent: skip if a zone
# is already configured).
if ! gexec layout show 2>/dev/null | grep -q "${ZONE}"; then
  node_id="$(gexec node id -q 2>/dev/null | cut -d@ -f1 | tr -d '\r')"
  [[ -n "${node_id}" ]] || die "could not resolve Garage node id"
  gexec layout assign "${node_id}" -z "${ZONE}" -c "${CAPACITY}"
  gexec layout apply --version 1
fi

# Import the cachestore S3 key from the Secret (idempotent), then grant
# create-bucket. The access key id is the import lookup key, so we match
# on it rather than the human-readable key name.
access_key="$(secret_value ORCA_CACHESTORE_S3_ACCESS_KEY)"
secret_key="$(secret_value ORCA_CACHESTORE_S3_SECRET_KEY)"
[[ -n "${access_key}" && -n "${secret_key}" ]] || die "cachestore S3 keys resolved empty from Secret ${SECRET_NAME}"

if ! gexec key list 2>/dev/null | grep -q "${access_key}"; then
  gexec key import "${access_key}" "${secret_key}" -n "${KEY_NAME}" --yes
fi
# Grant by the unique access key id, not the human-readable name: if the
# Secret's keys were ever regenerated, Garage ends up with multiple keys
# sharing the name "${KEY_NAME}", and a name-based grant is ambiguous (it can
# land on a stale key, leaving the key Orca actually uses unauthorized and
# Orca failing with a 403 on its first cachestore call). The access key id is
# unique, so granting on it always targets the key currently in the Secret.
gexec key allow --create-bucket "${access_key}" >/dev/null 2>&1 || true

# Ensure the cachestore bucket exists and is owned by the key.
gexec bucket info "${BUCKET}" >/dev/null 2>&1 || gexec bucket create "${BUCKET}"
gexec bucket allow --read --write --owner --key "${access_key}" "${BUCKET}" >/dev/null 2>&1 || true

# Verify the bucket is queryable before declaring success.
gexec bucket info "${BUCKET}" >/dev/null 2>&1 \
  || die "Garage bootstrap did not create ${BUCKET}"

log "Garage bootstrap complete (bucket ${BUCKET} ready)"
