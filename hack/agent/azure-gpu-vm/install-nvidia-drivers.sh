#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0

#
# install-nvidia-drivers.sh - Install NVIDIA GPU drivers on an Azure GPU VM.
#
# This script is designed to be executed on the VM itself (e.g. via
# `az vm run-command invoke` or over SSH). It accepts the Azure VM SKU as its
# sole argument and installs the appropriate NVIDIA driver packages.
#
# Driver strategy:
#   - Older GPUs (V100, P40, P100, M60, K80, T4) use the proprietary NVIDIA
#     kernel module (nvidia-dkms-<ver> / libnvidia-compute-<ver>).
#   - Newer GPUs (A100, A10, H100, H200) use NVIDIA's open kernel module.
#
# Secure Boot:
#   When Secure Boot is enabled (common on Azure), DKMS-built kernel modules
#   are unsigned and will be rejected by the kernel. In that case the script
#   falls back to the pre-built, pre-signed linux-modules-nvidia packages that
#   Canonical ships for the running Azure kernel.
#
# Fabric Manager:
#   NVSwitch-based multi-GPU systems (H100, H200, ND A100) require the NVIDIA
#   Fabric Manager service. Without it, CUDA initialization fails with error
#   802 (SYSTEM_DRIVER_MISMATCH). The script installs and enables it when the
#   SKU map indicates it is needed.
#
# Persistence Mode:
#   A systemd oneshot service is installed to run `nvidia-smi -pm 1` on boot,
#   keeping the driver loaded even when no GPU processes are active.
#
# GPUDirect Storage (optional, --gds):
#   Installs nvidia-fs-dkms from NVIDIA's CUDA apt repository and loads the
#   nvidia-fs module at boot through /etc/modules-load.d/nvidia-fs.conf. The
#   CUDA repository is pinned so that only nvidia-fs and the dkms version it
#   requires come from it; the driver keeps coming from Ubuntu. GPUDirect
#   Storage and dma-buf export need the open kernel module, and the DKMS build
#   needs the driver sources in /usr/src, so --gds requires an open SKU and
#   Secure Boot disabled. nvidia-fs alone does not enable GPUDirect reads from
#   local NVMe on a stock kernel; see docs/content/reference/gpu/nvidia.md.
#
# After a successful install the VM must be rebooted for the kernel module to
# load and /dev/nvidia* devices to appear.
#
# Usage:
#   sudo ./install-nvidia-drivers.sh <vm-sku> [--gds]
#
# Examples:
#   sudo ./install-nvidia-drivers.sh Standard_NC6s_v3         # V100 -> proprietary 580
#   sudo ./install-nvidia-drivers.sh Standard_NC24ads_A100_v4 # A100 -> open 580
#   sudo ./install-nvidia-drivers.sh Standard_ND96isr_H100_v5 # H100 -> open 580
#   sudo ./install-nvidia-drivers.sh Standard_ND96isr_H100_v5 --gds # plus nvidia-fs

set -euo pipefail

# --------------------------------------------------------------------------- #
# SKU -> driver configuration map
#
# Maps Azure VM SKU family prefixes to their GPU generation, driver flavor,
# and driver version. Add new entries here as Azure introduces new GPU SKUs.
#
# Format: SKU_PATTERN:GPU_NAME:DRIVER_TYPE:DRIVER_VERSION:NEEDS_FABRIC_MANAGER
#   DRIVER_TYPE is one of: proprietary | open
#   NEEDS_FABRIC_MANAGER: yes | no - Fabric Manager is required for NVSwitch-
#     based multi-GPU systems (H100, H200, multi-GPU A100 ND-series).
#
# IMPORTANT: More-specific patterns (newer GPUs) must come before generic
# catch-all patterns (older GPUs) because matching stops at the first hit.
# --------------------------------------------------------------------------- #
SKU_MAP=(
    # --- Newer GPUs -> open kernel module ---
    # NC A100 v4 (single-GPU, no NVSwitch)
    "Standard_NC.*A100_v4:A100:open:580:no"
    # ND A100 v4 (8-GPU with NVSwitch)
    "Standard_ND.*A100.*_v4:A100:open:580:yes"
    # ND H100 v5 (8-GPU with NVSwitch)
    "Standard_ND.*H100_v5:H100:open:580:yes"
    # ND H200 v5 (8-GPU with NVSwitch)
    "Standard_ND.*H200_v5:H200:open:580:yes"
    # NV A10 v5 (ads)
    "Standard_NV.*ads_A10_v5:A10:open:580:no"
    # NV A10 v5
    "Standard_NV.*A10.*_v5:A10:open:580:no"

    # --- Older GPUs -> proprietary nvidia-dkms ---
    # NC v3  (V100)
    "Standard_NC.*_v3:V100:proprietary:580:no"
    # ND v2  (V100 8-GPU, NVLink but no NVSwitch - fabric manager not required)
    "Standard_ND40rs_v2:V100:proprietary:580:no"
    # NC T4 v3  (T4)
    "Standard_NC.*T4_v3:T4:proprietary:580:no"
    # NV v3  (M60)
    "Standard_NV.*_v3:M60:proprietary:580:no"
    # NC v2  (P100)
    "Standard_NC.*_v2:P100:proprietary:580:no"
    # NC    (K80)
    "Standard_NC[0-9]*[^v]:K80:proprietary:580:no"
    # ND    (P40)
    "Standard_ND[0-9]*:P40:proprietary:580:no"
    # NV    (M60)  - original NV series
    "Standard_NV[0-9]*[^v]:M60:proprietary:580:no"
)

