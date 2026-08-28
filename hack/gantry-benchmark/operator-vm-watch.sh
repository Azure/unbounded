#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

set -Eeuo pipefail

script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
. "$script_dir/operator-vm-ssh-common.sh"

AZURE_RESOURCE_GROUP="${AZURE_RESOURCE_GROUP:-}"
OPERATOR_VM_NAME="${OPERATOR_VM_NAME:-gantry-benchmark-operator}"
WATCH_INTERVAL_SECONDS="${WATCH_INTERVAL_SECONDS:-30}"
follow=false

operator_ssh_init

usage() {
  cat <<'USAGE'
Usage: operator-vm-watch.sh [--follow] [--interval SECONDS]

Without --follow, prints one status snapshot. With --follow, refreshes until the
VM benchmark service becomes inactive and progress reaches completed or failed.
USAGE
}

while (($# > 0)); do
  case $1 in
    --follow)
      follow=true
      shift
      ;;
    --interval)
      [[ $# -ge 2 ]] || { usage >&2; exit 2; }
      WATCH_INTERVAL_SECONDS=$2
      shift 2
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      echo "unknown argument: $1" >&2
      usage >&2
      exit 2
      ;;
  esac
done

[[ "$WATCH_INTERVAL_SECONDS" =~ ^[1-9][0-9]*$ ]] || {
  echo "WATCH_INTERVAL_SECONDS must be a positive integer" >&2
  exit 2
}

status_once() {
  local output
  output=$(operator_ssh sudo -n /opt/gantry-benchmark/unbounded/hack/gantry-benchmark/operator-vm-status.sh)

  printf '%s\n' "$output" | sed \
    -e '/^Enable succeeded: *$/d' \
    -e '/^\[stdout\]$/d' \
    -e '/^\[stderr\]$/d'
}

if [[ "$follow" == false ]]; then
  printf '%s snapshot\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  status_once
  exit 0
fi

while true; do
  printf '\033[2J\033[H'
  status=$(status_once)
  printf '%s snapshot (refreshing every %ss)\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$WATCH_INTERVAL_SECONDS"
  printf '%s\n' "$status"

  service=$(awk -F': ' '/^service: /{print $2; exit}' <<<"$status")
  stage=$(awk -F': ' '/^stage: /{print $2; exit}' <<<"$status")
  if [[ "$service" == inactive || "$service" == failed ]]; then
    if [[ "$stage" == completed || "$stage" == failed || -z "$stage" ]]; then
      break
    fi
  fi

  sleep "$WATCH_INTERVAL_SECONDS"
done
