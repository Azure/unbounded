#!/bin/sh
# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0
set -eu

kernel_release=6.14.0-1003-azure-nvidia
source_kernel_release=7.0.0-1019-nvidia-64k

dpkg --install \
    /tmp/packages/wget_1.21.4-1ubuntu4.5_arm64.deb \
    /tmp/packages/ssh-import-id_5.11-0ubuntu2.24.04.1_all.deb
dpkg --unpack \
    /tmp/packages/linux-base-sgx_4.5ubuntu9+24.04.1_all.deb \
    /tmp/packages/linux-azure-nvidia-6.14-headers-6.14.0-1003_6.14.0-1003.3_all.deb \
    /tmp/packages/linux-headers-6.14.0-1003-azure-nvidia_6.14.0-1003.3_arm64.deb \
    /tmp/packages/linux-modules-6.14.0-1003-azure-nvidia_6.14.0-1003.3_arm64.deb \
    /tmp/packages/linux-modules-extra-6.14.0-1003-azure-nvidia_6.14.0-1003.3_arm64.deb \
    /tmp/packages/linux-image-6.14.0-1003-azure-nvidia_6.14.0-1003.3_arm64.deb
dpkg --configure -a
dpkg --purge \
    "linux-headers-${source_kernel_release}" \
    "linux-image-${source_kernel_release}" \
    "linux-modules-${source_kernel_release}" \
    linux-nvidia-7.0-headers-7.0.0-1019

boot_configs="$(find /boot/efi -type f -name grub.cfg -print)"
test -n "${boot_configs}"
for config in ${boot_configs}; do
    sed -i "s/${source_kernel_release}/${kernel_release}/g" "${config}"
    grep -q "/boot/vmlinuz-${kernel_release}" "${config}"
    grep -q "/boot/initrd.img-${kernel_release}" "${config}"
    if grep -q "${source_kernel_release}" "${config}"; then
        exit 1
    fi
done

for package in \
    linux-headers-6.14.0-1003-azure-nvidia \
    linux-image-6.14.0-1003-azure-nvidia \
    linux-modules-6.14.0-1003-azure-nvidia \
    linux-modules-extra-6.14.0-1003-azure-nvidia; do
    test "$(dpkg-query -W -f='${Version}' "${package}")" = 6.14.0-1003.3
done
test "$(dpkg-query -W -f='${Version}' ssh-import-id)" = 5.11-0ubuntu2.24.04.1
test "$(
    dpkg-query -W -f='${Package} ${Status}\n' 'linux-image-*' 2>/dev/null |
        awk '$2 == "install" && $3 == "ok" && $4 == "installed" { print $1 }'
)" = linux-image-6.14.0-1003-azure-nvidia
# shellcheck source=/dev/null
. /etc/os-release
test "${VERSION_ID}" = 24.04
test "$(readlink /boot/vmlinuz)" = "vmlinuz-${kernel_release}"
test -d "/lib/modules/${kernel_release}/updates/dkms"
find "/lib/modules/${kernel_release}/updates/dkms" \
    -name 'nvidia.ko*' -print -quit | grep -q .
cloud-init schema --config-file /etc/cloud/cloud.cfg.d/90-gb200-access-users.cfg

rm -rf /tmp/packages /var/lib/cloud/*
rm -f /etc/ssh/ssh_host_*
: > /etc/machine-id
ln -sf /etc/machine-id /var/lib/dbus/machine-id
