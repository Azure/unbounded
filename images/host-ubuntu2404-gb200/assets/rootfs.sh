#!/bin/sh
# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0
set -eu

# shellcheck source=versions.env
. /usr/local/lib/unbounded-image/versions.env
export DEBIAN_FRONTEND=noninteractive

select_profile() {
    GB200_PROFILE=${GB200_PROFILE:-canonical-control}
    case "$GB200_PROFILE" in
        canonical-control)
            KVER=$CANONICAL_KVER
            KERNEL_VERSION=$CANONICAL_KERNEL_VERSION
            NVIDIA_VERSION=$CANONICAL_NVIDIA_VERSION
            NVIDIA_DRIVER_VERSION=$CANONICAL_NVIDIA_DRIVER_VERSION
            NVIDIA_MODULE_SOURCE=ubuntu-precompiled-server-open
            ;;
        agentbaker-driver|agentbaker-doca|agentbaker-doca-imex)
            KVER=$AGENTBAKER_KVER
            KERNEL_VERSION=$AGENTBAKER_KERNEL_VERSION
            NVIDIA_VERSION=$AGENTBAKER_NVIDIA_VERSION
            NVIDIA_DRIVER_VERSION=$AGENTBAKER_NVIDIA_DRIVER_VERSION
            NVIDIA_MODULE_SOURCE=nvidia-dkms-open
            ;;
        *)
            echo "unsupported GB200 profile: $GB200_PROFILE" >&2
            exit 2
            ;;
    esac
    export GB200_PROFILE KVER KERNEL_VERSION NVIDIA_VERSION \
        NVIDIA_DRIVER_VERSION NVIDIA_MODULE_SOURCE
}

install_agentbaker_sources() {
    mkdir -p /etc/apt/keyrings /etc/apt/sources.list.d
    nvidia_key=/etc/apt/keyrings/nvidia.pub
    curl -fsSL \
        "https://raw.githubusercontent.com/Azure/AgentBaker/${AGENTBAKER_COMMIT}/parts/linux/cloud-init/artifacts/ubuntu/gb/nvidia.pub" \
        > "$nvidia_key"
    echo "${AGENTBAKER_NVIDIA_KEY_SHA256}  $nvidia_key" | sha256sum -c -
    cat > /etc/apt/sources.list.d/nvidia-gb200.list <<EOF
deb [arch=arm64 signed-by=$nvidia_key] https://developer.download.nvidia.com/compute/cuda/repos/ubuntu2404/sbsa/ /
EOF

    add-apt-repository -y ppa:canonical-kernel-team/ppa
}

install_doca_source() {
    doca_key=/etc/apt/keyrings/doca-net.pub
    curl -fsSL \
        "https://raw.githubusercontent.com/Azure/AgentBaker/${AGENTBAKER_COMMIT}/parts/linux/cloud-init/artifacts/ubuntu/gb/doca.pub" \
        > "$doca_key"
    echo "${AGENTBAKER_DOCA_KEY_SHA256}  $doca_key" | sha256sum -c -
    cat > /etc/apt/sources.list.d/doca-net.list <<EOF
deb [arch=arm64 signed-by=$doca_key] https://linux.mellanox.com/public/repo/doca/${DOCA_VERSION}/ubuntu24.04/arm64-sbsa/ ./
EOF
}

