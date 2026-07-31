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

cleanup() {
  local original_status=$?
  if [[ "$cleanup_started" == true ]]; then
    return
  fi
  cleanup_started=true

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

  return "$original_status"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

cd "$BENCHMARK_REPO_ROOT"

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

make -C hack/gantry-benchmark enable
run_id=$(kubectl -n "${BENCHMARK_NAMESPACE:-gantry-benchmark}" get configmap gantry-benchmark-state -o jsonpath='{.data.state\.json}' | jq -er '.run_id')
echo "enabled benchmark $run_id"

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
make -C hack/gantry-benchmark prepare
unset BASELINE_ACR_PASSWORD GANTRY_ACR_PASSWORD baseline_refresh_token gantry_refresh_token
podman logout "$BASELINE_ACR_LOGIN_SERVER" >/dev/null 2>&1 || true
podman logout "$GANTRY_ACR_LOGIN_SERVER" >/dev/null 2>&1 || true

make -C hack/gantry-benchmark preflight
make -C hack/gantry-benchmark run || run_status=$?

if [[ -f "$BENCHMARK_REPO_ROOT/tmp/gantry-benchmark/$run_id/comparison.md" ]]; then
  cat "$BENCHMARK_REPO_ROOT/tmp/gantry-benchmark/$run_id/comparison.md"
fi

exit "$run_status"
