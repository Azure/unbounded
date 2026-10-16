#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0

set -Eeuo pipefail

image=${1:?usage: operator-vm-verify-streaming-layers-remote.sh IMAGE [NODE_LIMIT] [PARALLELISM] [NODE_NAME] [METRICS_ONLY]}
node_limit=${2:-0}
parallelism=${3:-48}
node_name=${4:-}
metrics_only=${5:-false}

[[ "$node_limit" =~ ^[0-9]+$ ]] || { echo "NODE_LIMIT must be a non-negative integer" >&2; exit 2; }
[[ "$parallelism" =~ ^[1-9][0-9]*$ ]] || { echo "PARALLELISM must be a positive integer" >&2; exit 2; }
[[ "$node_name" =~ ^[a-zA-Z0-9._-]*$ ]] || { echo "NODE_NAME contains invalid characters" >&2; exit 2; }
[[ "$metrics_only" == true || "$metrics_only" == false ]] || { echo "METRICS_ONLY must be true or false" >&2; exit 2; }
[[ "$image" == */*:* ]] || { echo "IMAGE must be a tag reference: $image" >&2; exit 2; }

source /etc/gantry-benchmark/env

home=${BENCHMARK_OPERATOR_HOME:-/var/lib/gantry-benchmark}
export HOME="$home"
export KUBECONFIG=${KUBECONFIG:-$home/kubeconfig}

gantry_namespace=${GANTRY_NAMESPACE:-gantry-system}
registry=${image%%/*}
repository_and_tag=${image#*/}
repository=${repository_and_tag%:*}
tag=${repository_and_tag##*:}
selector='app.kubernetes.io/name=gantry,app.kubernetes.io/component=overlaybd-config'
accept='application/vnd.oci.image.manifest.v1+json,application/vnd.docker.distribution.manifest.v2+json'

work_dir=$(mktemp -d)
trap 'rm -rf "$work_dir"' EXIT
manifest_file=$work_dir/manifest.json
pod_file=$work_dir/pods.tsv
result_file=$work_dir/results.tsv
agent_file=$work_dir/agents.tsv
metric_file=$work_dir/metrics.tsv
chair_file=$work_dir/chairs.tsv
node_address_file=$work_dir/node-addresses.tsv
coordinator_file=$work_dir/coordinators.tsv

helper=$(kubectl --request-timeout=30s -n "$gantry_namespace" get pods \
  -l "$selector" \
  --field-selector=status.phase=Running \
  -o json | jq -r '.items | sort_by(.spec.nodeName) | .[0] | [.metadata.name, .spec.nodeName] | @tsv')
[[ -n "$helper" && "$helper" != "null" ]] || { echo "no running OverlayBD configurator pod is available" >&2; exit 1; }
IFS=$'\t' read -r helper_pod helper_node <<<"$helper"

manifest_url="http://127.0.0.1:8578/v2/$repository/manifests/$tag?ns=$registry"
headers=$(kubectl --request-timeout=30s -n "$gantry_namespace" exec "$helper_pod" -c configure -- \
  curl -fsS -D - -o /dev/null --max-time 20 -H "Accept: $accept" "$manifest_url")
kubectl --request-timeout=30s -n "$gantry_namespace" exec "$helper_pod" -c configure -- \
  curl -fsS --max-time 20 -H "Accept: $accept" "$manifest_url" >"$manifest_file"

manifest_digest=$(awk 'tolower($1) == "docker-content-digest:" {gsub("\r", "", $2); print $2}' <<<"$headers" | tail -1)
[[ "$manifest_digest" =~ ^sha256:[0-9a-f]{64}$ ]] || { echo "registry helper returned invalid manifest digest: $manifest_digest" >&2; exit 1; }
jq -e '
  (.layers | type == "array" and length > 0) and
  all(.layers[]; (.digest | test("^sha256:[0-9a-f]{64}$")) and (.size | type == "number" and . >= 0))
' "$manifest_file" >/dev/null

expected_layers=$(jq '.layers | length' "$manifest_file")
expected_bytes=$(jq '[.layers[].size] | add' "$manifest_file")
layer_specs=$(jq -r '[.layers[] | ((.digest | sub("^sha256:"; "")) + ":" + (.size | tostring))] | join(",")' "$manifest_file")

kubectl --request-timeout=30s -n "$gantry_namespace" get pods \
  -l "$selector" \
  --field-selector=status.phase=Running \
  -o json | jq -r '.items | sort_by(.spec.nodeName)[] | [.metadata.name, .spec.nodeName] | @tsv' >"$pod_file"

desired_hosts=$(kubectl --request-timeout=30s -n "$gantry_namespace" get daemonset gantry-overlaybd-config \
  -o jsonpath='{.status.desiredNumberScheduled}')
mapfile -t pod_records <"$pod_file"
if [[ -n "$node_name" ]]; then
  mapfile -t pod_records < <(awk -F '\t' -v node="$node_name" '$2 == node {print}' "$pod_file")
elif ((node_limit > 0 && node_limit < ${#pod_records[@]})); then
  pod_records=("${pod_records[@]:0:node_limit}")
fi
target_hosts=${#pod_records[@]}
((target_hosts > 0)) || { echo "no OverlayBD configurator hosts selected" >&2; exit 1; }

inspect_command=$(cat <<'COMMAND'
count=0
bytes=0
mismatched=0
old_ifs=$IFS
IFS=,
for spec in $GANTRY_LAYER_SPECS; do
  digest=${spec%%:*}
  expected=${spec#*:}
  path=/host/var/lib/containerd/io.containerd.content.v1.content/blobs/sha256/$digest
  if [ -f "$path" ]; then
    actual=$(stat -c %s "$path" 2>/dev/null || printf '%s' -1)
    count=$((count + 1))
    if [ "$actual" -ge 0 ] 2>/dev/null; then
      bytes=$((bytes + actual))
    fi
    if [ "$actual" != "$expected" ]; then
      mismatched=$((mismatched + 1))
    fi
  fi
done
IFS=$old_ifs
printf '%s %s %s\n' "$count" "$bytes" "$mismatched"
COMMAND
)

inspect_host() {
  local record=$1
  local pod node result
  IFS=$'\t' read -r pod node <<<"$record"
  if result=$(timeout 35s kubectl --request-timeout=25s -n "$GANTRY_VERIFY_NAMESPACE" \
    exec "$pod" -c configure -- env "GANTRY_LAYER_SPECS=$GANTRY_LAYER_SPECS" \
    sh -c "$GANTRY_VERIFY_COMMAND" 2>/dev/null) &&
    [[ "$result" =~ ^[0-9]+[[:space:]][0-9]+[[:space:]][0-9]+$ ]]; then
    read -r count bytes mismatched <<<"$result"
    printf '%s\t%s\t%s\t%s\t%s\tok\n' "$node" "$pod" "$count" "$bytes" "$mismatched"
  else
    printf '%s\t%s\t-1\t0\t-1\texec_failed\n' "$node" "$pod"
  fi
}

collect_metrics() {
  local record=$1
  local node ip pod metrics
  IFS=$'\t' read -r node ip pod <<<"$record"
  if metrics=$(curl -fsS --connect-timeout 3 --max-time 10 "http://$ip:9095/metrics" 2>/dev/null); then
    printf '%s\n' "$metrics" | awk -v node="$node" '
      /^p2p_origin_pull_total\{.*kind="layer"/ {origin_started += $2}
      /^p2p_origin_pull_success_total\{.*kind="layer"/ {origin_success += $2}
      /^p2p_origin_pull_failure_total\{.*kind="layer"/ {origin_failure += $2}
      /^gantry_layer_seed_total\{outcome="failed"\}/ {seed_failed += $2}
      /^gantry_layer_seed_total\{outcome="peer"\}/ {seed_peer += $2}
      /^gantry_layer_seed_total\{outcome="origin"\}/ {seed_origin += $2}
      /^gantry_layer_seed_total\{outcome="present"\}/ {seed_present += $2}
      /^p2p_prefetch_batches_total / {prefetch_batches += $2}
      /^p2p_prefetch_groups_total\{.*outcome="success"/ {prefetch_success += $2}
      /^p2p_prefetch_groups_total\{.*outcome="error"/ {prefetch_error += $2}
      /^p2p_dht_lookup_total/ {dht_lookups += $2}
      END {
        printf "%s\t%.0f\t%.0f\t%.0f\t%.0f\t%.0f\t%.0f\t%.0f\t%.0f\t%.0f\t%.0f\t%.0f\tok\n",
          node, origin_started, origin_success, origin_failure, seed_failed,
          seed_peer, seed_origin, seed_present, prefetch_batches,
          prefetch_success, prefetch_error, dht_lookups
      }
    '
  else
    printf '%s\t0\t0\t0\t0\t0\t0\t0\t0\t0\t0\t0\tmetrics_failed\n' "$node"
  fi
}

export -f inspect_host
export -f collect_metrics
export GANTRY_VERIFY_NAMESPACE="$gantry_namespace"
export GANTRY_LAYER_SPECS="$layer_specs"
export GANTRY_VERIFY_COMMAND="$inspect_command"

kubectl --request-timeout=30s -n "$gantry_namespace" get pods \
  -l 'app.kubernetes.io/name=gantry,app.kubernetes.io/component=agent' \
  --field-selector=status.phase=Running \
  -o json | jq -r '.items | sort_by(.spec.nodeName)[] | [.spec.nodeName, .status.podIP, .metadata.name] | @tsv' >"$agent_file"
if [[ -n "$node_name" ]]; then
  mapfile -t agent_records < <(awk -F '\t' -v node="$node_name" '$1 == node {print}' "$agent_file")
elif ((node_limit > 0)); then
  mapfile -t agent_records < <(awk -F '\t' -v limit="$node_limit" 'NR <= limit {print}' "$agent_file")
else
  mapfile -t agent_records <"$agent_file"
fi

if [[ "$metrics_only" == true ]]; then
  for record in "${pod_records[@]}"; do
    IFS=$'\t' read -r pod node <<<"$record"
    printf '%s\t%s\t-1\t0\t-1\tskipped\n' "$node" "$pod"
  done >"$result_file"
  host_check_pid=
else
  printf '%s\0' "${pod_records[@]}" | \
    xargs -0 -r -P "$parallelism" -n 1 bash -c 'inspect_host "$1"' _ >"$result_file" &
  host_check_pid=$!
fi
printf '%s\0' "${agent_records[@]}" | \
  xargs -0 -r -P "$parallelism" -n 1 bash -c 'collect_metrics "$1"' _ >"$metric_file" &
metric_check_pid=$!
[[ -z "$host_check_pid" ]] || wait "$host_check_pid"
wait "$metric_check_pid"
sort -o "$result_file" "$result_file"
sort -o "$metric_file" "$metric_file"

checked_hosts=$(wc -l <"$result_file")
unique_hosts=$(cut -f1 "$result_file" | sort -u | wc -l)
metric_hosts=$(awk -F '\t' '$13 == "ok" {count++} END {print count + 0}' "$metric_file")
complete_hosts=$(awk -F '\t' -v layers="$expected_layers" -v bytes="$expected_bytes" \
  '$3 == layers && $4 == bytes && $5 == 0 && $6 == "ok" {count++} END {print count + 0}' "$result_file")
present_layers=$(awk -F '\t' '$3 >= 0 {sum += $3} END {print sum + 0}' "$result_file")
present_bytes=$(awk -F '\t' '{sum += $4} END {printf "%.0f\n", sum}' "$result_file")
expected_fleet_bytes=$((expected_bytes * target_hosts))

printf 'observed_at=%s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)"
printf 'image=%s\n' "$image"
printf 'manifest_digest=%s\n' "$manifest_digest"
printf 'helper_node=%s\n' "$helper_node"
printf 'layers_per_host=%s\n' "$expected_layers"
printf 'bytes_per_host=%s\n' "$expected_bytes"
printf 'desired_hosts=%s\n' "$desired_hosts"
printf 'target_hosts=%s\n' "$target_hosts"
printf 'checked_hosts=%s\n' "$checked_hosts"
printf 'unique_hosts=%s\n' "$unique_hosts"
printf 'metric_hosts=%s\n' "$metric_hosts"
printf 'complete_hosts=%s\n' "$complete_hosts"
printf 'present_layers=%s\n' "$present_layers"
printf 'present_bytes=%s\n' "$present_bytes"
printf 'expected_fleet_bytes=%s\n' "$expected_fleet_bytes"
printf 'metrics_only=%s\n' "$metrics_only"
awk -F '\t' '
  {
    origin_started += $2; origin_success += $3; origin_failure += $4
    seed_failed += $5; seed_peer += $6; seed_origin += $7; seed_present += $8
    prefetch_batches += $9; prefetch_success += $10; prefetch_error += $11
    if ($9 > 0) prefetch_hosts++
  }
  END {
    printf "origin_started=%0.f\norigin_success=%0.f\norigin_failure=%0.f\n", origin_started, origin_success, origin_failure
    printf "seed_failed=%0.f\nseed_peer=%0.f\nseed_origin=%0.f\nseed_present=%0.f\n", seed_failed, seed_peer, seed_origin, seed_present
    printf "prefetch_hosts=%d\nprefetch_batches=%0.f\nprefetch_group_success=%0.f\nprefetch_group_error=%0.f\n", prefetch_hosts, prefetch_batches, prefetch_success, prefetch_error
  }
' "$metric_file"
printf '\nmetric_active_hosts:\n'
awk -F '\t' '
  $2 + $3 + $4 + $5 + $6 + $7 + $8 + $9 + $10 + $11 > 0 {
    print
    shown++
    if (shown == 20) exit
  }
' "$metric_file"
printf '\nlowest_dht_lookup_hosts:\n'
sort -t $'\t' -k12,12n "$metric_file" | awk -F '\t' 'NR <= 20 {print $1 "\t" $12 "\t" $13}'
kubectl --request-timeout=30s -n "$gantry_namespace" get leases -l 'gantry.io/chair=true' -o json | \
  jq -r '.items[] | select(.spec.holderIdentity != null and .metadata.annotations["gantry.io/holder-transfer-addr"] != null) | [.metadata.name, .spec.holderIdentity, .metadata.annotations["gantry.io/holder-transfer-addr"]] | @tsv' >"$chair_file"
kubectl --request-timeout=30s get nodes -o json | jq -r '
  .items[] |
  .metadata.name as $node |
  .status.addresses[] |
  select(.type == "InternalIP") |
  [$node, .address] | @tsv
' >"$node_address_file"
printf '\nprefetch_coordinators:\n'
while IFS=$'\t' read -r chair peer_id transfer_addr; do
  score=$(printf '%s%s' "$chair" "$manifest_digest" | sha256sum | awk '{print $1}')
  transfer_ip=${transfer_addr%:*}
  holder_node=$(awk -F '\t' -v ip="$transfer_ip" '$2 == ip {print $1; exit}' "$node_address_file")
  metric_row=$(awk -F '\t' -v node="$holder_node" '$1 == node {print; exit}' "$metric_file")
  printf '%s\t%s\t%s\t%s\t%s\t%s\n' "$score" "$chair" "$holder_node" "$peer_id" "$transfer_addr" "$metric_row"
done <"$chair_file" | sort -r | awk -F '\t' 'NR <= 3 {print}' >"$coordinator_file"
cat "$coordinator_file"
printf '\nprefetch_coordinator_logs:\n'
while IFS=$'\t' read -r score chair holder_node peer_id transfer_addr metric_row; do
  agent_pod=$(awk -F '\t' -v node="$holder_node" '$1 == node {print $3; exit}' "$agent_file")
  printf '[%s %s %s]\n' "$chair" "$holder_node" "$agent_pod"
  if [[ -n "$agent_pod" ]]; then
    kubectl --request-timeout=30s -n "$gantry_namespace" logs "$agent_pod" -c gantry --since=3h 2>/dev/null | \
      grep -E "streaming layer reconciliation|$manifest_digest" | tail -100 || true
  else
    echo "no running Gantry agent pod found"
  fi
done <"$coordinator_file"
printf '\nlayer_count_distribution:\n'
awk -F '\t' '{count[$3]++} END {for (layers in count) print layers, count[layers]}' "$result_file" | sort -n

if [[ "$metrics_only" == true ]]; then
  ((metric_hosts == target_hosts)) || { echo "metric result set is incomplete" >&2; exit 1; }
  exit 0
fi

if ((complete_hosts != target_hosts)); then
  printf '\nincomplete_hosts:\n'
  awk -F '\t' -v layers="$expected_layers" -v bytes="$expected_bytes" \
    '$3 != layers || $4 != bytes || $5 != 0 || $6 != "ok" {print; shown++; if (shown == 10) exit}' "$result_file"

  if ((target_hosts == 1)); then
    diagnostic_node=$(awk -F '\t' -v layers="$expected_layers" -v bytes="$expected_bytes" \
      '$3 != layers || $4 != bytes || $5 != 0 || $6 != "ok" {print $1; exit}' "$result_file")
    diagnostic_helper=$(awk -F '\t' -v node="$diagnostic_node" '$2 == node {print $1; exit}' "$pod_file")
    diagnostic_agent=$(kubectl --request-timeout=30s -n "$gantry_namespace" get pods \
      -l 'app.kubernetes.io/name=gantry,app.kubernetes.io/component=agent' \
      --field-selector=status.phase=Running \
      -o json | jq -r --arg node "$diagnostic_node" 'first(.items[] | select(.spec.nodeName == $node) | .metadata.name) // ""')

    printf '\nhost_content_store[%s]:\n' "$diagnostic_node"
    kubectl --request-timeout=30s -n "$gantry_namespace" exec "$diagnostic_helper" -c configure -- sh -c '
      root=/host/var/lib/containerd/io.containerd.content.v1.content/blobs/sha256
      printf "path=%s\n" "$root"
      printf "directory_present=%s\n" "$(test -d "$root" && echo true || echo false)"
      printf "total_blobs=%s\n" "$(find "$root" -maxdepth 1 -type f 2>/dev/null | wc -l)"
      printf "total_bytes=%s\n" "$(du -sb "$root" 2>/dev/null | awk "{print \$1}")"
    ' || true

    printf '\ngantry_seed_metrics[%s]:\n' "$diagnostic_node"
    kubectl --request-timeout=30s -n "$gantry_namespace" exec "$diagnostic_helper" -c configure -- \
      curl -fsS --max-time 10 http://127.0.0.1:9095/metrics 2>/dev/null | \
      grep -E '^(gantry_layer_seed_total|p2p_origin_pull_(total|success_total|failure_total)|p2p_prefetch_(batches|groups)_total)' || true

    printf '\ngantry_reconciliation_logs[%s]:\n' "$diagnostic_node"
    if [[ -n "$diagnostic_agent" ]]; then
      kubectl --request-timeout=30s -n "$gantry_namespace" logs "$diagnostic_agent" -c gantry --since=3h 2>/dev/null | \
        grep -E "streaming layer reconciliation|$manifest_digest" | tail -100 || true
    else
      echo "no running Gantry agent pod found"
    fi
  fi
  exit 1
fi
if ((checked_hosts != target_hosts || unique_hosts != target_hosts || metric_hosts != target_hosts)); then
  echo "host verification result set is incomplete or duplicated" >&2
  exit 1
fi
if ((node_limit == 0 && target_hosts != desired_hosts)); then
  echo "checked host count does not match DaemonSet desired host count" >&2
  exit 1
fi
