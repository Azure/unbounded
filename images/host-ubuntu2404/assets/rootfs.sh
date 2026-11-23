#!/bin/sh
# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0

# Builds the Ubuntu 24.04 root file system for the Metalman machine image. It
# runs inside the Ubuntu cloud image root file system for the target
# architecture and writes /rootfs.tar and /rootfs.kver for
# images/host-azurelinux3/assets/disk.sh.
#
# The image bakes in the host packages the unbounded agent requires, so first
# boot does not run the package manager.
#
# Usage: rootfs.sh install|finish
# The install step installs packages. The finish step configures the system
# and writes the archive. Separate steps let the build cache the slow step.
#
# Environment:
#   KERNEL_PACKAGE  kernel metapackage (default: linux-generic)
set -eu

KERNEL_PACKAGE="${KERNEL_PACKAGE:-linux-generic}"

# The kernel metapackage determines the image and headers packages, for
# example linux-generic-hwe-24.04 installs linux-image-generic-hwe-24.04 and
# linux-headers-generic-hwe-24.04.
KERNEL_FLAVOR="${KERNEL_PACKAGE#linux-}"

export DEBIAN_FRONTEND=noninteractive

retry() {
  n=1
  until "$@"; do
    if [ "$n" -ge 5 ]; then
      return 1
    fi
    echo "command failed (attempt $n/5), retrying in 30s: $*" >&2
    n=$((n + 1))
    sleep 30
  done
}

apt_install() {
  retry apt-get install -y --no-install-recommends "$@"
}

# The signed EFI loader names differ between amd64 and arm64. The cloud image
# root tarball is for the build's target architecture, so dpkg reports which
# names to use.
set_efi_names() {
  ARCH=$(dpkg --print-architecture)
  case "$ARCH" in
    amd64)
      GRUB_EFI_PACKAGE=grub-efi-amd64
      SHIM_EFI=shimx64.efi
      SHIM_EFI_SIGNED=/usr/lib/shim/shimx64.efi.signed.latest
      MOK_EFI=mmx64.efi
      GRUB_EFI=grubx64.efi
      GRUB_EFI_SIGNED=/usr/lib/grub/x86_64-efi-signed/grubx64.efi.signed
      BOOT_EFI=BOOTX64.EFI
      BOOT_CSV=/usr/lib/shim/BOOTX64.CSV
      ;;
    arm64)
      GRUB_EFI_PACKAGE=grub-efi-arm64
      SHIM_EFI=shimaa64.efi
      SHIM_EFI_SIGNED=/usr/lib/shim/shimaa64.efi.signed.latest
      MOK_EFI=mmaa64.efi
      GRUB_EFI=grubaa64.efi
      GRUB_EFI_SIGNED=/usr/lib/grub/arm64-efi-signed/grubaa64.efi.signed
      BOOT_EFI=BOOTAA64.EFI
      BOOT_CSV=/usr/lib/shim/BOOTAA64.CSV
      ;;
    *)
      echo "unsupported architecture: $ARCH" >&2
      exit 1
      ;;
  esac
}

install_packages() {
set_efi_names

# The image has no bootloader or kernel yet. Keep package scripts from
# updating a GRUB configuration or writing to NVRAM during the build. The
# disk assembly stage writes the boot configuration.
for tool in /usr/sbin/update-grub /usr/sbin/grub-install; do
  dpkg-divert --local --rename --add "$tool"
  ln -sf /bin/true "$tool"
done
echo "$GRUB_EFI_PACKAGE grub2/update_nvram boolean false" | debconf-set-selections

retry apt-get update
apt_install "$KERNEL_PACKAGE" "linux-headers-$KERNEL_FLAVOR" initramfs-tools \
  shim-signed "$GRUB_EFI_PACKAGE-signed" "$GRUB_EFI_PACKAGE-bin" efibootmgr \
  systemd-container nftables util-linux curl ca-certificates

apt-get clean
rm -rf /var/lib/apt/lists/*
}

kernel_version() {
  kver=$(ls /usr/lib/modules)
  case "$kver" in
    *" "*|*"
"*|"") echo "expected one kernel, found: $kver" >&2; exit 1 ;;
  esac
  echo "$kver"
}

finish() {
set_efi_names
KVER=$(kernel_version)

for tool in /usr/sbin/update-grub /usr/sbin/grub-install; do
  rm -f "$tool"
  dpkg-divert --local --rename --remove "$tool"
done

update-initramfs -c -k "$KVER" 2>/dev/null || update-initramfs -u -k "$KVER"
chmod 0644 "/boot/vmlinuz-$KVER" "/boot/initrd.img-$KVER"

# Ship shim and the Canonical-signed GRUB on the ESP. The Metalman installer
# adds a UEFI boot entry for EFI/BOOT/$BOOT_EFI. Shim loads $GRUB_EFI from the
# same directory, and GRUB reads its configuration from EFI/ubuntu. The
# fallback loader is left out so that firmware boots the entry directly.
for dir in /boot/efi/EFI/BOOT /boot/efi/EFI/ubuntu; do
  mkdir -p "$dir"
  cp "$SHIM_EFI_SIGNED" "$dir/$SHIM_EFI"
  cp "/usr/lib/shim/$MOK_EFI" "$dir/$MOK_EFI"
  cp "$GRUB_EFI_SIGNED" "$dir/$GRUB_EFI"
done
mv "/boot/efi/EFI/BOOT/$SHIM_EFI" "/boot/efi/EFI/BOOT/$BOOT_EFI"
cp "$BOOT_CSV" /boot/efi/EFI/ubuntu/ 2>/dev/null || true

# The host has no package source on first boot, so none of the daily apt or
# snap traffic is useful. Masking the units leaves the packages installed for
# an operator who wants to re-enable them.
systemctl mask apt-daily.timer apt-daily-upgrade.timer \
  unattended-upgrades.service motd-news.timer \
  snapd.refresh.timer snapd.snap-repair.timer

# Each machine generates its own machine ID and SSH host keys on first boot.
: > /etc/machine-id
rm -f /var/lib/dbus/machine-id
rm -f /etc/ssh/ssh_host_*

# Restore the files the container runtime manages. disk.sh links
# /etc/resolv.conf.
printf 'ubuntu\n' > /etc/hostname.unbounded
cat > /etc/hosts.unbounded <<'EOF'
127.0.0.1 localhost

# The following lines are desirable for IPv6 capable hosts
::1 ip6-localhost ip6-loopback
fe00::0 ip6-localnet
ff00::0 ip6-mcastprefix
ff02::1 ip6-allnodes
ff02::2 ip6-allrouters
EOF

tar --create --file=/rootfs.tar --numeric-owner --xattrs \
  --one-file-system \
  --exclude=./rootfs.tar --exclude=./rootfs.kver --exclude=./proc \
  --exclude=./sys --exclude=./dev --exclude=./run --exclude=./tmp \
  --exclude=./var/cache/apt/archives/*.deb \
  --exclude=./etc/resolv.conf --exclude=./etc/hostname --exclude=./etc/hosts \
  --exclude=./.dockerenv --exclude=./usr/local/bin/build-rootfs \
  --transform='s,^\./etc/hostname\.unbounded$,./etc/hostname,' \
  --transform='s,^\./etc/hosts\.unbounded$,./etc/hosts,' \
  -C / .
echo "$KVER" > /rootfs.kver
}

case "${1:-}" in
  install) install_packages ;;
  finish) finish ;;
  *) echo "usage: $0 install|finish" >&2; exit 2 ;;
esac
