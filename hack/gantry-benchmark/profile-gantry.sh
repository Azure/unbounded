#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0

set -Eeuo pipefail

repo_root=$(git rev-parse --show-toplevel)

KUBECTL="${KUBECTL:-kubectl}"
BENCHMARK_NAMESPACE="${BENCHMARK_NAMESPACE:-gantry-benchmark}"
GANTRY_NAMESPACE="${GANTRY_NAMESPACE:-gantry-system}"
MONITORING_NAMESPACE="${MONITORING_NAMESPACE:-monitoring}"
PROMETHEUS_SERVICE="${PROMETHEUS_SERVICE:-kps-kube-prometheus-stack-prometheus}"
GANTRY_PPROF_SECONDS="${GANTRY_PPROF_SECONDS:-30}"
GANTRY_PPROF_COUNT="${GANTRY_PPROF_COUNT:-3}"
GANTRY_PPROF_PORT="${GANTRY_PPROF_PORT:-6060}"
GANTRY_PPROF_LOCAL_PORT_BASE="${GANTRY_PPROF_LOCAL_PORT_BASE:-16060}"
GANTRY_PPROF_OUTPUT_ROOT="${GANTRY_PPROF_OUTPUT_ROOT:-$repo_root/tmp/gantry-pprof}"

[[ "$GANTRY_PPROF_SECONDS" =~ ^[1-9][0-9]{0,2}$ ]] || {
  echo "GANTRY_PPROF_SECONDS must be a positive integer" >&2
  exit 2
}
((GANTRY_PPROF_SECONDS <= 300)) || {
  echo "GANTRY_PPROF_SECONDS must not exceed 300" >&2
  exit 2
}
[[ "$GANTRY_PPROF_COUNT" =~ ^[1-9][0-9]?$ ]] || {
  echo "GANTRY_PPROF_COUNT must be a positive integer" >&2
  exit 2
}
((GANTRY_PPROF_COUNT <= 10)) || {
  echo "GANTRY_PPROF_COUNT must not exceed 10" >&2
  exit 2
}
[[ "$GANTRY_PPROF_PORT" =~ ^[1-9][0-9]{0,4}$ ]] || {
  echo "GANTRY_PPROF_PORT must be a positive integer" >&2
  exit 2
}
[[ "$GANTRY_PPROF_LOCAL_PORT_BASE" =~ ^[1-9][0-9]{0,4}$ ]] || {
  echo "GANTRY_PPROF_LOCAL_PORT_BASE must be a positive integer" >&2
  exit 2
}

for command in "$KUBECTL" curl flock go jq setsid; do
  command -v "$command" >/dev/null || {
    echo "required command not found: $command" >&2
    exit 1
  }
done

((GANTRY_PPROF_PORT <= 65535)) || {
  echo "GANTRY_PPROF_PORT must not exceed 65535" >&2
  exit 2
}
((GANTRY_PPROF_LOCAL_PORT_BASE + GANTRY_PPROF_COUNT - 1 <= 65535)) || {
  echo "GANTRY_PPROF_LOCAL_PORT_BASE plus GANTRY_PPROF_COUNT exceeds port 65535" >&2
  exit 2
}

install -d -m 0750 "$GANTRY_PPROF_OUTPUT_ROOT"
exec {profile_lock_fd}>"$GANTRY_PPROF_OUTPUT_ROOT/.profile.lock"
flock -n "$profile_lock_fd" || {
  echo "another Gantry profiling capture is active" >&2
  exit 1
}

state_json=$("$KUBECTL" -n "$BENCHMARK_NAMESPACE" get configmap gantry-benchmark-state \
  -o jsonpath='{.data.state\.json}')
run_id=$(jq -er '.run_id' <<<"$state_json")

job_json=$("$KUBECTL" -n "$BENCHMARK_NAMESPACE" get jobs \
  -l "gantry.unbounded-cloud.io/run-id=$run_id,gantry.unbounded-cloud.io/phase=gantry-cold" \
  -o json)
