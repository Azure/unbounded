#!/bin/sh
# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0
set -eu

# shellcheck source=versions.env
. /usr/local/lib/unbounded-image/versions.env
export KVER NVIDIA_DRIVER_VERSION
export DEBIAN_FRONTEND=noninteractive

install_packages() {
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
        python3 file linux-firmware \
        "linux-image-${KVER}=${KERNEL_VERSION}" \
        "linux-headers-${KVER}=${KERNEL_VERSION}"

    # NVIDIA's postinst runs update-initramfs. Bootstrap the target kernel
    # first, rather than letting it select the running builder's kernel.
    test -s /etc/initramfs-tools/initramfs.conf
    test -s "/boot/initrd.img-$KVER"
    # Its chroot path also explicitly targets uname -r. Defer automatic
    # updates; finish rebuilds and verifies only the pinned target initrd.
    sed -i 's/^update_initramfs=.*/update_initramfs=no/' \
        /etc/initramfs-tools/update-initramfs.conf
    grep -qx 'update_initramfs=no' /etc/initramfs-tools/update-initramfs.conf
    apt-get install -y --no-install-recommends \
        "linux-modules-nvidia-580-server-open-${KVER}=${NVIDIA_MODULES_VERSION}" \
        "nvidia-kernel-common-580-server=${NVIDIA_VERSION}" \
        "nvidia-headless-no-dkms-580-server-open=${NVIDIA_VERSION}" \
        "nvidia-utils-580-server=${NVIDIA_VERSION}" \
        "nvidia-fabricmanager-580=${NVIDIA_VERSION}" \
        "libnvidia-nscq-580=${NVIDIA_VERSION}" \
        nvidia-modprobe
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
    cat > /etc/systemd/system/nvidia-persistence-mode.service <<'EOF'
[Unit]
Description=Enable NVIDIA persistence mode
Wants=nvidia-fabricmanager.service
After=systemd-modules-load.service systemd-udev-trigger.service nvidia-fabricmanager.service
StartLimitIntervalSec=0

[Service]
Type=oneshot
RemainAfterExit=yes
ExecStartPre=-/usr/bin/nvidia-modprobe -u -c=0
ExecStart=/usr/bin/nvidia-smi -pm 1
Restart=on-failure
RestartSec=5

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
