#!/bin/bash
# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0
set -euo pipefail

test "$(id -u)" -eq 0
test "$(uname -m)" = aarch64

script_dir=$(cd "$(dirname "$0")" && pwd)
container_engine=${CONTAINER_ENGINE:-docker}
source_image=ghcr.io/azure/host-ubuntu2404-gb200-nvidia-580167@sha256:39f4fd5312307ee2ef7ec5e236e420faa5d08e663853800d86fb6a3e162181bb
ubuntu_image=docker.io/library/ubuntu@sha256:18b33ff3829707f31ada654a963b9ee35f2e6f0241b964bd17a22a9b330cb63d
kernel_release=6.14.0-1003-azure-nvidia
kernel_version=6.14.0-1003.3

volume_suffix=
if [ "$(basename "${container_engine}")" = podman ]; then
    volume_suffix=:Z
fi

customize_disk() {
    local work=$1
    local root="${work}/root"
    local loop=
    active_root=${root}
    active_loop=

    # shellcheck disable=SC2329
    cleanup_mounts() {
        set +e
        mountpoint -q "${active_root}/run" && umount -R "${active_root}/run"
        mountpoint -q "${active_root}/sys" && umount -R "${active_root}/sys"
        mountpoint -q "${active_root}/proc" && umount -R "${active_root}/proc"
        mountpoint -q "${active_root}/dev" && umount -R "${active_root}/dev"
        mountpoint -q "${active_root}/boot/efi" &&
            umount "${active_root}/boot/efi"
        if mountpoint -q "${active_root}"; then
            umount "${active_root}" || umount --lazy "${active_root}"
        fi
        test -z "${active_loop}" || losetup -d "${active_loop}"
    }
    trap cleanup_mounts EXIT

    loop=$(losetup --find --show --partscan "${work}/disk.img")
    active_loop=${loop}
    sleep 2
    mkdir -p "${root}"
    mount "${loop}p2" "${root}"
    mkdir -p "${root}/boot/efi"
    mount "${loop}p1" "${root}/boot/efi"
    for path in dev proc sys run; do
        mount --rbind "/${path}" "${root}/${path}"
        mount --make-rslave "${root}/${path}"
    done

    rm -rf "${root}/tmp/packages"
    cp -a "${work}/packages" "${root}/tmp/packages"
    cp "${script_dir}/assets/90-gb200-access-users.cfg" \
        "${root}/etc/cloud/cloud.cfg.d/90-gb200-access-users.cfg"
    cp "${script_dir}/assets/customize.sh" "${root}/tmp/customize.sh"
    test ! -e "${root}/usr/sbin/policy-rc.d"
    printf '#!/bin/sh\nexit 101\n' > "${root}/usr/sbin/policy-rc.d"
    chmod +x "${root}/usr/sbin/policy-rc.d" "${root}/tmp/customize.sh"
    chroot "${root}" /usr/bin/env \
        DEBIAN_FRONTEND=noninteractive \
        SYSTEMD_OFFLINE=1 \
        /bin/sh /tmp/customize.sh
    rm -f "${root}/tmp/customize.sh" "${root}/usr/sbin/policy-rc.d"

    # shellcheck disable=SC2016
    chroot "${root}" dpkg-query -W -f='${Version}\n' \
        "linux-image-${kernel_release}" |
        grep -qx "${kernel_version}"
    test "$(readlink "${root}/boot/vmlinuz")" = "vmlinuz-${kernel_release}"
    for module in nvidia nvidia_modeset nvidia_uvm nvidia_drm; do
        chroot "${root}" modinfo -k "${kernel_release}" "${module}" >/dev/null
    done
    if find "${root}/boot" "${root}/lib/modules" "${root}/usr/lib/modules" \
        -maxdepth 2 -name '*7.0.0-1019*' -print -quit | grep -q .; then
        exit 1
    fi
    cmp "${script_dir}/assets/90-gb200-access-users.cfg" \
        "${root}/etc/cloud/cloud.cfg.d/90-gb200-access-users.cfg"
    diff -u <(printf '%s\n' nvidia nvidia_modeset nvidia_uvm nvidia_drm) \
        "${root}/etc/modules-load.d/nvidia.conf"
    diff -u <(cat <<'EOF'
[Unit]
Description=Initialize NVIDIA devices and enable persistence mode
After=systemd-modules-load.service
Before=cloud-final.service
ConditionPathExistsGlob=/sys/bus/pci/drivers/nvidia/0*

[Service]
Type=oneshot
RemainAfterExit=yes
ExecStartPre=/usr/bin/nvidia-modprobe -u -c=0
ExecStartPre=/usr/bin/nvidia-modprobe -m
ExecStart=/usr/bin/nvidia-smi -pm 1

[Install]
WantedBy=multi-user.target
EOF
    ) "${root}/etc/systemd/system/nvidia-persistence-mode.service"
    test "$(
        chroot "${root}" readlink -f \
            /etc/systemd/system/multi-user.target.wants/nvidia-persistence-mode.service
    )" = /etc/systemd/system/nvidia-persistence-mode.service
    test ! -s "${root}/etc/machine-id"
    if find "${root}/etc/ssh" -maxdepth 1 -name 'ssh_host_*_key' \
        -print -quit | grep -q .; then
        exit 1
    fi

    for path in run sys proc dev; do
        umount -R "${root}/${path}"
    done
    umount "${root}/boot/efi"
    sync
    umount "${root}" 2>/dev/null || umount --lazy "${root}"
    losetup -d "${loop}"
    loop=
    active_loop=
    trap - EXIT
}

