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
RUN_CONFIG_FILE="${GANTRY_BENCHMARK_RUN_CONFIG:-}"
if [[ -n "$RUN_CONFIG_FILE" && -f "$RUN_CONFIG_FILE" ]]; then
  set -a
  # shellcheck source=/dev/null
  . "$RUN_CONFIG_FILE"
  set +a
fi
export ENV_FILE=/dev/null

: "${AZURE_SUBSCRIPTION_ID:?Set AZURE_SUBSCRIPTION_ID}"
: "${AZURE_RESOURCE_GROUP:?Set AZURE_RESOURCE_GROUP}"
: "${AZURE_AKS_CLUSTER_NAME:?Set AZURE_AKS_CLUSTER_NAME}"
: "${BASELINE_ACR_NAME:?Set BASELINE_ACR_NAME}"
: "${BASELINE_ACR_LOGIN_SERVER:?Set BASELINE_ACR_LOGIN_SERVER}"
: "${GANTRY_ACR_NAME:?Set GANTRY_ACR_NAME}"
: "${GANTRY_ACR_LOGIN_SERVER:?Set GANTRY_ACR_LOGIN_SERVER}"
: "${BENCHMARK_REPO_ROOT:?Set BENCHMARK_REPO_ROOT}"
: "${BENCHMARK_ARTIFACT_ROOT:?Set BENCHMARK_ARTIFACT_ROOT}"

BENCHMARK_PREPARE_ONLY="${BENCHMARK_PREPARE_ONLY:-false}"
[[ "$BENCHMARK_PREPARE_ONLY" == true || "$BENCHMARK_PREPARE_ONLY" == false ]] || {
  echo "BENCHMARK_PREPARE_ONLY must be true or false" >&2
  exit 2
}
lifecycle_mode=benchmark
[[ "$BENCHMARK_PREPARE_ONLY" != true ]] || lifecycle_mode=image-prepare

export HOME="${BENCHMARK_OPERATOR_HOME:-/var/lib/gantry-benchmark}"
export KUBECONFIG="${KUBECONFIG:-$HOME/kubeconfig}"
mkdir -p "$HOME" "$BENCHMARK_ARTIFACT_ROOT"
chmod 0700 "$HOME"

LOG_FILE="$BENCHMARK_ARTIFACT_ROOT/operator.log"
exec > >(tee -a "$LOG_FILE") 2>&1

