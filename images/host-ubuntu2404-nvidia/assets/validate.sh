#!/bin/sh
# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0

# Checks a host-ubuntu2404-nvidia disk image without booting it or using
# loop devices. The image build runs it on /disk/disk.img.gz, and it can be
# rerun on a published image after extracting /disk/disk.img.gz.
#
# Usage: validate.sh DISK_IMG_GZ
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
for file in EFI/BOOT/BOOTX64.EFI EFI/BOOT/grubx64.efi EFI/BOOT/mmx64.efi \
  EFI/BOOT/grub.cfg EFI/ubuntu/shimx64.efi EFI/ubuntu/grubx64.efi \
  EFI/ubuntu/grub.cfg; do
  mtype -i "$WORK/esp.img" "::/$file" >/dev/null 2>&1 || fail "ESP has no /$file"
done
if mtype -i "$WORK/esp.img" ::/EFI/BOOT/fbx64.efi >/dev/null 2>&1; then
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
# netplan configuration, and enables the NoCloud cloud-init datasource.
for path in /etc /var /etc/netplan /etc/cloud/cloud.cfg /usr/bin/cloud-init \
  /usr/sbin/netplan /usr/sbin/sshd /usr/bin/growpart \
  "/boot/vmlinuz-$KVER" "/boot/initrd.img-$KVER" \
  /usr/bin/nvidia-smi /usr/bin/nvidia-modprobe /usr/bin/nv-fabricmanager \
  /usr/bin/nvidia-fabricmanager-start.sh /opt/nvidia/nvlsm/sbin/nvlsm \
  /usr/share/nvidia/nvswitch/fabricmanager.cfg \
  /usr/share/nvidia/nvlsm/nvlsm.conf \
  /usr/sbin/ibstat /usr/bin/ibv_devinfo /usr/bin/ib_write_bw /usr/bin/lspci \
  /etc/modules-load.d/nvidia.conf /etc/modules-load.d/unbounded-rdma.conf \
  /etc/systemd/system/nvidia-persistence-mode.service \
  /etc/systemd/system/nvidia-fabricmanager.service.d/10-unbounded.conf \
  /etc/systemd/system/systemd-nspawn@.service.d/10-nvidia-fabricmanager.conf \
  /etc/systemd/system/unbounded-node-.service.d/10-nvidia-fabricmanager.conf \
  /etc/systemd/system/multi-user.target.wants/nvidia-persistence-mode.service \
  /etc/systemd/system/multi-user.target.wants/nvidia-fabricmanager.service \
  /etc/apt/preferences.d/nvidia-driver-pin \
  /usr/share/unbounded/nvidia-dkms-signing.der \
  /etc/hostname /etc/hosts; do
  root_exists "$path" || fail "root file system has no $path"
done

# The GPU kernel modules must exist for the image's kernel.
MODULES=$(root_cat "/usr/lib/modules/$KVER/modules.dep")
for module in nvidia nvidia-modeset nvidia-uvm nvidia-drm nvidia-peermem \
  ib_umad mlx5_ib mlx5_core; do
  printf '%s\n' "$MODULES" | grep -q "/$module\.ko[.a-z]*:" ||
    fail "kernel $KVER has no $module module"
done

for path in /var/lib/shim-signed/mok/MOK.priv /var/lib/dkms/mok.key \
  /etc/cloud/cloud-init.disabled \
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
