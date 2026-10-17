#!/bin/sh
# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0

# Builds the Azure Linux 3.0 root file system for a Metalman machine image.
# It runs inside an Azure Linux container for the target architecture and
# writes /rootfs.tar for the disk assembly stage.
#
# Usage: rootfs.sh install|finish
# The install step installs packages. The finish step configures the system
# and writes the archive. Separate steps let the build cache the slow step.
#
# Environment:
#   KERNEL_PACKAGE  single kernel package to install (default: kernel)
#   NVIDIA          set to 1 to install the NVIDIA open GPU driver
#   IMAGE_VARIANT   generic or nvidia
#   NVIDIA_DRIVER_SOURCE  azurelinux or nvidia-localrepo
#   TARGETARCH      target architecture
set -eu

KERNEL_PACKAGE="${KERNEL_PACKAGE:-kernel}"
NVIDIA="${NVIDIA:-0}"
IMAGE_VARIANT="${IMAGE_VARIANT:-generic}"
NVIDIA_DRIVER_SOURCE="${NVIDIA_DRIVER_SOURCE:-azurelinux}"
TARGETARCH="${TARGETARCH:-}"

case "$IMAGE_VARIANT:$NVIDIA" in
  generic:0|nvidia:1) ;;
  *)
    echo "inconsistent image variant and NVIDIA setting: $IMAGE_VARIANT:$NVIDIA" >&2
    exit 1
    ;;
esac

if [ "$NVIDIA" = 1 ] && [ "$TARGETARCH" = arm64 ]; then
  case "$NVIDIA_DRIVER_SOURCE" in
    azurelinux|nvidia-localrepo) ;;
    *)
      echo "unsupported NVIDIA driver source: $NVIDIA_DRIVER_SOURCE" >&2
      exit 1
      ;;
  esac
fi

retry() {
  n=1
  until "$@"; do
    if [ "$n" -ge 5 ]; then
      return 1
    fi
    echo "command failed (attempt $n/5), retrying in 30s: $*" >&2
    n=$((n + 1))
    sleep 30
  done
}

verify_file_sha256() {
  path="$1"
  expected="$2"
  actual=$(sha256sum "$path" | cut -d' ' -f1)
  [ "$actual" = "$expected" ] || {
    echo "$path sha256 is $actual, expected $expected" >&2
    exit 1
  }
}

find_nvidia_module() {
  module="$1"
  find "/usr/lib/modules/$NVIDIA_KVER" -type f \
    \( -name "$module.ko" -o -name "$module.ko.xz" -o -name "$module.ko.zst" \)
}

disable_nvidia_vendor_service() {
  unit="$1"
  if systemctl cat "$unit" >/dev/null 2>&1; then
    systemctl disable "$unit" >/dev/null
  fi
  state=$(systemctl is-enabled "$unit" 2>/dev/null || :)
  case "$state" in
    disabled|not-found) ;;
    *)
      echo "$unit enablement is $state, expected disabled or not-found" >&2
      exit 1
      ;;
  esac
  if find /etc/systemd/system /usr/lib/systemd/system -type l \
      \( -path "*/.wants/$unit" -o -path "*/.requires/$unit" \) |
      grep -q .; then
    echo "$unit is linked into a boot target" >&2
    exit 1
  fi
  printf '%s\n' "$state"
}

