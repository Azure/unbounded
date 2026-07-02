#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.
#
# smoke-integration.sh - Functional end-to-end smoke test for an Orca
# deploy on an integration cluster (real Azure Blob origin + Garage
# cachestore).
#
# It generates a large random object, uploads it to the Azure origin
# container, then fetches it through Orca's edge twice:
#   - COLD: first GET triggers an origin fetch + chunk commit into Garage.
#   - WARM: second GET is served from the Garage cachestore.
# Both responses are SHA-256 verified against the source. A ranged GET is
# also checked (206 Partial Content), and the Garage cachestore bucket is
# inspected to confirm chunks landed.
#
# A 256 MiB default object size exercises Orca's chunking (8 MiB base
# chunk => ~32 chunks) and range path far better than a tiny blob.
#
# Origin account / container and the Azure account key are resolved from
# the cluster by default (orca-config ConfigMap + orca-credentials
# Secret), so no values need to be passed for a standard install.
#
# Requires: kubectl, az (Azure CLI), curl, dd, and a SHA-256 tool
# (sha256sum, shasum, or openssl).
#
# Usage: smoke-integration.sh [flags]
#
#   --context CTX        kubectl context to target (default: current)
#   --namespace NS       namespace Orca is deployed in (default: unbounded-kube)
#   --size-mib N         object size in MiB (default: 256)
#   --key NAME           object key/blob name (default: smoke-<N>mib-<ts>.bin)
#   --account NAME       Azure storage account (default: from orca-config)
#   --container NAME     Azure blob container (default: from orca-config)
#   --account-key KEY    Azure account key (default: from orca-credentials Secret)
#   --local-port PORT    local port for the orca edge port-forward (default: 8443)
#   --keep               keep the generated local file and uploaded blob
#   -h | --help          show this help

set -euo pipefail

CONTEXT=""
NAMESPACE="unbounded-kube"
SIZE_MIB=256
KEY=""
ACCOUNT=""
CONTAINER=""
ACCOUNT_KEY=""
LOCAL_PORT=8443
KEEP=0

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
    --size-mib)    SIZE_MIB="$2"; shift 2 ;;
    --key)         KEY="$2"; shift 2 ;;
    --account)     ACCOUNT="$2"; shift 2 ;;
    --container)   CONTAINER="$2"; shift 2 ;;
    --account-key) ACCOUNT_KEY="$2"; shift 2 ;;
    --local-port)  LOCAL_PORT="$2"; shift 2 ;;
    --keep)        KEEP=1; shift ;;
    -h|--help)     usage 0 ;;
    *)             err "unknown flag: $1"; usage 1 ;;
  esac
done

[[ "${SIZE_MIB}" =~ ^[0-9]+$ ]] || die "--size-mib must be an integer (got ${SIZE_MIB})"

for bin in kubectl az curl dd; do
  command -v "${bin}" >/dev/null 2>&1 || die "${bin} is required"
done

# SHA-256 helper: reads stdin, prints the hex digest.
sha256() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum | awk '{print $1}'
  elif command -v shasum >/dev/null 2>&1; then
    shasum -a 256 | awk '{print $1}'
  elif command -v openssl >/dev/null 2>&1; then
    openssl dgst -sha256 | awk '{print $NF}'
  else
    die "no SHA-256 tool found (need sha256sum, shasum, or openssl)"
  fi
}

kubectl_ctx() {
  if [[ -n "${CONTEXT}" ]]; then
    kubectl --context "${CONTEXT}" "$@"
  else
    kubectl "$@"
  fi
}

# -----------------------------------------------------------------------------
# Resolve origin account / container / key from the cluster if not given.

if [[ -z "${ACCOUNT}" || -z "${CONTAINER}" ]]; then
  cfg="$(kubectl_ctx -n "${NAMESPACE}" get cm orca-config -o 'jsonpath={.data.config\.yaml}' 2>/dev/null || true)"
  [[ -n "${cfg}" ]] || die "could not read orca-config ConfigMap; pass --account and --container explicitly"
  [[ -n "${ACCOUNT}" ]]   || ACCOUNT="$(printf '%s\n' "${cfg}" | awk '/^[[:space:]]*account:/{print $2; exit}' | tr -d '"')"
  [[ -n "${CONTAINER}" ]] || CONTAINER="$(printf '%s\n' "${cfg}" | awk '/^[[:space:]]*container:/{print $2; exit}' | tr -d '"')"
fi
[[ -n "${ACCOUNT}" ]]   || die "Azure account not resolved; pass --account"
[[ -n "${CONTAINER}" ]] || die "Azure container not resolved; pass --container"

if [[ -z "${ACCOUNT_KEY}" ]]; then
  enc="$(kubectl_ctx -n "${NAMESPACE}" get secret orca-credentials -o 'jsonpath={.data.ORCA_AZUREBLOB_ACCOUNT_KEY}' 2>/dev/null || true)"
  [[ -n "${enc}" ]] || die "could not read ORCA_AZUREBLOB_ACCOUNT_KEY from orca-credentials Secret; pass --account-key"
  ACCOUNT_KEY="$(printf '%s' "${enc}" | base64 -d)"
fi

