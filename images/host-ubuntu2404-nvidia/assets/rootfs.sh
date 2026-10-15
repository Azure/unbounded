#!/bin/sh
# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0

# Builds the Ubuntu 24.04 arm64 GB200 root file system and writes /rootfs.tar.
set -eu

: "${TARGETARCH:?TARGETARCH is required}"
: "${KERNEL_RELEASE:?KERNEL_RELEASE is required}"
: "${KERNEL_VERSION:?KERNEL_VERSION is required}"
: "${NVIDIA_KERNEL_MODULE_VERSION:?NVIDIA_KERNEL_MODULE_VERSION is required}"
: "${NVIDIA_DRIVER_VERSION:?NVIDIA_DRIVER_VERSION is required}"
: "${NVIDIA_TOOLKIT_VERSION:?NVIDIA_TOOLKIT_VERSION is required}"
: "${NVIDIA_IMEX_VERSION:?NVIDIA_IMEX_VERSION is required}"

CUDA_KEY_FINGERPRINT=EB693B3035CD5710E231E123A4B469963BF863CC
IMEX_SHA256=01d1bef149c56c1dc597b42baf92193241ff1d155f53d59c8d1de8ac968365b6
MODPROBE_SHA256=62e1398d13663c2c0e4a75d5391ac1e3984f9c3369230260a8e92e2ad5df4e59
TOOLKIT_KEY_FINGERPRINT=C95B321B61E88C1809C4F759DDCAE044F796ECB0

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

verify_key() {
  key_file="$1"
  expected="$2"
  actual=$(gpg --show-keys --with-colons "$key_file" | awk -F: '$1 == "fpr" {print $10; exit}')
  if [ "$actual" != "$expected" ]; then
    echo "repository signing key fingerprint mismatch: got $actual, want $expected" >&2
    exit 1
  fi
}

verify_package_version() {
  package="$1"
  expected="$2"
  actual=$(dpkg-query -W -f='${Version}' "$package")
  if [ "$actual" != "$expected" ]; then
    echo "$package version mismatch: got $actual, want $expected" >&2
    exit 1
  fi
}

