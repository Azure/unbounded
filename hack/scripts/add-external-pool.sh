#!/bin/bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

#
# add-external-pool.sh - Create an external VMSS pool attached to the current AKS cluster
#
# This script creates a new Azure VMSS that bootstraps into the currently active
# Kubernetes cluster. It reuses the kubelet managed identity from the cluster's
# system pool and generates customdata via generate-customdata.sh.
#
# Usage:
#   ./add-external-pool.sh --name <vmss-name> --subnet-id <subnet-resource-id> [options]
#
# Required arguments:
#   -n, --name        Name for the new VMSS
#   -s, --subnet-id   Full Azure resource ID of the subnet to attach the VMSS to
#
# Optional arguments:
#   -g, --resource-group   Azure resource group (uses az CLI default if not set)
#   --sku                  VM SKU (default: Standard_D2ads_v6)
#   --count                Number of instances (default: 2)
#   --ilpip                Enable instance-level public IP on each VM
#   --nsg-id <id>          Full resource ID of NSG to attach to VMSS NICs
#   --allowed-host-ports <range>  Port range to allow inbound UDP (e.g. 51820-52999),
#                          creates an ASG and NSG rule (requires --nsg-id)
#   --gateway              Mark this pool as a gateway (adds net.unbounded-kube.io/gateway=true label)
#   --reimage-existing      After deployment, update and reimage the VMSS if it already existed
#   --password <pass>      Admin password (sets auth type to password or all)
#   --ssh-key <key|@path>   SSH public key value, or @<path> to read from file
#   -y, --yes              Skip confirmation prompt
#   -d, --debug            Write all temp files but pause before deploying for inspection
#   --help                 Show this help message
#
# At least one of --password or --ssh-key is required.
#
# usage-end

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"

# Defaults
VMSS_NAME=""
RESOURCE_GROUP=""
SKU="Standard_D2ads_v6"
COUNT=2
SUBNET_ID=""
ILPIP=false
NSG_ID=""
ALLOWED_HOST_PORTS=""
GATEWAY=false
ADMIN_PASSWORD=""
SSH_KEY=""
AUTO_CONFIRM=false
DEBUG_MODE=false
REIMAGE_EXISTING=false

usage() {
    awk 'NR >= 3 { if ($0 ~ /usage-end/) exit; print substr($0, 3)}' "$0"
}

die() {
    echo "Error: $1" >&2
    echo "" >&2
    usage >&2
    exit 1
}

# Parse arguments
while [[ $# -gt 0 ]]; do
    case "$1" in
        -n|--name)
            VMSS_NAME="$2"; shift 2 ;;
        -g|--resource-group)
            RESOURCE_GROUP="$2"; shift 2 ;;
        --sku)
            SKU="$2"; shift 2 ;;
        --count)
            COUNT="$2"; shift 2 ;;
        -s|--subnet-id)
            SUBNET_ID="$2"; shift 2 ;;
        --ilpip)
            ILPIP=true; shift ;;
        --nsg-id)
            NSG_ID="$2"; shift 2 ;;
        --allowed-host-ports)
            ALLOWED_HOST_PORTS="$2"; shift 2 ;;
        --gateway)
            GATEWAY=true; shift ;;
        --reimage-existing)
            REIMAGE_EXISTING=true; shift ;;
        --password)
            ADMIN_PASSWORD="$2"; shift 2 ;;
        --ssh-key)
            SSH_KEY="$2"; shift 2 ;;
        -y|--yes)
            AUTO_CONFIRM=true; shift ;;
        -d|--debug)
            DEBUG_MODE=true; shift ;;
        --help)
            usage ;;
        *)
            die "Unknown argument: $1" ;;
    esac
done

# Validate required arguments
[[ -n "$VMSS_NAME" ]] || die "--name is required"
[[ -n "$SUBNET_ID" ]] || die "--subnet-id is required"
[[ -n "$ADMIN_PASSWORD" || -n "$SSH_KEY" ]] || die "at least one of --password or --ssh-key is required"

# Validate VMSS name contains only a-z0-9
if [[ ! "$VMSS_NAME" =~ ^[a-z0-9]+$ ]]; then
    die "--name must contain only lowercase letters and digits (a-z0-9)"
fi
RANDOM_ID="$(shuf -i 10000000-99999999 -n 1)"
HOSTNAME_PREFIX="ext-${VMSS_NAME}-${RANDOM_ID}-vmss"

# Build the resource group flag (only if explicitly provided)
RG_FLAG=()
if [[ -n "$RESOURCE_GROUP" ]]; then
    RG_FLAG=(-g "$RESOURCE_GROUP")
fi

# Check if the VMSS already exists and reuse its computerNamePrefix
VMSS_EXISTED=false
EXISTING_PREFIX="$(az vmss show "${RG_FLAG[@]}" -n "$VMSS_NAME" \
    --query 'virtualMachineProfile.osProfile.computerNamePrefix' -o tsv 2>/dev/null || true)"