verify_nvidia_localrepo_driver() {
  for package in \
    nvidia-open \
    kmod-nvidia-open-dkms \
    nvidia-driver-cuda \
    nvidia-driver-cuda-libs \
    nvidia-kmod-common \
    nvidia-modprobe \
    nvidia-persistenced \
    libnvidia-cfg \
    libnvidia-gpucomp \
    libnvidia-ml; do
    version=$(rpm -q --queryformat '%{VERSION}-%{RELEASE}' "$package")
    [ "$version" = "$NVIDIA_DRIVER_RPM_VERSION" ] || {
      echo "$package resolved to $version, expected $NVIDIA_DRIVER_RPM_VERSION" >&2
      exit 1
    }
  done

  for package_and_version in \
    "binutils:$AZURELINUX_BINUTILS_VERSION" \
    "elfutils-libelf-devel:$AZURELINUX_ELFUTILS_LIBELF_DEVEL_VERSION" \
    "gcc:$AZURELINUX_GCC_VERSION" \
    "gcc-c++:$AZURELINUX_GCC_VERSION" \
    "kernel-devel:$AZURELINUX_KERNEL_VERSION" \
    "kernel-hwe-drivers-gpu:$AZURELINUX_KERNEL_HWE_DRIVERS_GPU_VERSION" \
    "make:$AZURELINUX_MAKE_VERSION" \
    "ocl-icd:$AZURELINUX_OCL_ICD_VERSION" \
    "opencl-filesystem:$AZURELINUX_OPENCL_FILESYSTEM_VERSION" \
    "patch:$AZURELINUX_PATCH_VERSION"; do
    package=${package_and_version%%:*}
    expected_version=${package_and_version#*:}
    version=$(rpm -q --queryformat '%{VERSION}-%{RELEASE}' "$package")
    [ "$version" = "$expected_version" ] || {
      echo "$package resolved to $version, expected $expected_version" >&2
      exit 1
    }
  done

  if rpm -q cuda-open-hwe nvidia-imex nvidia-fabricmanager nvlink5 nvlink5-580 \
      >/dev/null 2>&1; then
    echo "R580 A/B image unexpectedly contains another NVIDIA driver or fabric component" >&2
    exit 1
  fi

  for firmware in gsp_ga10x.bin gsp_tu10x.bin; do
    [ -s "/usr/lib/firmware/nvidia/$NVIDIA_DRIVER_VERSION/$firmware" ] || {
      echo "missing NVIDIA $NVIDIA_DRIVER_VERSION firmware: $firmware" >&2
      exit 1
    }
  done

  manifest=/etc/unbounded/nvidia-driver-manifest
  mkdir -p /etc/unbounded
  : > "$manifest"
  NVIDIA_PERSISTENCED_SERVICE_STATE=$(disable_nvidia_vendor_service nvidia-persistenced.service)
  NVIDIA_POWERD_SERVICE_STATE=$(disable_nvidia_vendor_service nvidia-powerd.service)
  printf 'driver_source=%s\ndriver_version=%s\nkernel=%s\npersistenced_service=%s\npowerd_service=%s\n' \
    "$NVIDIA_DRIVER_SOURCE" "$NVIDIA_DRIVER_RPM_VERSION" "$NVIDIA_KVER" \
    "$NVIDIA_PERSISTENCED_SERVICE_STATE" "$NVIDIA_POWERD_SERVICE_STATE" >> "$manifest"

  for module in nvidia nvidia-modeset nvidia-drm nvidia-uvm nvidia-peermem; do
    paths=$(find_nvidia_module "$module")
    path_count=$(printf '%s\n' "$paths" | sed '/^$/d' | wc -l | tr -d ' ')
    [ "$path_count" = 1 ] || {
      echo "expected one $module module for $NVIDIA_KVER, found $path_count: $paths" >&2
      exit 1
    }
    path=$paths
    version=$(modinfo -F version "$path")
    [ "$version" = "$NVIDIA_DRIVER_VERSION" ] || {
      echo "$module version is $version, expected $NVIDIA_DRIVER_VERSION" >&2
      exit 1
    }
    vermagic=$(modinfo -F vermagic "$path")
    case "$vermagic" in
      "$NVIDIA_KVER "*) ;;
      *)
        echo "$module vermagic is $vermagic, expected kernel $NVIDIA_KVER" >&2
        exit 1
        ;;
    esac
    module_key=$(printf '%s' "$module" | tr '-' '_')
    module_sha256=$(sha256sum "$path" | cut -d' ' -f1)
    printf 'module_%s_path=%s\nmodule_%s_sha256=%s\n' \
      "$module_key" "$path" "$module_key" "$module_sha256" >> "$manifest"
  done

  for firmware in \
    "nvidia/$NVIDIA_DRIVER_VERSION/gsp_ga10x.bin" \
    "nvidia/$NVIDIA_DRIVER_VERSION/gsp_tu10x.bin"; do
    modinfo -F firmware "$(find_nvidia_module nvidia)" | grep -Fx "$firmware" >/dev/null || {
      echo "nvidia module does not declare firmware $firmware" >&2
      exit 1
    }
  done
}

