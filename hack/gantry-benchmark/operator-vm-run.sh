#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

set -Eeuo pipefail

CONFIG_FILE="${GANTRY_BENCHMARK_CONFIG:-/etc/gantry-benchmark/env}"
: "${CONFIG_FILE:?Set GANTRY_BENCHMARK_CONFIG}"
[[ -f "$CONFIG_FILE" ]] || { echo "missing benchmark config: $CONFIG_FILE" >&2; exit 1; }

set -a
# shellcheck source=/dev/null
. "$CONFIG_FILE"
set +a

: "${AZURE_SUBSCRIPTION_ID:?Set AZURE_SUBSCRIPTION_ID}"
: "${AZURE_RESOURCE_GROUP:?Set AZURE_RESOURCE_GROUP}"
: "${AZURE_AKS_CLUSTER_NAME:?Set AZURE_AKS_CLUSTER_NAME}"
: "${BASELINE_ACR_NAME:?Set BASELINE_ACR_NAME}"
: "${BASELINE_ACR_LOGIN_SERVER:?Set BASELINE_ACR_LOGIN_SERVER}"
: "${GANTRY_ACR_NAME:?Set GANTRY_ACR_NAME}"
: "${GANTRY_ACR_LOGIN_SERVER:?Set GANTRY_ACR_LOGIN_SERVER}"
: "${BENCHMARK_REPO_ROOT:?Set BENCHMARK_REPO_ROOT}"
: "${BENCHMARK_ARTIFACT_ROOT:?Set BENCHMARK_ARTIFACT_ROOT}"

export HOME="${BENCHMARK_OPERATOR_HOME:-/var/lib/gantry-benchmark}"
export KUBECONFIG="${KUBECONFIG:-$HOME/kubeconfig}"
mkdir -p "$HOME" "$BENCHMARK_ARTIFACT_ROOT"
chmod 0700 "$HOME"

LOG_FILE="$BENCHMARK_ARTIFACT_ROOT/operator.log"
exec > >(tee -a "$LOG_FILE") 2>&1

run_id=""
run_status=0
cleanup_started=false

write_progress() {
  local stage=$1
  local message=$2
  local temporary="$BENCHMARK_ARTIFACT_ROOT/.progress.json.tmp"

  jq -n \
    --arg run_id "$run_id" \
    --arg stage "$stage" \
    --arg message "$message" \
    --arg stage_started_at "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
    '{run_id:$run_id,stage:$stage,message:$message,stage_started_at:$stage_started_at}' \
    >"$temporary"
  mv "$temporary" "$BENCHMARK_ARTIFACT_ROOT/progress.json"
}