install_packages() {
if [ "$TARGETARCH" != arm64 ] || [ "$(dpkg --print-architecture)" != arm64 ]; then
  echo "host-ubuntu2404-nvidia only supports arm64" >&2
  exit 1
fi

export DEBIAN_FRONTEND=noninteractive
retry apt-get update
retry apt-get install -y --no-install-recommends \
  ca-certificates curl gpg

curl -fsSL --retry 3 --retry-delay 2 \
  https://developer.download.nvidia.com/compute/cuda/repos/ubuntu2404/sbsa/3bf863cc.pub \
  -o /tmp/cuda-repository-key
verify_key /tmp/cuda-repository-key "$CUDA_KEY_FINGERPRINT"
gpg --dearmor -o /usr/share/keyrings/nvidia-cuda-sbsa.gpg /tmp/cuda-repository-key
rm /tmp/cuda-repository-key
cat > /etc/apt/sources.list.d/nvidia-cuda-sbsa.list <<'EOF'
deb [arch=arm64 signed-by=/usr/share/keyrings/nvidia-cuda-sbsa.gpg] https://developer.download.nvidia.com/compute/cuda/repos/ubuntu2404/sbsa/ /
EOF
cat > /etc/apt/preferences.d/nvidia-cuda-sbsa <<'EOF'
Package: nvidia-imex nvidia-modprobe
Pin: origin developer.download.nvidia.com
Pin-Priority: 1001

Package: *
Pin: origin developer.download.nvidia.com
Pin-Priority: -1
EOF

curl -fsSL --retry 3 --retry-delay 2 \
  https://nvidia.github.io/libnvidia-container/gpgkey \
  -o /tmp/nvidia-container-toolkit-key
verify_key /tmp/nvidia-container-toolkit-key "$TOOLKIT_KEY_FINGERPRINT"
gpg --dearmor -o /usr/share/keyrings/nvidia-container-toolkit.gpg \
  /tmp/nvidia-container-toolkit-key
rm /tmp/nvidia-container-toolkit-key
cat > /etc/apt/sources.list.d/nvidia-container-toolkit.list <<'EOF'
deb [arch=arm64 signed-by=/usr/share/keyrings/nvidia-container-toolkit.gpg] https://nvidia.github.io/libnvidia-container/stable/deb/arm64 /
EOF
cat > /etc/apt/preferences.d/nvidia-container-toolkit <<'EOF'
Package: libnvidia-container1 libnvidia-container-tools nvidia-container-toolkit-base nvidia-container-toolkit
Pin: origin nvidia.github.io
Pin-Priority: 1001

Package: *
Pin: origin nvidia.github.io
Pin-Priority: -1
EOF

retry apt-get update
retry apt-get install -y --no-install-recommends \
  "linux-image-${KERNEL_RELEASE}=${KERNEL_VERSION}" \
  "linux-modules-${KERNEL_RELEASE}=${KERNEL_VERSION}" \
  "linux-modules-extra-${KERNEL_RELEASE}=${KERNEL_VERSION}" \
  shim-signed grub-efi-arm64-signed initramfs-tools \
  systemd systemd-sysv systemd-resolved dbus udev \
  cloud-init cloud-guest-utils netplan.io \
  openssh-server sudo \
  chrony ca-certificates curl \
  iproute2 iputils-ping nftables kmod procps rdma-core ibverbs-providers \
  efibootmgr e2fsprogs gdisk util-linux less

# Some NVIDIA package scripts regenerate the initramfs for uname -r even while
# building a rootfs for a different kernel. Point that transient build-kernel
# lookup at the pinned target modules, then remove the compatibility artifacts.
BUILD_KERNEL=$(uname -r)
BUILD_KERNEL_COMPAT=0
if [ "$BUILD_KERNEL" != "$KERNEL_RELEASE" ] && [ ! -e "/lib/modules/$BUILD_KERNEL" ]; then
  ln -s "$KERNEL_RELEASE" "/lib/modules/$BUILD_KERNEL"
  ln -s "config-${KERNEL_RELEASE}" "/boot/config-${BUILD_KERNEL}"
  BUILD_KERNEL_COMPAT=1
fi

retry apt-get install -y --no-install-recommends \
  "linux-modules-nvidia-580-server-open-${KERNEL_RELEASE}=${NVIDIA_KERNEL_MODULE_VERSION}" \
  "nvidia-headless-no-dkms-580-server-open=${NVIDIA_DRIVER_VERSION}" \
  "nvidia-utils-580-server=${NVIDIA_DRIVER_VERSION}" \
  "nvidia-fabricmanager-580=${NVIDIA_DRIVER_VERSION}" \
  "libnvidia-nscq-580=${NVIDIA_DRIVER_VERSION}" \
  "nvidia-container-toolkit=${NVIDIA_TOOLKIT_VERSION}" \
  "nvidia-container-toolkit-base=${NVIDIA_TOOLKIT_VERSION}" \
  "libnvidia-container-tools=${NVIDIA_TOOLKIT_VERSION}" \
  "libnvidia-container1=${NVIDIA_TOOLKIT_VERSION}"

(
  cd /tmp
  apt-get download \
    "nvidia-imex=${NVIDIA_IMEX_VERSION}" \
    "nvidia-modprobe=${NVIDIA_IMEX_VERSION}"
  printf '%s  %s\n' \
    "$IMEX_SHA256" "nvidia-imex_${NVIDIA_IMEX_VERSION}_arm64.deb" \
    "$MODPROBE_SHA256" "nvidia-modprobe_${NVIDIA_IMEX_VERSION}_arm64.deb" \
    | sha256sum --check --strict
  retry apt-get install -y --no-install-recommends \
    "./nvidia-imex_${NVIDIA_IMEX_VERSION}_arm64.deb" \
    "./nvidia-modprobe_${NVIDIA_IMEX_VERSION}_arm64.deb"
  rm "nvidia-imex_${NVIDIA_IMEX_VERSION}_arm64.deb" \
    "nvidia-modprobe_${NVIDIA_IMEX_VERSION}_arm64.deb"
)

if [ "$BUILD_KERNEL_COMPAT" = 1 ]; then
  rm -f "/boot/config-${BUILD_KERNEL}" "/boot/initrd.img-${BUILD_KERNEL}"
  rm "/lib/modules/$BUILD_KERNEL"
fi

verify_package_version "linux-image-${KERNEL_RELEASE}" "$KERNEL_VERSION"
verify_package_version "linux-modules-${KERNEL_RELEASE}" "$KERNEL_VERSION"
verify_package_version "linux-modules-extra-${KERNEL_RELEASE}" "$KERNEL_VERSION"
verify_package_version \
  "linux-modules-nvidia-580-server-open-${KERNEL_RELEASE}" \
  "$NVIDIA_KERNEL_MODULE_VERSION"
verify_package_version nvidia-headless-no-dkms-580-server-open "$NVIDIA_DRIVER_VERSION"
verify_package_version nvidia-kernel-common-580-server "$NVIDIA_DRIVER_VERSION"
verify_package_version nvidia-utils-580-server "$NVIDIA_DRIVER_VERSION"
verify_package_version nvidia-fabricmanager-580 "$NVIDIA_DRIVER_VERSION"
verify_package_version libnvidia-nscq-580 "$NVIDIA_DRIVER_VERSION"
verify_package_version nvidia-container-toolkit "$NVIDIA_TOOLKIT_VERSION"
verify_package_version nvidia-container-toolkit-base "$NVIDIA_TOOLKIT_VERSION"
verify_package_version libnvidia-container-tools "$NVIDIA_TOOLKIT_VERSION"
verify_package_version libnvidia-container1 "$NVIDIA_TOOLKIT_VERSION"
verify_package_version nvidia-imex "$NVIDIA_IMEX_VERSION"
verify_package_version nvidia-modprobe "$NVIDIA_IMEX_VERSION"

apt-mark hold \
  "linux-image-${KERNEL_RELEASE}" \
  "linux-modules-${KERNEL_RELEASE}" \
  "linux-modules-extra-${KERNEL_RELEASE}" \
  "linux-modules-nvidia-580-server-open-${KERNEL_RELEASE}" \
  nvidia-headless-no-dkms-580-server-open \
  nvidia-utils-580-server nvidia-kernel-common-580-server \
  nvidia-fabricmanager-580 libnvidia-nscq-580 \
  nvidia-container-toolkit nvidia-container-toolkit-base \
  libnvidia-container-tools libnvidia-container1 \
  nvidia-imex nvidia-modprobe

apt-get clean
rm -rf /var/lib/apt/lists/*
}

finish() {
test -f "/boot/vmlinuz-${KERNEL_RELEASE}"
test -f /usr/lib/shim/shimaa64.efi.signed.latest
test -f /usr/lib/grub/arm64-efi-signed/grubaa64.efi.signed

MANA_CONFIG=$(grep '^CONFIG_MICROSOFT_MANA=' "/boot/config-${KERNEL_RELEASE}")
MANA_IB_MODULE=$(find "/lib/modules/${KERNEL_RELEASE}" -type f -name 'mana_ib.ko*' -print -quit)
NVIDIA_MODULE=$(find "/lib/modules/${KERNEL_RELEASE}" -type f -name 'nvidia.ko*' -print -quit)
[ "$MANA_CONFIG" = "CONFIG_MICROSOFT_MANA=y" ]
test -n "$MANA_IB_MODULE"
test -n "$NVIDIA_MODULE"

cat > /etc/initramfs-tools/modules <<'EOF'
hv_vmbus
hv_netvsc
mana_ib
EOF
update-initramfs -u -k "$KERNEL_RELEASE"
chmod 0644 "/boot/vmlinuz-${KERNEL_RELEASE}" "/boot/initrd.img-${KERNEL_RELEASE}"

cat > /etc/modules-load.d/nvidia.conf <<'EOF'
nvidia
nvidia_modeset
nvidia_uvm
nvidia_drm
EOF

cat > /etc/systemd/system/nvidia-persistence-mode.service <<'EOF'
[Unit]
Description=Enable NVIDIA persistence mode
After=nvidia-fabricmanager.service
Requires=nvidia-fabricmanager.service
ConditionPathExistsGlob=/sys/bus/pci/drivers/nvidia/0*

[Service]
Type=oneshot
RemainAfterExit=yes
ExecStartPre=/usr/bin/nvidia-modprobe -u -c=0
ExecStart=/usr/bin/nvidia-smi -pm 1

[Install]
WantedBy=multi-user.target
EOF

systemctl enable systemd-networkd.service systemd-resolved.service \
  ssh.service chrony.service nvidia-fabricmanager.service \
  nvidia-persistence-mode.service
for unit in cloud-init-local.service cloud-init.service cloud-init-network.service \
  cloud-config.service cloud-final.service; do
  if [ -f "/usr/lib/systemd/system/$unit" ]; then
    systemctl enable "$unit"
  fi
done

# ComputeDomain activation owns the IMEX configuration and unmask/start action.
systemctl disable nvidia-imex.service || true
systemctl mask nvidia-imex.service

mkdir -p /etc/systemd/system/systemd-networkd-wait-online.service.d
cat > /etc/systemd/system/systemd-networkd-wait-online.service.d/any.conf <<'EOF'
[Service]
ExecStart=
ExecStart=/usr/lib/systemd/systemd-networkd-wait-online --any --timeout=120
EOF

cat > /etc/netplan/90-unbounded-dhcp.yaml <<'EOF'
network:
  version: 2
  renderer: networkd
  ethernets:
    all:
      match:
        name: "e*"
      dhcp4: true
      dhcp6: true
EOF
chmod 0600 /etc/netplan/90-unbounded-dhcp.yaml

: > /etc/machine-id
rm -f /etc/ssh/ssh_host_*

cat > /etc/unbounded-host-image <<EOF
architecture=arm64
os=ubuntu-24.04
kernel=${KERNEL_RELEASE}
kernel_package_version=${KERNEL_VERSION}
nvidia_kernel_module_package_version=${NVIDIA_KERNEL_MODULE_VERSION}
nvidia_driver_version=${NVIDIA_DRIVER_VERSION}
nvidia_container_toolkit_version=${NVIDIA_TOOLKIT_VERSION}
nvidia_fabric_manager_version=${NVIDIA_DRIVER_VERSION}
nvidia_nscq_version=${NVIDIA_DRIVER_VERSION}
nvidia_imex_version=${NVIDIA_IMEX_VERSION}
mana_driver=builtin
mana_kernel_config=${MANA_CONFIG}
mana_ib_module=${MANA_IB_MODULE}
nvidia_module=${NVIDIA_MODULE}
imex_activation=masked
EOF

tar --create --file=/rootfs.tar --numeric-owner --xattrs \
  --one-file-system \
  --exclude=./rootfs.tar --exclude=./proc --exclude=./sys --exclude=./dev \
  --exclude=./run --exclude=./tmp --exclude=./var/cache/apt \
  --exclude=./var/lib/apt/lists --exclude=./etc/resolv.conf \
  --exclude=./etc/hostname --exclude=./etc/hosts --exclude=./.dockerenv \
  --exclude=./usr/local/bin/build-rootfs \
  -C / .
echo "$KERNEL_RELEASE" > /rootfs.kver
}

case "${1:-}" in
  install) install_packages ;;
  finish) finish ;;
  *) echo "usage: $0 install|finish" >&2; exit 2 ;;
esac