if [ "${1:-}" = --customize-disk ]; then
    test "$#" -eq 2
    customize_disk "$2"
    exit
fi

test "$#" -eq 0
output="${script_dir}/output"
work=$(mktemp -d /var/tmp/gb200-access.XXXXXX)
source_container=

cleanup() {
    set +e
    test -z "${source_container}" ||
        "${container_engine}" rm -f "${source_container}" >/dev/null
    rm -rf "${work}"
}
trap cleanup EXIT

mkdir -p "${work}/packages"
chmod 0777 "${work}/packages"
# shellcheck disable=SC2016
"${container_engine}" run --rm --platform linux/arm64 \
    -v "${work}/packages:/packages${volume_suffix}" \
    "${ubuntu_image}" sh -euxc '
        apt-get update
        apt-get install -y --no-install-recommends ca-certificates curl
        cd /packages
        apt-get download \
            ssh-import-id=5.11-0ubuntu2.24.04.1 \
            wget=1.21.4-1ubuntu4.5
        snapshot=https://snapshot.ubuntu.com/ubuntu/20251001T235959Z
        curl_options="-fL --retry 5 --retry-all-errors --retry-delay 2 -C - -O"
        curl ${curl_options} "${snapshot}/pool/main/l/linux-base/linux-base-sgx_4.5ubuntu9+24.04.1_all.deb"
        curl ${curl_options} "${snapshot}/pool/main/l/linux-azure-nvidia-6.14/linux-azure-nvidia-6.14-headers-6.14.0-1003_6.14.0-1003.3_all.deb"
        curl ${curl_options} "${snapshot}/pool/main/l/linux-azure-nvidia-6.14/linux-headers-6.14.0-1003-azure-nvidia_6.14.0-1003.3_arm64.deb"
        curl ${curl_options} "${snapshot}/pool/main/l/linux-signed-azure-nvidia-6.14/linux-image-6.14.0-1003-azure-nvidia_6.14.0-1003.3_arm64.deb"
        curl ${curl_options} "${snapshot}/pool/main/l/linux-azure-nvidia-6.14/linux-modules-6.14.0-1003-azure-nvidia_6.14.0-1003.3_arm64.deb"
        curl ${curl_options} "${snapshot}/pool/main/l/linux-azure-nvidia-6.14/linux-modules-extra-6.14.0-1003-azure-nvidia_6.14.0-1003.3_arm64.deb"
    '
(
    cd "${work}/packages"
    sha256sum -c <<'EOF'
97bb2840a271ee0573da934ce1811f961e4b844fe876e34602b4661db25135b5  ssh-import-id_5.11-0ubuntu2.24.04.1_all.deb
ed71643b8873240cce1ac905b7993f3372436d39410894f413968f43839f84e6  wget_1.21.4-1ubuntu4.5_arm64.deb
6cea5d366ee41b7a7431d065ce573b7ded4916172a5b5c90f0fa0e07d9b45ecd  linux-base-sgx_4.5ubuntu9+24.04.1_all.deb
68f0cf36c19cc690fe4095e7c8d4b6fcf0bcb8a50fb6c98b61453e06a4e793ba  linux-azure-nvidia-6.14-headers-6.14.0-1003_6.14.0-1003.3_all.deb
0a764e87be216b2ce8868675120a49daf72f57b00592bc73bc2bcc1da8aa4c51  linux-headers-6.14.0-1003-azure-nvidia_6.14.0-1003.3_arm64.deb
4714c5910f7a5ccf0484237e8c6a661a5a610aab5d7369d3889976887c12d86e  linux-image-6.14.0-1003-azure-nvidia_6.14.0-1003.3_arm64.deb
a25a29b4f78e08f81a2ce83d68119f03ff5dd686336cdc8a71155458a46e7775  linux-modules-6.14.0-1003-azure-nvidia_6.14.0-1003.3_arm64.deb
080225782d2e7278133252e46d79eb91da6d58e1ac38bd5e56982c0fffa5b5b1  linux-modules-extra-6.14.0-1003-azure-nvidia_6.14.0-1003.3_arm64.deb
EOF
)

