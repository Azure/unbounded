#!/bin/sh
# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0

set -eu

DISK_GZ="${1:-/disk/disk.img.gz}"
[ -f "$DISK_GZ" ] || {
  echo "missing Metalman disk artifact: $DISK_GZ" >&2
  exit 1
}

for command in basename debugfs find gzip modinfo mtype python3 sfdisk sha256sum; do
  command -v "$command" >/dev/null 2>&1 || {
    echo "required inspection command is missing: $command" >&2
    exit 1
  }
done

WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT
DISK="$WORK/disk.img"
ROOT="$WORK/root.img"
gzip -dc "$DISK_GZ" > "$DISK"

partition_field() {
  partition="$1"
  field="$2"
  sfdisk --json "$DISK" | python3 -c \
    'import json,sys
data=json.load(sys.stdin)["partitiontable"]["partitions"][int(sys.argv[1])]
print(data[sys.argv[2]])' "$partition" "$field"
}

ESP_START=$(partition_field 0 start)
ROOT_START=$(partition_field 1 start)
ROOT_SIZE=$(partition_field 1 size)
[ "$ESP_START" = 2048 ] || {
  echo "unexpected ESP start sector: $ESP_START" >&2
  exit 1
}
dd if="$DISK" of="$ROOT" bs=512 skip="$ROOT_START" count="$ROOT_SIZE" status=none

debugfs_exists() {
  debugfs -R "stat $1" "$ROOT" 2>/dev/null | grep -q '^Inode:'
}

for path in \
  /etc/default/grub.d/51-azure-nvidia.cfg \
  /etc/grub.d/10_azure_nvidia \
  /etc/modprobe.d/blacklist-nouveau.conf \
  /etc/modprobe.d/nvidia-gb.conf \
  /etc/systemd/system/nvidia-persistence-mode.service \
  /etc/unbounded/image-inputs \
  /etc/unbounded/nvidia-driver-manifest \
  /usr/bin/nvidia-smi \
  /usr/lib/firmware/nvidia/580.178.04/gsp_ga10x.bin \
  /usr/lib/firmware/nvidia/580.178.04/gsp_tu10x.bin \
  /usr/lib/modules/6.18.48.1-1.azl3/kernel/drivers/gpu/drm/drm_ttm_helper.ko.xz \
  /usr/lib/modules/6.18.48.1-1.azl3/kernel/drivers/gpu/drm/ttm/ttm.ko.xz \
  /usr/lib/grub/arm64-efi/smbios.mod; do
  debugfs_exists "$path" || {
    echo "missing expected file in root partition: $path" >&2
    exit 1
  }
done

INPUTS=$(debugfs -R "cat /etc/unbounded/image-inputs" "$ROOT" 2>/dev/null)
for expected in \
  architecture=arm64 \
  image_variant=nvidia \
  boot_profile=nvidia-hwe \
  kernel=6.6.157.1-1.azl3 \
  kernel_hwe=6.18.48.1-1.azl3 \
  kernel_hwe_drivers_gpu=6.18.48.1-1.azl3 \
  grub=2.06-27.azl3 \
  nvidia_driver_source=nvidia-localrepo \
  nvidia_driver=580.178.04-1.azl3 \
  nvidia_dkms=3.4.1-1.azl3 \
  nvidia_local_repo_sha256=7c734d8daa0876d3c5731622e78fe20811ee8c9f61a805b74b0b09576b992df4 \
  nvidia_local_repo_key_sha256=43464fc9a8371860b10b81edbd95ab95f28aa1dc1cc988ec15d2369e10d1fc7b \
  nvidia_fabric_probe_sha256=ebb6fa74a476dd7a55b7e705c2ad19c00bd20e31fb984babcbc114130efe05b5 \
  mlnx_ofa_kernel=26.01-1_6.6.158.1.1.azl3 \
  mlnx_ofa_kernel_hwe_devel=26.01-2_6.18.48.1.1.azl3 \
  mlnx_ofa_kernel_hwe_modules=26.01-2_6.18.48.1.1.azl3 \
  ocl_icd=2.2.13-2.azl3 \
  opencl_filesystem=1.0-13.azl3 \
  binutils=2.41-13.azl3 \
  elfutils_libelf_devel=0.189-6.azl3 \
  gcc=13.2.0-7.azl3 \
  make=4.4.1-2.azl3 \
  patch=2.7.6-10.azl3; do
  printf '%s\n' "$INPUTS" | grep -Fx "$expected" >/dev/null || {
    echo "missing image input: $expected" >&2
    exit 1
  }
