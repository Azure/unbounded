#!/bin/sh
# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0

set -eu

DISK_GZ="${1:-/disk/disk.img.gz}"
[ -f "$DISK_GZ" ] || {
  echo "missing Metalman disk artifact: $DISK_GZ" >&2
  exit 1
}

for command in debugfs gzip mtype python3 sfdisk; do
  command -v "$command" >/dev/null 2>&1 || {
    echo "required inspection command is missing: $command" >&2
    exit 1
  }
done

WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT
DISK="$WORK/disk.img"
ROOT="$WORK/root.img"
gzip -dc "$DISK_GZ" > "$DISK"

partition_field() {
  partition="$1"
  field="$2"
  sfdisk --json "$DISK" | python3 -c \
    'import json,sys
data=json.load(sys.stdin)["partitiontable"]["partitions"][int(sys.argv[1])]
print(data[sys.argv[2]])' "$partition" "$field"
}

ESP_START=$(partition_field 0 start)
ROOT_START=$(partition_field 1 start)
ROOT_SIZE=$(partition_field 1 size)
[ "$ESP_START" = 2048 ] || {
  echo "unexpected ESP start sector: $ESP_START" >&2
  exit 1
}
dd if="$DISK" of="$ROOT" bs=512 skip="$ROOT_START" count="$ROOT_SIZE" status=none

debugfs_exists() {
  debugfs -R "stat $1" "$ROOT" >/dev/null 2>&1
}

for path in \
  /etc/default/grub.d/51-azure-nvidia.cfg \
  /etc/grub.d/10_azure_nvidia \
  /etc/modprobe.d/blacklist-nouveau.conf \
  /etc/modprobe.d/nvidia-gb.conf \
  /etc/systemd/system/nvidia-persistence-mode.service \
  /etc/unbounded/image-inputs \
  /usr/lib/grub/arm64-efi/smbios.mod; do
  debugfs_exists "$path" || {
    echo "missing expected file in root partition: $path" >&2
    exit 1
  }
done

INPUTS=$(debugfs -R "cat /etc/unbounded/image-inputs" "$ROOT" 2>/dev/null)
for expected in \
  architecture=arm64 \
  image_variant=nvidia \
  boot_profile=nvidia-hwe \
  kernel=6.6.157.1-1.azl3 \
  kernel_hwe=6.18.48.1-1.azl3 \
  grub=2.06-27.azl3 \
  cuda_open_hwe=580.159.04-2_6.18.48.1.1.azl3; do
  printf '%s\n' "$INPUTS" | grep -Fx "$expected" >/dev/null || {
    echo "missing image input: $expected" >&2
    exit 1
  }
done
echo "PASS package inputs:"
printf '%s\n' "$INPUTS" | sed 's/^/  /'

for kver in 6.6.157.1-1.azl3 6.18.48.1-1.azl3; do
  debugfs_exists "/boot/vmlinuz-$kver" || {
    echo "missing kernel: $kver" >&2
    exit 1
  }
  debugfs_exists "/boot/initramfs-$kver.img" || {
    echo "missing initramfs: $kver" >&2
    exit 1
  }
done
echo "PASS kernels:"
echo "  standard=6.6.157.1-1.azl3"
echo "  nvidia-grace=6.18.48.1-1.azl3"

GRUB=$(mtype -i "$DISK@@$((ESP_START * 512))" ::/EFI/BOOT/grub.cfg)
for expected in \
  'set default=nvidia-grace' \
  'linux /boot/vmlinuz-6.18.48.1-1.azl3'; do
  printf '%s\n' "$GRUB" | grep -F "$expected" >/dev/null || {
    echo "missing GRUB contract: $expected" >&2
    exit 1
  }
done
for unexpected in \
  'insmod smbios' \
  'smbios --type 4' \
  'cpu_manufacturer' \
  'if [ x$cpu_manufacturer' \
  'iommu.passthrough=1' \
  'irqchip.gicv3_nolpi=y' \
  'arm_smmu_v3.disable_msipolling=1'; do
  if printf '%s\n' "$GRUB" | grep -F "$unexpected" >/dev/null; then
    echo "NVIDIA image unexpectedly uses runtime SMBIOS selection: $unexpected" >&2
    exit 1
  fi
done

STANDARD_ENTRY=$(printf '%s\n' "$GRUB" | sed -n \
  '/menuentry "Azure Linux" --id standard {/,/^}/p')
GRACE_ENTRY=$(printf '%s\n' "$GRUB" | sed -n \
  '/menuentry "Azure Linux NVIDIA Grace" --id nvidia-grace {/,/^}/p')
printf '%s\n' "$STANDARD_ENTRY" | grep -F \
  'linux /boot/vmlinuz-6.6.157.1-1.azl3' >/dev/null || {
  echo "ordinary ARM64 entry does not select the standard kernel" >&2
  exit 1
}
if printf '%s\n' "$STANDARD_ENTRY" | grep -F 'nvidia_args' >/dev/null; then
  echo "ordinary ARM64 entry unexpectedly contains Grace arguments" >&2
  exit 1
fi
printf '%s\n' "$GRACE_ENTRY" | grep -F \
  'linux /boot/vmlinuz-6.18.48.1-1.azl3' >/dev/null || {
  echo "NVIDIA Grace entry does not select kernel-hwe" >&2
  exit 1
}
echo "PASS specialized NVIDIA GRUB policy:"
echo "  default=nvidia-grace (no runtime SMBIOS decision)"
echo "  recovery entry -> vmlinuz-6.6.157.1-1.azl3, no Grace arguments"
echo "  default entry -> vmlinuz-6.18.48.1-1.azl3, no extra Grace arguments"
echo "PASS MANA-compatible kernel command line:"
echo "  irqchip.gicv3_nolpi is absent; GICv3 LPI/ITS MSI-X remains enabled"
echo "  iommu.passthrough and arm_smmu_v3.disable_msipolling are absent"
echo "PASS GRUB package module: /usr/lib/grub/arm64-efi/smbios.mod"
echo "verified Azure Linux NVIDIA ARM64 disk: $DISK_GZ"
