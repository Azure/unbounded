#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0

set -Eeuo pipefail

script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)

AZURE_RESOURCE_GROUP="${AZURE_RESOURCE_GROUP:-}"
OPERATOR_VM_NAME="${OPERATOR_VM_NAME:-gantry-benchmark-operator}"
OPERATOR_SSH_HOST="${OPERATOR_SSH_HOST:-}"
OPERATOR_SSH_KEY="${OPERATOR_SSH_KEY:-}"
OPERATOR_SSH_USER="${OPERATOR_SSH_USER:-benchmark}"
WATCH_INTERVAL_SECONDS="${WATCH_INTERVAL_SECONDS:-30}"
OPERATOR_RUN_COMMAND_LOCK="${OPERATOR_RUN_COMMAND_LOCK:-${TMPDIR:-/tmp}/gantry-benchmark-${AZURE_RESOURCE_GROUP}-${OPERATOR_VM_NAME}.run-command.lock}"
follow=false

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

  if [[ -n "$OPERATOR_SSH_HOST" ]]; then
    : "${OPERATOR_SSH_KEY:?Set OPERATOR_SSH_KEY when OPERATOR_SSH_HOST is set}"
    output=$(ssh \
      -i "$OPERATOR_SSH_KEY" \
      -o BatchMode=yes \
      -o ConnectTimeout=20 \
      -o ServerAliveInterval=30 \
      "$OPERATOR_SSH_USER@$OPERATOR_SSH_HOST" \
      'sudo -n /opt/gantry-benchmark/unbounded/hack/gantry-benchmark/operator-vm-status.sh')
  else
    : "${AZURE_RESOURCE_GROUP:?Set AZURE_RESOURCE_GROUP when OPERATOR_SSH_HOST is not set}"
    local run_command_lock_fd
    exec {run_command_lock_fd}>"$OPERATOR_RUN_COMMAND_LOCK"
    flock "$run_command_lock_fd"
    output=$(az vm run-command invoke \
      -g "$AZURE_RESOURCE_GROUP" \
      -n "$OPERATOR_VM_NAME" \
      --command-id RunShellScript \
      --scripts @"$script_dir/operator-vm-status.sh" \
      --only-show-errors \
      --query 'value[0].message' \
      -o tsv)
    flock -u "$run_command_lock_fd"
    exec {run_command_lock_fd}>&-
  fi

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
