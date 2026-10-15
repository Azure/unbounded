#!/bin/sh
# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0

# Assembles an arm64 bootable GPT disk without loop devices or privileges.
set -eu

: "${TARGETARCH:?TARGETARCH is required}"
if [ "$TARGETARCH" != arm64 ]; then
  echo "host-ubuntu2404-nvidia only supports arm64" >&2
  exit 1
fi

WORK=/tmp/disk
ROOT="$WORK/root"
ESP_MIB=256
ALIGN_MIB=1

KVER=$(cat /rootfs.kver)
mkdir -p "$ROOT"
tar --extract --file=/rootfs.tar --numeric-owner --xattrs \
  --xattrs-include='*' -C "$ROOT"
mkdir -p "$ROOT/proc" "$ROOT/sys" "$ROOT/dev" "$ROOT/run" "$ROOT/tmp" \
  "$ROOT/boot/efi"
chmod 1777 "$ROOT/tmp"
ln -sf ../run/systemd/resolve/stub-resolv.conf "$ROOT/etc/resolv.conf"

ROOT_UUID=$(cat /proc/sys/kernel/random/uuid)
ESP_ID=$(od -An -N4 -tx4 /dev/urandom | tr -d ' \n')
ESP_UUID=$(printf '%s' "$ESP_ID" | tr 'a-f' 'A-F' | sed 's/^\(....\)/\1-/')

cat > "$ROOT/etc/fstab" <<EOF
UUID=$ROOT_UUID / ext4 defaults 0 1
UUID=$ESP_UUID /boot/efi vfat umask=0077 0 2
EOF

cat > "$WORK/grub.cfg" <<EOF
set timeout=0
search --no-floppy --fs-uuid --set=root $ROOT_UUID
linux /boot/vmlinuz-$KVER root=UUID=$ROOT_UUID rw
initrd /boot/initrd.img-$KVER
boot
EOF

ESP_IMG="$WORK/esp.img"
truncate -s "${ESP_MIB}M" "$ESP_IMG"
mkfs.vfat -F 32 -i "$ESP_ID" -n EFI "$ESP_IMG" >/dev/null
export MTOOLS_SKIP_CHECK=1
mmd -i "$ESP_IMG" ::/EFI ::/EFI/BOOT
mcopy -i "$ESP_IMG" \
  "$ROOT/usr/lib/shim/shimaa64.efi.signed.latest" \
  ::/EFI/BOOT/BOOTAA64.EFI
mcopy -i "$ESP_IMG" \
  "$ROOT/usr/lib/grub/arm64-efi-signed/grubaa64.efi.signed" \
  ::/EFI/BOOT/grubaa64.efi
mcopy -i "$ESP_IMG" "$WORK/grub.cfg" ::/EFI/BOOT/grub.cfg
rm -rf "${ROOT:?}/boot/efi/"*

USED_MIB=$(du -sm "$ROOT" | cut -f1)
ROOT_MIB=$((USED_MIB + USED_MIB * 3 / 10 + 1024))
ROOT_IMG="$WORK/root.img"
truncate -s "${ROOT_MIB}M" "$ROOT_IMG"
mkfs.ext4 -q -F -O ^orphan_file -U "$ROOT_UUID" -L root \
  -E root_owner=0:0 -d "$ROOT" "$ROOT_IMG"

DISK_MIB=$((ALIGN_MIB + ESP_MIB + ROOT_MIB + 1))
DISK="$WORK/disk.img"
truncate -s "${DISK_MIB}M" "$DISK"
sfdisk --quiet "$DISK" <<EOF
label: gpt
first-lba: 2048
start=${ALIGN_MIB}MiB, size=${ESP_MIB}MiB, type=C12A7328-F81F-11D2-BA4B-00A0C93EC93B, name="EFI"
start=$((ALIGN_MIB + ESP_MIB))MiB, size=${ROOT_MIB}MiB, type=0FC63DAF-8483-4772-8E79-3D69D8477DE4, name="root"
EOF
dd if="$ESP_IMG" of="$DISK" bs=1M seek="$ALIGN_MIB" conv=notrunc,sparse status=none
dd if="$ROOT_IMG" of="$DISK" bs=1M seek=$((ALIGN_MIB + ESP_MIB)) conv=notrunc,sparse status=none

mkdir -p /disk
pigz -1 -c "$DISK" > /disk/disk.img.gz
rm -rf "$WORK"
