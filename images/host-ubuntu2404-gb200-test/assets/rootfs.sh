#!/bin/sh
# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0
set -eu

assets=/usr/local/lib/unbounded-image
# shellcheck disable=SC1091
. "$assets/versions.env"
export DEBIAN_FRONTEND=noninteractive

fail() {
    echo "$*" >&2
    exit 1
}

install_nvidia_source() {
    mkdir -p /etc/apt/keyrings /etc/apt/sources.list.d
    nvidia_key=/etc/apt/keyrings/nvidia.pub
    curl -fsSL \
        "https://raw.githubusercontent.com/Azure/AgentBaker/${AGENTBAKER_COMMIT}/parts/linux/cloud-init/artifacts/ubuntu/gb/nvidia.pub" \
        > "$nvidia_key"
    echo "${NVIDIA_KEY_SHA256}  $nvidia_key" | sha256sum -c -
    cat > /etc/apt/sources.list.d/nvidia-gb200.list <<'EOF'
deb [arch=arm64 signed-by=/etc/apt/keyrings/nvidia.pub] https://developer.download.nvidia.com/compute/cuda/repos/ubuntu2404/sbsa/ /
EOF
}

install_efi() {
    efi=$(mktemp -d)
    chmod 755 "$efi"
    mkdir -p /usr/share/unbounded /boot/efi/EFI/BOOT /boot/efi/EFI/ubuntu
    (
        cd "$efi"
        apt-get download shim-signed grub-efi-arm64-signed
        for deb in ./*.deb; do
            dpkg-deb --extract "$deb" extracted
            dpkg-deb --show \
                --showformat="\${Package}\t\${Version}\t\${Architecture}\n" "$deb" \
                >> /usr/share/unbounded/efi-packages.tsv
        done
        sha256sum ./*.deb > /usr/share/unbounded/efi-packages.sha256
    )
    install -m 644 "$efi/extracted/usr/lib/shim/shimaa64.efi.signed.latest" \
        /boot/efi/EFI/BOOT/BOOTAA64.EFI
    install -m 644 "$efi/extracted/usr/lib/shim/mmaa64.efi" \
        /boot/efi/EFI/BOOT/mmaa64.efi
    install -m 644 \
        "$efi/extracted/usr/lib/grub/arm64-efi-signed/grubaa64.efi.signed" \
        /boot/efi/EFI/BOOT/grubaa64.efi
    cp /boot/efi/EFI/BOOT/BOOTAA64.EFI /boot/efi/EFI/ubuntu/shimaa64.efi
    cp /boot/efi/EFI/BOOT/grubaa64.efi /boot/efi/EFI/ubuntu/grubaa64.efi
    cp /boot/efi/EFI/BOOT/mmaa64.efi /boot/efi/EFI/ubuntu/mmaa64.efi
    rm -rf "$efi"
}

install_packages() {
    test "$(dpkg --print-architecture)" = arm64
    printf '#!/bin/sh\nexit 101\n' > /usr/sbin/policy-rc.d
    chmod 755 /usr/sbin/policy-rc.d

    apt-get update
    apt-get install -y --no-install-recommends \
        systemd-sysv systemd-container udev dbus systemd-resolved netplan.io \
        cloud-init cloud-guest-utils \
        "ssh-import-id=${SSH_IMPORT_ID_VERSION}" \
        "wget=${WGET_VERSION}" \
        gdisk e2fsprogs initramfs-tools \
        openssh-server sudo passwd ca-certificates curl tar gzip zstd \
        iproute2 iputils-ping nftables util-linux kmod procps \
        efibootmgr chrony pciutils ethtool rdma-core ibverbs-providers \
        python3 file linux-firmware \
        "linux-image-${KVER}=${KERNEL_VERSION}" \
        "linux-headers-${KVER}=${KERNEL_VERSION}" \
        gcc-13

    install_nvidia_source
    apt-get update

    test -s /etc/initramfs-tools/initramfs.conf
    test -s "/boot/initrd.img-$KVER"
    sed -i 's/^update_initramfs=.*/update_initramfs=no/' \
        /etc/initramfs-tools/update-initramfs.conf
    grep -qx 'update_initramfs=no' /etc/initramfs-tools/update-initramfs.conf

    if ! apt-get install -y --allow-downgrades --no-install-recommends \
        "libxnvctrl0=${NVIDIA_VERSION}" \
        "libnvidia-common-580=${NVIDIA_VERSION}" \
        "libnvidia-cfg1-580=${NVIDIA_VERSION}" \
        "libnvidia-gpucomp-580=${NVIDIA_VERSION}" \
        "libnvidia-gl-580=${NVIDIA_VERSION}" \
        "nvidia-firmware-580=${NVIDIA_VERSION}" \
        "nvidia-dkms-580-open=${NVIDIA_VERSION}" \
        "nvidia-kernel-common-580=${NVIDIA_VERSION}" \
        "nvidia-kernel-source-580-open=${NVIDIA_VERSION}" \
        "libnvidia-compute-580=${NVIDIA_VERSION}" \
        "libnvidia-extra-580=${NVIDIA_VERSION}" \
        "libnvidia-decode-580=${NVIDIA_VERSION}" \
        "libnvidia-encode-580=${NVIDIA_VERSION}" \
        "xserver-xorg-video-nvidia-580=${NVIDIA_VERSION}" \
        "libnvidia-fbc1-580=${NVIDIA_VERSION}" \
        "nvidia-driver-580-open=${NVIDIA_VERSION}" \
        "nvidia-utils-580=${NVIDIA_VERSION}" \
        "nvidia-fabricmanager=${NVIDIA_VERSION}" \
        "libnvidia-nscq=${NVIDIA_VERSION}" \
        "nvidia-modprobe=${NVIDIA_VERSION}"; then
        for log in /var/lib/dkms/nvidia/*/build/make.log; do
            if [ -f "$log" ]; then
                echo "NVIDIA DKMS failure log: $log" >&2
                cat "$log" >&2
            fi
        done
        exit 1
    fi

    sed -i 's/^update_initramfs=.*/update_initramfs=yes/' \
        /etc/initramfs-tools/update-initramfs.conf
    install_efi
}

finish() {
    : "${SOURCE_COMMIT:?SOURCE_COMMIT is required}"
    test "${#SOURCE_COMMIT}" -eq 40
    case "$SOURCE_COMMIT" in
        *[!0-9a-f]*) fail "SOURCE_COMMIT must be a lowercase Git SHA" ;;
    esac

    mkdir -p /etc/netplan /etc/initramfs-tools/conf.d /etc/modules-load.d \
        /etc/systemd/system/systemd-networkd-wait-online.service.d \
        /opt/unbounded/bootstrap /usr/share/unbounded
    install -m 644 "$assets/90-gb200-test-users.cfg" \
        /etc/cloud/cloud.cfg.d/90-gb200-test-users.cfg
    install -m 644 "$assets/99-nvidia-disable-nvlink.conf" \
        /etc/modprobe.d/99-nvidia-disable-nvlink.conf

    agent_archive=/opt/unbounded/bootstrap/unbounded-agent-linux-arm64.tar.gz
    curl -fL --retry 5 --retry-all-errors --retry-delay 2 \
        "https://github.com/Azure/unbounded/releases/download/${AGENT_VERSION}/unbounded-agent-linux-arm64.tar.gz" \
        -o "$agent_archive"
    echo "${AGENT_SHA256}  $agent_archive" | sha256sum -c -

    printf 'MODULES=most\n' > /etc/initramfs-tools/conf.d/unbounded
    printf 'mana\nnvme\n' >> /etc/initramfs-tools/modules
    cat > /etc/modules-load.d/nvidia.conf <<'EOF'
nvidia
nvidia_modeset
nvidia_uvm
nvidia_drm
EOF
    depmod -a "$KVER"
    update-initramfs -u -k "$KVER"
    chmod 644 "/boot/vmlinuz-$KVER" "/boot/initrd.img-$KVER"

    cat > /etc/netplan/50-cloud-init.yaml <<'EOF'
network:
  version: 2
  renderer: networkd
  ethernets:
    provisioning:
      match:
        name: "e*"
      dhcp4: true
EOF
    chmod 600 /etc/netplan/50-cloud-init.yaml
    cat > /etc/systemd/system/systemd-networkd-wait-online.service.d/any.conf <<'EOF'
[Service]
ExecStart=
ExecStart=/usr/lib/systemd/systemd-networkd-wait-online --any --timeout=120
EOF

    systemctl enable systemd-networkd.service systemd-resolved.service \
        ssh.service chrony.service nvidia-fabricmanager.service
    for unit in cloud-init-local.service cloud-init.service cloud-init-network.service \
        cloud-config.service cloud-final.service; do
        if [ -f "/usr/lib/systemd/system/$unit" ]; then
            systemctl enable "$unit"
        fi
    done
    systemctl mask nftables.service

    rm -f /usr/sbin/policy-rc.d /etc/ssh/ssh_host_*
    rm -rf /var/lib/cloud/*
    : > /etc/machine-id
    ln -sf /etc/machine-id /var/lib/dbus/machine-id

    cat > /usr/share/unbounded/image-inputs.json <<EOF
{
  "schema": "unbounded.gb200.test-host-image.v1",
  "source_commit": "${SOURCE_COMMIT}",
  "architecture": "arm64",
  "ubuntu": "24.04",
  "kernel": "${KVER}",
  "nvidia_driver": "${NVIDIA_DRIVER_VERSION}",
  "agent": "${AGENT_VERSION}",
  "ssh_import_id": "${SSH_IMPORT_ID_VERSION}",
  "wget": "${WGET_VERSION}"
}
EOF

    apt-get clean
    rm -rf /var/lib/apt/lists/*
    tar --create --file=/rootfs.tar --numeric-owner --xattrs --one-file-system \
        --exclude=./rootfs.tar --exclude=./rootfs.kver \
        --exclude=./proc --exclude=./sys --exclude=./dev --exclude=./run \
        --exclude=./tmp --exclude=./etc/resolv.conf --exclude=./etc/hostname \
        --exclude=./etc/hosts --exclude=./.dockerenv \
        --exclude=./usr/local/lib/unbounded-image -C / .
    printf '%s\n' "$KVER" > /rootfs.kver
}

case "${1:-}" in
    install) install_packages ;;
    finish) finish ;;
    *) echo "usage: $0 install|finish" >&2; exit 2 ;;
esac