done
echo "PASS package inputs:"
printf '%s\n' "$INPUTS" | sed 's/^/  /'

for service in nvidia_persistenced_service nvidia_powerd_service; do
  state=$(printf '%s\n' "$INPUTS" | sed -n "s/^$service=//p")
  case "$state" in
    disabled|not-found) ;;
    *)
      echo "$service is $state, expected disabled or not-found" >&2
      exit 1
      ;;
  esac
done
mkdir -p "$WORK/systemd-etc" "$WORK/systemd-usr"
debugfs -R "rdump /etc/systemd/system $WORK/systemd-etc" "$ROOT" >/dev/null 2>&1
debugfs -R "rdump /usr/lib/systemd/system $WORK/systemd-usr" "$ROOT" >/dev/null 2>&1
if find "$WORK/systemd-etc" "$WORK/systemd-usr" -type l \
    \( -name nvidia-persistenced.service -o -name nvidia-powerd.service \) |
    grep -q .; then
  echo "NVIDIA vendor service is unexpectedly linked into a boot target" >&2
  exit 1
fi
echo "PASS idle-service baseline:"
echo "  nvidia-persistenced.service is disabled or absent"
echo "  nvidia-powerd.service is disabled or absent"

DRIVER_MANIFEST=$(debugfs -R "cat /etc/unbounded/nvidia-driver-manifest" "$ROOT" 2>/dev/null)
for expected in \
  driver_source=nvidia-localrepo \
  driver_version=580.178.04-1.azl3 \
  kernel=6.18.48.1-1.azl3; do
  printf '%s\n' "$DRIVER_MANIFEST" | grep -Fx "$expected" >/dev/null || {
    echo "missing NVIDIA driver manifest value: $expected" >&2
    exit 1
  }
done
for service in persistenced_service powerd_service; do
  state=$(printf '%s\n' "$DRIVER_MANIFEST" | sed -n "s/^$service=//p")
  case "$state" in
    disabled|not-found) ;;
    *)
      echo "NVIDIA driver manifest $service is $state, expected disabled or not-found" >&2
      exit 1
      ;;
  esac
done

manifest_value() {
  key="$1"
  printf '%s\n' "$DRIVER_MANIFEST" | sed -n "s/^$key=//p"
}

MODULES_DEP=$(debugfs -R \
  "cat /usr/lib/modules/6.18.48.1-1.azl3/modules.dep" "$ROOT" 2>/dev/null)
NVIDIA_DRM_DEP=$(printf '%s\n' "$MODULES_DEP" | grep '/nvidia-drm\.ko')
for dependency in \
  kernel/drivers/gpu/drm/ttm/ttm.ko \
  kernel/drivers/gpu/drm/drm_ttm_helper.ko; do
  printf '%s\n' "$NVIDIA_DRM_DEP" | grep -F "$dependency" >/dev/null || {
    echo "published nvidia_drm dependency is missing: $dependency" >&2
    exit 1
  }
done
echo "PASS NVIDIA DRM dependency closure:"
echo "  kernel-hwe-drivers-gpu provides ttm and drm_ttm_helper"

for module in nvidia nvidia_modeset nvidia_drm nvidia_uvm nvidia_peermem; do
  module_path=$(manifest_value "module_${module}_path")
  module_sha256=$(manifest_value "module_${module}_sha256")
  [ -n "$module_path" ] && [ -n "$module_sha256" ] || {
    echo "missing $module path or sha256 in NVIDIA driver manifest" >&2
    exit 1
  }
  debugfs_exists "$module_path" || {
    echo "missing NVIDIA module in root partition: $module_path" >&2
    exit 1
  }
  module_file="$WORK/$(basename "$module_path")"
  debugfs -R "dump $module_path $module_file" "$ROOT" >/dev/null 2>&1
  actual_sha256=$(sha256sum "$module_file" | cut -d' ' -f1)
  [ "$actual_sha256" = "$module_sha256" ] || {
    echo "$module sha256 is $actual_sha256, expected $module_sha256" >&2
    exit 1
  }
  version=$(modinfo -F version "$module_file")
  [ "$version" = 580.178.04 ] || {
    echo "$module version is $version, expected 580.178.04" >&2
    exit 1
  }
  vermagic=$(modinfo -F vermagic "$module_file")
  case "$vermagic" in
    "6.18.48.1-1.azl3 "*) ;;
    *)
      echo "$module vermagic is $vermagic, expected kernel 6.18.48.1-1.azl3" >&2
      exit 1
      ;;
  esac
