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
#   NVIDIA          set to 1 to install the NVIDIA open GPU driver
#   TARGETARCH      target architecture
set -eu

NVIDIA="${NVIDIA:-0}"
TARGETARCH="${TARGETARCH:-}"

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
  [ "$TARGETARCH" = arm64 ] || {
    echo "the Azure Linux NVIDIA host image supports only arm64" >&2
    exit 1
  }
  : "${AZURELINUX_KERNEL_VERSION:?AZURELINUX_KERNEL_VERSION is required}"
  : "${AZURELINUX_KERNEL_HWE_VERSION:?AZURELINUX_KERNEL_HWE_VERSION is required}"
  : "${AZURELINUX_GRUB_VERSION:?AZURELINUX_GRUB_VERSION is required}"
  : "${AZURELINUX_CUDA_OPEN_HWE_VERSION:?AZURELINUX_CUDA_OPEN_HWE_VERSION is required}"

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

if [ "$NVIDIA" = 1 ]; then
  retry tdnf install -y \
    "kernel-${AZURELINUX_KERNEL_VERSION}" \
    "kernel-hwe-${AZURELINUX_KERNEL_HWE_VERSION}"
else
  retry tdnf install -y kernel
fi

retry tdnf install -y \
  shim dracut \
  systemd systemd-udev systemd-networkd systemd-resolved systemd-container dbus \
  cloud-init cloud-utils-growpart gptfdisk e2fsprogs \
  openssh-server sudo shadow-utils \
  ca-certificates curl tar gzip \
  iproute iputils nftables util-linux kmod procps-ng \
  efibootmgr chrony less

if [ "$NVIDIA" = 1 ]; then
  retry tdnf install -y \
    "grub2-${AZURELINUX_GRUB_VERSION}" \
    "grub2-efi-binary-${AZURELINUX_GRUB_VERSION}" \
    "grub2-efi-${AZURELINUX_GRUB_VERSION}"
  retry tdnf install -y "cuda-open-hwe-${AZURELINUX_CUDA_OPEN_HWE_VERSION}"
else
  retry tdnf install -y grub2-efi-binary
fi

tdnf clean all
}

finish() {
if [ "$NVIDIA" = 1 ]; then
  for package_and_version in \
    "kernel:$AZURELINUX_KERNEL_VERSION" \
    "kernel-hwe:$AZURELINUX_KERNEL_HWE_VERSION"; do
    package=${package_and_version%%:*}
    expected_version=${package_and_version#*:}
    version=$(rpm -q --queryformat '%{VERSION}-%{RELEASE}' "$package")
    [ "$version" = "$expected_version" ] || {
      echo "$package resolved to $version, expected $expected_version" >&2
      exit 1
    }
  done

  STANDARD_KVER=$(rpm -ql kernel | sed -n 's#^/boot/vmlinuz-##p' | sort -V | tail -n1)
  NVIDIA_KVER=$(rpm -ql kernel-hwe | sed -n 's#^/boot/vmlinuz-##p' | sort -V | tail -n1)
  [ -n "$STANDARD_KVER" ] && [ -n "$NVIDIA_KVER" ] || {
    echo "both kernel and kernel-hwe must provide a bootable kernel" >&2
    exit 1
  }

  for package in grub2 grub2-efi-binary grub2-efi; do
    version=$(rpm -q --queryformat '%{VERSION}-%{RELEASE}' "$package")
    [ "$version" = "$AZURELINUX_GRUB_VERSION" ] || {
      echo "$package resolved to $version, expected $AZURELINUX_GRUB_VERSION" >&2
      exit 1
    }
  done
  [ -s /usr/lib/grub/arm64-efi/smbios.mod ] || {
    echo "required GRUB module /usr/lib/grub/arm64-efi/smbios.mod is missing" >&2
    exit 1
  }

  driver_version=$(rpm -q --queryformat '%{VERSION}-%{RELEASE}' cuda-open-hwe)
  [ "$driver_version" = "$AZURELINUX_CUDA_OPEN_HWE_VERSION" ] || {
    echo "cuda-open-hwe resolved to $driver_version, expected $AZURELINUX_CUDA_OPEN_HWE_VERSION" >&2
    exit 1
  }
  driver_kernel_version=$(printf '%s' "$AZURELINUX_KERNEL_HWE_VERSION" | tr '-' '.')
  case "$AZURELINUX_CUDA_OPEN_HWE_VERSION" in
    *"_${driver_kernel_version}") ;;
    *)
      echo "cuda-open-hwe is not matched to kernel-hwe $AZURELINUX_KERNEL_HWE_VERSION" >&2
      exit 1
      ;;
  esac

  install -D -m 0755 /usr/local/share/unbounded/10_azure_nvidia \
    /etc/grub.d/10_azure_nvidia
  install -D -m 0644 /usr/local/share/unbounded/51-azure-nvidia.cfg \
    /etc/default/grub.d/51-azure-nvidia.cfg
  printf 'standard=%s\nnvidia=%s\n' "$STANDARD_KVER" "$NVIDIA_KVER" > /rootfs.kernels
else
  KVER=$(ls /usr/lib/modules)
  case "$KVER" in
    *" "*|"") echo "expected one kernel, found: $KVER" >&2; exit 1 ;;
  esac
  printf 'standard=%s\n' "$KVER" > /rootfs.kernels
fi

# Build a generic initramfs. The image must boot on any machine, so do not
# limit it to the build host's hardware. The GPU driver loads after boot.
while IFS= read -r entry; do
  KVER=${entry#*=}
  dracut --force --no-hostonly --no-hostonly-cmdline \
    --omit-drivers "nvidia nvidia_modeset nvidia_uvm nvidia_drm nvidia_peermem" \
    --kver "$KVER" "/boot/initramfs-$KVER.img"
  chmod 0644 "/boot/vmlinuz-$KVER" "/boot/initramfs-$KVER.img"
done < /rootfs.kernels

if [ "$NVIDIA" = 1 ]; then
  cat > /etc/modprobe.d/blacklist-nouveau.conf <<'EOF'
blacklist nouveau
options nouveau modeset=0
EOF
  cat > /etc/modprobe.d/nvidia-gb.conf <<'EOF'
options nvidia NVreg_RestrictProfilingToAdminUsers=0
options nvidia NVreg_CreateImexChannel0=1
options nvidia NVreg_CoherentGPUMemoryMode=driver
options nvidia NVreg_RegistryDwords="RMBug5172204War=4"
EOF
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

  mkdir -p /etc/unbounded
  cat > /etc/unbounded/image-inputs <<EOF
architecture=$TARGETARCH
kernel=$AZURELINUX_KERNEL_VERSION
kernel_hwe=$AZURELINUX_KERNEL_HWE_VERSION
grub=$AZURELINUX_GRUB_VERSION
cuda_open_hwe=$AZURELINUX_CUDA_OPEN_HWE_VERSION
agentbaker_reference=31804b47b7eb6d6999c7ffa6f643590f7b130bab
EOF
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
  --exclude=./rootfs.tar --exclude=./rootfs.kernels \
  --exclude=./proc --exclude=./sys --exclude=./dev \
  --exclude=./run --exclude=./tmp --exclude=./var/cache/tdnf \
  --exclude=./etc/resolv.conf --exclude=./etc/hostname --exclude=./etc/hosts \
  --exclude=./.dockerenv --exclude=./usr/local/bin/build-rootfs \
  -C / .
}

case "${1:-}" in
  install) install_packages ;;
  finish) finish ;;
  *) echo "usage: $0 install|finish" >&2; exit 2 ;;
esac
