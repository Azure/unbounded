#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

#
# gpu-vm.sh - Manage Azure GPU VMs with NVIDIA drivers.
#
# Usage:
#   ./hack/agent/azure-gpu-vm/gpu-vm.sh <command> [OPTIONS]
#
# Commands:
#   create         Create a GPU VM
#   setup-nvidia   Install NVIDIA drivers on the VM and reboot
#   delete         Delete the GPU VM and its resource group
#
# Global Options:
#   -s, --subscription <id>   Subscription ID (default: $AZURE_SUBSCRIPTION_ID)
#   -g, --resource-group <rg> Resource group name (default: unbounded-gpu-vm-rg)
#   -h, --help                Show this help message
#
# Create Options:
#   -n, --name <name>         VM name (default: unbounded-gpu-vm)
#   -l, --location <region>   Azure region (default: eastus)
#   -z, --size <sku>          VM SKU (default: Standard_NC6s_v3)
#   -i, --image <image>       OS image (default: Ubuntu2204)
#   -u, --admin-user <user>   Admin username (default: azureuser)
#
# Delete Options:
#   -y, --yes                 Skip confirmation prompt
#
# Examples:
#   # Create with defaults (NC6s_v3, eastus)
#   ./hack/agent/azure-gpu-vm/gpu-vm.sh create
#
#   # Install NVIDIA drivers on the VM (uses --size to select driver)
#   ./hack/agent/azure-gpu-vm/gpu-vm.sh setup-nvidia
#
#   # Create with a different SKU and region
#   ./hack/agent/azure-gpu-vm/gpu-vm.sh create --size Standard_NC24ads_A100_v4 --location eastus2
#
#   # Delete without confirmation
#   ./hack/agent/azure-gpu-vm/gpu-vm.sh delete -y

set -euo pipefail

# --------------------------------------------------------------------------- #
# Defaults
# --------------------------------------------------------------------------- #
SUBSCRIPTION="${AZURE_SUBSCRIPTION_ID:-}"
RESOURCE_GROUP="unbounded-gpu-vm-rg"
VM_NAME="unbounded-gpu-vm"
LOCATION="eastus"
VM_SIZE="Standard_NC6s_v3"
IMAGE="Ubuntu2204"
ADMIN_USER="azureuser"
SKIP_CONFIRM=false

# --------------------------------------------------------------------------- #
# Helpers
# --------------------------------------------------------------------------- #
usage() {
    cat <<'EOF'
Usage:
  gpu-vm.sh <command> [OPTIONS]

Commands:
  create         Create a GPU VM
  setup-nvidia   Install NVIDIA drivers on the VM and reboot
  delete         Delete the GPU VM and its resource group

Global Options:
  -s, --subscription <id>   Subscription ID (default: $AZURE_SUBSCRIPTION_ID)
  -g, --resource-group <rg> Resource group name (default: unbounded-gpu-vm-rg)
  -h, --help                Show this help message

Create Options:
  -n, --name <name>         VM name (default: unbounded-gpu-vm)
  -l, --location <region>   Azure region (default: eastus)
  -z, --size <sku>          VM SKU (default: Standard_NC6s_v3)
  -i, --image <image>       OS image (default: Ubuntu2204)
  -u, --admin-user <user>   Admin username (default: azureuser)

Delete Options:
  -y, --yes                 Skip confirmation prompt

Examples:
  gpu-vm.sh create
  gpu-vm.sh setup-nvidia
  gpu-vm.sh create --size Standard_NC24ads_A100_v4 --location eastus2
  gpu-vm.sh delete -y
EOF
    exit 0
}

die() { echo "ERROR: $*" >&2; exit 1; }

# --------------------------------------------------------------------------- #
# Subcommand: create
# --------------------------------------------------------------------------- #
cmd_create() {
    command -v az >/dev/null 2>&1 || die "'az' CLI is required but not found in PATH"
    command -v jq >/dev/null 2>&1 || die "'jq' is required but not found in PATH"
    [[ -n "${SUBSCRIPTION}" ]] || die "Subscription ID is required. Set AZURE_SUBSCRIPTION_ID or use --subscription."

    echo "==> Configuration"
    echo "    Subscription:    ${SUBSCRIPTION}"
    echo "    Resource Group:  ${RESOURCE_GROUP}"
    echo "    VM Name:         ${VM_NAME}"
    echo "    Location:        ${LOCATION}"
    echo "    VM Size:         ${VM_SIZE}"
    echo "    Image:           ${IMAGE}"
    echo "    Admin User:      ${ADMIN_USER}"
    echo ""

    # --- Resource group ---
    echo "==> Creating resource group '${RESOURCE_GROUP}' in '${LOCATION}'..."
    az group create \
        --subscription "${SUBSCRIPTION}" \
        --name "${RESOURCE_GROUP}" \
        --location "${LOCATION}" \
        --output none

    # --- VM ---
    echo "==> Creating VM '${VM_NAME}' (${VM_SIZE})..."
    az vm create \
        --subscription "${SUBSCRIPTION}" \
        --resource-group "${RESOURCE_GROUP}" \
        --name "${VM_NAME}" \
        --location "${LOCATION}" \
        --size "${VM_SIZE}" \
        --image "${IMAGE}" \
        --admin-username "${ADMIN_USER}" \
        --generate-ssh-keys \
        --public-ip-sku Standard \
        --output table

    # --- Summary ---
    local public_ip
    public_ip=$(az vm list-ip-addresses \
        --subscription "${SUBSCRIPTION}" \
        --resource-group "${RESOURCE_GROUP}" \
        --name "${VM_NAME}" \
        --query "[0].virtualMachine.network.publicIpAddresses[0].ipAddress" \
        --output tsv 2>/dev/null)

    echo ""
    echo "==> VM ready"
    echo "    Name:       ${VM_NAME}"
    echo "    Size:       ${VM_SIZE}"
    echo "    Public IP:  ${public_ip}"
    echo "    SSH:        ssh ${ADMIN_USER}@${public_ip}"
}

