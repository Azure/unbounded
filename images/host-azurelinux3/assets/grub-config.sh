#!/bin/sh
# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0

set -eu

: "${OUTPUT:?OUTPUT is required}"
: "${ROOT_UUID:?ROOT_UUID is required}"
: "${STANDARD_KVER:?STANDARD_KVER is required}"

CONSOLE="${CONSOLE:-}"
NVIDIA_KVER="${NVIDIA_KVER:-}"

if [ -z "$NVIDIA_KVER" ]; then
  cat > "$OUTPUT" <<EOF
set timeout=0
search --no-floppy --fs-uuid --set=root $ROOT_UUID
linux /boot/vmlinuz-$STANDARD_KVER root=UUID=$ROOT_UUID rw $CONSOLE
initrd /boot/initramfs-$STANDARD_KVER.img
boot
EOF
  exit 0
fi

cat > "$OUTPUT" <<EOF
set timeout=0
search --no-floppy --fs-uuid --set=root $ROOT_UUID
set azure_nvidia_saved_prefix="\$prefix"
set prefix="(\$root)/usr/lib/grub/arm64-efi"
insmod smbios
set prefix="\$azure_nvidia_saved_prefix"
unset azure_nvidia_saved_prefix

smbios --type 4 --get-string 7 --set cpu_manufacturer
if [ x\$cpu_manufacturer = xNVIDIA ]; then
  set default=nvidia-grace
  set nvidia_args="iommu.passthrough=1 irqchip.gicv3_nolpi=y arm_smmu_v3.disable_msipolling=1"
else
  set default=standard
fi

menuentry "Azure Linux" --id standard {
  linux /boot/vmlinuz-$STANDARD_KVER root=UUID=$ROOT_UUID rw $CONSOLE
  initrd /boot/initramfs-$STANDARD_KVER.img
}

menuentry "Azure Linux NVIDIA Grace" --id nvidia-grace {
  linux /boot/vmlinuz-$NVIDIA_KVER root=UUID=$ROOT_UUID rw $CONSOLE \$nvidia_args
  initrd /boot/initramfs-$NVIDIA_KVER.img
}
EOF