# --------------------------------------------------------------------------- #
# Helpers
# --------------------------------------------------------------------------- #
die() { echo "ERROR: $*" >&2; exit 1; }

log() { echo "==> $*"; }

# Resolve a VM SKU to its driver config.
# Prints "GPU_NAME DRIVER_TYPE DRIVER_VERSION NEEDS_FABRIC_MANAGER" or exits.
resolve_sku() {
    local sku="$1"
    for entry in "${SKU_MAP[@]}"; do
        IFS=':' read -r pattern gpu driver version fabric <<< "${entry}"
        if [[ "${sku}" =~ ^${pattern} ]]; then
            echo "${gpu} ${driver} ${version} ${fabric}"
            return 0
        fi
    done
    die "Unsupported VM SKU: ${sku}. Update SKU_MAP in this script to add support."
}

# Check whether Secure Boot is enabled.
is_secure_boot() {
    mokutil --sb-state 2>/dev/null | grep -qi "SecureBoot enabled"
}

# --------------------------------------------------------------------------- #
# Install functions
# --------------------------------------------------------------------------- #

# Common prerequisite packages needed for DKMS driver builds.
install_prerequisites() {
    log "Installing build prerequisites..."
    apt-get update -qq
    DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
        linux-headers-"$(uname -r)" \
        dkms \
        build-essential
}

# Install the proprietary NVIDIA driver via DKMS.
install_proprietary_dkms() {
    local gpu="$1" ver="$2"
    log "Installing proprietary NVIDIA driver (nvidia-dkms-${ver}) for ${gpu}..."

    install_prerequisites

    DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
        "nvidia-dkms-${ver}" \
        "libnvidia-compute-${ver}"

    log "Proprietary DKMS driver ${ver} installed for ${gpu}."
}

# Install the open NVIDIA kernel module via DKMS.
install_open_dkms() {
    local gpu="$1" ver="$2"
    log "Installing NVIDIA open kernel module (nvidia-dkms-${ver}-open) for ${gpu}..."

    install_prerequisites

    DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
        "nvidia-dkms-${ver}-open" \
        "libnvidia-compute-${ver}"

    log "Open DKMS driver ${ver} installed for ${gpu}."
}

# Install the pre-built, pre-signed NVIDIA kernel module for the running
# Azure kernel. These packages are signed by Canonical and work with Secure
# Boot without any MOK enrollment.
install_prebuilt_azure() {
    local gpu="$1" ver="$2" driver_type="$3"
    local kernel
    kernel="$(uname -r)"

    local suffix=""
    if [[ "${driver_type}" == "open" ]]; then
        suffix="-open"
    fi

    local pkg="linux-modules-nvidia-${ver}${suffix}-${kernel}"
    log "Secure Boot detected. Installing pre-signed module (${pkg}) for ${gpu}..."

    apt-get update -qq
    if ! DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
            "${pkg}" \
            "nvidia-utils-${ver}" \
            "libnvidia-compute-${ver}" 2>&1; then
        die "Failed to install pre-signed package '${pkg}'. Check that the package exists for your kernel (${kernel}) and driver version (${ver})."
    fi

    log "Pre-signed module ${ver}${suffix} installed for ${gpu} (kernel ${kernel})."
}

