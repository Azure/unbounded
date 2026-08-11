#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

set -Eeuo pipefail

script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
. "$script_dir/operator-vm-ssh-common.sh"

start_service=true
if [[ "${1:-}" == --check ]]; then
  start_service=false
  shift
fi
prepared_run_id="${1:-}"
[[ $# -le 1 ]] || { echo "usage: operator-vm-start.sh [--check] [prepared-run-id]" >&2; exit 2; }
if [[ -n "$prepared_run_id" ]]; then
  [[ "$prepared_run_id" =~ ^[A-Za-z0-9._-]+$ && "$prepared_run_id" != "." && "$prepared_run_id" != ".." ]] || {
    echo "PREPARED_RUN_ID contains unsupported characters" >&2
    exit 2
  }
fi

runner_payload=$(base64 -w 0 "$script_dir/operator-vm-run.sh")
prepared_run_arg="${prepared_run_id:--}"
operator_ssh_init
operator_ssh sudo -n bash -s -- "$prepared_run_arg" "$runner_payload" "$start_service" <<'SCRIPT'
set -Eeuo pipefail
prepared_run_id=$1
runner_payload=$2
start_service=$3
[[ "$prepared_run_id" != - ]] || prepared_run_id=""
benchmark_service=gantry-benchmark-operator.service
prepare_service=gantry-benchmark-image-prepare.service
pool_service=gantry-benchmark-image-builder.service
runner=/var/lib/gantry-benchmark/operator-vm-run.sh
run_config=/etc/gantry-benchmark/operator-run.env

for service in "$benchmark_service" "$prepare_service" "$pool_service"; do
  if systemctl is-active --quiet "$service"; then
    echo "$service is already active" >&2
    exit 1
  fi
done

source /etc/gantry-benchmark/env
install -d -m 0755 /var/lib/gantry-benchmark
printf '%s' "$runner_payload" | base64 -d >"$runner"
chmod 0755 "$runner"

if [[ -n "$prepared_run_id" ]]; then
  state="$BENCHMARK_ARTIFACT_ROOT/$prepared_run_id/state.json"
  [[ -s "$state" ]] || {
    echo "prepared run $prepared_run_id has no retained state" >&2
    exit 1
  }
  baseline_image=$(jq -er 'select(.mode == "direct" and .status == "disabled") | .baseline_image' "$state")
  gantry_image=$(jq -er '.gantry_cold_image' "$state")
  payload_sha=$(jq -er '.workload_payload_sha256' "$state")
  [[ "$baseline_image" == "$BASELINE_ACR_LOGIN_SERVER/$BENCHMARK_WORKLOAD_REPOSITORY@sha256:"* ]] || {
    echo "prepared baseline image does not match $BASELINE_ACR_LOGIN_SERVER" >&2
    exit 1
  }
  [[ "$gantry_image" == "$GANTRY_ACR_LOGIN_SERVER/$BENCHMARK_WORKLOAD_REPOSITORY@sha256:"* ]] || {
    echo "prepared Gantry image does not match $GANTRY_ACR_LOGIN_SERVER" >&2
    exit 1
  }
  [[ "$payload_sha" =~ ^sha256:[0-9a-f]{64}$ ]] || {
    echo "prepared payload SHA is invalid" >&2
    exit 1
  }
  cat >"$run_config" <<ENV
ADOPT_BASELINE_IMAGE="$baseline_image"
ADOPT_GANTRY_IMAGE="$gantry_image"
ADOPT_PAYLOAD_SHA256="$payload_sha"
ENV
  chmod 0600 "$run_config"
  printf 'adopting prepared images from %s\n' "$prepared_run_id"
else
  rm -f "$run_config"
  printf 'building fresh benchmark images\n'
fi

cat >/etc/systemd/system/gantry-benchmark-operator.service <<UNIT
[Unit]
Description=Gantry dual-ACR benchmark operator
After=network-online.target
Wants=network-online.target

[Service]
Type=oneshot
User=root
Environment=GANTRY_BENCHMARK_CONFIG=/etc/gantry-benchmark/env
Environment=GANTRY_BENCHMARK_RUN_CONFIG=$run_config
ExecStart=$runner
StandardOutput=append:/var/log/gantry-benchmark/service.log
StandardError=append:/var/log/gantry-benchmark/service.log
TimeoutStartSec=0
TimeoutStopSec=45min

[Install]
WantedBy=multi-user.target
UNIT
systemctl daemon-reload

if [[ "$start_service" != true ]]; then
  systemctl show "$benchmark_service" --property=LoadState --property=FragmentPath --no-pager
  printf 'benchmark start check passed\n'
  exit 0
fi

if systemctl is-failed --quiet "$benchmark_service"; then
  systemctl reset-failed "$benchmark_service"
fi
systemctl start --no-block "$benchmark_service"
systemctl show "$benchmark_service" --property=ActiveState --property=SubState --no-pager
SCRIPT
