#!/bin/sh
# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0

# Checks a host-ubuntu2404 disk image without booting it or using loop
# devices. The image build runs it on /disk/disk.img.gz, and it can be rerun
# on a published image after extracting /disk/disk.img.gz.
#
# Usage: [TARGETARCH=amd64|arm64] validate.sh DISK_IMG_GZ
# Requires: sfdisk, debugfs (e2fsprogs), mtools, pigz or gzip.
set -eu

IMG_GZ="${1:?usage: $0 DISK_IMG_GZ}"
WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT
FAILED=0

fail() {
  echo "FAIL: $*" >&2
  FAILED=1
}

if command -v pigz >/dev/null; then
  pigz -dc "$IMG_GZ" > "$WORK/disk.img"
else
  gzip -dc "$IMG_GZ" > "$WORK/disk.img"
fi

# Partition 1 is the ESP and partition 2 is the root file system.
part() {
  sfdisk --dump "$WORK/disk.img" | grep "^$WORK/disk.img$1 " |
    sed -n 's/.*start= *\([0-9]*\), size= *\([0-9]*\),.*/\1 \2/p'
}
# shellcheck disable=SC2046 # Split "start size" into two fields.
set -- $(part 1)
ESP_START=$1 ESP_SIZE=$2
# shellcheck disable=SC2046 # Split "start size" into two fields.
set -- $(part 2)
ROOT_START=$1 ROOT_SIZE=$2
dd if="$WORK/disk.img" of="$WORK/esp.img" bs=512 skip="$ESP_START" count="$ESP_SIZE" status=none
dd if="$WORK/disk.img" of="$WORK/root.img" bs=512 skip="$ROOT_START" count="$ROOT_SIZE" status=none
rm -f "$WORK/disk.img"

export MTOOLS_SKIP_CHECK=1

# The signed loader names differ per architecture. The build passes TARGETARCH;
# a published image can be validated without it.
if [ -z "${TARGETARCH:-}" ]; then
  if mtype -i "$WORK/esp.img" ::/EFI/BOOT/BOOTX64.EFI >/dev/null 2>&1; then
    TARGETARCH=amd64
  elif mtype -i "$WORK/esp.img" ::/EFI/BOOT/BOOTAA64.EFI >/dev/null 2>&1; then
    TARGETARCH=arm64
  else
    fail "ESP has no shim loader, cannot detect the architecture"
    exit 1
  fi
fi

case "$TARGETARCH" in
  amd64)
    SHIM_EFI=shimx64.efi
    MOK_EFI=mmx64.efi
    GRUB_EFI=grubx64.efi
    BOOT_EFI=BOOTX64.EFI
    FALLBACK_EFI=fbx64.efi
    ;;
  arm64)
    SHIM_EFI=shimaa64.efi
    MOK_EFI=mmaa64.efi
    GRUB_EFI=grubaa64.efi
    BOOT_EFI=BOOTAA64.EFI
    FALLBACK_EFI=fbaa64.efi
    ;;
  *)
    fail "unsupported architecture: $TARGETARCH"
    exit 1
    ;;
esac

for file in "EFI/BOOT/$BOOT_EFI" "EFI/BOOT/$GRUB_EFI" "EFI/BOOT/$MOK_EFI" \
  EFI/BOOT/grub.cfg "EFI/ubuntu/$SHIM_EFI" "EFI/ubuntu/$GRUB_EFI" \
  EFI/ubuntu/grub.cfg; do
  mtype -i "$WORK/esp.img" "::/$file" >/dev/null 2>&1 || fail "ESP has no /$file"
done
if mtype -i "$WORK/esp.img" "::/EFI/BOOT/$FALLBACK_EFI" >/dev/null 2>&1; then
  fail "ESP has the shim fallback loader in /EFI/BOOT"
fi

root_cat() {
  debugfs -R "cat $1" "$WORK/root.img" 2>/dev/null
}

root_exists() {
  debugfs -R "stat $1" "$WORK/root.img" 2>&1 | grep -q '^Inode:'
}

KVER=$(root_cat /boot/grub/grub.cfg | sed -n 's|^linux /boot/vmlinuz-\([^ ]*\) .*|\1|p')
[ -n "$KVER" ] || fail "/boot/grub/grub.cfg has no kernel entry"
echo "kernel: $KVER"

# Metalman's installer finds the root partition by /etc and /var, writes
# netplan configuration, and enables the NoCloud cloud-init datasource. The
# host binaries are the ones the unbounded agent requires before it can start
# a node, and they must come from the image because first boot has no package
# source.
for path in /etc /var /etc/netplan /etc/cloud/cloud.cfg /usr/bin/cloud-init \
  /usr/sbin/netplan /usr/sbin/sshd /usr/bin/growpart \
  "/boot/vmlinuz-$KVER" "/boot/initrd.img-$KVER" \
  /usr/bin/systemd-nspawn /usr/sbin/nft /usr/bin/curl /usr/bin/nsenter \
  /usr/bin/lsblk /usr/bin/mountpoint \
  /etc/systemd/system/apt-daily.timer \
  /etc/systemd/system/apt-daily-upgrade.timer \
  /etc/systemd/system/unattended-upgrades.service \
  /etc/systemd/system/motd-news.timer \
  /etc/hostname /etc/hosts; do
  root_exists "$path" || fail "root file system has no $path"
done

for path in /etc/cloud/cloud-init.disabled \
  /etc/ssh/ssh_host_rsa_key /etc/ssh/ssh_host_ed25519_key \
  /etc/ssh/ssh_host_ecdsa_key; do
  if root_exists "$path"; then
    fail "root file system must not contain $path"
  fi
done
[ -z "$(root_cat /etc/machine-id)" ] || fail "/etc/machine-id is not empty"

if [ "$FAILED" != 0 ]; then
  echo "disk image validation failed" >&2
  exit 1
fi
echo "disk image validation passed"
