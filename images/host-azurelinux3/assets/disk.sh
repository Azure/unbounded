#!/bin/sh
# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0

# Assembles a bootable GPT disk image from a root file system archive without
# loop devices or privileges. It runs on the build platform.
#
# Environment:
#   TARGETARCH  amd64 or arm64
#   DISTRO      azurelinux (default) or ubuntu; selects the initramfs name
#               and where the signed GRUB looks for its configuration
#   MSHV        set to 1 to start the Microsoft Hypervisor before Linux
#   ROOTFS_TAR  root file system archive (default /rootfs.tar)
#   ROOTFS_KVER file holding the kernel version (default /rootfs.kver)
#   WORK        scratch directory, removed on success (default /tmp/disk)
#   OUTPUT      compressed disk image (default /disk/disk.img.gz)
set -eu

: "${TARGETARCH:?TARGETARCH is required}"
DISTRO="${DISTRO:-azurelinux}"
MSHV="${MSHV:-0}"

ROOTFS_TAR="${ROOTFS_TAR:-/rootfs.tar}"
ROOTFS_KVER="${ROOTFS_KVER:-/rootfs.kver}"
WORK="${WORK:-/tmp/disk}"
OUTPUT="${OUTPUT:-/disk/disk.img.gz}"
ROOT="$WORK/root"
ESP_MIB=256
ALIGN_MIB=1

case "$TARGETARCH" in
  amd64) CONSOLE="console=tty0 console=ttyS0,115200n8" ;;
  # arm64 servers describe their console in the ACPI SPCR table.
  arm64) CONSOLE="" ;;
  *) echo "unsupported architecture: $TARGETARCH" >&2; exit 1 ;;
esac

case "$DISTRO" in
  azurelinux) INITRD_PREFIX=initramfs- INITRD_SUFFIX=.img ;;
  ubuntu) INITRD_PREFIX=initrd.img- INITRD_SUFFIX= ;;
  *) echo "unsupported distribution: $DISTRO" >&2; exit 1 ;;
esac

if [ "$MSHV" = 1 ] && [ "$DISTRO" != azurelinux ]; then
  echo "MSHV images support Azure Linux only, not $DISTRO" >&2
  exit 1
fi

if [ "$MSHV" = 1 ] && [ "$TARGETARCH" != amd64 ]; then
  echo "MSHV images support amd64 only, not $TARGETARCH" >&2
  exit 1
fi

KVER=$(cat "$ROOTFS_KVER")
mkdir -p "$ROOT"
tar --extract --file="$ROOTFS_TAR" --numeric-owner --xattrs \
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

# The ESP holds shim and GRUB. The signed Azure Linux GRUB reads
# /boot/grub2/grub.cfg from the partition it was loaded from.
echo "set timeout=0" > "$WORK/grub.cfg"
if [ "$MSHV" = 1 ]; then
  # HvLoader.efi starts the hypervisor through lxhvloader.dll and returns to
  # GRUB, which then boots Linux as the root partition. This matches the
  # "Dom0" entry that the kernel-mshv package adds to grub2-mkconfig. GRUB
  # unescapes "\\Windows" to "\Windows", the hypervisor directory on the ESP.
  cat >> "$WORK/grub.cfg" <<EOF
search --no-floppy --fs-uuid --set=root $ESP_UUID
chainloader /HvLoader.efi lxhvloader.dll MSHV_ROOT=\\\\Windows MSHV_ENABLE=TRUE MSHV_SCHEDULER_TYPE=ROOT MSHV_X2APIC_POLICY=ENABLE MSHV_SEV_SNP=TRUE MSHV_LOAD_OPTION=INCLUDETRACEMETADATA=1
boot
EOF
fi
cat >> "$WORK/grub.cfg" <<EOF
search --no-floppy --fs-uuid --set=root $ROOT_UUID
linux /boot/vmlinuz-$KVER root=UUID=$ROOT_UUID rw $CONSOLE
initrd /boot/$INITRD_PREFIX$KVER$INITRD_SUFFIX
boot
EOF

ESP_IMG="$WORK/esp.img"
truncate -s "${ESP_MIB}M" "$ESP_IMG"
mkfs.vfat -F 32 -i "$ESP_ID" -n EFI "$ESP_IMG" >/dev/null
export MTOOLS_SKIP_CHECK=1
# Copy everything the packages installed on the ESP: shim and GRUB under
# EFI/, plus the hypervisor and its loaders on MSHV images.
mcopy -s -i "$ESP_IMG" "$ROOT/boot/efi/"* ::/
if [ "$DISTRO" = ubuntu ]; then
  # The Canonical-signed GRUB reads EFI/ubuntu/grub.cfg on the ESP. Keep
  # the boot entry on the root file system in /boot/grub/grub.cfg, where
  # update-grub on the machine replaces it with a generated configuration.
  mkdir -p "$ROOT/boot/grub"
  cp "$WORK/grub.cfg" "$ROOT/boot/grub/grub.cfg"
  cat > "$WORK/esp-grub.cfg" <<EOF
search --no-floppy --fs-uuid --set=root $ROOT_UUID
set prefix=(\$root)/boot/grub
configfile \$prefix/grub.cfg
EOF
  mcopy -i "$ESP_IMG" "$WORK/esp-grub.cfg" ::/EFI/ubuntu/grub.cfg
  mcopy -i "$ESP_IMG" "$WORK/esp-grub.cfg" ::/EFI/BOOT/grub.cfg
else
  mmd -i "$ESP_IMG" ::/boot ::/boot/grub2
  mcopy -i "$ESP_IMG" "$WORK/grub.cfg" ::/boot/grub2/grub.cfg
  mcopy -i "$ESP_IMG" "$WORK/grub.cfg" ::/EFI/BOOT/grub.cfg
fi
# The ESP is mounted at /boot/efi, so keep the root copy empty.
rm -rf "${ROOT:?}/boot/efi/"*

# Size the root file system with room for first boot. cloud-init grows the
# partition and file system to fill the disk.
USED_MIB=$(du -sm "$ROOT" | cut -f1)
ROOT_MIB=$((USED_MIB + USED_MIB * 3 / 10 + 512))
ROOT_IMG="$WORK/root.img"
truncate -s "${ROOT_MIB}M" "$ROOT_IMG"
mkfs.ext4 -q -F -O ^orphan_file -U "$ROOT_UUID" -L root \
  -E root_owner=0:0 -d "$ROOT" "$ROOT_IMG"

# GPT: ESP, then root. Leave 1 MiB at the end for the backup GPT header.
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

mkdir -p "$(dirname "$OUTPUT")"
pigz -1 -c "$DISK" > "$OUTPUT"
rm -rf "$WORK"
