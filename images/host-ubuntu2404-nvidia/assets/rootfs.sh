#!/bin/sh
# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0

# Builds the Ubuntu 24.04 root file system for the NVIDIA HGX Metalman machine
# image. It runs inside the Ubuntu cloud image root file system for amd64 and
# writes /rootfs.tar and /rootfs.kver for images/host-azurelinux3/assets/disk.sh.
#
# Usage: rootfs.sh install|finish
# The install step installs packages and builds the GPU kernel modules. The
# finish step configures the system and writes the archive. Separate steps
# let the build cache the slow step.
#
# Environment:
#   KERNEL_PACKAGE         kernel metapackage (default: linux-generic)
#   NVIDIA_DRIVER_VERSION  NVIDIA open GPU driver version (default: 595.71.05)
#   NVLSM_VERSION          NVLink Subnet Manager package version
#                          (default: 2025.10.12-1)
#   CUDA_REPO_URL          NVIDIA CUDA apt repository for Ubuntu 24.04
set -eu

KERNEL_PACKAGE="${KERNEL_PACKAGE:-linux-generic}"
NVIDIA_DRIVER_VERSION="${NVIDIA_DRIVER_VERSION:-595.71.05}"
NVLSM_VERSION="${NVLSM_VERSION:-2025.10.12-1}"
CUDA_REPO_URL="${CUDA_REPO_URL:-https://developer.download.nvidia.com/compute/cuda/repos/ubuntu2404/x86_64}"

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

