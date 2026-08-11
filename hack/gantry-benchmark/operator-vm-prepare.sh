#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

set -Eeuo pipefail

script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
. "$script_dir/operator-vm-ssh-common.sh"
AZURE_RESOURCE_GROUP="${AZURE_RESOURCE_GROUP:-}"
OPERATOR_VM_NAME="${OPERATOR_VM_NAME:-gantry-benchmark-operator}"

: "${AZURE_RESOURCE_GROUP:?Set AZURE_RESOURCE_GROUP}"

prepare_runner_payload=$(base64 -w 0 "$script_dir/operator-vm-run.sh")
wrapped_script=$(cat <<SCRIPT
#!/usr/bin/env bash
set -Eeuo pipefail
prepare_service=gantry-benchmark-image-prepare.service
benchmark_service=gantry-benchmark-operator.service
pool_service=gantry-benchmark-image-builder.service

for service in "\$prepare_service" "\$benchmark_service" "\$pool_service"; do
  if systemctl is-active --quiet "\$service"; then
    echo "\$service is already active" >&2
    exit 1
  fi
done

install -d -m 0755 /var/lib/gantry-benchmark
printf '%s' '$prepare_runner_payload' | base64 -d >/var/lib/gantry-benchmark/operator-vm-prepare-run.sh
chmod 0755 /var/lib/gantry-benchmark/operator-vm-prepare-run.sh
cat >/etc/systemd/system/gantry-benchmark-image-prepare.service <<'UNIT'
[Unit]
Description=Gantry dual-ACR benchmark image preparation
After=network-online.target
Wants=network-online.target

[Service]
Type=oneshot
User=root
Environment=GANTRY_BENCHMARK_CONFIG=/etc/gantry-benchmark/env
Environment=BENCHMARK_PREPARE_ONLY=true
ExecStart=/var/lib/gantry-benchmark/operator-vm-prepare-run.sh
StandardOutput=append:/var/log/gantry-benchmark/service.log
StandardError=append:/var/log/gantry-benchmark/service.log
TimeoutStartSec=0
TimeoutStopSec=45min

[Install]
WantedBy=multi-user.target
UNIT
systemctl daemon-reload

source /etc/gantry-benchmark/env
printf 'image preparation: %s MiB in %s layers\n' "\$BENCHMARK_IMAGE_SIZE_MIB" "\$BENCHMARK_IMAGE_LAYERS"
printf 'baseline ACR: %s\n' "\$BASELINE_ACR_LOGIN_SERVER"
printf 'Gantry ACR: %s\n' "\$GANTRY_ACR_LOGIN_SERVER"

if systemctl is-failed --quiet "\$prepare_service"; then
  systemctl reset-failed "\$prepare_service"
fi
systemctl start --no-block "\$prepare_service"
systemctl show "\$prepare_service" --property=ActiveState --property=SubState --no-pager
SCRIPT
)

operator_ssh_init
operator_ssh sudo -n bash -s <<<"$wrapped_script"
