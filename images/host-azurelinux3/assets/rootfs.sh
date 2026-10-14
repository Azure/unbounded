#!/bin/sh
# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0

# Builds the Azure Linux 3.0 root file system for a Metalman machine image.
# It runs inside an Azure Linux container for the target architecture and
# writes /rootfs.tar for the disk assembly stage.
#
# Usage: rootfs.sh install|finish
# The install step installs packages. The finish step configures the system
# and writes the archive. Separate steps let the build cache the slow step.
#
# Environment:
#   KERNEL_PACKAGE  kernel package to install (default: kernel)
#   NVIDIA          set to 1 to install the NVIDIA open GPU driver
set -eu

KERNEL_PACKAGE="${KERNEL_PACKAGE:-kernel}"
NVIDIA="${NVIDIA:-0}"

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

install_packages() {
if [ "$NVIDIA" = 1 ]; then
  cat > /etc/yum.repos.d/azurelinux-nvidia.repo <<'EOF'
[azurelinux-official-nvidia]
name=Azure Linux Official NVIDIA $releasever $basearch
baseurl=https://packages.microsoft.com/azurelinux/$releasever/prod/nvidia/$basearch
gpgkey=file:///etc/pki/rpm-gpg/MICROSOFT-RPM-GPG-KEY
gpgcheck=1
repo_gpgcheck=1
enabled=1
skip_if_unavailable=False
sslverify=1
EOF
fi

# Install the kernel first so that driver packages build against it.
retry tdnf install -y "$KERNEL_PACKAGE"

retry tdnf install -y \
  shim grub2-efi-binary dracut \
  systemd systemd-udev systemd-networkd systemd-resolved systemd-container dbus \
  cloud-init cloud-utils-growpart gptfdisk e2fsprogs \
  openssh-server sudo shadow-utils \
  ca-certificates curl tar gzip \
  iproute iputils nftables util-linux kmod procps-ng \
  efibootmgr chrony less

if [ "$NVIDIA" = 1 ]; then
  retry tdnf install -y cuda-open-hwe
fi

tdnf clean all
}

finish() {
KVER=$(ls /usr/lib/modules)
case "$KVER" in
  *" "*|"") echo "expected one kernel, found: $KVER" >&2; exit 1 ;;
esac

# Build a generic initramfs. The image must boot on any machine, so do not
# limit it to the build host's hardware. The GPU driver loads after boot.
dracut --force --no-hostonly --no-hostonly-cmdline \
  --omit-drivers "nvidia nvidia_modeset nvidia_uvm nvidia_drm nvidia_peermem" \
  --kver "$KVER" "/boot/initramfs-$KVER.img"
chmod 0644 "/boot/vmlinuz-$KVER" "/boot/initramfs-$KVER.img"

if [ "$NVIDIA" = 1 ]; then
  cat > /etc/modules-load.d/nvidia.conf <<'EOF'
nvidia
nvidia_modeset
nvidia_uvm
nvidia_drm
EOF
  # The unbounded agent checks for this unit. It creates the device nodes
  # and keeps the driver loaded between GPU jobs. It is skipped on machines
  # without a GPU bound to the driver.
  cat > /etc/systemd/system/nvidia-persistence-mode.service <<'EOF'
[Unit]
Description=Enable NVIDIA persistence mode
After=systemd-modules-load.service
ConditionPathExistsGlob=/sys/bus/pci/drivers/nvidia/0*

[Service]
Type=oneshot
RemainAfterExit=yes
ExecStartPre=/usr/bin/nvidia-modprobe -u -c=0
ExecStart=/usr/bin/nvidia-smi -pm 1

[Install]
WantedBy=multi-user.target
EOF
  systemctl enable nvidia-persistence-mode.service
fi

# Use DHCP on wired interfaces by default. The Metalman installer replaces
# this file with a static configuration for the machine's lease.
cat > /etc/systemd/network/90-unbounded-dhcp.network <<'EOF'
[Match]
Name=en* eth*

[Network]
DHCP=yes
EOF

# Wait for any one link, not every link. Machines often have unused ports.
mkdir -p /etc/systemd/system/systemd-networkd-wait-online.service.d
cat > /etc/systemd/system/systemd-networkd-wait-online.service.d/any.conf <<'EOF'
[Service]
ExecStart=
ExecStart=/usr/lib/systemd/systemd-networkd-wait-online --any --timeout=120
EOF

systemctl enable systemd-networkd.service systemd-resolved.service \
  sshd.service chronyd.service
systemctl disable systemd-homed.service || true
for unit in cloud-init-local.service cloud-init.service cloud-init-network.service \
  cloud-config.service cloud-final.service; do
  if [ -f "/usr/lib/systemd/system/$unit" ]; then
    systemctl enable "$unit"
  fi
done

# Azure Linux firewall services default to DROP policies. Kubernetes and the
# unbounded agent own the node's firewall rules.
systemctl mask iptables.service ip6tables.service nftables.service

# Each machine generates its own machine ID and SSH host keys on first boot.
: > /etc/machine-id
rm -f /etc/ssh/ssh_host_*

tar --create --file=/rootfs.tar --numeric-owner --xattrs \
  --one-file-system \
  --exclude=./rootfs.tar --exclude=./proc --exclude=./sys --exclude=./dev \
  --exclude=./run --exclude=./tmp --exclude=./var/cache/tdnf \
  --exclude=./etc/resolv.conf --exclude=./etc/hostname --exclude=./etc/hosts \
  --exclude=./.dockerenv \
  -C / .
echo "$KVER" > /rootfs.kver
}

case "${1:-}" in
  install) install_packages ;;
  finish) finish ;;
  *) echo "usage: $0 install|finish" >&2; exit 2 ;;
esac