# --------------------------------------------------------------------------- #
# Subcommand: setup-nvidia
# --------------------------------------------------------------------------- #
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

cmd_setup-nvidia() {
    command -v az >/dev/null 2>&1 || die "'az' CLI is required but not found in PATH"
    [[ -n "${SUBSCRIPTION}" ]] || die "Subscription ID is required. Set AZURE_SUBSCRIPTION_ID or use --subscription."

    local driver_script="${SCRIPT_DIR}/install-nvidia-drivers.sh"
    [[ -f "${driver_script}" ]] || die "Driver install script not found: ${driver_script}"

    echo "==> Installing NVIDIA drivers on VM '${VM_NAME}'"
    echo "    VM SKU:          ${VM_SIZE}"
    echo "    Resource Group:  ${RESOURCE_GROUP}"
    echo ""

    echo "==> Uploading and executing driver install script..."
    local result
    result=$(az vm run-command invoke \
        --subscription "${SUBSCRIPTION}" \
        --resource-group "${RESOURCE_GROUP}" \
        --name "${VM_NAME}" \
        --command-id RunShellScript \
        --scripts @"${driver_script}" \
        --parameters "${VM_SIZE}" \
        --output json)

    local stdout stderr
    stdout=$(echo "${result}" | jq -r '.value[] | select(.code=="ProvisioningState/succeeded") | .message' 2>/dev/null || true)
    if [[ -n "${stdout}" ]]; then
        echo "${stdout}"
    else
        echo "${result}"
    fi

    echo ""
    echo "==> Driver installation initiated. The VM will reboot automatically."
    echo "    After reboot, SSH into the VM and verify with:"
    echo "      ls -la /dev/nvidia*"
}

# --------------------------------------------------------------------------- #
# Subcommand: delete
# --------------------------------------------------------------------------- #
cmd_delete() {
    command -v az >/dev/null 2>&1 || die "'az' CLI is required but not found in PATH"
    [[ -n "${SUBSCRIPTION}" ]] || die "Subscription ID is required. Set AZURE_SUBSCRIPTION_ID or use --subscription."

    if [[ "${SKIP_CONFIRM}" != true ]]; then
        echo "This will delete resource group '${RESOURCE_GROUP}' and ALL resources in it."
        read -r -p "Are you sure? [y/N] " response
        if [[ ! "${response}" =~ ^[Yy]$ ]]; then
            echo "Aborted."
            exit 0
        fi
    fi

    echo "==> Deleting resource group '${RESOURCE_GROUP}'..."
    az group delete \
        --subscription "${SUBSCRIPTION}" \
        --name "${RESOURCE_GROUP}" \
        --yes \
        --no-wait

    echo "==> Deletion initiated (running in background)."
}

# --------------------------------------------------------------------------- #
# Main: parse subcommand then flags
# --------------------------------------------------------------------------- #
if [[ $# -eq 0 ]]; then
    usage
fi

COMMAND="$1"; shift

case "${COMMAND}" in
    create|setup-nvidia|delete) ;;
    -h|--help)     usage ;;
    *)             die "Unknown command: ${COMMAND}. Use 'create', 'setup-nvidia', or 'delete'." ;;
esac

while [[ $# -gt 0 ]]; do
    case "$1" in
        -s|--subscription)   SUBSCRIPTION="$2"; shift 2 ;;
        -g|--resource-group) RESOURCE_GROUP="$2"; shift 2 ;;
        -n|--name)           VM_NAME="$2"; shift 2 ;;
        -l|--location)       LOCATION="$2"; shift 2 ;;
        -z|--size)           VM_SIZE="$2"; shift 2 ;;
        -i|--image)          IMAGE="$2"; shift 2 ;;
        -u|--admin-user)     ADMIN_USER="$2"; shift 2 ;;
        -y|--yes)            SKIP_CONFIRM=true; shift ;;
        -h|--help)           usage ;;
        *)                   die "Unknown option: $1" ;;
    esac
done

"cmd_${COMMAND}"