active=$(jq '[.items[].status.active // 0] | add // 0' <<<"$job_json")
((active > 0)) || {
  echo "run $run_id has no active Gantry-cold pods to profile" >&2
  exit 1
}
job_name=$(jq -er '
  .items
  | map(select((.status.active // 0) > 0))
  | sort_by(.metadata.creationTimestamp)
  | last
  | .metadata.name
' <<<"$job_json")

monitoring_namespace=$(jq -r '.monitoring_namespace // empty' <<<"$state_json")
prometheus_service=$(jq -r '.prometheus_service // empty' <<<"$state_json")
MONITORING_NAMESPACE="${monitoring_namespace:-$MONITORING_NAMESPACE}"
PROMETHEUS_SERVICE="${prometheus_service:-$PROMETHEUS_SERVICE}"

gantry_pods=$("$KUBECTL" -n "$GANTRY_NAMESPACE" get pods \
  -l app.kubernetes.io/name=gantry -o json)
revision=$(jq -er '
  [.items[]
    | select(any(.status.conditions[]?; .type == "Ready" and .status == "True"))
    | .metadata.labels["controller-revision-hash"]
  ][0]
' <<<"$gantry_pods")

labels="namespace=\"$GANTRY_NAMESPACE\",gantry_benchmark=\"true\",controller_revision_hash=\"$revision\""
cpu_expression="sum by(pod)(rate(process_cpu_seconds_total{$labels}[1m]))"
prometheus_path="/api/v1/namespaces/$MONITORING_NAMESPACE/services/http:$PROMETHEUS_SERVICE:9090/proxy/api/v1/query"
cpu_response=$("$KUBECTL" get --raw "$prometheus_path?query=$(jq -rn --arg value "$cpu_expression" '$value|@uri')")

mapfile -t hottest_pods < <(jq -r --argjson count "$GANTRY_PPROF_COUNT" '
  [.data.result[] | {
    pod: .metric.pod,
    cpu_cores: (.value[1] | tonumber)
  }]
  | sort_by(.cpu_cores)
  | reverse
  | .[:$count][]
  | [.pod, (.cpu_cores | tostring)]
  | @tsv
' <<<"$cpu_response")

((${#hottest_pods[@]} == GANTRY_PPROF_COUNT)) || {
  echo "Prometheus returned ${#hottest_pods[@]} Gantry process CPU samples, want $GANTRY_PPROF_COUNT" >&2
  exit 1
}

output="$GANTRY_PPROF_OUTPUT_ROOT/$run_id-$(date -u +%Y%m%dT%H%M%S.%NZ)"
install -d -m 0750 "$output"
printf 'node\tgantry_cpu_cores\tpod\n' >"$output/targets.tsv"

declare -a targets=()
for target in "${hottest_pods[@]}"; do
  IFS=$'\t' read -r pod cpu_cores <<<"$target"
  node=$(jq -er --arg pod "$pod" '
    .items[]
    | select(.metadata.name == $pod)
    | select(any(.status.conditions[]?; .type == "Ready" and .status == "True"))
    | .spec.nodeName
  ' <<<"$gantry_pods")
  targets+=("$node"$'\t'"$cpu_cores"$'\t'"$pod")
  printf '%s\t%s\t%s\n' "$node" "$cpu_cores" "$pod" >>"$output/targets.tsv"
done

echo "diagnostic profiling adds CPU overhead; do not use this run for benchmark comparison"
echo "profiling $GANTRY_PPROF_COUNT hottest Gantry pods for ${GANTRY_PPROF_SECONDS}s into $output"
cat "$output/targets.tsv"

profile_started_at=$(date -u +%Y-%m-%dT%H:%M:%SZ)
"$KUBECTL" -n "$BENCHMARK_NAMESPACE" annotate job "$job_name" \
  gantry.unbounded-cloud.io/cpu-profiled-at="$profile_started_at" \
  gantry.unbounded-cloud.io/cpu-profile-seconds="$GANTRY_PPROF_SECONDS" \
  gantry.unbounded-cloud.io/cpu-profile-pods="$GANTRY_PPROF_COUNT" \
  --overwrite >/dev/null

profile_target() {
  local index=$1
  local node=$2
  local cpu_cores=$3
  local pod=$4
  local output=$5
  local local_port=$((GANTRY_PPROF_LOCAL_PORT_BASE + index))
  local target_dir="$output/$node"
  local port_forward_pid

  install -d -m 0750 "$target_dir"

  "$KUBECTL" -n "$GANTRY_NAMESPACE" port-forward \
    --address 127.0.0.1 "pod/$pod" "$local_port:$GANTRY_PPROF_PORT" \
    >"$target_dir/port-forward.log" 2>&1 &
  port_forward_pid=$!
  trap "kill $port_forward_pid >/dev/null 2>&1 || true; wait $port_forward_pid >/dev/null 2>&1 || true" EXIT

  curl --fail --silent --show-error \
    --retry 30 --retry-delay 1 --retry-connrefused --connect-timeout 1 --max-time 45 \
    "http://127.0.0.1:$local_port/debug/pprof/" >/dev/null

  curl --fail --silent --show-error \
    --max-time "$((GANTRY_PPROF_SECONDS + 30))" \
    --output "$target_dir/cpu.pb.gz" \
    "http://127.0.0.1:$local_port/debug/pprof/profile?seconds=$GANTRY_PPROF_SECONDS"

  go tool pprof -top -nodecount=60 "$target_dir/cpu.pb.gz" >"$target_dir/top.txt"
  printf '%s\t%s\t%s\t%s\n' "$node" "$cpu_cores" "$pod" "$target_dir/cpu.pb.gz"
}

declare -a profile_pids=()
declare -a profile_group_pid_files=()
cleanup_profiles() {
  local group_pid
  local group_pid_file
  local profile_pid

  for group_pid_file in "${profile_group_pid_files[@]}"; do
    if read -r group_pid <"$group_pid_file" 2>/dev/null && [[ "$group_pid" =~ ^[1-9][0-9]*$ ]]; then
      kill -TERM -- "-$group_pid" >/dev/null 2>&1 || true
    fi
  done

  for profile_pid in "${profile_pids[@]}"; do
    kill -TERM "$profile_pid" >/dev/null 2>&1 || true
  done

  for profile_pid in "${profile_pids[@]}"; do
    wait "$profile_pid" >/dev/null 2>&1 || true
  done

  rm -f -- "${profile_group_pid_files[@]}"
}
trap cleanup_profiles EXIT
trap 'exit 130' INT TERM

export -f profile_target
export KUBECTL GANTRY_NAMESPACE GANTRY_PPROF_LOCAL_PORT_BASE GANTRY_PPROF_PORT GANTRY_PPROF_SECONDS

for index in "${!targets[@]}"; do
  IFS=$'\t' read -r node cpu_cores pod <<<"${targets[$index]}"
  group_pid_file="$output/.worker-$index.pid"
  profile_group_pid_files+=("$group_pid_file")
  setsid --fork --wait bash -c '
    set -Eeuo pipefail
    group_pid_file=$1
    shift
    printf "%s\n" "$$" >"$group_pid_file"
    profile_target "$@"
  ' profile-worker "$group_pid_file" "$index" "$node" "$cpu_cores" "$pod" "$output" &
  profile_pids+=("$!")
done

failed_profiles=0
for profile_pid in "${profile_pids[@]}"; do
  wait "$profile_pid" || failed_profiles=$((failed_profiles + 1))
done

mapfile -t profiles < <(find "$output" -mindepth 2 -maxdepth 2 -type f -name cpu.pb.gz | sort)
captured_profiles=${#profiles[@]}
"$KUBECTL" -n "$BENCHMARK_NAMESPACE" annotate job "$job_name" \
  gantry.unbounded-cloud.io/cpu-profile-captured-pods="$captured_profiles" \
  --overwrite >/dev/null

((captured_profiles >= 2)) || {
  echo "captured $captured_profiles/$GANTRY_PPROF_COUNT Gantry CPU profiles; need at least 2 to merge" >&2
  echo "inspect $output/*/port-forward.log" >&2
  exit 1
}
if ((failed_profiles > 0)); then
  echo "warning: captured $captured_profiles/$GANTRY_PPROF_COUNT profiles; inspect failed target logs under $output" >&2
fi

go tool pprof -proto -output="$output/merged.pb.gz" "${profiles[@]}"
go tool pprof -top -nodecount=100 "$output/merged.pb.gz" >"$output/merged-top.txt"

echo
echo "=== merged Gantry CPU profile ==="
sed -n '1,45p' "$output/merged-top.txt"
echo
echo "profiles: $output"
echo "interactive: go tool pprof -http=127.0.0.1:0 $output/merged.pb.gz"