cleanup() {
  local original_status=$?
  if [[ "$cleanup_started" == true ]]; then
    return
  fi
  cleanup_started=true
  write_progress "cleanup" "restoring cluster state and preserving artifacts"

  unset BASELINE_ACR_PASSWORD GANTRY_ACR_PASSWORD baseline_refresh_token gantry_refresh_token aad_access_token
  podman logout "$BASELINE_ACR_LOGIN_SERVER" >/dev/null 2>&1 || true
  podman logout "$GANTRY_ACR_LOGIN_SERVER" >/dev/null 2>&1 || true

  if [[ -f "$KUBECONFIG" ]]; then
    if kubectl -n "${BENCHMARK_NAMESPACE:-gantry-benchmark}" get configmap gantry-benchmark-state >/dev/null 2>&1 || \
      kubectl -n "${GANTRY_NAMESPACE:-gantry-system}" get configmap gantry-benchmark-lock >/dev/null 2>&1; then
      echo "restoring benchmark cluster state"
      make -C "$BENCHMARK_REPO_ROOT/hack/gantry-benchmark" disable || original_status=$?
    fi
  fi

  if [[ -n "$run_id" && -d "$BENCHMARK_REPO_ROOT/tmp/gantry-benchmark/$run_id" ]]; then
    rm -rf "$BENCHMARK_ARTIFACT_ROOT/$run_id"
    cp -a "$BENCHMARK_REPO_ROOT/tmp/gantry-benchmark/$run_id" "$BENCHMARK_ARTIFACT_ROOT/$run_id"
    ln -sfn "$run_id" "$BENCHMARK_ARTIFACT_ROOT/latest"

    podman image rm \
      "$BASELINE_ACR_LOGIN_SERVER/${BENCHMARK_WORKLOAD_REPOSITORY:-gantry-benchmark-pull}:$run_id" \
      "$GANTRY_ACR_LOGIN_SERVER/${BENCHMARK_WORKLOAD_REPOSITORY:-gantry-benchmark-pull}:$run_id" \
      >/dev/null 2>&1 || true
  fi

  jq -n \
    --arg run_id "$run_id" \
    --arg finished_at "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
    --argjson exit_code "$original_status" \
    '{run_id:$run_id,finished_at:$finished_at,exit_code:$exit_code}' \
    >"$BENCHMARK_ARTIFACT_ROOT/last-run.json"

  if ((original_status == 0)); then
    write_progress "completed" "benchmark lifecycle completed successfully"
  else
    write_progress "failed" "benchmark lifecycle exited with code $original_status"
  fi

  return "$original_status"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

cd "$BENCHMARK_REPO_ROOT"

write_progress "authenticate" "authenticating managed identity and loading kubeconfig"
echo "authenticating operator VM managed identity"
az login --identity --allow-no-subscriptions --output none
az account set --subscription "$AZURE_SUBSCRIPTION_ID"

az aks get-credentials \
  --resource-group "$AZURE_RESOURCE_GROUP" \
  --name "$AZURE_AKS_CLUSTER_NAME" \
  --admin \
  --file "$KUBECONFIG" \
  --overwrite-existing \
  --only-show-errors
chmod 0600 "$KUBECONFIG"

kubectl auth can-i '*' '*' --all-namespaces | grep -qx yes
export BENCHMARK_CONFIRM_CONTEXT="$(kubectl config current-context)"
echo "using Kubernetes context $BENCHMARK_CONFIRM_CONTEXT"

if kubectl -n "${BENCHMARK_NAMESPACE:-gantry-benchmark}" get configmap gantry-benchmark-state >/dev/null 2>&1; then
  echo "an active benchmark state already exists; run disable before starting a new VM lifecycle" >&2
  exit 1
fi

write_progress "enable" "installing benchmark state, lock, and monitoring"
make -C hack/gantry-benchmark enable
run_id=$(kubectl -n "${BENCHMARK_NAMESPACE:-gantry-benchmark}" get configmap gantry-benchmark-state -o jsonpath='{.data.state\.json}' | jq -er '.run_id')
echo "enabled benchmark $run_id"
if [[ -n "${GANTRY_ONLY_BASELINE_RUN_ID:-}" ]]; then
  if [[ -n "${GANTRY_ONLY_ADOPT_IMAGE:-}" ]]; then
    : "${GANTRY_ONLY_ADOPT_PAYLOAD_SHA256:?Set GANTRY_ONLY_ADOPT_PAYLOAD_SHA256 with GANTRY_ONLY_ADOPT_IMAGE}"
    write_progress "prepare" "adopting an already-pushed fresh Gantry image against baseline $GANTRY_ONLY_BASELINE_RUN_ID"
  elif [[ "${GANTRY_ONLY_FRESH_IMAGE:-false}" == true ]]; then
    write_progress "prepare" "generating a brand-new random Gantry image against baseline $GANTRY_ONLY_BASELINE_RUN_ID"
  else
    write_progress "prepare" "rebuilding a cache-cold Gantry image from baseline $GANTRY_ONLY_BASELINE_RUN_ID"
  fi
else
  write_progress "prepare" "generating shared payload and pushing both private ACR images"
fi

tenant_id=$(az account show --query tenantId -o tsv)
aad_access_token=$(az account get-access-token --resource https://containerregistry.azure.net --query accessToken -o tsv)

baseline_refresh_token=$(curl -fsS -X POST \
  -H 'Content-Type: application/x-www-form-urlencoded' \
  --data-urlencode grant_type=access_token \
  --data-urlencode "service=$BASELINE_ACR_LOGIN_SERVER" \
  --data-urlencode "tenant=$tenant_id" \
  --data-urlencode "access_token=$aad_access_token" \
  "https://$BASELINE_ACR_LOGIN_SERVER/oauth2/exchange" | jq -er '.refresh_token')

gantry_refresh_token=$(curl -fsS -X POST \
  -H 'Content-Type: application/x-www-form-urlencoded' \
  --data-urlencode grant_type=access_token \
  --data-urlencode "service=$GANTRY_ACR_LOGIN_SERVER" \
  --data-urlencode "tenant=$tenant_id" \
  --data-urlencode "access_token=$aad_access_token" \
  "https://$GANTRY_ACR_LOGIN_SERVER/oauth2/exchange" | jq -er '.refresh_token')

unset aad_access_token
export BASELINE_ACR_PASSWORD="$baseline_refresh_token"
export GANTRY_ACR_PASSWORD="$gantry_refresh_token"
if [[ -n "${GANTRY_ONLY_BASELINE_RUN_ID:-}" ]]; then
  if [[ -n "${GANTRY_ONLY_ADOPT_IMAGE:-}" ]]; then
    make -C hack/gantry-benchmark prepare-gantry-adopt \
      GANTRY_ONLY_BASELINE_RUN_ID="$GANTRY_ONLY_BASELINE_RUN_ID" \
      GANTRY_ONLY_ADOPT_IMAGE="$GANTRY_ONLY_ADOPT_IMAGE" \
      GANTRY_ONLY_ADOPT_PAYLOAD_SHA256="$GANTRY_ONLY_ADOPT_PAYLOAD_SHA256"
  elif [[ "${GANTRY_ONLY_FRESH_IMAGE:-false}" == true ]]; then
    make -C hack/gantry-benchmark prepare-gantry-fresh \
      GANTRY_ONLY_BASELINE_RUN_ID="$GANTRY_ONLY_BASELINE_RUN_ID"
  else
    make -C hack/gantry-benchmark prepare-gantry \
      GANTRY_ONLY_BASELINE_RUN_ID="$GANTRY_ONLY_BASELINE_RUN_ID" \
      GANTRY_ONLY_PREPARED_RUN_ID="${GANTRY_ONLY_PREPARED_RUN_ID:-}"
  fi
else
  make -C hack/gantry-benchmark prepare
fi
unset BASELINE_ACR_PASSWORD GANTRY_ACR_PASSWORD baseline_refresh_token gantry_refresh_token
podman logout "$BASELINE_ACR_LOGIN_SERVER" >/dev/null 2>&1 || true
podman logout "$GANTRY_ACR_LOGIN_SERVER" >/dev/null 2>&1 || true

write_progress "preflight" "validating nodes, Gantry, monitoring, ACRs, and telemetry"
make -C hack/gantry-benchmark preflight
if [[ -n "${GANTRY_ONLY_BASELINE_RUN_ID:-}" ]]; then
  write_progress "run" "executing Gantry-only phase against baseline $GANTRY_ONLY_BASELINE_RUN_ID"
  make -C hack/gantry-benchmark run-gantry || run_status=$?
else
  write_progress "run" "executing baseline and Gantry phases"
  make -C hack/gantry-benchmark run || run_status=$?
fi

if [[ -f "$BENCHMARK_REPO_ROOT/tmp/gantry-benchmark/$run_id/comparison.md" ]]; then
  write_progress "report" "comparison generated; preparing cleanup"
  cat "$BENCHMARK_REPO_ROOT/tmp/gantry-benchmark/$run_id/comparison.md"
fi

exit "$run_status"
