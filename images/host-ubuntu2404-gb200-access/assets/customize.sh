#!/bin/sh
# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0
set -eu

kernel_release=6.14.0-1003-azure-nvidia
source_kernel_release=7.0.0-1019-nvidia-64k
kernel_parameters="iommu.passthrough=1 arm_smmu_v3.disable_msipolling=1 init_on_alloc=0"
forbidden_kernel_parameters="net.ifnames=0 irqchip.gicv3_nolpi=y"
agent_version=v0.17.0-rc.1
agent_archive=/opt/unbounded/bootstrap/unbounded-agent-linux-arm64.tar.gz

fail() {
    echo "$*" >&2
    exit 1
}

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
apt-mark hold \
    "linux-azure-nvidia-6.14-headers-6.14.0-1003" \
    "linux-headers-${kernel_release}" \
    "linux-image-${kernel_release}" \
    "linux-modules-${kernel_release}" \
    "linux-modules-extra-${kernel_release}"

echo "Removing the John Cabaj package source."
for source in /etc/apt/sources.list.d/*; do
    test -f "${source}" || continue
    if printf '%s\n' "${source}" | grep -qi john-cabaj ||
        grep -qi john-cabaj "${source}"; then
        rm -f "${source}"
    fi
done
if grep -Rqi john-cabaj /etc/apt/sources.list /etc/apt/sources.list.d 2>/dev/null; then
    echo "The John Cabaj package source is still configured." >&2
    exit 1
fi

echo "Updating target-kernel EFI GRUB entries."
boot_configs="$(find /boot/efi -type f -name grub.cfg -print)"
if test -z "${boot_configs}"; then
    echo "No EFI GRUB configuration was found." >&2
    exit 1
fi
validated_boot_configs=0
for config in ${boot_configs}; do
    if ! grep -Eq \
        "/boot/(vmlinuz|initrd\\.img)-(${source_kernel_release}|${kernel_release})([[:space:]]|$)" \
        "${config}"; then
        continue
    fi

    sed -i "s/${source_kernel_release}/${kernel_release}/g" "${config}"
    sed -i \
        "/^[[:space:]]*linux[[:space:]].*\\/boot\\/vmlinuz-${kernel_release}/ {
            /iommu\\.passthrough=1/! s|\$| ${kernel_parameters}|
        }" \
        "${config}"
    grep -q "/boot/vmlinuz-${kernel_release}" "${config}"
    grep -q "/boot/initrd.img-${kernel_release}" "${config}"
    if grep -q "${source_kernel_release}" "${config}"; then
        exit 1
    fi
    kernel_lines="$(
        grep -E "^[[:space:]]*linux[[:space:]].*/boot/vmlinuz-${kernel_release}([[:space:]]|$)" \
            "${config}"
    )"
    test -n "${kernel_lines}"
    for parameter in ${kernel_parameters}; do
        printf '%s\n' "${kernel_lines}" | grep -Fq " ${parameter}"
    done
    for parameter in ${forbidden_kernel_parameters}; do
        if printf '%s\n' "${kernel_lines}" | grep -Fq " ${parameter}"; then
            exit 1
        fi
    done
    validated_boot_configs=$((validated_boot_configs + 1))
done
if test "${validated_boot_configs}" -eq 0; then
    echo "No EFI GRUB configuration referenced the source or target kernel." >&2
    exit 1
fi

echo "Validating installed kernel and access packages."
for package in \
    linux-azure-nvidia-6.14-headers-6.14.0-1003 \
    linux-headers-6.14.0-1003-azure-nvidia \
    linux-image-6.14.0-1003-azure-nvidia \
    linux-modules-6.14.0-1003-azure-nvidia \
    linux-modules-extra-6.14.0-1003-azure-nvidia; do
    package_version="$(dpkg-query -W -f='${Version}' "${package}")"
    test "${package_version}" = 6.14.0-1003.3 ||
        fail "${package} has version ${package_version}"
    apt-mark showhold | grep -qx "${package}" ||
        fail "${package} is not held"
done
ssh_import_id_version="$(dpkg-query -W -f='${Version}' ssh-import-id)"
test "${ssh_import_id_version}" = 5.11-0ubuntu2.24.04.1 ||
    fail "ssh-import-id has version ${ssh_import_id_version}"
installed_kernels="$(
    dpkg-query -W -f='${Package} ${Status}\n' 'linux-image-*' 2>/dev/null |
        awk '$3 == "ok" && $4 == "installed" { print $1 }'
)"
test "${installed_kernels}" = linux-image-6.14.0-1003-azure-nvidia ||
    fail "unexpected installed kernel packages: ${installed_kernels}"
# shellcheck source=/dev/null
. /etc/os-release
test "${VERSION_ID}" = 24.04 ||
    fail "unexpected Ubuntu version: ${VERSION_ID}"
boot_kernel="$(readlink /boot/vmlinuz)"
test "${boot_kernel}" = "vmlinuz-${kernel_release}" ||
    fail "/boot/vmlinuz points to ${boot_kernel}"
test -d "/lib/modules/${kernel_release}/updates/dkms" ||
    fail "NVIDIA DKMS output directory is missing"
find "/lib/modules/${kernel_release}/updates/dkms" \
    -name 'nvidia.ko*' -print -quit | grep -q . ||
    fail "nvidia.ko is missing from the target kernel"
echo "Validating cloud-init access policy."
cloud-init schema --config-file /etc/cloud/cloud.cfg.d/90-gb200-access-users.cfg
python3 /tmp/verify.py \
    --image-config /etc/cloud/cloud.cfg.d/90-gb200-access-users.cfg \
    --vendor-template /tmp/vendor-data.tmpl \
    --dpkg-status /var/lib/dpkg/status \
    --agent-archive "${agent_archive}"
agent_verify_dir="$(mktemp -d)"
trap 'rm -rf "${agent_verify_dir}"' EXIT
tar -xzf "${agent_archive}" -C "${agent_verify_dir}" unbounded-agent
"${agent_verify_dir}/unbounded-agent" version |
    grep -Fq "unbounded-agent ${agent_version}"
rm -rf "${agent_verify_dir}"
trap - EXIT
grep -qx 'blacklist nouveau' /etc/modprobe.d/blacklist-nouveau.conf

cat > /etc/modules-load.d/nvidia.conf <<'EOF'
nvidia
nvidia_modeset
nvidia_uvm
nvidia_drm
EOF
cat > /etc/systemd/system/nvidia-persistence-mode.service <<'EOF'
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
test -x /usr/bin/nvidia-modprobe
test -x /usr/bin/nvidia-smi
systemd-analyze verify /etc/systemd/system/nvidia-persistence-mode.service
systemctl enable nvidia-persistence-mode.service

rm -rf /tmp/packages /var/lib/cloud/*
rm -f /etc/ssh/ssh_host_*
: > /etc/machine-id
ln -sf /etc/machine-id /var/lib/dbus/machine-id
