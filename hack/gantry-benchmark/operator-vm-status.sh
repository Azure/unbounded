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

build_process=$(pgrep -af 'podman (pull|build|push|create|cp)' 2>/dev/null || true)
completed_image_steps=0
active_image_operation=""

print_prepared_image_status() {
  local label=$1
  local phase=$2
  local target=$3
  local build_dir=$4
  local digest_file="$build_dir/push-digest.$phase.txt"
  local build_state=waiting
  local push_state=waiting
  local digest=""
  local layers="unknown"
  local image_bytes="unknown"
  local image_size="unknown"
  local active_pid=""
  local elapsed=""

  if podman image exists "$target" 2>/dev/null; then
    build_state=complete
    layers=$(podman image inspect --format '{{ len .RootFS.Layers }}' "$target" 2>/dev/null || printf 'unknown')
    image_bytes=$(podman image inspect --format '{{ .Size }}' "$target" 2>/dev/null || printf 'unknown')
    if [[ "$image_bytes" =~ ^[0-9]+$ ]]; then
      image_size=$(numfmt --to=iec-i --suffix=B "$image_bytes" 2>/dev/null || printf '%s bytes' "$image_bytes")
    fi
  fi
  if grep -Fq "podman build" <<<"$build_process" && grep -Fq -- "--tag $target" <<<"$build_process"; then
    build_state=active
    active_pid=$(awk -v target="$target" 'index($0, "podman build") && index($0, target) {print $1; exit}' <<<"$build_process")
    active_image_operation="$label build"
  fi
  if grep -Fq "podman push" <<<"$build_process" && grep -Fq "$target" <<<"$build_process"; then
    build_state=complete
    push_state=active
    active_pid=$(awk -v target="$target" 'index($0, "podman push") && index($0, target) {print $1; exit}' <<<"$build_process")
    active_image_operation="$label push"
  fi
  if [[ -s "$digest_file" ]]; then
    digest=$(tr -d '[:space:]' <"$digest_file")
    build_state=complete
    push_state=complete
  fi
  if [[ -n "$active_pid" ]]; then
    elapsed=$(ps -o etime= -p "$active_pid" 2>/dev/null | xargs)
  fi

  [[ "$build_state" == complete ]] && ((completed_image_steps += 1))
  [[ "$push_state" == complete ]] && ((completed_image_steps += 1))

  printf '%s image:\n' "$label"
  printf '  target: %s\n' "$target"
  printf '  size: %s (%s bytes)\n' "$image_size" "$image_bytes"
  printf '  build: %s (%s image layers)\n' "$build_state" "$layers"
  printf '  push: %s' "$push_state"
  [[ -z "$elapsed" ]] || printf ' (elapsed %s)' "$elapsed"
  printf '\n'
  if [[ -n "$digest" ]]; then
    printf '  digest: %s\n' "$digest"
  fi
}

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
      payload_target_mib=0
      payload_layers="?"
      if [[ -n "$state_json" ]]; then
        payload_target_mib=$(jq -r '.image_size_mib // 0' <<<"$state_json" 2>/dev/null) || payload_target_mib=0
        payload_layers=$(jq -r '.image_layers // "?"' <<<"$state_json" 2>/dev/null) || payload_layers="?"
      fi
      payload_target_bytes=$((payload_target_mib * 1024 * 1024))
      percent=0
      if ((payload_target_bytes > 0)); then
        percent=$((payload_bytes * 100 / payload_target_bytes))
        ((percent > 100)) && percent=100
      fi
      printf 'payload generation: %d/%s files, %d%% (%s bytes)\n' \
        "$payload_files" "$payload_layers" "$percent" "$payload_bytes"
    fi

    image_tag=${run_id//_/-}
    workload_repository=${BENCHMARK_WORKLOAD_REPOSITORY:-gantry-benchmark-pull}
    printf '\n=== Image preparation ===\n'
    print_prepared_image_status \
      baseline baseline "$BASELINE_ACR_LOGIN_SERVER/$workload_repository:$image_tag" "$build_dir"
    print_prepared_image_status \
      Gantry-cold gantry_cold "$GANTRY_ACR_LOGIN_SERVER/$workload_repository:$image_tag" "$build_dir"
    printf 'image steps: %d/4 complete\n' "$completed_image_steps"
    [[ -z "$active_image_operation" ]] || printf 'active image operation: %s\n' "$active_image_operation"
    if [[ "$active_image_operation" == *" push" ]]; then
      printf 'push byte progress: unavailable from Podman 4.9; total image size and elapsed time shown above\n'
    fi
  fi
fi

if [[ -n "$build_process" ]]; then
  printf 'image process:\n%s\n' "$build_process"
fi

benchmark_process=$(pgrep -af 'gantry-benchmark (prepare|preflight|run)' 2>/dev/null || true)
if [[ -n "$benchmark_process" ]]; then
  printf 'benchmark process:\n%s\n' "$benchmark_process"
fi

printf '\n=== VM resources ===\n'
df -h / "$REPO_ROOT" 2>/dev/null | awk 'NR == 1 || !seen[$1]++' || true
podman info --format 'Podman graph root: {{.Store.GraphRoot}}' 2>/dev/null || true
podman system df 2>/dev/null || true

if [[ -f "$KUBECONFIG" ]]; then
  printf '\n=== Kubernetes ===\n'
  jobs_json=$(kubectl -n "$NAMESPACE" get jobs -o json 2>/dev/null || true)
  if [[ -n "$jobs_json" ]]; then
    jq -r --argjson now "$(date -u +%s)" '
      .items[] |
      (.spec.completions // 1) as $desired |
      (.status.succeeded // 0) as $succeeded |
      (.status.active // 0) as $active |
      (.status.failed // 0) as $failed |
      (if $desired > 0 then (($succeeded * 100 / $desired) | floor) else 0 end) as $percent |
      (if .status.startTime then ($now - (.status.startTime | fromdateiso8601)) else 0 end) as $elapsed |
      "phase: \(.metadata.name | sub("^gantry-benchmark-"; "") | split("-run-")[0])\n" +
      "  job: \(.metadata.name)\n" +
      "  pods: \($succeeded)/\($desired) complete (\($percent)%), \($active) active, \($failed) failed\n" +
      "  elapsed: \($elapsed)s"' <<<"$jobs_json" 2>/dev/null || true
  fi
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