done
echo "PASS NVIDIA 580.178.04 target-kernel DKMS modules:"
echo "  nvidia, nvidia-modeset, nvidia-drm, nvidia-uvm, nvidia-peermem"
echo "  kernel=6.18.48.1-1.azl3, source=verified NVIDIA AZL3 local repository"
echo "  GSP firmware=580.178.04"

for kver in 6.6.157.1-1.azl3 6.18.48.1-1.azl3; do
  debugfs_exists "/boot/vmlinuz-$kver" || {
    echo "missing kernel: $kver" >&2
    exit 1
  }
  debugfs_exists "/boot/initramfs-$kver.img" || {
    echo "missing initramfs: $kver" >&2
    exit 1
  }
done
echo "PASS kernels:"
echo "  standard=6.6.157.1-1.azl3"
echo "  nvidia-grace=6.18.48.1-1.azl3"

GRUB=$(mtype -i "$DISK@@$((ESP_START * 512))" ::/EFI/BOOT/grub.cfg)
for expected in \
  'set default=nvidia-grace' \
  'linux /boot/vmlinuz-6.18.48.1-1.azl3'; do
  printf '%s\n' "$GRUB" | grep -F "$expected" >/dev/null || {
    echo "missing GRUB contract: $expected" >&2
    exit 1
  }
done
for unexpected in \
  'insmod smbios' \
  'smbios --type 4' \
  'cpu_manufacturer' \
  'if [ x$cpu_manufacturer' \
  'iommu.passthrough=1' \
  'irqchip.gicv3_nolpi=y' \
  'arm_smmu_v3.disable_msipolling=1'; do
  if printf '%s\n' "$GRUB" | grep -F "$unexpected" >/dev/null; then
    echo "NVIDIA image unexpectedly uses runtime SMBIOS selection: $unexpected" >&2
    exit 1
  fi
done

STANDARD_ENTRY=$(printf '%s\n' "$GRUB" | sed -n \
  '/menuentry "Azure Linux" --id standard {/,/^}/p')
GRACE_ENTRY=$(printf '%s\n' "$GRUB" | sed -n \
  '/menuentry "Azure Linux NVIDIA Grace" --id nvidia-grace {/,/^}/p')
printf '%s\n' "$STANDARD_ENTRY" | grep -F \
  'linux /boot/vmlinuz-6.6.157.1-1.azl3' >/dev/null || {
  echo "ordinary ARM64 entry does not select the standard kernel" >&2
  exit 1
}
if printf '%s\n' "$STANDARD_ENTRY" | grep -F 'nvidia_args' >/dev/null; then
  echo "ordinary ARM64 entry unexpectedly contains Grace arguments" >&2
  exit 1
fi
printf '%s\n' "$GRACE_ENTRY" | grep -F \
  'linux /boot/vmlinuz-6.18.48.1-1.azl3' >/dev/null || {
  echo "NVIDIA Grace entry does not select kernel-hwe" >&2
  exit 1
}
echo "PASS specialized NVIDIA GRUB policy:"
echo "  default=nvidia-grace (no runtime SMBIOS decision)"
echo "  recovery entry -> vmlinuz-6.6.157.1-1.azl3, no Grace arguments"
echo "  default entry -> vmlinuz-6.18.48.1-1.azl3, no extra Grace arguments"
echo "PASS MANA-compatible kernel command line:"
echo "  irqchip.gicv3_nolpi is absent; GICv3 LPI/ITS MSI-X remains enabled"
echo "  iommu.passthrough and arm_smmu_v3.disable_msipolling are absent"
echo "PASS GRUB package module: /usr/lib/grub/arm64-efi/smbios.mod"
echo "verified Azure Linux NVIDIA ARM64 disk: $DISK_GZ"