"${container_engine}" pull --platform linux/arm64 "${source_image}"
source_container=$(
    "${container_engine}" create --platform linux/arm64 "${source_image}" /unused
)
"${container_engine}" cp "${source_container}:/disk/disk.img.gz" "${work}/disk.img.gz"
"${container_engine}" rm "${source_container}" >/dev/null
source_container=
pigz --test "${work}/disk.img.gz"
pigz --decompress "${work}/disk.img.gz"

unshare --mount --propagation private "$0" --customize-disk "${work}"
for _ in $(seq 1 40); do
    udevadm settle
    test -z "$(losetup -j "${work}/disk.img")" && break
    sleep 0.25
done
test -z "$(losetup -j "${work}/disk.img")"

code=$(find /usr/share -name AAVMF_CODE.fd -print -quit)
vars=$(find /usr/share -name AAVMF_VARS.fd -print -quit)
test -n "${code}"
test -n "${vars}"
cp "${vars}" "${work}/AAVMF_VARS.fd"
qemu-img create -q -f qcow2 -F raw \
    -b "${work}/disk.img" "${work}/boot-overlay.qcow2"
set +e
timeout 240 qemu-system-aarch64 \
    -machine virt,accel=kvm,gic-version=3 \
    -cpu host -m 8192 -smp 8 -nographic \
    -drive if=pflash,format=raw,readonly=on,file="${code}" \
    -drive if=pflash,format=raw,file="${work}/AAVMF_VARS.fd" \
    -drive if=virtio,format=qcow2,file="${work}/boot-overlay.qcow2" \
    -net none > "${work}/boot-kvm.log" 2>&1
status=$?
set -e
if grep -Eq \
    "64 KB granular kernel is not supported|failed to initialize kvm|Could not access KVM|No accelerator found" \
    "${work}/boot-kvm.log"; then
    rm -f "${work}/boot-overlay.qcow2"
    qemu-img create -q -f qcow2 -F raw \
        -b "${work}/disk.img" "${work}/boot-overlay.qcow2"
    set +e
    timeout 360 qemu-system-aarch64 \
        -machine virt,accel=tcg,gic-version=3 \
        -cpu max -m 8192 -smp 4 -nographic \
        -drive if=pflash,format=raw,readonly=on,file="${code}" \
        -drive if=pflash,format=raw,file="${work}/AAVMF_VARS.fd" \
        -drive if=virtio,format=qcow2,file="${work}/boot-overlay.qcow2" \
        -net none > "${work}/boot.log" 2>&1
    status=$?
    set -e
    test "${status}" -eq 0 || test "${status}" -eq 124
    echo kernel > "${work}/boot-level"
else
    if [ "${status}" -ne 0 ] && [ "${status}" -ne 124 ]; then
        cat "${work}/boot-kvm.log"
        exit "${status}"
    fi
    mv "${work}/boot-kvm.log" "${work}/boot.log"
    echo system > "${work}/boot-level"
fi
grep -m1 "Linux version ${kernel_release}" "${work}/boot.log"
if grep -qx system "${work}/boot-level"; then
    grep -Em1 'Reached target .*Multi-User System| login:' "${work}/boot.log"
else
    echo "KVM is unavailable or incompatible; TCG started the target kernel."
fi

rm -rf "${output}"
mkdir -p "${output}/disk"
cp "${script_dir}/assets/image-inputs.json" \
    "${output}/disk/gb200-access-inputs.json"
pigz -1 "${work}/disk.img"
mv "${work}/disk.img.gz" "${output}/disk/disk.img.gz"
pigz --test "${output}/disk/disk.img.gz"
(
    cd "${output}/disk"
    sha256sum disk.img.gz > disk.img.gz.sha256
    cat disk.img.gz.sha256
)
chmod -R a+rX "${output}"