log() { printf '%s [operator] %s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$*"; }

LOCK_FILE="${BENCHMARK_LIFECYCLE_LOCK:-$HOME/benchmark-lifecycle.lock}"
exec {lifecycle_lock_fd}>"$LOCK_FILE"
if ! flock -n "$lifecycle_lock_fd"; then
  log "another benchmark or image-pool lifecycle owns $LOCK_FILE"
  exit 1
fi

run_id=""
run_status=0
cleanup_started=false

write_progress() {
  local stage=$1
  local message=$2
  local temporary="$BENCHMARK_ARTIFACT_ROOT/.progress.json.tmp"

  jq -n \
    --arg run_id "$run_id" \
    --arg mode "$lifecycle_mode" \
    --arg stage "$stage" \
    --arg message "$message" \
    --arg stage_started_at "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
    '{run_id:$run_id,mode:$mode,stage:$stage,message:$message,stage_started_at:$stage_started_at}' \
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
      log "restoring benchmark cluster state"
      make -C "$BENCHMARK_REPO_ROOT/hack/gantry-benchmark" disable || original_status=$?
    fi
  fi

  if [[ -n "$run_id" && -d "$BENCHMARK_REPO_ROOT/tmp/gantry-benchmark/$run_id" ]]; then
    rm -rf "$BENCHMARK_ARTIFACT_ROOT/$run_id"
    install -d -m 0750 "$BENCHMARK_ARTIFACT_ROOT/$run_id"
    find "$BENCHMARK_REPO_ROOT/tmp/gantry-benchmark/$run_id" -maxdepth 1 -type f \
      -exec cp -a -- {} "$BENCHMARK_ARTIFACT_ROOT/$run_id/" \;
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
    if [[ "$BENCHMARK_PREPARE_ONLY" == true ]]; then
      write_progress "completed" "benchmark images prepared successfully"
    else
      write_progress "completed" "benchmark lifecycle completed successfully"
    fi
  else
    if [[ "$BENCHMARK_PREPARE_ONLY" == true ]]; then
      write_progress "failed" "benchmark image preparation exited with code $original_status"
    else
      write_progress "failed" "benchmark lifecycle exited with code $original_status"
    fi
  fi

  return "$original_status"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

cd "$BENCHMARK_REPO_ROOT"

restore_gantry_only_baseline() {
  [[ -n "${GANTRY_ONLY_BASELINE_RUN_ID:-}" ]] || return 0
  [[ "$(basename "$GANTRY_ONLY_BASELINE_RUN_ID")" == "$GANTRY_ONLY_BASELINE_RUN_ID" ]] || {
    echo "invalid GANTRY_ONLY_BASELINE_RUN_ID=$GANTRY_ONLY_BASELINE_RUN_ID" >&2
    return 1
  }

  local source="$BENCHMARK_ARTIFACT_ROOT/$GANTRY_ONLY_BASELINE_RUN_ID"
  local destination="$BENCHMARK_REPO_ROOT/tmp/gantry-benchmark/$GANTRY_ONLY_BASELINE_RUN_ID"
  [[ -s "$source/state.json" && -s "$source/baseline.json" ]] || {
    echo "retained baseline $GANTRY_ONLY_BASELINE_RUN_ID is missing state.json or baseline.json under $source" >&2
    return 1
  }

  install -d -m 0750 "$destination"
  install -m 0640 "$source/state.json" "$source/baseline.json" "$destination/"
  log "restored retained baseline metadata for $GANTRY_ONLY_BASELINE_RUN_ID"
}

restore_gantry_only_baseline

[[ "${GANTRY_ONLY_USE_IMAGE_POOL:-false}" == true || "${GANTRY_ONLY_USE_IMAGE_POOL:-false}" == false ]] || {
  echo "GANTRY_ONLY_USE_IMAGE_POOL must be true or false" >&2
  exit 2
}
[[ "${GANTRY_ONLY_FRESH_IMAGE:-false}" == true || "${GANTRY_ONLY_FRESH_IMAGE:-false}" == false ]] || {
  echo "GANTRY_ONLY_FRESH_IMAGE must be true or false" >&2
  exit 2
}
if [[ -n "${GANTRY_ONLY_BASELINE_RUN_ID:-}" ]]; then
  preparation_modes=0
  [[ -z "${GANTRY_ONLY_ADOPT_IMAGE:-}" ]] || ((preparation_modes += 1))
  [[ "${GANTRY_ONLY_USE_IMAGE_POOL:-false}" != true ]] || ((preparation_modes += 1))
  [[ "${GANTRY_ONLY_FRESH_IMAGE:-false}" != true ]] || ((preparation_modes += 1))
  [[ -z "${GANTRY_ONLY_PREPARED_RUN_ID:-}" ]] || ((preparation_modes += 1))
  ((preparation_modes <= 1)) || {
    echo "set only one of GANTRY_ONLY_ADOPT_IMAGE, GANTRY_ONLY_USE_IMAGE_POOL, GANTRY_ONLY_FRESH_IMAGE, or GANTRY_ONLY_PREPARED_RUN_ID" >&2
    exit 2
  }
fi
if [[ "$BENCHMARK_PREPARE_ONLY" == true ]]; then
  [[ -z "${GANTRY_ONLY_BASELINE_RUN_ID:-}" ]] || {
    echo "BENCHMARK_PREPARE_ONLY cannot be combined with a Gantry-only run" >&2
    exit 2
  }
  [[ -z "${ADOPT_BASELINE_IMAGE:-}" && -z "${ADOPT_GANTRY_IMAGE:-}" && -z "${ADOPT_PAYLOAD_SHA256:-}" ]] || {
    echo "BENCHMARK_PREPARE_ONLY requires fresh images rather than adopted images" >&2
    exit 2
  }
fi

write_progress "authenticate" "authenticating managed identity and loading kubeconfig"
log "authenticating operator VM managed identity"
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
log "using Kubernetes context $BENCHMARK_CONFIRM_CONTEXT"

if kubectl -n "${BENCHMARK_NAMESPACE:-gantry-benchmark}" get configmap gantry-benchmark-state >/dev/null 2>&1; then
  echo "an active benchmark state already exists; run disable before starting a new VM lifecycle" >&2
  exit 1
fi

write_progress "enable" "installing benchmark state, lock, and monitoring"
make -C hack/gantry-benchmark enable
run_id=$(kubectl -n "${BENCHMARK_NAMESPACE:-gantry-benchmark}" get configmap gantry-benchmark-state -o jsonpath='{.data.state\.json}' | jq -er '.run_id')
log "enabled benchmark $run_id"
if [[ -n "${ADOPT_BASELINE_IMAGE:-}" || -n "${ADOPT_GANTRY_IMAGE:-}" || -n "${ADOPT_PAYLOAD_SHA256:-}" ]]; then
  : "${ADOPT_BASELINE_IMAGE:?Set ADOPT_BASELINE_IMAGE with the full adoption set}"
  : "${ADOPT_GANTRY_IMAGE:?Set ADOPT_GANTRY_IMAGE with the full adoption set}"
  : "${ADOPT_PAYLOAD_SHA256:?Set ADOPT_PAYLOAD_SHA256 with the full adoption set}"
  write_progress "prepare" "adopting already-pushed identical-payload images"
elif [[ -n "${GANTRY_ONLY_BASELINE_RUN_ID:-}" ]]; then
  if [[ -n "${GANTRY_ONLY_ADOPT_IMAGE:-}" ]]; then
    : "${GANTRY_ONLY_ADOPT_PAYLOAD_SHA256:?Set GANTRY_ONLY_ADOPT_PAYLOAD_SHA256 with GANTRY_ONLY_ADOPT_IMAGE}"
    write_progress "prepare" "adopting an already-pushed fresh Gantry image against baseline $GANTRY_ONLY_BASELINE_RUN_ID"
  elif [[ "${GANTRY_ONLY_USE_IMAGE_POOL:-false}" == true ]]; then
    write_progress "prepare" "claiming a prebuilt Gantry image against baseline $GANTRY_ONLY_BASELINE_RUN_ID"
  elif [[ "${GANTRY_ONLY_FRESH_IMAGE:-false}" == true ]]; then
    write_progress "prepare" "generating a brand-new random Gantry image against baseline $GANTRY_ONLY_BASELINE_RUN_ID"
  else
    write_progress "prepare" "rebuilding a cache-cold Gantry image from baseline $GANTRY_ONLY_BASELINE_RUN_ID"
  fi
else
  write_progress "prepare" "generating shared payload and pushing both private ACR images"
fi

needs_baseline_credentials=false
needs_gantry_credentials=false
if [[ -z "${ADOPT_BASELINE_IMAGE:-}" ]]; then
  if [[ -z "${GANTRY_ONLY_BASELINE_RUN_ID:-}" ]]; then
    needs_baseline_credentials=true
    needs_gantry_credentials=true
  elif [[ -z "${GANTRY_ONLY_ADOPT_IMAGE:-}" && "${GANTRY_ONLY_USE_IMAGE_POOL:-false}" != true && -z "${GANTRY_ONLY_PREPARED_RUN_ID:-}" ]]; then
    needs_gantry_credentials=true
  fi
fi

if [[ "$needs_baseline_credentials" == true || "$needs_gantry_credentials" == true ]]; then
  tenant_id=$(az account show --query tenantId -o tsv)
  aad_access_token=$(az account get-access-token --resource https://containerregistry.azure.net --query accessToken -o tsv)

  if [[ "$needs_baseline_credentials" == true ]]; then
    baseline_refresh_token=$(curl -fsS -X POST \
      -H 'Content-Type: application/x-www-form-urlencoded' \
      --data-urlencode grant_type=access_token \
      --data-urlencode "service=$BASELINE_ACR_LOGIN_SERVER" \
      --data-urlencode "tenant=$tenant_id" \
      --data-urlencode "access_token=$aad_access_token" \
      "https://$BASELINE_ACR_LOGIN_SERVER/oauth2/exchange" | jq -er '.refresh_token')
    export BASELINE_ACR_PASSWORD="$baseline_refresh_token"
  fi

  if [[ "$needs_gantry_credentials" == true ]]; then
    gantry_refresh_token=$(curl -fsS -X POST \
      -H 'Content-Type: application/x-www-form-urlencoded' \
      --data-urlencode grant_type=access_token \
      --data-urlencode "service=$GANTRY_ACR_LOGIN_SERVER" \
      --data-urlencode "tenant=$tenant_id" \
      --data-urlencode "access_token=$aad_access_token" \
      "https://$GANTRY_ACR_LOGIN_SERVER/oauth2/exchange" | jq -er '.refresh_token')
    export GANTRY_ACR_PASSWORD="$gantry_refresh_token"
  fi

  unset aad_access_token
fi

if [[ -n "${ADOPT_BASELINE_IMAGE:-}" ]]; then
  make -C hack/gantry-benchmark prepare-adopt \
    ADOPT_BASELINE_IMAGE="$ADOPT_BASELINE_IMAGE" \
    ADOPT_GANTRY_IMAGE="$ADOPT_GANTRY_IMAGE" \
    ADOPT_PAYLOAD_SHA256="$ADOPT_PAYLOAD_SHA256"
elif [[ -n "${GANTRY_ONLY_BASELINE_RUN_ID:-}" ]]; then
  if [[ -n "${GANTRY_ONLY_ADOPT_IMAGE:-}" ]]; then
    make -C hack/gantry-benchmark prepare-gantry-adopt \
      GANTRY_ONLY_BASELINE_RUN_ID="$GANTRY_ONLY_BASELINE_RUN_ID" \
      GANTRY_ONLY_ADOPT_IMAGE="$GANTRY_ONLY_ADOPT_IMAGE" \
      GANTRY_ONLY_ADOPT_PAYLOAD_SHA256="$GANTRY_ONLY_ADOPT_PAYLOAD_SHA256"
  elif [[ "${GANTRY_ONLY_USE_IMAGE_POOL:-false}" == true ]]; then
    make -C hack/gantry-benchmark prepare-gantry-pool \
      GANTRY_ONLY_BASELINE_RUN_ID="$GANTRY_ONLY_BASELINE_RUN_ID"
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

if [[ "$BENCHMARK_PREPARE_ONLY" == true ]]; then
  write_progress "prepared" "both benchmark images were pushed; preparing cleanup"
  exit 0
fi

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
