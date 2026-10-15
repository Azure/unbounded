#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0

set -euo pipefail

if [[ $# -ne 1 ]]; then
    echo "usage: $0 <directory-containing-disk/disk.img.gz>" >&2
    exit 2
fi

artifact_dir=$1
compressed_disk="${artifact_dir}/disk/disk.img.gz"
[[ -s "$compressed_disk" ]] || {
    echo "missing OCI artifact payload: $compressed_disk" >&2
    exit 1
}

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
gzip -dc "$compressed_disk" > "$work/disk.img"

partition_json=$(sfdisk --json "$work/disk.img")
[[ $(jq -r '.partitiontable.label' <<<"$partition_json") == gpt ]]
[[ $(jq '.partitiontable.partitions | length' <<<"$partition_json") -eq 2 ]]
[[ $(jq -r '.partitiontable.partitions[0].type' <<<"$partition_json" | tr '[:lower:]' '[:upper:]') == C12A7328-F81F-11D2-BA4B-00A0C93EC93B ]]
[[ $(jq -r '.partitiontable.partitions[1].type' <<<"$partition_json" | tr '[:lower:]' '[:upper:]') == 0FC63DAF-8483-4772-8E79-3D69D8477DE4 ]]

sector_size=$(jq -r '.partitiontable.sectorsize' <<<"$partition_json")
esp_start=$(jq -r '.partitiontable.partitions[0].start' <<<"$partition_json")
root_start=$(jq -r '.partitiontable.partitions[1].start' <<<"$partition_json")
root_size=$(jq -r '.partitiontable.partitions[1].size' <<<"$partition_json")

export MTOOLS_SKIP_CHECK=1
mdir -i "$work/disk.img@@$((esp_start * sector_size))" ::/EFI/BOOT/BOOTAA64.EFI >/dev/null
mdir -i "$work/disk.img@@$((esp_start * sector_size))" ::/EFI/BOOT/grubaa64.efi >/dev/null
mdir -i "$work/disk.img@@$((esp_start * sector_size))" ::/EFI/BOOT/grub.cfg >/dev/null

dd if="$work/disk.img" of="$work/root.img" bs="$sector_size" \
    skip="$root_start" count="$root_size" status=none
debugfs -R "cat /etc/unbounded-host-image" "$work/root.img" \
    > "$work/unbounded-host-image" 2>/dev/null

grep -Fxq 'architecture=arm64' "$work/unbounded-host-image"
grep -Fxq 'os=ubuntu-24.04' "$work/unbounded-host-image"
grep -Fxq 'kernel=6.8.0-1039-azure-nvidia' "$work/unbounded-host-image"
grep -Fxq 'kernel_package_version=6.8.0-1039.42' "$work/unbounded-host-image"
grep -Fxq 'nvidia_kernel_module_package_version=6.8.0-1039.42+3' "$work/unbounded-host-image"
grep -Fxq 'nvidia_driver_version=580.178.04-0ubuntu0.24.04.1' "$work/unbounded-host-image"
grep -Fxq 'nvidia_container_toolkit_version=1.19.1-1' "$work/unbounded-host-image"
grep -Fxq 'nvidia_fabric_manager_version=580.178.04-0ubuntu0.24.04.1' "$work/unbounded-host-image"
grep -Fxq 'nvidia_nscq_version=580.178.04-0ubuntu0.24.04.1' "$work/unbounded-host-image"
grep -Fxq 'nvidia_imex_version=580.178.04-1ubuntu1' "$work/unbounded-host-image"
grep -Fxq 'mana_driver=builtin' "$work/unbounded-host-image"
grep -Fxq 'mana_kernel_config=CONFIG_MICROSOFT_MANA=y' "$work/unbounded-host-image"
grep -Eq '^mana_ib_module=.+/mana_ib\.ko' "$work/unbounded-host-image"
grep -Eq '^nvidia_module=.+/nvidia\.ko' "$work/unbounded-host-image"
grep -Fxq 'imex_activation=masked' "$work/unbounded-host-image"

debugfs -R "stat /etc/systemd/system/nvidia-imex.service" "$work/root.img" 2>&1 \
    | grep -Fq 'Fast link dest: "/dev/null"'
debugfs -R "stat /boot/vmlinuz-6.8.0-1039-azure-nvidia" "$work/root.img" >/dev/null 2>&1
debugfs -R "stat /boot/initrd.img-6.8.0-1039-azure-nvidia" "$work/root.img" >/dev/null 2>&1

echo "verified arm64 Ubuntu GB200 GPT/EFI disk artifact"
