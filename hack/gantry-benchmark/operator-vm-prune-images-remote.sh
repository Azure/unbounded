#!/usr/bin/env bash

set -Eeuo pipefail

source /etc/gantry-benchmark/env
export HOME="${BENCHMARK_OPERATOR_HOME:-/var/lib/gantry-benchmark}"
export KUBECONFIG="${KUBECONFIG:-$HOME/kubeconfig}"

check_prune_safety() {
  test -s "$KUBECONFIG" || {
    echo "operator kubeconfig is missing: $KUBECONFIG" >&2
    exit 1
  }

  for lifecycle_service in gantry-benchmark-operator.service gantry-benchmark-image-builder.service; do
    if systemctl is-active --quiet "$lifecycle_service"; then
      echo "$lifecycle_service is active; refusing to prune images" >&2
      exit 1
    fi
  done

  lifecycle_lock="${BENCHMARK_LIFECYCLE_LOCK:-$HOME/benchmark-lifecycle.lock}"
  exec {lifecycle_lock_fd}>"$lifecycle_lock"
  if ! flock -n "$lifecycle_lock_fd"; then
    echo "another benchmark or image-pool lifecycle owns $lifecycle_lock" >&2
    exit 1
  fi

  benchmark_state="$(kubectl --request-timeout=15s -n "${BENCHMARK_NAMESPACE:-gantry-benchmark}" get configmap gantry-benchmark-state --ignore-not-found -o name)"
  benchmark_lock="$(kubectl --request-timeout=15s -n "${GANTRY_NAMESPACE:-gantry-system}" get configmap gantry-benchmark-lock --ignore-not-found -o name)"
  if [[ -n "$benchmark_state" || -n "$benchmark_lock" ]]; then
    echo "benchmark state or lock exists; run disable before pruning images" >&2
    exit 1
  fi
}

(($# == 1)) || { echo "usage: $0 check|run" >&2; exit 2; }

case "$1" in
  check)
    check_prune_safety
    ;;
  run)
    check_prune_safety
    printf '=== Before ===\n'
    df -h / "$BENCHMARK_BUILD_MOUNT" | awk 'NR == 1 || !seen[$1]++'
    podman system df
    printf '\n=== Prune ===\n'
    podman image prune --all --force
    printf '\n=== After ===\n'
    df -h / "$BENCHMARK_BUILD_MOUNT" | awk 'NR == 1 || !seen[$1]++'
    podman system df
    ;;
  *)
    echo "usage: $0 check|run" >&2
    exit 2
    ;;
esac