if [[ -n "$EXISTING_PREFIX" ]]; then
    echo "  Reusing existing VMSS computerNamePrefix: $EXISTING_PREFIX"
    HOSTNAME_PREFIX="$EXISTING_PREFIX"
    VMSS_EXISTED=true
fi

echo "==> Resolving kubelet managed identity from system pool..."

# Extract fields from the providerID of a system node (same technique as generate-customdata.sh)
PROVIDER_ID="$(kubectl get nodes -l kubernetes.azure.com/mode=system \
    --template '{{ (index .items 0).spec.providerID }}')"
SUBSCRIPTION_ID="$(echo "$PROVIDER_ID" | awk -F'/+' '{print $3}')"
CLUSTER_RG="$(echo "$PROVIDER_ID" | awk -F'/+' '{print $5}')"
SYSTEM_VMSS="$(echo "$PROVIDER_ID" | awk -F'/+' '{print $9}')"

echo "  System VMSS: $SYSTEM_VMSS (resource group: $CLUSTER_RG)"

# Get the kubelet managed identity from the system VMSS
IDENTITY_ID="$(az vmss show -g "$CLUSTER_RG" -n "$SYSTEM_VMSS" \
    --query 'identity.userAssignedIdentities | keys(@) | [0]' -o tsv)"
[[ -n "$IDENTITY_ID" ]] || die "Could not resolve kubelet managed identity from system VMSS $SYSTEM_VMSS"
echo "  Kubelet identity: $IDENTITY_ID"

if [[ -n "$NSG_ID" ]]; then
    echo "  NSG: $NSG_ID"
fi
if [[ -n "$ALLOWED_HOST_PORTS" && -z "$NSG_ID" ]]; then
    die "--allowed-host-ports requires --nsg-id"
fi

echo "==> Generating customdata via generate-customdata.sh..."

umask 077
TEMP_DIR="$(mktemp -d /tmp/pool-deploy-XXXXXX)"
if [[ "$DEBUG_MODE" == true ]]; then
    echo "==> Debug mode: temp directory preserved at $TEMP_DIR"
    trap '' EXIT
else
    trap 'rm -rf "$TEMP_DIR"' EXIT
fi

CUSTOMDATA_FILE="${TEMP_DIR}/customdata.yaml"
# Pass NSG name as second arg if --nsg-id was specified
NSG_NAME_ARG=""
if [[ -n "$NSG_ID" ]]; then
    NSG_NAME_ARG="$(echo "$NSG_ID" | awk -F'/' '{print $NF}')"
fi
EXTRA_LABELS=""
if [[ "$GATEWAY" == true ]]; then
    EXTRA_LABELS=",net.unbounded-kube.io/gateway=true"
fi
"${SCRIPT_DIR}/generate-customdata.sh" "$VMSS_NAME" "$NSG_NAME_ARG" "$EXTRA_LABELS" > "$CUSTOMDATA_FILE"

# Base64-encode the customdata for the Bicep template
CUSTOMDATA_B64_FILE="${TEMP_DIR}/customdata.b64"
base64 -w0 < "$CUSTOMDATA_FILE" > "$CUSTOMDATA_B64_FILE"

echo "  Customdata generated and base64-encoded"

echo "  Hostname prefix: $HOSTNAME_PREFIX"

# Resolve SSH public key: validate format and prepare for jq
# SSH_KEY_FILE is set when reading from a file (used with jq --rawfile)
# SSH_PUBLIC_KEY is set when given as a literal value (used with jq --arg)
SSH_PUBLIC_KEY=""
SSH_KEY_FILE=""
if [[ -n "$SSH_KEY" ]]; then
    if [[ "$SSH_KEY" == @* ]]; then
        SSH_KEY_FILE="${SSH_KEY#@}"
        SSH_KEY_FILE="${SSH_KEY_FILE/#\~/$HOME}"
        [[ -f "$SSH_KEY_FILE" ]] || die "SSH key file not found: $SSH_KEY_FILE"
        head -c 20 "$SSH_KEY_FILE" | grep -qE '^ssh-(rsa|ed25519) ' \
            || die "SSH key file does not start with ssh-rsa or ssh-ed25519: $SSH_KEY_FILE"
    else
        re='^ssh-(rsa|ed25519) '
        [[ "$SSH_KEY" =~ $re ]] \
            || die "SSH key value does not start with ssh-rsa or ssh-ed25519"
        SSH_PUBLIC_KEY="$SSH_KEY"
    fi
fi