# Install NVIDIA Fabric Manager (required for NVSwitch-based multi-GPU systems).
#
# The nvidia-fabricmanager-<ver> package pulls in nvidia-kernel-common-<ver>-server
# which conflicts with the non-server nvidia-kernel-common-<ver>. We work around
# this by downloading the .deb and installing it with --force-depends.
install_fabric_manager() {
    local ver="$1"
    log "Installing NVIDIA Fabric Manager (nvidia-fabricmanager-${ver})..."

    apt-get update -qq

    local tmpdir
    tmpdir=$(mktemp -d)
    (
        cd "${tmpdir}"
        apt-get download "nvidia-fabricmanager-${ver}" 2>&1
        # Use --force-depends to avoid the nvidia-kernel-common-*-server conflict.
        dpkg --force-depends -i nvidia-fabricmanager-"${ver}"*.deb
    )
    rm -rf "${tmpdir}"

    # Enable so it starts on boot.
    systemctl enable nvidia-fabricmanager

    log "Fabric Manager ${ver} installed and enabled."
}

# Configure a systemd service that enables GPU persistence mode after boot.
# nvidia-smi -pm 1 must run after the NVIDIA kernel module is loaded and the
# Fabric Manager (if applicable) has initialized.
install_persistence_mode_service() {
    local wants="multi-user.target"
    local after="multi-user.target"

    # If Fabric Manager is installed, persistence mode should start after it.
    if systemctl list-unit-files nvidia-fabricmanager.service &>/dev/null; then
        after="nvidia-fabricmanager.service"
    fi

    log "Installing nvidia-persistence-mode.service..."
    cat > /etc/systemd/system/nvidia-persistence-mode.service <<UNIT
[Unit]
Description=Enable NVIDIA GPU persistence mode
After=${after}

[Service]
Type=oneshot
ExecStart=/usr/bin/nvidia-smi -pm 1
RemainAfterExit=yes

[Install]
WantedBy=${wants}
UNIT

    systemctl daemon-reload
    systemctl enable nvidia-persistence-mode

    log "nvidia-persistence-mode.service installed and enabled."
}

# Print the NVIDIA CUDA apt repository directory for an os-release file and
# machine architecture, for example "ubuntu2404/x86_64".
cuda_repo_path() {
    local os_release="$1" machine="$2"
    local id version_id arch
    # shellcheck disable=SC1090
    id=$(. "${os_release}" && echo "${ID:-}")
    # shellcheck disable=SC1090
    version_id=$(. "${os_release}" && echo "${VERSION_ID:-}")
    [[ "${id}" == "ubuntu" ]] || die "GPUDirect Storage install supports Ubuntu only, found '${id}'."
    [[ "${version_id}" =~ ^[0-9]+\.[0-9]+$ ]] || die "Unexpected Ubuntu VERSION_ID '${version_id}'."
    case "${machine}" in
        x86_64)  arch="x86_64" ;;
        aarch64) arch="sbsa" ;;
        *)       die "GPUDirect Storage install does not support architecture '${machine}'." ;;
    esac
    echo "ubuntu${version_id//./}/${arch}"
}

# Check that the GPUDirect Storage module can be installed for this driver.
check_gds_supported() {
    local driver_type="$1" secure_boot="$2"
    [[ "${driver_type}" == "open" ]] || die "--gds needs the NVIDIA open kernel module; this SKU uses the ${driver_type} driver."
    [[ "${secure_boot}" == "no" ]] || die "--gds builds nvidia-fs with DKMS, which Secure Boot rejects unless you enroll a MOK key. Disable Secure Boot or install nvidia-fs yourself."
}

# Write the apt pin that limits the CUDA repository to nvidia-fs and dkms.
write_cuda_pin() {
    local path="$1"
    cat > "${path}" <<'PIN'
# Written by install-nvidia-drivers.sh --gds. Take only nvidia-fs and the dkms
# version it requires from the CUDA repository; keep the Ubuntu driver.
Package: *
Pin: origin developer.download.nvidia.com
Pin-Priority: 100

Package: nvidia-fs nvidia-fs-dkms dkms
Pin: origin developer.download.nvidia.com
Pin-Priority: 600
PIN
}

# Write the modules-load.d entry that loads nvidia-fs at boot. modprobe also
# loads nvidia, which nvidia-fs depends on.
write_gds_modules_load() {
    local path="$1"
    echo "nvidia-fs" > "${path}"
}