install_nvidia_localrepo_driver() {
  : "${AZURELINUX_KERNEL_HWE_DRIVERS_GPU_VERSION:?AZURELINUX_KERNEL_HWE_DRIVERS_GPU_VERSION is required}"
  : "${AZURELINUX_MLNX_OFA_KERNEL_VERSION:?AZURELINUX_MLNX_OFA_KERNEL_VERSION is required}"
  : "${AZURELINUX_MLNX_OFA_KERNEL_HWE_DEVEL_VERSION:?AZURELINUX_MLNX_OFA_KERNEL_HWE_DEVEL_VERSION is required}"
  : "${AZURELINUX_MLNX_OFA_KERNEL_HWE_MODULES_VERSION:?AZURELINUX_MLNX_OFA_KERNEL_HWE_MODULES_VERSION is required}"
  : "${AZURELINUX_OCL_ICD_VERSION:?AZURELINUX_OCL_ICD_VERSION is required}"
  : "${AZURELINUX_OPENCL_FILESYSTEM_VERSION:?AZURELINUX_OPENCL_FILESYSTEM_VERSION is required}"
  : "${AZURELINUX_BINUTILS_VERSION:?AZURELINUX_BINUTILS_VERSION is required}"
  : "${AZURELINUX_ELFUTILS_LIBELF_DEVEL_VERSION:?AZURELINUX_ELFUTILS_LIBELF_DEVEL_VERSION is required}"
  : "${AZURELINUX_GCC_VERSION:?AZURELINUX_GCC_VERSION is required}"
  : "${AZURELINUX_MAKE_VERSION:?AZURELINUX_MAKE_VERSION is required}"
  : "${AZURELINUX_PATCH_VERSION:?AZURELINUX_PATCH_VERSION is required}"
  : "${NVIDIA_DRIVER_VERSION:?NVIDIA_DRIVER_VERSION is required}"
  : "${NVIDIA_DRIVER_RPM_VERSION:?NVIDIA_DRIVER_RPM_VERSION is required}"
  : "${NVIDIA_DKMS_RPM_VERSION:?NVIDIA_DKMS_RPM_VERSION is required}"
  : "${NVIDIA_LOCAL_REPO_URL:?NVIDIA_LOCAL_REPO_URL is required}"
  : "${NVIDIA_LOCAL_REPO_SHA256:?NVIDIA_LOCAL_REPO_SHA256 is required}"
  : "${NVIDIA_LOCAL_REPO_KEY_SHA256:?NVIDIA_LOCAL_REPO_KEY_SHA256 is required}"
  : "${NVIDIA_FABRIC_PROBE_SHA256:?NVIDIA_FABRIC_PROBE_SHA256 is required}"

  retry tdnf install -y \
    "kernel-devel-${AZURELINUX_KERNEL_VERSION}" \
    "kernel-hwe-devel-${AZURELINUX_KERNEL_HWE_VERSION}" \
    "kernel-hwe-drivers-gpu-${AZURELINUX_KERNEL_HWE_DRIVERS_GPU_VERSION}" \
    "mlnx-ofa_kernel-${AZURELINUX_MLNX_OFA_KERNEL_VERSION}" \
    "mlnx-ofa_kernel-hwe-devel-${AZURELINUX_MLNX_OFA_KERNEL_HWE_DEVEL_VERSION}" \
    "mlnx-ofa_kernel-hwe-modules-${AZURELINUX_MLNX_OFA_KERNEL_HWE_MODULES_VERSION}"

  cat > /etc/yum.repos.d/azurelinux-extended.repo <<'EOF'
[azurelinux-official-extended]
name=Azure Linux Official Extended $releasever $basearch
baseurl=https://packages.microsoft.com/azurelinux/$releasever/prod/extended/$basearch
gpgkey=file:///etc/pki/rpm-gpg/MICROSOFT-RPM-GPG-KEY
gpgcheck=1
repo_gpgcheck=1
enabled=1
EOF
  retry tdnf install -y \
    "binutils-${AZURELINUX_BINUTILS_VERSION}" \
    "elfutils-libelf-devel-${AZURELINUX_ELFUTILS_LIBELF_DEVEL_VERSION}" \
    "gcc-${AZURELINUX_GCC_VERSION}" \
    "gcc-c++-${AZURELINUX_GCC_VERSION}" \
    "make-${AZURELINUX_MAKE_VERSION}" \
    "ocl-icd-${AZURELINUX_OCL_ICD_VERSION}" \
    "opencl-filesystem-${AZURELINUX_OPENCL_FILESYSTEM_VERSION}" \
    "patch-${AZURELINUX_PATCH_VERSION}"
  for command in as gcc g++ ld make patch; do
    command -v "$command" >/dev/null 2>&1 || {
      echo "NVIDIA DKMS toolchain command is missing: $command" >&2
      exit 1
    }
  done

  repo_rpm=/tmp/nvidia-driver-local-repo.rpm
  retry curl --fail --location --silent --show-error \
    "$NVIDIA_LOCAL_REPO_URL" --output "$repo_rpm"
  verify_file_sha256 "$repo_rpm" "$NVIDIA_LOCAL_REPO_SHA256"
  # The wrapper bootstraps the repository key, so tdnf cannot authenticate the
  # local-file transaction yet. Install only the hash-pinned wrapper directly,
  # then verify its signature before consuming any repository package.
  rpm --install --nodeps --nosignature "$repo_rpm"

  repo_dir="/var/nvidia-driver-local-repo-azl3-$NVIDIA_DRIVER_VERSION"
  repo_key="$repo_dir/F2192135.pub"
  [ -d "$repo_dir" ] && [ -s "$repo_key" ] || {
    echo "NVIDIA local repository payload is incomplete" >&2
    exit 1
  }
  verify_file_sha256 "$repo_key" "$NVIDIA_LOCAL_REPO_KEY_SHA256"
  rpm --import "$repo_key"

  for package in "$repo_rpm" "$repo_dir"/*.rpm; do
    signature=$(rpm --checksig "$package")
    case "$signature" in
      *"digests signatures OK") ;;
      *)
        echo "NVIDIA package signature verification failed: $signature" >&2
        exit 1
        ;;
    esac
  done

  retry tdnf install -y "dkms-${NVIDIA_DKMS_RPM_VERSION}"
  kmod_rpm="$repo_dir/kmod-nvidia-open-dkms-${NVIDIA_DRIVER_RPM_VERSION}.noarch.rpm"
  [ -s "$kmod_rpm" ] || {
    echo "missing exact NVIDIA DKMS source RPM: $kmod_rpm" >&2
    exit 1
  }
  # NVIDIA's RPM scriptlet builds against discovered kernels and masks errors.
  # Install the already signature-verified source without scriptlets, then run
  # exactly one fail-closed build for the image's pinned HWE kernel.
  rpm --install --nodeps --noscripts "$kmod_rpm"
  retry tdnf install -y "nvidia-open-${NVIDIA_DRIVER_RPM_VERSION}"

  fabric_probe="/usr/src/nvidia-$NVIDIA_DRIVER_VERSION/src/nvidia/src/kernel/gpu/gpu_fabric_probe.c"
  [ -s "$fabric_probe" ] || {
    echo "missing NVIDIA fabric probe source: $fabric_probe" >&2
    exit 1
  }
  verify_file_sha256 "$fabric_probe" "$NVIDIA_FABRIC_PROBE_SHA256"

  dkms add -m nvidia -v "$NVIDIA_DRIVER_VERSION" --rpm_safe_upgrade
  if ! dkms build -m nvidia -v "$NVIDIA_DRIVER_VERSION" \
      -k "$NVIDIA_KVER" --force; then
    cat "/var/lib/dkms/nvidia/$NVIDIA_DRIVER_VERSION/build/make.log" >&2 || true
    exit 1
  fi
  dkms install -m nvidia -v "$NVIDIA_DRIVER_VERSION" \
    -k "$NVIDIA_KVER" --force
  dkms status -m nvidia -v "$NVIDIA_DRIVER_VERSION" -k "$NVIDIA_KVER" |
    grep -F "nvidia/$NVIDIA_DRIVER_VERSION, $NVIDIA_KVER, aarch64: installed" >/dev/null || {
    echo "NVIDIA DKMS did not install for $NVIDIA_KVER" >&2
    exit 1
  }
  depmod "$NVIDIA_KVER"

  nvidia_drm_dependencies=$(modprobe --show-depends \
    --set-version "$NVIDIA_KVER" nvidia_drm)
  for dependency in \
    kernel/drivers/gpu/drm/ttm/ttm.ko \
    kernel/drivers/gpu/drm/drm_ttm_helper.ko; do
    printf '%s\n' "$nvidia_drm_dependencies" | grep -F "$dependency" >/dev/null || {
      echo "nvidia_drm dependency is missing from $NVIDIA_KVER: $dependency" >&2
      exit 1
    }
  done

  verify_nvidia_localrepo_driver

  rpm -e "nvidia-driver-local-repo-azl3-$NVIDIA_DRIVER_VERSION"
  rm -f "$repo_rpm"
}

install_packages() {
if [ "$NVIDIA" = 1 ] && [ "$TARGETARCH" = arm64 ]; then
  : "${AZURELINUX_KERNEL_VERSION:?AZURELINUX_KERNEL_VERSION is required}"
  : "${AZURELINUX_KERNEL_HWE_VERSION:?AZURELINUX_KERNEL_HWE_VERSION is required}"
  : "${AZURELINUX_GRUB_VERSION:?AZURELINUX_GRUB_VERSION is required}"

  if [ "$NVIDIA_DRIVER_SOURCE" = azurelinux ]; then
    : "${AZURELINUX_CUDA_OPEN_HWE_VERSION:?AZURELINUX_CUDA_OPEN_HWE_VERSION is required}"
    cat > /etc/yum.repos.d/azurelinux-nvidia.repo <<'EOF'
[azurelinux-official-nvidia]
name=Azure Linux Official NVIDIA $releasever $basearch
baseurl=https://packages.microsoft.com/azurelinux/$releasever/prod/nvidia/$basearch
gpgkey=file:///etc/pki/rpm-gpg/MICROSOFT-RPM-GPG-KEY
gpgcheck=1
repo_gpgcheck=1
enabled=1
skip_if_unavailable=False
sslverify=1
EOF
  fi
fi

if [ "$NVIDIA" = 1 ] && [ "$TARGETARCH" = arm64 ]; then
  retry tdnf install -y \
    "kernel-${AZURELINUX_KERNEL_VERSION}" \
    "kernel-hwe-${AZURELINUX_KERNEL_HWE_VERSION}"
elif [ "$NVIDIA" = 1 ]; then
  retry tdnf install -y "$KERNEL_PACKAGE"
else
  retry tdnf install -y "$KERNEL_PACKAGE"
fi

retry tdnf install -y \
  shim dracut \
  systemd systemd-udev systemd-networkd systemd-resolved systemd-container dbus \
  cloud-init cloud-utils-growpart gptfdisk e2fsprogs \
  openssh-server sudo shadow-utils \
  ca-certificates curl tar gzip \
  iproute iputils nftables util-linux kmod procps-ng \
  efibootmgr chrony less

if [ "$NVIDIA" = 1 ] && [ "$TARGETARCH" = arm64 ]; then
  retry tdnf install -y \
    "grub2-${AZURELINUX_GRUB_VERSION}" \
    "grub2-efi-binary-${AZURELINUX_GRUB_VERSION}" \
    "grub2-efi-${AZURELINUX_GRUB_VERSION}"
  if [ "$NVIDIA_DRIVER_SOURCE" = nvidia-localrepo ]; then
    NVIDIA_KVER=$(rpm -ql kernel-hwe | sed -n 's#^/boot/vmlinuz-##p' | sort -V | tail -n1)
    [ -n "$NVIDIA_KVER" ] || {
      echo "kernel-hwe must provide a bootable kernel before the NVIDIA DKMS build" >&2
      exit 1
    }
    install_nvidia_localrepo_driver
  else
    retry tdnf install -y "cuda-open-hwe-${AZURELINUX_CUDA_OPEN_HWE_VERSION}"
  fi
elif [ "$NVIDIA" = 1 ]; then
  retry tdnf install -y grub2-efi-binary cuda-open-hwe
else
  retry tdnf install -y grub2-efi-binary
fi

tdnf clean all
}

finish() {
if [ "$NVIDIA" = 1 ] && [ "$TARGETARCH" = arm64 ]; then
  for package_and_version in \
    "kernel:$AZURELINUX_KERNEL_VERSION" \
    "kernel-hwe:$AZURELINUX_KERNEL_HWE_VERSION"; do
    package=${package_and_version%%:*}
    expected_version=${package_and_version#*:}
    version=$(rpm -q --queryformat '%{VERSION}-%{RELEASE}' "$package")
    [ "$version" = "$expected_version" ] || {
      echo "$package resolved to $version, expected $expected_version" >&2
      exit 1
    }
  done

  STANDARD_KVER=$(rpm -ql kernel | sed -n 's#^/boot/vmlinuz-##p' | sort -V | tail -n1)
  NVIDIA_KVER=$(rpm -ql kernel-hwe | sed -n 's#^/boot/vmlinuz-##p' | sort -V | tail -n1)
  [ -n "$STANDARD_KVER" ] && [ -n "$NVIDIA_KVER" ] || {
    echo "both kernel and kernel-hwe must provide a bootable kernel" >&2
    exit 1
  }

  for package in grub2 grub2-efi-binary grub2-efi; do
    version=$(rpm -q --queryformat '%{VERSION}-%{RELEASE}' "$package")
    [ "$version" = "$AZURELINUX_GRUB_VERSION" ] || {
      echo "$package resolved to $version, expected $AZURELINUX_GRUB_VERSION" >&2
      exit 1
    }
  done
  [ -s /usr/lib/grub/arm64-efi/smbios.mod ] || {
    echo "required GRUB module /usr/lib/grub/arm64-efi/smbios.mod is missing" >&2
    exit 1
  }

  if [ "$NVIDIA_DRIVER_SOURCE" = nvidia-localrepo ]; then
    verify_nvidia_localrepo_driver
  else
    driver_version=$(rpm -q --queryformat '%{VERSION}-%{RELEASE}' cuda-open-hwe)
    [ "$driver_version" = "$AZURELINUX_CUDA_OPEN_HWE_VERSION" ] || {
      echo "cuda-open-hwe resolved to $driver_version, expected $AZURELINUX_CUDA_OPEN_HWE_VERSION" >&2
      exit 1
    }
    driver_kernel_version=$(printf '%s' "$AZURELINUX_KERNEL_HWE_VERSION" | tr '-' '.')
    case "$AZURELINUX_CUDA_OPEN_HWE_VERSION" in
      *"_${driver_kernel_version}") ;;
      *)
        echo "cuda-open-hwe is not matched to kernel-hwe $AZURELINUX_KERNEL_HWE_VERSION" >&2
        exit 1
        ;;
    esac
  fi

  install -D -m 0755 /usr/local/share/unbounded/10_azure_nvidia \
    /etc/grub.d/10_azure_nvidia
  install -D -m 0644 /usr/local/share/unbounded/51-azure-nvidia.cfg \
    /etc/default/grub.d/51-azure-nvidia.cfg
  printf 'image_variant=%s\nstandard=%s\nnvidia=%s\n' \
    "$IMAGE_VARIANT" "$STANDARD_KVER" "$NVIDIA_KVER" > /rootfs.kernels
else
  KVER=$(ls /usr/lib/modules)
  case "$KVER" in
    *" "*|"") echo "expected one kernel, found: $KVER" >&2; exit 1 ;;
  esac
  printf 'image_variant=%s\nstandard=%s\n' "$IMAGE_VARIANT" "$KVER" > /rootfs.kernels
fi

# Build a generic initramfs. The image must boot on any machine, so do not
# limit it to the build host's hardware. The GPU driver loads after boot.
while IFS= read -r entry; do
  case "$entry" in
    standard=*|nvidia=*) ;;
    *) continue ;;
  esac
  KVER=${entry#*=}
  dracut --force --no-hostonly --no-hostonly-cmdline \
    --omit-drivers "nvidia nvidia_modeset nvidia_uvm nvidia_drm nvidia_peermem" \
    --kver "$KVER" "/boot/initramfs-$KVER.img"
  chmod 0644 "/boot/vmlinuz-$KVER" "/boot/initramfs-$KVER.img"
done < /rootfs.kernels

if [ "$NVIDIA" = 1 ]; then
  cat > /etc/modprobe.d/blacklist-nouveau.conf <<'EOF'
blacklist nouveau
options nouveau modeset=0
EOF
  cat > /etc/modprobe.d/nvidia-gb.conf <<'EOF'
options nvidia NVreg_RestrictProfilingToAdminUsers=0
options nvidia NVreg_CreateImexChannel0=1
options nvidia NVreg_CoherentGPUMemoryMode=driver
options nvidia NVreg_RegistryDwords="RMBug5172204War=4"
EOF
  cat > /etc/modules-load.d/nvidia.conf <<'EOF'
nvidia
nvidia_modeset
nvidia_uvm
nvidia_drm
EOF
  if [ "$TARGETARCH" = arm64 ]; then
    # Create the device nodes needed by bootstrap, but keep legacy persistence
    # mode off for the GB200 driver A/B.
    cat > /etc/systemd/system/nvidia-device-setup.service <<'EOF'
[Unit]
Description=Initialize NVIDIA device nodes without persistence mode
After=systemd-modules-load.service
ConditionPathExistsGlob=/sys/bus/pci/drivers/nvidia/0*

[Service]
Type=oneshot
ExecStart=/usr/bin/nvidia-modprobe -u -c=0
ExecStart=/usr/bin/nvidia-smi -pm 0

[Install]
WantedBy=multi-user.target
EOF
    systemctl enable nvidia-device-setup.service
    systemctl mask nvidia-persistence-mode.service
  else
    # The unbounded agent checks for this unit. It creates the device nodes
    # and keeps the driver loaded between GPU jobs. It is skipped on machines
    # without a GPU bound to the driver.
    cat > /etc/systemd/system/nvidia-persistence-mode.service <<'EOF'
[Unit]
Description=Enable NVIDIA persistence mode
After=systemd-modules-load.service
ConditionPathExistsGlob=/sys/bus/pci/drivers/nvidia/0*

[Service]
Type=oneshot
RemainAfterExit=yes
ExecStartPre=/usr/bin/nvidia-modprobe -u -c=0
ExecStart=/usr/bin/nvidia-smi -pm 1

[Install]
WantedBy=multi-user.target
EOF
    systemctl enable nvidia-persistence-mode.service
  fi

  if [ "$TARGETARCH" = arm64 ]; then
    mkdir -p /etc/unbounded
    cat > /etc/unbounded/image-inputs <<EOF
architecture=$TARGETARCH
image_variant=$IMAGE_VARIANT
boot_profile=nvidia-hwe
kernel=$AZURELINUX_KERNEL_VERSION
kernel_hwe=$AZURELINUX_KERNEL_HWE_VERSION
kernel_hwe_drivers_gpu=$AZURELINUX_KERNEL_HWE_DRIVERS_GPU_VERSION
grub=$AZURELINUX_GRUB_VERSION
nvidia_driver_source=$NVIDIA_DRIVER_SOURCE
agentbaker_reference=31804b47b7eb6d6999c7ffa6f643590f7b130bab
EOF
    if [ "$NVIDIA_DRIVER_SOURCE" = nvidia-localrepo ]; then
      cat >> /etc/unbounded/image-inputs <<EOF
nvidia_driver=$NVIDIA_DRIVER_RPM_VERSION
nvidia_dkms=$NVIDIA_DKMS_RPM_VERSION
nvidia_local_repo_sha256=$NVIDIA_LOCAL_REPO_SHA256
nvidia_local_repo_key_sha256=$NVIDIA_LOCAL_REPO_KEY_SHA256
nvidia_fabric_probe_sha256=$NVIDIA_FABRIC_PROBE_SHA256
mlnx_ofa_kernel=$AZURELINUX_MLNX_OFA_KERNEL_VERSION
mlnx_ofa_kernel_hwe_devel=$AZURELINUX_MLNX_OFA_KERNEL_HWE_DEVEL_VERSION
mlnx_ofa_kernel_hwe_modules=$AZURELINUX_MLNX_OFA_KERNEL_HWE_MODULES_VERSION
ocl_icd=$AZURELINUX_OCL_ICD_VERSION
opencl_filesystem=$AZURELINUX_OPENCL_FILESYSTEM_VERSION
binutils=$AZURELINUX_BINUTILS_VERSION
elfutils_libelf_devel=$AZURELINUX_ELFUTILS_LIBELF_DEVEL_VERSION
gcc=$AZURELINUX_GCC_VERSION
make=$AZURELINUX_MAKE_VERSION
patch=$AZURELINUX_PATCH_VERSION
nvidia_persistenced_service=$NVIDIA_PERSISTENCED_SERVICE_STATE
nvidia_powerd_service=$NVIDIA_POWERD_SERVICE_STATE
nvidia_device_setup_service=enabled
nvidia_persistence_mode_service=masked
legacy_persistence_mode=off
EOF
    else
      printf 'cuda_open_hwe=%s\n' "$AZURELINUX_CUDA_OPEN_HWE_VERSION" \
        >> /etc/unbounded/image-inputs
    fi
  fi
fi

# Use DHCP on wired interfaces by default. The Metalman installer replaces
# this file with a static configuration for the machine's lease.
cat > /etc/systemd/network/90-unbounded-dhcp.network <<'EOF'
[Match]
Name=en* eth*

[Network]
DHCP=yes
EOF

# Wait for any one link, not every link. Machines often have unused ports.
mkdir -p /etc/systemd/system/systemd-networkd-wait-online.service.d
cat > /etc/systemd/system/systemd-networkd-wait-online.service.d/any.conf <<'EOF'
[Service]
ExecStart=
ExecStart=/usr/lib/systemd/systemd-networkd-wait-online --any --timeout=120
EOF

systemctl enable systemd-networkd.service systemd-resolved.service \
  sshd.service chronyd.service
systemctl disable systemd-homed.service || true
for unit in cloud-init-local.service cloud-init.service cloud-init-network.service \
  cloud-config.service cloud-final.service; do
  if [ -f "/usr/lib/systemd/system/$unit" ]; then
    systemctl enable "$unit"
  fi
done

# Azure Linux firewall services default to DROP policies. Kubernetes and the
# unbounded agent own the node's firewall rules.
systemctl mask iptables.service ip6tables.service nftables.service

# Each machine generates its own machine ID and SSH host keys on first boot.
: > /etc/machine-id
rm -f /etc/ssh/ssh_host_*

tar --create --file=/rootfs.tar --numeric-owner --xattrs \
  --one-file-system \
  --exclude=./rootfs.tar --exclude=./rootfs.kernels \
  --exclude=./proc --exclude=./sys --exclude=./dev \
  --exclude=./run --exclude=./tmp --exclude=./var/cache/tdnf \
  --exclude=./etc/resolv.conf --exclude=./etc/hostname --exclude=./etc/hosts \
  --exclude=./.dockerenv --exclude=./usr/local/bin/build-rootfs \
  -C / .
}

case "${1:-}" in
  install) install_packages ;;
  finish) finish ;;
  *) echo "usage: $0 install|finish" >&2; exit 2 ;;
esac
