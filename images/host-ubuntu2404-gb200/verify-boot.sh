#!/bin/bash
# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0
set -euo pipefail

disk_dir=$(realpath "${1:?usage: verify-boot.sh DISK_DIRECTORY}")
image_name=${2:-}
script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
cd "$disk_dir"
sha256sum --check disk.img.gz.sha256
gzip -dc disk.img.gz > disk.img
mkdir -p seed
cat > seed/meta-data <<'EOF'
instance-id: unbounded-gb200-image-ci
local-hostname: unbounded-gb200-image-ci
EOF

if [ "$image_name" = host-ubuntu2404-gb200-hillsboro ]; then
    printf '%s\n' '#cloud-config' > seed/user-data
    python3 "$script_dir/../host-ubuntu2404-gb200-hillsboro/assets/render_boot_vendor_data.py" \
        --template "$script_dir/../netboot/assets/vendor-data.tmpl" \
        --install-script "$script_dir/../host-ubuntu2404-gb200-hillsboro/assets/verify-boot-install.sh" \
        --ssh-import-id-script "$script_dir/../host-ubuntu2404-gb200-hillsboro/assets/verify-boot-ssh-import-id.sh" \
        --preserve-image-users \
        --output seed/vendor-data
else
    cat > seed/user-data <<'EOF'
#cloud-config
runcmd:
  - |
    set -eu
    test "$(uname -m)" = aarch64
    test "$(getconf PAGESIZE)" = 65536
    test "$(uname -r)" = "$(python3 -c 'import json; print(json.load(open("/usr/share/unbounded/image-inputs.json"))["kernel"])')"
    systemctl is-active --quiet systemd-networkd.service
    systemctl is-active --quiet ssh.service
    test -x /usr/bin/ssh-import-id
    modprobe --dry-run mana
    modprobe --dry-run nvidia
    echo UNBOUNDED_GB200_UBUNTU_BOOT_PASS > /dev/ttyAMA0
    systemctl poweroff --no-block
EOF
fi
cat > seed/network-config <<'EOF'
version: 2
ethernets:
  provisioning:
    match:
      name: "e*"
    dhcp4: true
EOF
truncate -s 16M seed.img
mkfs.vfat -n CIDATA seed.img >/dev/null
mcopy -i seed.img seed/meta-data seed/user-data seed/network-config ::/
if [ -f seed/vendor-data ]; then
    mcopy -i seed.img seed/vendor-data ::/
fi
# QEMU 8.2 TCG's max CPU advertises MOPS, which faults in clear_page on this
# 64K kernel. Use a stable server CPU profile for the architecture boot gate.
timeout --signal=TERM --kill-after=10s 300s \
    qemu-system-aarch64 -machine virt,gic-version=3 -cpu cortex-a72 -smp 2 -m 4096 \
    -accel tcg,thread=multi -nographic -no-reboot \
    -bios /usr/share/qemu-efi-aarch64/QEMU_EFI.fd \
    -drive file=disk.img,format=raw,if=virtio \
    -drive file=seed.img,format=raw,if=virtio \
    -netdev user,id=net0 -device virtio-net-device,netdev=net0 \
    > boot.log 2>&1
grep -Fq UNBOUNDED_GB200_UBUNTU_BOOT_PASS boot.log
printf '%s\n' "ARM64 UEFI/kernel/64K/cloud-init/networkd/SSH boot check passed."
