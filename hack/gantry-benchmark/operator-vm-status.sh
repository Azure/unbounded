#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

set -u

CONFIG_FILE="${GANTRY_BENCHMARK_CONFIG:-/etc/gantry-benchmark/env}"
LOG_LINES="${GANTRY_BENCHMARK_STATUS_LOG_LINES:-25}"

if [[ -f "$CONFIG_FILE" ]]; then
  set -a
  # shellcheck source=/dev/null
  . "$CONFIG_FILE"
  set +a
fi

HOME="${BENCHMARK_OPERATOR_HOME:-/var/lib/gantry-benchmark}"
KUBECONFIG="${KUBECONFIG:-$HOME/kubeconfig}"
ARTIFACT_ROOT="${BENCHMARK_ARTIFACT_ROOT:-$HOME/artifacts}"
REPO_ROOT="${BENCHMARK_REPO_ROOT:-/opt/gantry-benchmark/unbounded}"
NAMESPACE="${BENCHMARK_NAMESPACE:-gantry-benchmark}"
GANTRY_NS="${GANTRY_NAMESPACE:-gantry-system}"

export HOME KUBECONFIG

service_state=$(systemctl is-active gantry-benchmark-operator.service 2>/dev/null || true)
service_state=${service_state:-unknown}
service_started=$(systemctl show gantry-benchmark-operator.service --property=ActiveEnterTimestamp --value 2>/dev/null || true)

state_json=""
if [[ -f "$KUBECONFIG" ]]; then
  state_json=$(kubectl -n "$NAMESPACE" get configmap gantry-benchmark-state \
    -o jsonpath='{.data.state\.json}' 2>/dev/null || true)
fi

progress_json=""
if [[ -f "$ARTIFACT_ROOT/progress.json" ]]; then
  progress_json=$(cat "$ARTIFACT_ROOT/progress.json")
fi

last_run_json=""
if [[ -f "$ARTIFACT_ROOT/last-run.json" ]]; then
  last_run_json=$(cat "$ARTIFACT_ROOT/last-run.json")
fi

run_id=$(jq -r '.run_id // empty' <<<"${state_json:-{}}" 2>/dev/null || true)
if [[ -z "$run_id" ]]; then
  run_id=$(jq -r '.run_id // empty' <<<"${progress_json:-{}}" 2>/dev/null || true)
fi
if [[ -z "$run_id" ]]; then
  run_id=$(jq -r '.run_id // empty' <<<"${last_run_json:-{}}" 2>/dev/null || true)
fi

printf '=== Gantry benchmark operator ===\n'
printf 'time: %s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)"
printf 'service: %s\n' "$service_state"
printf 'service started: %s\n' "${service_started:-unknown}"
printf 'run ID: %s\n' "${run_id:-none}"

if [[ -n "$progress_json" ]]; then
  jq -r '"stage: \(.stage // "unknown")\nstage started: \(.stage_started_at // "unknown")\nmessage: \(.message // "")"' <<<"$progress_json" 2>/dev/null || true
fi

if [[ -n "$state_json" ]]; then
  jq -r '"benchmark state: \(.status)\nnodes: \(.node_count)\nimage payload: \(.image_size_mib) MiB in \(.image_layers) layers\nAzure telemetry: \(.azure_telemetry)"' <<<"$state_json" 2>/dev/null || true
fi

if [[ -n "$run_id" ]]; then
  build_dir="$REPO_ROOT/tmp/gantry-benchmark/$run_id/build/shared-payload"
  if [[ -d "$build_dir" ]]; then
    payload_files=$(find "$build_dir" -maxdepth 1 -name 'payload*.bin' -type f 2>/dev/null | wc -l)
    if ((payload_files > 0)); then
      payload_bytes=$(du -sb "$build_dir" 2>/dev/null | awk '{print $1}')
      payload_target_mib=$(jq -r '.image_size_mib // 0' <<<"${state_json:-{}}" 2>/dev/null || echo 0)
      payload_target_bytes=$((payload_target_mib * 1024 * 1024))
      percent=0
      if ((payload_target_bytes > 0)); then
        percent=$((payload_bytes * 100 / payload_target_bytes))
        ((percent > 100)) && percent=100
      fi
      printf 'payload generation: %d/%s files, %d%% (%s bytes)\n' \
        "$payload_files" "$(jq -r '.image_layers // "?"' <<<"${state_json:-{}}" 2>/dev/null || echo '?')" "$percent" "$payload_bytes"
    fi
  fi
fi

build_process=$(pgrep -af 'podman (build|push)' 2>/dev/null || true)
if [[ -n "$build_process" ]]; then
  printf 'image process:\n%s\n' "$build_process"
fi

printf '\n=== VM resources ===\n'
df -h / 2>/dev/null | tail -1 || true
podman system df 2>/dev/null || true

if [[ -f "$KUBECONFIG" ]]; then
  printf '\n=== Kubernetes ===\n'
  kubectl -n "$NAMESPACE" get jobs \
    -o custom-columns=NAME:.metadata.name,ACTIVE:.status.active,SUCCEEDED:.status.succeeded,FAILED:.status.failed,COMPLETIONS:.spec.completions \
    2>/dev/null || true
  kubectl -n "$GANTRY_NS" get daemonset gantry \
    -o custom-columns=DESIRED:.status.desiredNumberScheduled,READY:.status.numberReady,UPDATED:.status.updatedNumberScheduled,AVAILABLE:.status.numberAvailable \
    2>/dev/null || true
fi

if [[ -n "$last_run_json" ]]; then
  printf '\n=== Last completed lifecycle ===\n'
  jq . <<<"$last_run_json" 2>/dev/null || true
fi

latest=$(readlink -f "$ARTIFACT_ROOT/latest" 2>/dev/null || true)
if [[ "$service_state" != active && "$service_state" != activating && -n "$latest" && -f "$latest/comparison.md" ]]; then
  printf '\n=== Latest comparison ===\n'
  cat "$latest/comparison.md"
fi

printf '\n=== Recent log ===\n'
tail -n "$LOG_LINES" /var/log/gantry-benchmark/service.log 2>/dev/null || true
