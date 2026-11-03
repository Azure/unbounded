#!/bin/sh
# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0
set -eu

agent_archive=/opt/unbounded/bootstrap/unbounded-agent-linux-arm64.tar.gz
agent_version=v0.17.0-rc.1
nvlink_config=/etc/modprobe.d/99-nvidia-disable-nvlink.conf

fail() {
    echo "$*" >&2
    exit 1
}

nvlink_disable_value() {
    modprobe -C "$1" -c |
        awk '
            $1 == "options" && $2 == "nvidia" {
                for (field = 3; field <= NF; field++) {
                    if ($field ~ /^NVreg_NvLinkDisable=/) {
                        split($field, option, "=")
                        value = option[2]
                    }
                }
            }
            END { print value }
        '
}

dpkg --install /tmp/wget.deb /tmp/ssh-import-id.deb
test "$(dpkg-query -W -f='${Version}' wget)" = 1.21.4-1ubuntu4.5 ||
    fail "unexpected wget version"
test "$(dpkg-query -W -f='${Version}' ssh-import-id)" = 5.11-0ubuntu2.24.04.1 ||
    fail "unexpected ssh-import-id version"

cloud-init schema --config-file /etc/cloud/cloud.cfg.d/90-gb200-access-users.cfg
grep -qx 'options nvidia NVreg_NvLinkDisable=1' "${nvlink_config}" ||
    fail "rootfs GPU NVLink policy changed"

set -- /boot/vmlinuz-*
test "$#" -eq 1 && test -f "$1" ||
    fail "expected exactly one boot kernel"
kernel_release="${1##*/vmlinuz-}"
modinfo -k "${kernel_release}" nvidia >/dev/null ||
    fail "NVIDIA module is missing from the boot kernel"

update-initramfs -u -k "${kernel_release}"
initramfs_dir="$(mktemp -d /var/lib/unbounded-initramfs.XXXXXX)"
trap 'rm -rf "${initramfs_dir}"' EXIT
unmkinitramfs "/boot/initrd.img-${kernel_release}" "${initramfs_dir}"
initramfs_config="$(
    find "${initramfs_dir}" -type f \
        -path '*/etc/modprobe.d/99-nvidia-disable-nvlink.conf' -print
)"
test "$(printf '%s\n' "${initramfs_config}" | awk 'NF { n++ } END { print n + 0 }')" -eq 1 ||
    fail "initramfs GPU NVLink policy is missing or duplicated"
cmp "${nvlink_config}" "${initramfs_config}" ||
    fail "initramfs GPU NVLink policy changed"
test "$(nvlink_disable_value /etc/modprobe.d)" = 1 ||
    fail "rootfs GPU NVLink policy is not effective"
test "$(nvlink_disable_value "$(dirname "${initramfs_config}")")" = 1 ||
    fail "initramfs GPU NVLink policy is not effective"
rm -rf "${initramfs_dir}"
trap - EXIT

agent_dir="$(mktemp -d /var/lib/unbounded-agent.XXXXXX)"
trap 'rm -rf "${agent_dir}"' EXIT
tar -xzf "${agent_archive}" -C "${agent_dir}" unbounded-agent
agent_output="$("${agent_dir}/unbounded-agent" version 2>&1)" ||
    fail "embedded agent version check failed: ${agent_output}"
printf '%s\n' "${agent_output}" |
    grep -Fq "unbounded-agent ${agent_version#v} " ||
    fail "embedded agent reports unexpected version: ${agent_output}"
rm -rf "${agent_dir}"
trap - EXIT

test -x /usr/bin/curl || fail "curl is required by the bootstrap script"
rm -rf /tmp/ssh-import-id.deb /tmp/wget.deb /var/lib/cloud/*
rm -f /etc/ssh/ssh_host_*
: > /etc/machine-id
ln -sf /etc/machine-id /var/lib/dbus/machine-id