# Install the GPUDirect Storage kernel module (nvidia-fs) through DKMS.
install_gds() {
    local repo
    repo=$(cuda_repo_path /etc/os-release "$(uname -m)")
    log "Installing GPUDirect Storage kernel module (nvidia-fs-dkms) from CUDA repo ${repo}..."

    local tmpdir
    tmpdir=$(mktemp -d)
    curl -fsSL -o "${tmpdir}/cuda-keyring.deb" \
        "https://developer.download.nvidia.com/compute/cuda/repos/${repo}/cuda-keyring_1.1-1_all.deb"
    dpkg -i "${tmpdir}/cuda-keyring.deb"
    rm -rf "${tmpdir}"

    write_cuda_pin /etc/apt/preferences.d/unbounded-cuda-gds
    apt-get update -qq
    DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends nvidia-fs-dkms

    write_gds_modules_load /etc/modules-load.d/nvidia-fs.conf
    dkms status nvidia-fs || true

    log "nvidia-fs installed; it loads at boot."
}

# --------------------------------------------------------------------------- #
# Main
# --------------------------------------------------------------------------- #
main() {
[[ $# -ge 1 ]] || die "Usage: $0 <vm-sku> [--gds]"
VM_SKU="$1"
INSTALL_GDS=no
shift
while [[ $# -gt 0 ]]; do
    case "$1" in
        --gds) INSTALL_GDS=yes ;;
        *)     die "Unknown option: $1" ;;
    esac
    shift
done
[[ $(id -u) -eq 0 ]] || die "This script must be run as root (use sudo)."

log "VM SKU: ${VM_SKU}"

read -r GPU_NAME DRIVER_TYPE DRIVER_VERSION NEEDS_FABRIC <<< "$(resolve_sku "${VM_SKU}")"
log "Detected GPU: ${GPU_NAME}, driver type: ${DRIVER_TYPE}, driver version: ${DRIVER_VERSION}, fabric manager: ${NEEDS_FABRIC}"

SECURE_BOOT=no
if is_secure_boot; then
    SECURE_BOOT=yes
fi
if [[ "${INSTALL_GDS}" == "yes" ]]; then
    check_gds_supported "${DRIVER_TYPE}" "${SECURE_BOOT}"
fi

if [[ "${SECURE_BOOT}" == "yes" ]]; then
    log "Secure Boot is enabled - using pre-signed kernel module packages."
    install_prebuilt_azure "${GPU_NAME}" "${DRIVER_VERSION}" "${DRIVER_TYPE}"
else
    log "Secure Boot is not enabled - using DKMS packages."
    case "${DRIVER_TYPE}" in
        proprietary) install_proprietary_dkms "${GPU_NAME}" "${DRIVER_VERSION}" ;;
        open)        install_open_dkms "${GPU_NAME}" "${DRIVER_VERSION}" ;;
        *)           die "Unknown driver type: ${DRIVER_TYPE}" ;;
    esac
fi

# Install Fabric Manager for NVSwitch-based multi-GPU systems (H100, H200,
# ND A100). Without it, CUDA initialization fails with error 802.
if [[ "${NEEDS_FABRIC}" == "yes" ]]; then
    install_fabric_manager "${DRIVER_VERSION}"
fi

# Enable GPU persistence mode on boot. This keeps the driver loaded even when
# no GPU processes are running, which avoids cold-start latency and is required
# for some multi-GPU configurations.
install_persistence_mode_service

# Install nvidia-fs after the driver so DKMS builds it against the driver's
# sources and the new nv-p2p interface.
if [[ "${INSTALL_GDS}" == "yes" ]]; then
    install_gds
fi

log "Driver installation complete."
log ""
log "A reboot is required for the kernel module to load."
log "After reboot, verify with:"
log "  ls -la /dev/nvidia*"
log "  nvidia-smi"
if [[ "${INSTALL_GDS}" == "yes" ]]; then
    log "  lsmod | grep nvidia_fs"
    log "  ls -la /dev/nvidia-fs*"
    log "  cat /proc/driver/nvidia-fs/stats"
fi
log ""
log "Rebooting now..."
shutdown -r +0
}

# Tests set INSTALL_NVIDIA_DRIVERS_SOURCED=1 to source the helpers without
# running main. Do not test BASH_SOURCE: run-command may pipe the script.
if [[ "${INSTALL_NVIDIA_DRIVERS_SOURCED:-0}" != "1" ]]; then
    main "$@"
fi
