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
agent_version=v0.17.0-rc.1
agent_asset=unbounded-agent-linux-arm64.tar.gz
agent_url=https://github.com/Azure/unbounded/releases/download/${agent_version}/${agent_asset}

customize_disk() {
    local work=$1
    local root="${work}/root"
    local loop
    active_root=${root}
    active_loop=

    # shellcheck disable=SC2329 # Invoked by the EXIT trap.
    cleanup_mounts() {
        set +e
        for path in run sys proc dev; do
            mountpoint -q "${active_root}/${path}" && umount -R "${active_root}/${path}"
        done
        mountpoint -q "${active_root}" && umount "${active_root}"
        test -z "${active_loop}" || losetup -d "${active_loop}"
    }
    trap cleanup_mounts EXIT

    loop=$(losetup --find --show --partscan "${work}/disk.img")
    active_loop=${loop}
    sleep 2
    mkdir -p "${root}"
    mount "${loop}p2" "${root}"
    for path in dev proc sys run; do
        mount --rbind "/${path}" "${root}/${path}"
        mount --make-rslave "${root}/${path}"
    done

    install -D -m 0644 "${work}/packages/ssh-import-id.deb" \
        "${root}/tmp/ssh-import-id.deb"
    install -D -m 0644 "${work}/packages/wget.deb" \
        "${root}/tmp/wget.deb"
    install -D -m 0644 "${work}/packages/${agent_asset}" \
        "${root}/opt/unbounded/bootstrap/${agent_asset}"
    install -D -m 0644 "${script_dir}/assets/90-gb200-test-users.cfg" \
        "${root}/etc/cloud/cloud.cfg.d/90-gb200-test-users.cfg"
    install -D -m 0644 "${script_dir}/assets/99-nvidia-disable-nvlink.conf" \
        "${root}/etc/modprobe.d/99-nvidia-disable-nvlink.conf"
    install -D -m 0755 "${script_dir}/assets/customize.sh" \
        "${root}/tmp/customize.sh"
    test ! -e "${root}/usr/sbin/policy-rc.d"
    printf '#!/bin/sh\nexit 101\n' > "${root}/usr/sbin/policy-rc.d"
    chmod +x "${root}/usr/sbin/policy-rc.d"

    chroot "${root}" /usr/bin/env \
        DEBIAN_FRONTEND=noninteractive \
        SYSTEMD_OFFLINE=1 \
        /bin/sh /tmp/customize.sh
    rm -f "${root}/tmp/customize.sh" "${root}/usr/sbin/policy-rc.d"

    cmp "${script_dir}/assets/90-gb200-test-users.cfg" \
        "${root}/etc/cloud/cloud.cfg.d/90-gb200-test-users.cfg"
    cmp "${script_dir}/assets/99-nvidia-disable-nvlink.conf" \
        "${root}/etc/modprobe.d/99-nvidia-disable-nvlink.conf"
    test ! -s "${root}/etc/machine-id"
    test -z "$(find "${root}/etc/ssh" -maxdepth 1 -name 'ssh_host_*_key' -print -quit)"
    test -z "$(find "${root}/var/lib/cloud" -mindepth 1 -print -quit)"

    for path in run sys proc dev; do
        umount -R "${root}/${path}"
    done
    sync
    umount "${root}"
    losetup -d "${loop}"
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
work=$(mktemp -d /var/tmp/gb200-test.XXXXXX)
source_container=

cleanup() {
    set +e
    test -z "${source_container}" ||
        "${container_engine}" rm -f "${source_container}" >/dev/null
    rm -rf "${work}"
}
trap cleanup EXIT

mkdir -p "${work}/packages"
"${container_engine}" run --rm --platform linux/arm64 \
    -v "${work}/packages:/packages" \
    "${ubuntu_image}" sh -euxc '
        apt-get update
        apt-get install -y --no-install-recommends ca-certificates curl
        cd /packages
        apt-get download \
            ssh-import-id=5.11-0ubuntu2.24.04.1 \
            wget=1.21.4-1ubuntu4.5
        mv ssh-import-id_*.deb ssh-import-id.deb
        mv wget_*.deb wget.deb
        curl -fL --retry 5 --retry-all-errors --retry-delay 2 -O \
            "'"${agent_url}"'"
    '
(
    cd "${work}/packages"
    sha256sum -c <<'EOF'
97bb2840a271ee0573da934ce1811f961e4b844fe876e34602b4661db25135b5  ssh-import-id.deb
ed71643b8873240cce1ac905b7993f3372436d39410894f413968f43839f84e6  wget.deb
faf21645f33cd5ef80cce04ccb2d8c9fc289cfb1dedbaeb47cf85e58186a8ff0  unbounded-agent-linux-arm64.tar.gz
EOF
)

"${container_engine}" pull --platform linux/arm64 "${source_image}"
source_container=$("${container_engine}" create --platform linux/arm64 "${source_image}" /unused)
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

rm -rf "${output}"
mkdir -p "${output}/disk"
pigz -1 "${work}/disk.img"
mv "${work}/disk.img.gz" "${output}/disk/disk.img.gz"
pigz --test "${output}/disk/disk.img.gz"
(
    cd "${output}/disk"
    sha256sum disk.img.gz > disk.img.gz.sha256
)
chmod -R a+rX "${output}"