install_packages() {
    select_profile
    test "$(dpkg --print-architecture)" = arm64
    printf '#!/bin/sh\nexit 101\n' > /usr/sbin/policy-rc.d
    chmod 755 /usr/sbin/policy-rc.d
    apt-get update
    apt-get install -y --no-install-recommends \
        systemd-sysv systemd-container udev dbus systemd-resolved netplan.io \
        cloud-init cloud-guest-utils gdisk e2fsprogs initramfs-tools \
        openssh-server sudo passwd ca-certificates curl tar gzip zstd \
        iproute2 iputils-ping nftables util-linux kmod procps \
        efibootmgr chrony pciutils ethtool rdma-core ibverbs-providers \
        python3 file linux-firmware software-properties-common

    case "$GB200_PROFILE" in
        canonical-control)
            apt-get install -y --no-install-recommends \
                "linux-image-${KVER}=${KERNEL_VERSION}" \
                "linux-headers-${KVER}=${KERNEL_VERSION}"
            ;;
        agentbaker-*)
            install_agentbaker_sources
            case "$GB200_PROFILE" in
                agentbaker-doca|agentbaker-doca-imex) install_doca_source ;;
            esac
            apt-get update
            apt-get install -y --no-install-recommends \
                "linux-azure-nvidia=${KERNEL_VERSION}"
            ;;
    esac

    # NVIDIA's postinst runs update-initramfs. Bootstrap the target kernel
    # first, rather than letting it select the running builder's kernel.
    test -s /etc/initramfs-tools/initramfs.conf
    test -s "/boot/initrd.img-$KVER"
    # Its chroot path also explicitly targets uname -r. Defer automatic
    # updates; finish rebuilds and verifies only the pinned target initrd.
    sed -i 's/^update_initramfs=.*/update_initramfs=no/' \
        /etc/initramfs-tools/update-initramfs.conf
    grep -qx 'update_initramfs=no' /etc/initramfs-tools/update-initramfs.conf
    case "$GB200_PROFILE" in
        canonical-control)
            apt-get install -y --no-install-recommends \
                "linux-modules-nvidia-580-server-open-${KVER}=${CANONICAL_NVIDIA_MODULES_VERSION}" \
                "nvidia-kernel-common-580-server=${NVIDIA_VERSION}" \
                "nvidia-headless-no-dkms-580-server-open=${NVIDIA_VERSION}" \
                "nvidia-utils-580-server=${NVIDIA_VERSION}" \
                "nvidia-fabricmanager-580=${NVIDIA_VERSION}" \
                "libnvidia-nscq-580=${NVIDIA_VERSION}" \
                nvidia-modprobe
            ;;
        agentbaker-*)
            case "$GB200_PROFILE" in
                agentbaker-doca|agentbaker-doca-imex)
                    apt-get install -y --allow-downgrades --no-install-recommends \
                        "librdmacm-dev=${DOCA_RDMA_VERSION}" \
                        "libibverbs-dev=${DOCA_RDMA_VERSION}" \
                        "libibverbs1=${DOCA_RDMA_VERSION}" \
                        "ibverbs-providers=${DOCA_RDMA_VERSION}" \
                        "doca-ofed=${DOCA_VERSION}"
                    ;;
            esac
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
            if [ "$GB200_PROFILE" = agentbaker-doca-imex ]; then
                apt-get install -y --allow-downgrades --no-install-recommends \
                    "nvidia-imex=${NVIDIA_VERSION}"
            fi
            ;;
    esac
    sed -i 's/^update_initramfs=.*/update_initramfs=yes/' \
        /etc/initramfs-tools/update-initramfs.conf

    # Extract signed EFI binaries without running grub-install against the builder.
    efi=$(mktemp -d)
    chmod 755 "$efi"
    mkdir -p /usr/share/unbounded /boot/efi/EFI/BOOT /boot/efi/EFI/ubuntu
    (
        cd "$efi"
        apt-get download shim-signed grub-efi-arm64-signed
        for deb in ./*.deb; do
            dpkg-deb --extract "$deb" extracted
            dpkg-deb --show --showformat="\${Package}\t\${Version}\t\${Architecture}\n" "$deb" \
                >> /usr/share/unbounded/efi-packages.tsv
        done
        sha256sum ./*.deb > /usr/share/unbounded/efi-packages.sha256
    )
    install -m 644 "$efi/extracted/usr/lib/shim/shimaa64.efi.signed.latest" \
        /boot/efi/EFI/BOOT/BOOTAA64.EFI
    install -m 644 "$efi/extracted/usr/lib/shim/mmaa64.efi" \
        /boot/efi/EFI/BOOT/mmaa64.efi
    install -m 644 "$efi/extracted/usr/lib/grub/arm64-efi-signed/grubaa64.efi.signed" \
        /boot/efi/EFI/BOOT/grubaa64.efi
    cp /boot/efi/EFI/BOOT/BOOTAA64.EFI /boot/efi/EFI/ubuntu/shimaa64.efi
    cp /boot/efi/EFI/BOOT/grubaa64.efi /boot/efi/EFI/ubuntu/grubaa64.efi
    cp /boot/efi/EFI/BOOT/mmaa64.efi /boot/efi/EFI/ubuntu/mmaa64.efi
    rm -rf "$efi"
}

finish() {
    select_profile
    : "${SOURCE_COMMIT:?SOURCE_COMMIT is required}"
    mkdir -p /etc/netplan /etc/initramfs-tools/conf.d /etc/modules-load.d \
        /etc/systemd/system/systemd-networkd-wait-online.service.d
    printf 'MODULES=most\n' > /etc/initramfs-tools/conf.d/unbounded
    # MANA must be available before the cloud-init datasource can be reached.
    printf 'mana\nnvme\n' >> /etc/initramfs-tools/modules
    depmod -a "$KVER"
    update-initramfs -u -k "$KVER"
    chmod 644 "/boot/vmlinuz-$KVER" "/boot/initrd.img-$KVER"

    # Metalman's static-network injection removes this exact default file.
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
    cat > /etc/modules-load.d/nvidia.conf <<'EOF'
nvidia
nvidia_modeset
nvidia_uvm
nvidia_drm
EOF
    if [ "$GB200_PROFILE" = agentbaker-doca ] || \
        [ "$GB200_PROFILE" = agentbaker-doca-imex ]; then
        printf 'nvidia_peermem\n' >> /etc/modules-load.d/nvidia.conf
    fi
    if [ "$GB200_PROFILE" = agentbaker-doca-imex ]; then
        cat > /etc/modprobe.d/nvidia-gb200.conf <<'EOF'
options nvidia NVreg_RestrictProfilingToAdminUsers=0
options nvidia NVreg_CreateImexChannel0=1
options nvidia NVreg_CoherentGPUMemoryMode=driver
options nvidia NVreg_RegistryDwords="RMBug5172204War=4"
EOF
    fi
    systemctl enable systemd-networkd.service systemd-resolved.service \
        ssh.service chrony.service nvidia-fabricmanager.service
    if [ "$GB200_PROFILE" = agentbaker-doca-imex ]; then
        systemctl enable nvidia-imex.service
    fi
    for unit in cloud-init-local.service cloud-init.service cloud-init-network.service \
        cloud-config.service cloud-final.service; do
        if [ -f "/usr/lib/systemd/system/$unit" ]; then
            systemctl enable "$unit"
        fi
    done
    systemctl mask nftables.service
    rm -f /usr/sbin/policy-rc.d /etc/ssh/ssh_host_*
    : > /etc/machine-id
    ln -sf /etc/machine-id /var/lib/dbus/machine-id
    python3 /usr/local/lib/unbounded-image/verify.py
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
