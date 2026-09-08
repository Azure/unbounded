#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0

set -Eeuo pipefail

CONFIG_FILE="${GANTRY_BENCHMARK_CONFIG:-/etc/gantry-benchmark/env}"
: "${CONFIG_FILE:?Set GANTRY_BENCHMARK_CONFIG}"
[[ -f "$CONFIG_FILE" ]] || { echo "missing benchmark config: $CONFIG_FILE" >&2; exit 1; }

set -a
# shellcheck source=/dev/null
. "$CONFIG_FILE"
set +a

: "${AZURE_SUBSCRIPTION_ID:?Set AZURE_SUBSCRIPTION_ID}"
: "${GANTRY_ACR_NAME:?Set GANTRY_ACR_NAME}"
: "${GANTRY_ACR_LOGIN_SERVER:?Set GANTRY_ACR_LOGIN_SERVER}"
: "${GANTRY_ACR_USERNAME:?Set GANTRY_ACR_USERNAME}"
: "${BENCHMARK_REPO_ROOT:?Set BENCHMARK_REPO_ROOT}"
: "${BENCHMARK_OPERATOR_HOME:?Set BENCHMARK_OPERATOR_HOME}"
: "${GANTRY_IMAGE_POOL_COUNT:?Set GANTRY_IMAGE_POOL_COUNT}"

[[ "$GANTRY_IMAGE_POOL_COUNT" =~ ^[1-9][0-9]*$ ]] || {
  echo "GANTRY_IMAGE_POOL_COUNT must be a positive integer" >&2
  exit 2
}
((GANTRY_IMAGE_POOL_COUNT <= 100)) || {
  echo "GANTRY_IMAGE_POOL_COUNT must not exceed 100" >&2
  exit 2
}

export HOME="$BENCHMARK_OPERATOR_HOME"
export KUBECONFIG="${KUBECONFIG:-$HOME/kubeconfig}"
export ENV_FILE=/dev/null
mkdir -p "$HOME"
chmod 0700 "$HOME"

PROGRESS_FILE="${BENCHMARK_IMAGE_POOL_PROGRESS:-$HOME/image-pool-progress.json}"
LOG_FILE="${BENCHMARK_IMAGE_POOL_LOG:-$HOME/image-pool-builder.log}"
exec > >(tee -a "$LOG_FILE") 2>&1

LOCK_FILE="${BENCHMARK_LIFECYCLE_LOCK:-$HOME/benchmark-lifecycle.lock}"
exec {lifecycle_lock_fd}>"$LOCK_FILE"
if ! flock -n "$lifecycle_lock_fd"; then
  echo "another benchmark or image-pool lifecycle owns $LOCK_FILE" >&2
  exit 1
fi

write_progress() {
  local stage=$1
  local message=$2
  local temporary="${PROGRESS_FILE}.tmp"

  jq -n \
    --arg stage "$stage" \
    --arg message "$message" \
    --arg updated_at "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
    --argjson requested "$GANTRY_IMAGE_POOL_COUNT" \
    '{stage:$stage,message:$message,requested:$requested,updated_at:$updated_at}' \
    >"$temporary"
  mv "$temporary" "$PROGRESS_FILE"
}

cleanup() {
  local status=$?

  unset GANTRY_ACR_PASSWORD gantry_refresh_token aad_access_token
  podman logout "$GANTRY_ACR_LOGIN_SERVER" >/dev/null 2>&1 || true

  if ((status == 0)); then
    write_progress completed "prebuilt image batch completed"
  else
    write_progress failed "prebuilt image batch exited with code $status"
  fi

  return "$status"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

if [[ -f "$KUBECONFIG" ]] && kubectl -n "${BENCHMARK_NAMESPACE:-gantry-benchmark}" get configmap gantry-benchmark-state >/dev/null 2>&1; then
  echo "an active benchmark state exists; pool pushes would contaminate ACR telemetry" >&2
  exit 1
fi

write_progress authenticate "authenticating managed identity once for the image batch"
echo "$(date -u +%Y-%m-%dT%H:%M:%SZ) [image-pool] authenticating managed identity"
az login --identity --allow-no-subscriptions --output none
az account set --subscription "$AZURE_SUBSCRIPTION_ID"

tenant_id=$(az account show --query tenantId -o tsv)
aad_access_token=$(az account get-access-token --resource https://containerregistry.azure.net --query accessToken -o tsv)
gantry_refresh_token=$(curl -fsS -X POST \
  -H 'Content-Type: application/x-www-form-urlencoded' \
  --data-urlencode grant_type=access_token \
  --data-urlencode "service=$GANTRY_ACR_LOGIN_SERVER" \
  --data-urlencode "tenant=$tenant_id" \
  --data-urlencode "access_token=$aad_access_token" \
  "https://$GANTRY_ACR_LOGIN_SERVER/oauth2/exchange" | jq -er '.refresh_token')
unset aad_access_token
export GANTRY_ACR_PASSWORD="$gantry_refresh_token"

write_progress build "building and pushing $GANTRY_IMAGE_POOL_COUNT reusable Gantry images"
cd "$BENCHMARK_REPO_ROOT"
make -C hack/gantry-benchmark prebuild-gantry \
  ENV_FILE=/dev/null \
  GANTRY_IMAGE_POOL_COUNT="$GANTRY_IMAGE_POOL_COUNT"