install_packages() {
if [ "$(dpkg --print-architecture)" != amd64 ]; then
  echo "this image supports amd64 only, not $(dpkg --print-architecture)" >&2
  exit 1
fi

# The image has no bootloader or kernel yet. Keep package scripts from
# updating a GRUB configuration or writing to NVRAM during the build. The
# disk assembly stage writes the boot configuration.
for tool in /usr/sbin/update-grub /usr/sbin/grub-install; do
  dpkg-divert --local --rename --add "$tool"
  ln -sf /bin/true "$tool"
done
echo "grub-efi-amd64 grub2/update_nvram boolean false" | debconf-set-selections

retry apt-get update
apt_install ca-certificates curl gnupg

# Install the kernel first so that DKMS builds the GPU driver against it.
apt_install "$KERNEL_PACKAGE" "linux-headers-$KERNEL_FLAVOR" initramfs-tools \
  shim-signed grub-efi-amd64-signed grub-efi-amd64-bin efibootmgr mokutil

# The NVIDIA CUDA repository carries the open GPU driver, Fabric Manager,
# NSCQ, NVSDM, and NVLSM builds that match each other. The pinning package
# holds every driver component at one version, so a later apt upgrade cannot
# mix driver and Fabric Manager versions.
curl -fsSL --retry 5 -o /tmp/cuda-keyring.deb "$CUDA_REPO_URL/cuda-keyring_1.1-1_all.deb"
dpkg -i /tmp/cuda-keyring.deb
rm -f /tmp/cuda-keyring.deb
retry apt-get update
apt_install "nvidia-driver-pinning-$NVIDIA_DRIVER_VERSION"

apt_install \
  nvidia-headless-open \
  nvidia-fabricmanager \
  libnvidia-nscq \
  libnvsdm \
  "nvlsm=$NVLSM_VERSION" \
  rdma-core ibverbs-providers ibverbs-utils infiniband-diags perftest \
  pciutils kmod

# The pinning package does not cover NVLSM, and the kernel metapackages
# would pull in a kernel that the image did not validate the driver against.
# Machines take kernel and driver updates by reinstalling a newer image.
apt-mark hold nvlsm "$KERNEL_PACKAGE" "linux-image-$KERNEL_FLAVOR" \
  "linux-headers-$KERNEL_FLAVOR"

KVER=$(kernel_version)

# DKMS builds for the running kernel by default, which is the build host's.
dkms autoinstall -k "$KVER"
for module in nvidia nvidia_modeset nvidia_uvm nvidia_drm ib_umad mlx5_ib; do
  if ! modinfo -k "$KVER" "$module" >/dev/null; then
    echo "kernel $KVER has no $module module" >&2
    exit 1
  fi
done
modinfo -k "$KVER" -F version nvidia | grep -qx "$NVIDIA_DRIVER_VERSION" || {
  echo "nvidia module for $KVER is not version $NVIDIA_DRIVER_VERSION" >&2
  exit 1
}

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
KVER=$(kernel_version)

for tool in /usr/sbin/update-grub /usr/sbin/grub-install; do
  rm -f "$tool"
  dpkg-divert --local --rename --remove "$tool"
done

# DKMS signs the modules it builds with a key it generates. On Ubuntu the key
# pair lives in /var/lib/shim-signed/mok; older DKMS releases use
# /var/lib/dkms. A private key in a published image would let anyone sign
# modules that a machine trusts, so keep only a copy of the certificate.
# Machines with Secure Boot can enroll it with mokutil. A later DKMS build on
# the machine generates a new key pair.
mkdir -p /usr/share/unbounded
for cert in /var/lib/shim-signed/mok/MOK.der /var/lib/dkms/mok.pub; do
  if [ -f "$cert" ]; then
    cp "$cert" /usr/share/unbounded/nvidia-dkms-signing.der
    break
  fi
done
if [ ! -f /usr/share/unbounded/nvidia-dkms-signing.der ]; then
  echo "DKMS did not create a module signing certificate" >&2
  exit 1
fi
rm -f /var/lib/shim-signed/mok/MOK.priv /var/lib/shim-signed/mok/MOK.der \
  /var/lib/dkms/mok.key /var/lib/dkms/mok.pub

# Keep the GPU driver out of the initramfs. It loads after the root file
# system is mounted, together with the RDMA modules Fabric Manager needs.
cat > /etc/initramfs-tools/conf.d/unbounded-nvidia.conf <<'EOF'
MODULES=most
EOF
update-initramfs -c -k "$KVER" 2>/dev/null || update-initramfs -u -k "$KVER"
if lsinitramfs "/boot/initrd.img-$KVER" | grep -q '/nvidia\.ko'; then
  echo "initramfs for $KVER contains the nvidia module" >&2
  exit 1
fi
chmod 0644 "/boot/vmlinuz-$KVER" "/boot/initrd.img-$KVER"

# Ship shim and the Canonical-signed GRUB on the ESP. The Metalman installer
# adds a UEFI boot entry for EFI/BOOT/BOOTX64.EFI. Shim loads grubx64.efi
# from the same directory, and GRUB reads its configuration from EFI/ubuntu.
# The fallback loader is left out so that firmware boots the entry directly.
for dir in /boot/efi/EFI/BOOT /boot/efi/EFI/ubuntu; do
  mkdir -p "$dir"
  cp /usr/lib/shim/shimx64.efi.signed.latest "$dir/shimx64.efi"
  cp /usr/lib/shim/mmx64.efi "$dir/mmx64.efi"
  cp /usr/lib/grub/x86_64-efi-signed/grubx64.efi.signed "$dir/grubx64.efi"
done
mv /boot/efi/EFI/BOOT/shimx64.efi /boot/efi/EFI/BOOT/BOOTX64.EFI
cp /usr/lib/shim/BOOTX64.CSV /boot/efi/EFI/ubuntu/BOOTX64.CSV 2>/dev/null || true

# Load the GPU driver and the InfiniBand management datagram module at boot.
# Fabric Manager starts NVLSM, which talks to the NVSwitch trays through the
# NVLink management NICs with ib_umad.
cat > /etc/modules-load.d/nvidia.conf <<'EOF'
nvidia
nvidia_modeset
nvidia_uvm
nvidia_drm
EOF
cat > /etc/modules-load.d/unbounded-rdma.conf <<'EOF'
ib_umad
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

# Fabric Manager must start after the GPUs are initialized and ib_umad is
# loaded, and before CUDA workloads. The package unit only orders it after
# the network. Restart it if NVLSM loses the race with the NVLink management
# link coming up.
#
# The node stack runs in the unbounded-agent's units, not the distro's:
# unbounded-containerd and unbounded-kubelet in the none isolation mode,
# systemd-nspawn@<machine> in nspawn mode, and unbounded-node-<slot> in
# container mode. Order all of them after Fabric Manager. The last two are
# matched by a template and a prefix drop-in, since the names vary by machine.
# These are ordering-only: a host where Fabric Manager is skipped or fails
# still starts its node.
mkdir -p /etc/systemd/system/nvidia-fabricmanager.service.d
cat > /etc/systemd/system/nvidia-fabricmanager.service.d/10-unbounded.conf <<'EOF'
[Unit]
Wants=nvidia-persistence-mode.service
After=systemd-modules-load.service nvidia-persistence-mode.service nvidia-persistenced.service
Before=unbounded-containerd.service unbounded-kubelet.service containerd.service kubelet.service
ConditionPathExistsGlob=/sys/bus/pci/drivers/nvidia/0*
StartLimitIntervalSec=0

[Service]
ExecStartPre=/usr/sbin/modprobe -a ib_umad mlx5_ib
Restart=on-failure
RestartSec=30s
EOF
systemctl enable nvidia-fabricmanager.service
for unit_dir in systemd-nspawn@.service.d unbounded-node-.service.d; do
  mkdir -p "/etc/systemd/system/$unit_dir"
  cat > "/etc/systemd/system/$unit_dir/10-nvidia-fabricmanager.conf" <<'EOF'
[Unit]
After=nvidia-fabricmanager.service
EOF
done
# IMEX is for multi-node NVLink domains. It is not installed, but make sure
# a dependency never starts it.
systemctl mask nvidia-imex.service

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