# ------------------------------------------------------------------
# Confirmation
# ------------------------------------------------------------------
echo ""
echo "==> Deployment plan for VMSS '$VMSS_NAME':"
echo "    Resource group:     ${RESOURCE_GROUP:-(az CLI default)}"
echo "    VM SKU:             $SKU"
echo "    Instance count:     $COUNT"
echo "    Subnet:             $SUBNET_ID"
echo "    Hostname prefix:    $HOSTNAME_PREFIX"
echo "    Public IP per VM:   $ILPIP"
echo "    Kubelet identity:   $IDENTITY_ID"
echo "    NSG:                ${NSG_ID:-(none)}"
echo "    Allowed host ports: ${ALLOWED_HOST_PORTS:-(none)}"
echo "    Auth:               $(if [[ -n "$ADMIN_PASSWORD" && -n "$SSH_KEY" ]]; then echo 'password + ssh'; elif [[ -n "$ADMIN_PASSWORD" ]]; then echo 'password'; else echo 'ssh'; fi)"
echo ""

if [[ "$AUTO_CONFIRM" != true ]]; then
    read -erp "Proceed with deployment? [y/N] " confirm
    if [[ ! "$confirm" =~ ^[Yy]$ ]]; then
        echo "Aborted."
        exit 0
    fi
fi

echo "==> Building parameters file and deploying VMSS '$VMSS_NAME' (sku=$SKU, count=$COUNT)..."

BICEP_TEMPLATE="${SCRIPT_DIR}/templates/external-vmss.bicep"
PARAMS_FILE="${TEMP_DIR}/parameters.json"

# Build the parameters JSON object using files to avoid ARG_MAX limits
# Use --rawfile for customdata (large) and SSH key when provided via file
SSH_JQ_FLAG=(--arg sshPublicKey "$SSH_PUBLIC_KEY")
if [[ -n "$SSH_KEY_FILE" ]]; then
    SSH_JQ_FLAG=(--rawfile sshPublicKey "$SSH_KEY_FILE")
fi

jq -n \
    --arg vmssName "$VMSS_NAME" \
    --arg vmSku "$SKU" \
    --argjson instanceCount "$COUNT" \
    --arg subnetId "$SUBNET_ID" \
    --arg identityId "$IDENTITY_ID" \
    --rawfile customDataBase64 "$CUSTOMDATA_B64_FILE" \
    --arg computerNamePrefix "$HOSTNAME_PREFIX" \
    --argjson enablePublicIPPerVM "$ILPIP" \
    --arg nsgId "$NSG_ID" \
    --arg allowedHostPorts "$ALLOWED_HOST_PORTS" \
    --arg adminPassword "$ADMIN_PASSWORD" \
    "${SSH_JQ_FLAG[@]}" \
    '{
        "$schema": "https://schema.management.azure.com/schemas/2019-04-01/deploymentParameters.json#",
        "contentVersion": "1.0.0.0",
        "parameters": {
            "vmssName": {"value": $vmssName},
            "vmSku": {"value": $vmSku},
            "instanceCount": {"value": $instanceCount},
            "subnetId": {"value": $subnetId},
            "identityId": {"value": $identityId},
            "customDataBase64": {"value": $customDataBase64},
            "computerNamePrefix": {"value": $computerNamePrefix},
            "enablePublicIPPerVM": {"value": $enablePublicIPPerVM},
            "nsgId": {"value": $nsgId},
            "allowedHostPorts": {"value": $allowedHostPorts},
            "adminPassword": {"value": $adminPassword},
            "sshPublicKey": {"value": $sshPublicKey}
        }
    }' > "$PARAMS_FILE"

if [[ "$DEBUG_MODE" == true ]]; then
    echo ""
    echo "==> Debug mode: dropping into shell in $TEMP_DIR"
    echo "    Exit the shell (Ctrl-D or 'exit') to continue."
    ls -lA "$TEMP_DIR"
    echo ""
    (cd "$TEMP_DIR" && PS1="[debug \W]\$ " "$SHELL" -i) || true
    echo ""
    read -erp "Continue with deployment? [y/N] " confirm
    if [[ ! "$confirm" =~ ^[Yy]$ ]]; then
        echo "Aborted. Temp files preserved at $TEMP_DIR"
        exit 0
    fi
fi

az deployment group create \
    "${RG_FLAG[@]}" \
    --name "deploy-${VMSS_NAME}-$(date +%Y%m%d%H%M%S)" \
    --template-file "$BICEP_TEMPLATE" \
    --parameters "@${PARAMS_FILE}"

echo "==> VMSS '$VMSS_NAME' deployed successfully."
echo "    Instances will bootstrap into the cluster on first boot."

# ------------------------------------------------------------------
# Reimage pre-existing VMSS if requested
# ------------------------------------------------------------------
if [[ "$REIMAGE_EXISTING" == true && "$VMSS_EXISTED" == true ]]; then
    echo ""
    echo "==> Reimaging pre-existing VMSS '$VMSS_NAME'..."
    echo "  Updating instances..."
    az vmss update-instances "${RG_FLAG[@]}" -n "$VMSS_NAME" --instance-ids '*'
    echo "  Reimaging (no-wait)..."
    az vmss reimage "${RG_FLAG[@]}" -n "$VMSS_NAME" --instance-ids '*' --no-wait
    echo "==> Reimage initiated for VMSS '$VMSS_NAME'."
fi