if [[ -z "${KEY}" ]]; then
  KEY="smoke-${SIZE_MIB}mib-$(date +%Y%m%d-%H%M%S).bin"
fi

# -----------------------------------------------------------------------------
# Temp file + cleanup.

workdir="$(mktemp -d)"
srcfile="${workdir}/${KEY}"
PF_PID=""

cleanup() {
  [[ -n "${PF_PID}" ]] && kill "${PF_PID}" >/dev/null 2>&1 || true
  if [[ "${KEEP}" == "1" ]]; then
    log "Keeping local file ${srcfile} and blob ${CONTAINER}/${KEY} (--keep)"
  else
    rm -rf "${workdir}"
    az storage blob delete \
      --account-name "${ACCOUNT}" --account-key "${ACCOUNT_KEY}" --auth-mode key \
      --container-name "${CONTAINER}" --name "${KEY}" --only-show-errors >/dev/null 2>&1 || true
  fi
}
trap cleanup EXIT

# -----------------------------------------------------------------------------
# Generate the source object and record its checksum.

log "Generating ${SIZE_MIB} MiB random object (${KEY})"
dd if=/dev/urandom of="${srcfile}" bs=1M count="${SIZE_MIB}" status=none
SRC_SUM="$(sha256 < "${srcfile}")"
SRC_BYTES="$((SIZE_MIB * 1024 * 1024))"
log "Source sha256=${SRC_SUM} size=${SRC_BYTES}"

# -----------------------------------------------------------------------------
# Upload to the Azure origin container.

log "Uploading to Azure: account=${ACCOUNT} container=${CONTAINER} blob=${KEY}"
az storage blob upload \
  --account-name "${ACCOUNT}" --account-key "${ACCOUNT_KEY}" --auth-mode key \
  --container-name "${CONTAINER}" --name "${KEY}" --file "${srcfile}" \
  --overwrite --only-show-errors >/dev/null

# -----------------------------------------------------------------------------
# Port-forward the orca edge.

log "Port-forwarding svc/orca ${LOCAL_PORT}:8443"
kubectl_ctx -n "${NAMESPACE}" port-forward svc/orca "${LOCAL_PORT}:8443" >"${workdir}/pf.log" 2>&1 &
PF_PID=$!

url="http://localhost:${LOCAL_PORT}/${CONTAINER}/${KEY}"

# Wait for the forward to accept connections. GET / returns 501
# (ListBuckets not supported), which still proves the listener is up.
ok=0
for _ in $(seq 1 30); do
  code="$(curl -s -o /dev/null -w '%{http_code}' "http://localhost:${LOCAL_PORT}/" || true)"
  if [[ -n "${code}" && "${code}" != "000" ]]; then ok=1; break; fi
  sleep 1
done
[[ "${ok}" == "1" ]] || { cat "${workdir}/pf.log" >&2; die "orca edge port-forward did not come up"; }

# -----------------------------------------------------------------------------
# COLD fetch (origin -> cache commit).

log "COLD GET (origin fetch + cache commit)"
t0="$(date +%s.%N)"
cold_sum="$(curl -fsS "${url}" | sha256)"
t1="$(date +%s.%N)"
cold_secs="$(awk -v a="${t0}" -v b="${t1}" 'BEGIN{printf "%.2f", b-a}')"
[[ "${cold_sum}" == "${SRC_SUM}" ]] || die "COLD checksum mismatch (got ${cold_sum}, want ${SRC_SUM})"
log "COLD ok in ${cold_secs}s (sha256 matches)"

# -----------------------------------------------------------------------------
# WARM fetch (served from Garage cachestore).

log "WARM GET (cachestore hit)"
t0="$(date +%s.%N)"
warm_sum="$(curl -fsS "${url}" | sha256)"
t1="$(date +%s.%N)"
warm_secs="$(awk -v a="${t0}" -v b="${t1}" 'BEGIN{printf "%.2f", b-a}')"
[[ "${warm_sum}" == "${SRC_SUM}" ]] || die "WARM checksum mismatch (got ${warm_sum}, want ${SRC_SUM})"
log "WARM ok in ${warm_secs}s (sha256 matches)"

# -----------------------------------------------------------------------------
# Ranged GET (exercise chunked range serving): first 1 MiB -> 206.

log "RANGE GET bytes=0-1048575 (expect 206)"
range_code="$(curl -fsS -o /dev/null -w '%{http_code}' -H 'Range: bytes=0-1048575' "${url}")"
[[ "${range_code}" == "206" ]] || die "ranged GET returned ${range_code}, want 206"
log "RANGE ok (206 Partial Content)"

# -----------------------------------------------------------------------------
# Confirm chunks landed in Garage.

log "Garage cachestore (orca-cache) bucket info:"
kubectl_ctx -n "${NAMESPACE}" exec deploy/garage -- \
  /garage -c /etc/garage.toml bucket info orca-cache >&2 || true

cat >&2 <<EOF

SMOKE PASSED
  object:    ${CONTAINER}/${KEY} (${SIZE_MIB} MiB)
  sha256:    ${SRC_SUM}
  cold GET:  ${cold_secs}s (origin fetch + commit)
  warm GET:  ${warm_secs}s (cachestore hit)
  range GET: 206 OK
EOF
