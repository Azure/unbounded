#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

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
#   Fabric Manager service. Without it, CUDA initialisation fails with error
#   802 (SYSTEM_DRIVER_MISMATCH). The script installs and enables it when the
#   SKU map indicates it is needed.
#
# Persistence Mode:
#   A systemd oneshot service is installed to run `nvidia-smi -pm 1` on boot,
#   keeping the driver loaded even when no GPU processes are active.
#
# After a successful install the VM must be rebooted for the kernel module to
# load and /dev/nvidia* devices to appear.
#
# Usage:
#   sudo ./install-nvidia-drivers.sh <vm-sku>
#
# Examples:
#   sudo ./install-nvidia-drivers.sh Standard_NC6s_v3         # V100 -> proprietary 580
#   sudo ./install-nvidia-drivers.sh Standard_NC24ads_A100_v4 # A100 -> open 580
#   sudo ./install-nvidia-drivers.sh Standard_ND96isr_H100_v5 # H100 -> open 580

set -euo pipefail

# --------------------------------------------------------------------------- #
# SKU -> driver configuration map
#
# Maps Azure VM SKU family prefixes to their GPU generation, driver flavour,
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
# Boot without any MOK enrolment.
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
# Fabric Manager (if applicable) has initialised.
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

# --------------------------------------------------------------------------- #
# Main
# --------------------------------------------------------------------------- #
[[ $# -ge 1 ]] || die "Usage: $0 <vm-sku>"
[[ $(id -u) -eq 0 ]] || die "This script must be run as root (use sudo)."

VM_SKU="$1"
log "VM SKU: ${VM_SKU}"

read -r GPU_NAME DRIVER_TYPE DRIVER_VERSION NEEDS_FABRIC <<< "$(resolve_sku "${VM_SKU}")"
log "Detected GPU: ${GPU_NAME}, driver type: ${DRIVER_TYPE}, driver version: ${DRIVER_VERSION}, fabric manager: ${NEEDS_FABRIC}"

if is_secure_boot; then
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
# ND A100). Without it, CUDA initialisation fails with error 802.
if [[ "${NEEDS_FABRIC}" == "yes" ]]; then
    install_fabric_manager "${DRIVER_VERSION}"
fi

# Enable GPU persistence mode on boot. This keeps the driver loaded even when
# no GPU processes are running, which avoids cold-start latency and is required
# for some multi-GPU configurations.
install_persistence_mode_service

log "Driver installation complete."
log ""
log "A reboot is required for the kernel module to load."
log "After reboot, verify with:"
log "  ls -la /dev/nvidia*"
log "  nvidia-smi"
log ""
log "Rebooting now..."
shutdown -r +0
