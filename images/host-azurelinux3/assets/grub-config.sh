#!/bin/sh
# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0

set -eu

: "${OUTPUT:?OUTPUT is required}"
: "${ROOT_UUID:?ROOT_UUID is required}"
: "${STANDARD_KVER:?STANDARD_KVER is required}"
: "${BOOT_PROFILE:?BOOT_PROFILE is required}"

CONSOLE="${CONSOLE:-}"
NVIDIA_KVER="${NVIDIA_KVER:-}"

if [ "$BOOT_PROFILE" = standard ]; then
  cat > "$OUTPUT" <<EOF
set timeout=0
search --no-floppy --fs-uuid --set=root $ROOT_UUID
linux /boot/vmlinuz-$STANDARD_KVER root=UUID=$ROOT_UUID rw $CONSOLE
initrd /boot/initramfs-$STANDARD_KVER.img
boot
EOF
  exit 0
fi

[ "$BOOT_PROFILE" = nvidia-hwe ] || {
  echo "unsupported boot profile: $BOOT_PROFILE" >&2
  exit 1
}
[ -n "$NVIDIA_KVER" ] || {
  echo "nvidia-hwe boot profile requires NVIDIA_KVER" >&2
  exit 1
}

cat > "$OUTPUT" <<EOF
set timeout=0
search --no-floppy --fs-uuid --set=root $ROOT_UUID
set default=nvidia-grace

menuentry "Azure Linux" --id standard {
  linux /boot/vmlinuz-$STANDARD_KVER root=UUID=$ROOT_UUID rw $CONSOLE
  initrd /boot/initramfs-$STANDARD_KVER.img
}

menuentry "Azure Linux NVIDIA Grace" --id nvidia-grace {
  linux /boot/vmlinuz-$NVIDIA_KVER root=UUID=$ROOT_UUID rw $CONSOLE
  initrd /boot/initramfs-$NVIDIA_KVER.img
}
EOF
