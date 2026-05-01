#!/bin/bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

#
# add-nebius-site.sh - Deploy a complete Nebius site and join it to an AKS cluster
#
# This script creates Nebius VPC networking (network, subnet, route table) and
# one or more VM pools that bootstrap into the currently active Kubernetes cluster.
# It resolves the bootstrap configuration from the cluster automatically, generates
# per-VM cloud-init userdata inline, and creates Kubernetes CRDs (Site, GatewayPool,
# SiteGatewayPoolAssignment).
#
# Usage:
#   ./add-nebius-site.sh --name <site-name> --ipv4-range <cidr> --pod-ipv4-cidr <cidr> --ssh-key <key|@path> [options]
#
# Required arguments:
#   -n, --name              Site name (a-z0-9 only, used as resource prefix)
#   --ipv4-range            IPv4 address range for the VPC (CIDR notation)
#   --pod-ipv4-cidr         Pod IPv4 CIDR for the site
#   --ssh-key <key|@path>   SSH public key value, or @<path> to read from file
#
# Optional arguments:
#   --ipv6-range            IPv6 address range for the VPC (CIDR notation; empty to disable)
#   --pod-ipv6-cidr         Pod IPv6 CIDR for the site (required when --ipv6-range is set)
#   --region                Nebius region (default: eu-north1)
#   --prefix                Resource name prefix (default: site name)
#   --username              Username prefix for VM names (default: output of whoami)
#   --external-gateway-pools  Number of external gateway pools with public IPs (default: 1, 0 to skip)
#   --internal-gateway-pools  Number of internal gateway pools without public IPs (default: 0)
#   --user-pools            Number of user pools (default: 1, 0 to skip)
#   --gpu-pools             Number of GPU pools (default: 0)
#   --compute-platform      Compute VM platform (default: cpu-d3)
#   --compute-preset        Compute VM preset (default: 4vcpu-16gb)
#   --gpu-platform          GPU VM platform (default: gpu-h100-sxm)
#   --gpu-preset            GPU VM preset (default: 1gpu-16vcpu-200gb)
#   --compute-image-family  Compute image family (default: ubuntu24.04-driverless)
#   --gpu-image-family      GPU image family (default: ubuntu24.04-cuda13.0)
#   --gateway-count         VMs per gateway pool (default: 2)
#   --user-count            VMs per user pool (default: 2)
#   --gpu-count             VMs per GPU pool (default: 1)
#   --disk-size             Boot disk size in GiB (default: 256)
#   --password <pass>       Admin password for the ubuntu user
#   -v, --verbose           Print nebius CLI commands to stderr as they execute
#   -y, --yes               Skip confirmation prompt
#   -d, --debug             Write all temp files but pause before deploying for inspection
#   --help                  Show this help message
#
# At least --ssh-key is required. --password is optional.
#
# usage-end

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"

# Defaults
SITE_NAME=""
REGION="eu-north1"
PREFIX=""
USERNAME=""
IPV4_RANGE=""
IPV6_RANGE=""
POD_IPV4_CIDR=""
POD_IPV6_CIDR=""
EXT_GATEWAY_POOLS=1
INT_GATEWAY_POOLS=0
USER_POOLS=1
GPU_POOLS=0
COMPUTE_PLATFORM="cpu-d3"
COMPUTE_PRESET="4vcpu-16gb"
GPU_PLATFORM="gpu-h100-sxm"
GPU_PRESET="1gpu-16vcpu-200gb"
COMPUTE_IMAGE_FAMILY="ubuntu24.04-driverless"
GPU_IMAGE_FAMILY="ubuntu24.04-cuda13.0"
GATEWAY_COUNT=2
USER_COUNT=2
GPU_COUNT=1
DISK_SIZE=256
ADMIN_PASSWORD=""
SSH_KEY=""
AUTO_CONFIRM=false
VERBOSE=false
DEBUG_MODE=false
GATEWAY_POOL_NAMES=()
GATEWAY_POOL_TYPES=()
PRIMARY_SITE_NAME=""

usage() {
    awk 'NR >= 3 { if ($0 ~ /usage-end/) exit; print substr($0, 3)}' "$0"
}

die() {
    echo "Error: $1" >&2
    echo "" >&2
    usage >&2
    exit 1
}

# Wrapper that prints the nebius command to stderr when verbose mode is on
nebius() {
    if [[ "$VERBOSE" == true ]]; then
        echo "+ nebius $*" >&2
    fi
    command nebius "$@"
}

ensure_unmanaged_kube_proxy_daemonset() {
    echo "==> Ensuring kube-proxy-unmanaged DaemonSet in kube-system..."
    if ! kubectl get daemonset -n kube-system kube-proxy >/dev/null 2>&1; then
        echo "  kube-proxy DaemonSet not found in kube-system; skipping kube-proxy-unmanaged creation."
        return
    fi

    kubectl get daemonset -n kube-system kube-proxy -o json | jq '
      .metadata.name = "kube-proxy-unmanaged" |
      .metadata.namespace = "kube-system" |
      del(
        .status,
        .metadata.uid,
        .metadata.resourceVersion,
        .metadata.generation,
        .metadata.creationTimestamp,
        .metadata.managedFields,
        .metadata.annotations."deprecated.daemonset.template.generation",
        .metadata.annotations."addonmanager.kubernetes.io/mode",
        .metadata.labels."addonmanager.kubernetes.io/mode",
        .metadata.labels."kubernetes.azure.com/managedby",
        .metadata.labels."app.kubernetes.io/managed-by"
      ) |
      .spec.selector.matchLabels = {"k8s-app":"kube-proxy-unmanaged"} |
      .spec.template.metadata.labels = ((.spec.template.metadata.labels // {}) + {"k8s-app":"kube-proxy-unmanaged"}) |
      del(
        .spec.template.metadata.annotations."addonmanager.kubernetes.io/mode",
        .spec.template.metadata.labels."addonmanager.kubernetes.io/mode",
        .spec.template.metadata.labels."kubernetes.azure.com/managedby",
        .spec.template.metadata.labels."app.kubernetes.io/managed-by"
      ) |
      .spec.template.spec.affinity = (.spec.template.spec.affinity // {}) |
      .spec.template.spec.affinity.nodeAffinity = (.spec.template.spec.affinity.nodeAffinity // {}) |
      .spec.template.spec.affinity.nodeAffinity.requiredDuringSchedulingIgnoredDuringExecution = {
        "nodeSelectorTerms": [
          {
            "matchExpressions": [
              {
                "key": "kubernetes.azure.com/cluster",
                "operator": "DoesNotExist"
              }
            ]
          }
        ]
      }
    ' | kubectl apply -f -
}

ensure_site_gateway_resources() {
    local site_name="$1"
    local -a node_cidrs=()
    local -a pod_cidrs=()

    # For Nebius sites, the node CIDR is the VPC subnet range
    node_cidrs+=("$IPV4_RANGE")
    [[ -n "$IPV6_RANGE" ]] && node_cidrs+=("$IPV6_RANGE")
    [[ -n "${POD_IPV4_CIDR}" ]] && pod_cidrs+=("${POD_IPV4_CIDR}")
    [[ -n "${POD_IPV6_CIDR}" ]] && pod_cidrs+=("${POD_IPV6_CIDR}")

    echo "==> Ensuring Site '${site_name}' CRD..."
    {
        echo "apiVersion: net.unbounded-cloud.io/v1alpha1"
        echo "kind: Site"
        echo "metadata:"
        echo "  name: ${site_name}"
        echo "  annotations:"
        echo "    net.unbounded-cloud.io/nebius-route-table-id: \"${ROUTE_TABLE_ID}\""
        echo "spec:"
        echo "  nodeCidrs:"
        for cidr in "${node_cidrs[@]}"; do
            echo "    - \"${cidr}\""
        done
        echo "  manageCniPlugin: true"
        echo "  podCidrAssignments:"
        echo "    - assignmentEnabled: true"
        echo "      cidrBlocks:"
        for cidr in "${pod_cidrs[@]}"; do
            echo "        - \"${cidr}\""
        done
    } | kubectl apply -f -

    if [[ "${#GATEWAY_POOL_NAMES[@]}" -gt 0 ]]; then
        echo "==> Ensuring GatewayPool and SiteGatewayPoolAssignment CRDs..."
        for i in "${!GATEWAY_POOL_NAMES[@]}"; do
            local pool_name="${GATEWAY_POOL_NAMES[$i]}"
            local pool_type="${GATEWAY_POOL_TYPES[$i]}"
            cat <<EOF | kubectl apply -f -
apiVersion: net.unbounded-cloud.io/v1alpha1
kind: GatewayPool
metadata:
  name: ${pool_name}
spec:
  type: ${pool_type}
  nodeSelector:
    net.unbounded-cloud.io/agentpool: "${pool_name}"
EOF

            cat <<EOF | kubectl apply -f -
apiVersion: net.unbounded-cloud.io/v1alpha1
kind: SiteGatewayPoolAssignment
metadata:
  name: ${site_name}-${pool_name}
spec:
  enabled: true
  sites:
    - "${site_name}"
  gatewayPools:
    - "${pool_name}"
EOF
        done

        # Peer each new gateway pool with the primary site external gateway pool
        local primary_gw="${PRIMARY_SITE_NAME}extgw1"
        if kubectl get gatewaypool "$primary_gw" >/dev/null 2>&1; then
            echo "==> Ensuring GatewayPoolPeerings to primary pool '${primary_gw}'..."
            for pool_name in "${GATEWAY_POOL_NAMES[@]}"; do
                [[ "$pool_name" == "$primary_gw" ]] && continue
                local first_pool="$primary_gw"
                local second_pool="$pool_name"
                if [[ "$first_pool" > "$second_pool" ]]; then
                    first_pool="$pool_name"
                    second_pool="$primary_gw"
                fi
                local peering_name="${first_pool}-${second_pool}"
                cat <<EOF | kubectl apply -f -
apiVersion: net.unbounded-cloud.io/v1alpha1
kind: GatewayPoolPeering
metadata:
  name: ${peering_name}
spec:
  enabled: true
  gatewayPools:
    - "${primary_gw}"
    - "${pool_name}"
EOF
            done
        else
            echo "  Primary gateway pool ${primary_gw} not found; skipping GatewayPoolPeering."
        fi
    else
        # No local gateway pools -- assign to primary site external gateway pool
        local primary_gw="${PRIMARY_SITE_NAME}extgw1"
        if kubectl get gatewaypool "$primary_gw" >/dev/null 2>&1; then
            echo "==> No local gateway pools; assigning site to primary gateway pool '${primary_gw}'..."
            cat <<EOF | kubectl apply -f -
apiVersion: net.unbounded-cloud.io/v1alpha1
kind: SiteGatewayPoolAssignment
metadata:
  name: ${site_name}-${primary_gw}
spec:
  enabled: true
  sites:
    - "${site_name}"
  gatewayPools:
    - "${primary_gw}"
EOF
        else
            echo "  No local gateway pools and primary gateway pool ${primary_gw} not found; skipping SGPA."
        fi
    fi
}

# ------------------------------------------------------------------
# Parse arguments
# ------------------------------------------------------------------
while [[ $# -gt 0 ]]; do
    case "$1" in
        -n|--name)           SITE_NAME="$2"; shift 2 ;;
        --region)            REGION="$2"; shift 2 ;;
        --prefix)            PREFIX="$2"; shift 2 ;;
        --username)          USERNAME="$2"; shift 2 ;;
        --ipv4-range)        IPV4_RANGE="$2"; shift 2 ;;
        --ipv6-range)        IPV6_RANGE="$2"; shift 2 ;;
        --pod-ipv4-cidr)     POD_IPV4_CIDR="$2"; shift 2 ;;
        --pod-ipv6-cidr)     POD_IPV6_CIDR="$2"; shift 2 ;;
        --external-gateway-pools) EXT_GATEWAY_POOLS="$2"; shift 2 ;;
        --internal-gateway-pools) INT_GATEWAY_POOLS="$2"; shift 2 ;;
        --user-pools)        USER_POOLS="$2"; shift 2 ;;
        --gpu-pools)         GPU_POOLS="$2"; shift 2 ;;
        --compute-platform)  COMPUTE_PLATFORM="$2"; shift 2 ;;
        --compute-preset)    COMPUTE_PRESET="$2"; shift 2 ;;
        --gpu-platform)      GPU_PLATFORM="$2"; shift 2 ;;
        --gpu-preset)        GPU_PRESET="$2"; shift 2 ;;
        --compute-image-family) COMPUTE_IMAGE_FAMILY="$2"; shift 2 ;;
        --gpu-image-family)  GPU_IMAGE_FAMILY="$2"; shift 2 ;;
        --gateway-count)     GATEWAY_COUNT="$2"; shift 2 ;;
        --user-count)        USER_COUNT="$2"; shift 2 ;;
        --gpu-count)         GPU_COUNT="$2"; shift 2 ;;
        --disk-size)         DISK_SIZE="$2"; shift 2 ;;
        --password)          ADMIN_PASSWORD="$2"; shift 2 ;;
        --ssh-key)           SSH_KEY="$2"; shift 2 ;;
        -y|--yes)            AUTO_CONFIRM=true; shift ;;
        -v|--verbose)        VERBOSE=true; shift ;;
        -d|--debug)          DEBUG_MODE=true; shift ;;
        --help)              usage; exit 0 ;;
        *)                   die "Unknown argument: $1" ;;
    esac
done

# Validate required arguments
[[ -n "$SITE_NAME" ]] || die "--name is required"
[[ -n "$IPV4_RANGE" ]] || die "--ipv4-range is required"
[[ -n "$POD_IPV4_CIDR" ]] || die "--pod-ipv4-cidr is required"
[[ -n "$SSH_KEY" ]] || die "--ssh-key is required"
if [[ -n "$IPV6_RANGE" && -z "$POD_IPV6_CIDR" ]]; then
    die "--pod-ipv6-cidr is required when --ipv6-range is set"
fi

if [[ ! "$SITE_NAME" =~ ^[a-z0-9]+$ ]]; then
    die "--name must contain only lowercase letters and digits (a-z0-9)"
fi

# Default prefix to site name
if [[ -z "$PREFIX" ]]; then
    PREFIX="$SITE_NAME"
fi

# Default username from whoami
if [[ -z "$USERNAME" ]]; then
    USERNAME="$(whoami)"
fi

# Resolve SSH public key
SSH_PUBLIC_KEY=""
if [[ "$SSH_KEY" == @* ]]; then
    SSH_KEY_FILE="${SSH_KEY#@}"
    SSH_KEY_FILE="${SSH_KEY_FILE/#\~/$HOME}"
    [[ -f "$SSH_KEY_FILE" ]] || die "SSH key file not found: $SSH_KEY_FILE"
    SSH_PUBLIC_KEY="$(cat "$SSH_KEY_FILE")"
else
    SSH_PUBLIC_KEY="$SSH_KEY"
fi

re='^ssh-(rsa|ed25519) '
[[ "$SSH_PUBLIC_KEY" =~ $re ]] \
    || die "SSH key value does not start with ssh-rsa or ssh-ed25519"

# ------------------------------------------------------------------
# Resolve cluster context
# ------------------------------------------------------------------
echo "==> Resolving cluster context..."

API_SERVER="$(kubectl config view --flatten --minify \
    --template '{{ (index .clusters 0).cluster.server }}' | awk -F'[:/]+' '{print $2}')"
CA_CERT_BASE64="$(kubectl config view --flatten --minify \
    --template '{{ index (index .clusters 0).cluster "certificate-authority-data" }}')"
CLUSTER_DNS="$(kubectl get svc -n kube-system kube-dns \
    -o go-template --template '{{.spec.clusterIP}}')"
KUBERNETES_VERSION="$(kubectl version -o json | jq -r '.serverVersion.gitVersion')"

# Extract fields from the providerID of a system node
PROVIDER_ID="$(kubectl get nodes -l kubernetes.azure.com/mode=system \
    --template '{{ (index .items 0).spec.providerID }}')"
CLUSTER_RG="$(echo "$PROVIDER_ID" | awk -F'/+' '{print $5}')"

# Resolve primary site name from the cluster VNet
SYSTEM_VMSS="$(echo "$PROVIDER_ID" | awk -F'/+' '{print $9}')"
CLUSTER_SUBNET_ID="$(az vmss show -g "$CLUSTER_RG" -n "$SYSTEM_VMSS" \
    --query 'virtualMachineProfile.networkProfile.networkInterfaceConfigurations[0].ipConfigurations[0].subnet.id' -o tsv 2>/dev/null || true)"
if [[ -n "$CLUSTER_SUBNET_ID" ]]; then
    CLUSTER_VNET_NAME="$(echo "$CLUSTER_SUBNET_ID" | awk -F'/' '{for(i=1;i<=NF;i++) if($i=="virtualNetworks") print $(i+1)}')"
    PRIMARY_SITE_NAME="${CLUSTER_VNET_NAME%-vnet}"
else
    # Fallback: try to find the primary site from existing Site CRDs
    PRIMARY_SITE_NAME="$(kubectl get sites -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)"
fi

echo "    API server:         $API_SERVER"
echo "    Kubernetes version: $KUBERNETES_VERSION"
echo "    Cluster RG:         $CLUSTER_RG"
echo "    System VMSS:        $SYSTEM_VMSS"
echo "    Primary site:       ${PRIMARY_SITE_NAME:-(unknown)}"

# ------------------------------------------------------------------
# Resolve existing Nebius networking (read-only lookups)
# ------------------------------------------------------------------
echo ""
echo "==> Checking for existing Nebius networking in region $REGION..."

get_image_by_family() {
    nebius compute image list-public --region "$REGION" --format json \
        | jq --arg family "$1" -r \
            '.items | map(select(.spec.image_family==$family)) | sort_by(.created_at) | reverse | .[0].metadata.id'
}

PUBLIC_POOL_ID="$(nebius vpc pool get-by-name --name default-public-pool --format jsonpath='.metadata.id' 2>/dev/null || true)"
[[ -n "$PUBLIC_POOL_ID" ]] || die "default-public-pool not found in Nebius"

PRIVATE_POOL_ID="$(nebius vpc pool get-by-name --name "${USERNAME}-${PREFIX}-pool" --format jsonpath='.metadata.id' 2>/dev/null || true)"
PRIVATE_POOL_STATUS="existing"
if [[ -z "$PRIVATE_POOL_ID" ]]; then
    PRIVATE_POOL_STATUS="new"
fi

IPV6_POOL_ID=""
IPV6_POOL_STATUS=""
if [[ -n "$IPV6_RANGE" ]]; then
    IPV6_POOL_ID="$(nebius vpc pool get-by-name --name "${USERNAME}-${PREFIX}-pool-v6" --format jsonpath='.metadata.id' 2>/dev/null || true)"
    IPV6_POOL_STATUS="existing"
    if [[ -z "$IPV6_POOL_ID" ]]; then
        IPV6_POOL_STATUS="new"
    fi
fi

NETWORK_ID="$(nebius vpc network get-by-name --name "${USERNAME}-${PREFIX}-network" --format jsonpath='.metadata.id' 2>/dev/null || true)"
NETWORK_STATUS="existing"
if [[ -z "$NETWORK_ID" ]]; then
    NETWORK_STATUS="new"
fi

ROUTE_TABLE_ID="$(nebius vpc route-table get-by-name --name "${USERNAME}-${PREFIX}-route-table" --format jsonpath='.metadata.id' 2>/dev/null || true)"
ROUTE_TABLE_STATUS="existing"
if [[ -z "$ROUTE_TABLE_ID" ]]; then
    ROUTE_TABLE_STATUS="new"
fi

SUBNET_ID="$(nebius vpc subnet get-by-name --name "${USERNAME}-${PREFIX}-subnet" --format jsonpath='.metadata.id' 2>/dev/null || true)"
SUBNET_STATUS="existing"
if [[ -z "$SUBNET_ID" ]]; then
    SUBNET_STATUS="new"
fi

# Read the cloud-provider-nebius service account ID from its K8s Secret
SA_ID="$(kubectl get secret cloud-provider-nebius-credentials -n kube-system \
    -o jsonpath='{.metadata.annotations.net\.unbounded-kube\.io/nebius-service-account-id}' 2>/dev/null || true)"
if [[ -z "$SA_ID" ]]; then
    die "cloud-provider-nebius-credentials Secret not found in kube-system or missing service-account-id annotation. Deploy cloud-provider-nebius first."
fi
echo "    Cloud provider SA:  $SA_ID (from cloud-provider-nebius-credentials Secret)"

GROUP_NAME="${USERNAME}-${PREFIX}-vms"
GROUP_ID="$(nebius iam group get-by-name --name "$GROUP_NAME" --format jsonpath='.metadata.id' 2>/dev/null || true)"
GROUP_STATUS="existing"
if [[ -z "$GROUP_ID" ]]; then
    GROUP_STATUS="new"
fi

# ------------------------------------------------------------------
# Confirmation
# ------------------------------------------------------------------
TOTAL_VMS=$((EXT_GATEWAY_POOLS * GATEWAY_COUNT + INT_GATEWAY_POOLS * GATEWAY_COUNT + USER_POOLS * USER_COUNT + GPU_POOLS * GPU_COUNT))
echo ""
echo "==> Deployment plan for Nebius site '$SITE_NAME':"
echo "    Region:                 $REGION"
echo "    Username:               $USERNAME"
echo "    Prefix:                 $PREFIX"
echo "    IPv4 range:             $IPV4_RANGE"
echo "    IPv6 range:             ${IPV6_RANGE:-(disabled)}"
echo "    Pod IPv4 CIDR:          $POD_IPV4_CIDR"
echo "    Pod IPv6 CIDR:          ${POD_IPV6_CIDR:-(unset)}"
echo "    External gateway pools: $EXT_GATEWAY_POOLS (platform=$COMPUTE_PLATFORM, preset=$COMPUTE_PRESET, count=$GATEWAY_COUNT each)"
echo "    Internal gateway pools: $INT_GATEWAY_POOLS (platform=$COMPUTE_PLATFORM, preset=$COMPUTE_PRESET, count=$GATEWAY_COUNT each)"
echo "    User pools:             $USER_POOLS (platform=$COMPUTE_PLATFORM, preset=$COMPUTE_PRESET, count=$USER_COUNT each)"
echo "    GPU pools:              $GPU_POOLS (platform=$GPU_PLATFORM, preset=$GPU_PRESET, count=$GPU_COUNT each)"
echo "    Disk size:              ${DISK_SIZE} GiB"
echo "    Total VMs:              $TOTAL_VMS"
echo "    VM name pattern:        ${USERNAME}-${PREFIX}<pool>-<seq>"
echo "    Hostname pattern:       ext-${PREFIX}<pool>-<seq>"
echo "    API server:             $API_SERVER"
echo "    Kubernetes version:     $KUBERNETES_VERSION"
echo "    Cluster RG:             $CLUSTER_RG"
echo "    System VMSS:            $SYSTEM_VMSS"
echo "    Primary site:           ${PRIMARY_SITE_NAME:-(unknown)}"
echo ""
echo "    Nebius networking:"
echo "      Private pool:         ${USERNAME}-${PREFIX}-pool (${PRIVATE_POOL_STATUS})${PRIVATE_POOL_ID:+ $PRIVATE_POOL_ID}"
if [[ -n "$IPV6_RANGE" ]]; then
    echo "      IPv6 pool:            ${USERNAME}-${PREFIX}-pool-v6 (${IPV6_POOL_STATUS})${IPV6_POOL_ID:+ $IPV6_POOL_ID}"
fi
echo "      Network:              ${USERNAME}-${PREFIX}-network (${NETWORK_STATUS})${NETWORK_ID:+ $NETWORK_ID}"
echo "      Route table:          ${USERNAME}-${PREFIX}-route-table (${ROUTE_TABLE_STATUS})${ROUTE_TABLE_ID:+ $ROUTE_TABLE_ID}"
echo "      Subnet:               ${USERNAME}-${PREFIX}-subnet (${SUBNET_STATUS})${SUBNET_ID:+ $SUBNET_ID}"
echo ""
echo "    Nebius IAM:"
echo "      Cloud provider SA:    $SA_ID (from Secret)"
echo "      VM group:             ${GROUP_NAME} (${GROUP_STATUS})${GROUP_ID:+ $GROUP_ID}"
echo ""
if [[ -n "$ADMIN_PASSWORD" ]]; then
    echo "    Auth:                   ssh + password"
else
    echo "    Auth:                   ssh"
fi
echo ""

if [[ "$TOTAL_VMS" -eq 0 ]]; then
    die "No VMs to deploy (all pool counts are 0)"
fi

if [[ "$AUTO_CONFIRM" != true ]]; then
    read -erp "Proceed with deployment? [y/N] " confirm
    if [[ ! "$confirm" =~ ^[Yy]$ ]]; then
        echo "Aborted."
        exit 0
    fi
fi

# ------------------------------------------------------------------
# Create Nebius networking (only resources that do not exist yet)
# ------------------------------------------------------------------
echo ""
echo "==> Ensuring Nebius networking..."

if [[ "$PRIVATE_POOL_STATUS" == "new" ]]; then
    echo "    Creating private pool: ${USERNAME}-${PREFIX}-pool"
    PRIVATE_POOL_ID="$(nebius vpc pool create --cidrs "[{\"cidr\": \"${IPV4_RANGE}\"}]" --name "${USERNAME}-${PREFIX}-pool" --version ipv4 --visibility private --format jsonpath='.metadata.id')"
fi
PRIVATE_POOL_LIST="$(jq -cn --arg id "$PRIVATE_POOL_ID" '[{"id": $id}]')"

IPV6_POOL_FLAGS=()
if [[ -n "$IPV6_RANGE" ]]; then
    if [[ "$IPV6_POOL_STATUS" == "new" ]]; then
        echo "    Creating IPv6 pool: ${USERNAME}-${PREFIX}-pool-v6"
        IPV6_POOL_ID="$(nebius vpc pool create --cidrs "[{\"cidr\": \"${IPV6_RANGE}\"}]" --name "${USERNAME}-${PREFIX}-pool-v6" --version ipv6 --visibility private --format jsonpath='.metadata.id')"
    fi
    IPV6_POOL_FLAGS=(--ipv6-private-pools-pools "$(jq -cn --arg id "$IPV6_POOL_ID" '[{"id": $id}]')")
fi

PUBLIC_POOL_LIST="$(jq -cn --arg id "$PUBLIC_POOL_ID" '[{"id": $id}]')"

if [[ "$NETWORK_STATUS" == "new" ]]; then
    echo "    Creating network: ${USERNAME}-${PREFIX}-network"
    NETWORK_ID="$(nebius vpc network create --name "${USERNAME}-${PREFIX}-network" --ipv4-private-pools-pools "$PRIVATE_POOL_LIST" --ipv4-public-pools-pools "$PUBLIC_POOL_LIST" "${IPV6_POOL_FLAGS[@]}" --format jsonpath='.metadata.id')"
fi

if [[ "$ROUTE_TABLE_STATUS" == "new" ]]; then
    echo "    Creating route table: ${USERNAME}-${PREFIX}-route-table"
    ROUTE_TABLE_ID="$(nebius vpc route-table create --name "${USERNAME}-${PREFIX}-route-table" --network-id "$NETWORK_ID" --format jsonpath='.metadata.id')"
    echo "    Adding default egress route to ${USERNAME}-${PREFIX}-route-table"
    nebius vpc route create --name "default-egress" --parent-id $ROUTE_TABLE_ID --destination-cidr "0.0.0.0/0" --next-hop-default-egress-gateway true
fi

if [[ "$SUBNET_STATUS" == "new" ]]; then
    echo "    Creating subnet: ${USERNAME}-${PREFIX}-subnet"
    SUBNET_ID="$(nebius vpc subnet create --name "${USERNAME}-${PREFIX}-subnet" --network-id "$NETWORK_ID" --route-table-id "$ROUTE_TABLE_ID" --format jsonpath='.metadata.id')"
fi

echo "    Network:     ${USERNAME}-${PREFIX}-network ($NETWORK_ID)"
echo "    Route table: ${USERNAME}-${PREFIX}-route-table ($ROUTE_TABLE_ID)"
echo "    Subnet:      ${USERNAME}-${PREFIX}-subnet ($SUBNET_ID)"

# ------------------------------------------------------------------
# Ensure IAM resources (group + permissions for cloud-provider SA)
# ------------------------------------------------------------------
echo ""
echo "==> Ensuring IAM resources..."

if [[ "$GROUP_STATUS" == "new" ]]; then
    echo "    Creating group: $GROUP_NAME"
    GROUP_ID="$(nebius iam group create --name "$GROUP_NAME" --format jsonpath='.metadata.id')"
fi
echo "    Group: $GROUP_NAME ($GROUP_ID)"

# Add cloud-provider service account as a member of the group
echo "    Adding cloud-provider SA to group..."
nebius iam group-membership create --parent-id "$GROUP_ID" --member-id "$SA_ID" 2>/dev/null || true

# Grant the group editor at the project level (VPC resources do not support
# per-resource access permits, so project-level is required)
PROJECT_ID="$(nebius iam group get --id "$GROUP_ID" --format jsonpath='.metadata.parent_id')"
echo "    Granting group editor on project $PROJECT_ID..."
nebius iam access-permit create \
    --parent-id "$GROUP_ID" \
    --resource-id "$PROJECT_ID" \
    --role editor 2>/dev/null || true

# Look up image IDs
echo ""
echo "==> Resolving image IDs..."
TOTAL_COMPUTE_POOLS=$((EXT_GATEWAY_POOLS + INT_GATEWAY_POOLS + USER_POOLS))
IMAGE_ID_COMPUTE=""
IMAGE_ID_GPU=""
if [[ "$TOTAL_COMPUTE_POOLS" -gt 0 ]]; then
    IMAGE_ID_COMPUTE="$(get_image_by_family "$COMPUTE_IMAGE_FAMILY")"
    echo "    Compute image ($COMPUTE_IMAGE_FAMILY): $IMAGE_ID_COMPUTE"
fi
if [[ "$GPU_POOLS" -gt 0 ]]; then
    IMAGE_ID_GPU="$(get_image_by_family "$GPU_IMAGE_FAMILY")"
    echo "    GPU image ($GPU_IMAGE_FAMILY): $IMAGE_ID_GPU"
fi

# ------------------------------------------------------------------
# Set up temp directory and userdata generation
# ------------------------------------------------------------------
umask 077
TEMP_DIR="$(mktemp -d "${SCRIPT_DIR}/nebius-deploy-XXXXXX.tmp")"
if [[ "$DEBUG_MODE" == true ]]; then
    echo "==> Debug mode: temp directory preserved at $TEMP_DIR"
    trap '' EXIT
else
    trap 'rm -rf "$TEMP_DIR"' EXIT
fi

USERDATA_TEMPLATE="${SCRIPT_DIR}/userdata-bootstrap-nonazure.yaml"
[[ -f "$USERDATA_TEMPLATE" ]] || die "userdata-bootstrap-nonazure.yaml not found at $USERDATA_TEMPLATE"

# generate_vm_userdata <pool_name> <hostname> [extra_labels]
# Outputs the rendered cloud-init YAML to a file and prints the file path.
generate_vm_userdata() {
    local pool_name="$1"
    local vm_hostname="$2"
    local extra_labels="${3:-}"
    local output_file="${TEMP_DIR}/${pool_name}-${vm_hostname}-userdata.yaml"

    # Check for an existing bootstrap token secret for this pool
    local existing_secret
    existing_secret="$(kubectl get secrets -n kube-system \
        -l net.unbounded-cloud.io/pool-name="$pool_name" \
        --field-selector type=bootstrap.kubernetes.io/token \
        -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)"

    local token_id token_secret
    if [[ -n "$existing_secret" ]]; then
        echo "  Reusing existing bootstrap token: $existing_secret" >&2
        token_id="$(kubectl get secret -n kube-system "$existing_secret" \
            -o jsonpath='{.data.token-id}' | base64 -d)"
        token_secret="$(kubectl get secret -n kube-system "$existing_secret" \
            -o jsonpath='{.data.token-secret}' | base64 -d)"
    else
        echo "  Creating new bootstrap token for pool $pool_name" >&2
        token_id="$(openssl rand -hex 16 | cut -c1-6)"
        token_secret="$(openssl rand -hex 16 | cut -c1-16)"

        kubectl create secret generic "bootstrap-token-${token_id}" \
            --namespace kube-system \
            --type 'bootstrap.kubernetes.io/token' \
            --from-literal=description="Bootstrap token for ${pool_name}" \
            --from-literal=token-id="$token_id" \
            --from-literal=token-secret="$token_secret" \
            --from-literal=usage-bootstrap-authentication="true" \
            --from-literal=usage-bootstrap-signing="true" >&2
        kubectl label secret "bootstrap-token-${token_id}" \
            --namespace kube-system \
            net.unbounded-cloud.io/pool-name="$pool_name" >&2
    fi

    # Generate gzip+base64 encoded bootstrap script with non-Azure node labels
    local bootstrap_script="${SCRIPT_DIR}/bootstrap-node.sh"
    [[ -f "$bootstrap_script" ]] || die "bootstrap-node.sh not found at $bootstrap_script"

    local customdata_ts
    customdata_ts="$(date -u +%Y%m%d%H%M%SZ)"
    local nonazure_labels="kubernetes.azure.com/managed=false,node.kubernetes.io/exclude-from-external-load-balancers=true,net.unbounded-cloud.io/provider=nebius,net.unbounded-cloud.io/customdata-generated=${customdata_ts},net.unbounded-cloud.io/agentpool=${pool_name}${extra_labels}"

    # Patch node labels, add --cloud-provider=external, remove credential-provider flags, then gzip+base64
    local bootstrap_gz
    bootstrap_gz="$(sed \
        -e 's|local node_labels="kubernetes.azure.com/managed=false,node.kubernetes.io/exclude-from-external-load-balancers=true"|local node_labels="'"$nonazure_labels"'"|' \
        -e 's|KUBELET_KUBEADM_ARGS="|KUBELET_KUBEADM_ARGS="--cloud-provider=external |' \
        -e 's|--image-credential-provider-bin-dir=[^ ]* ||' \
        -e 's|--image-credential-provider-config=[^ ]* ||' \
        "$bootstrap_script" | gzip -9 | base64 -w0)"

    # Render the userdata template: substitute placeholders and strip comments
    # Use a temp file for the gzipped script to avoid sed argument length limits
    local gz_file="${TEMP_DIR}/${pool_name}-bootstrap.gz.b64"
    echo "$bootstrap_gz" > "$gz_file"

    sed -e 's|__API_SERVER__|'"$API_SERVER"'|g' \
        -e 's|__BOOTSTRAP_TOKEN__|'"${token_id}.${token_secret}"'|g' \
        -e 's|__CLUSTER_DNS__|'"$CLUSTER_DNS"'|g' \
        -e 's|__CLUSTER_RG__|'"$CLUSTER_RG"'|g' \
        -e 's|__KUBERNETES_VERSION__|'"$KUBERNETES_VERSION"'|g' \
        -e 's|__CA_CERT_BASE64__|'"$CA_CERT_BASE64"'|g' \
        -e 's|__SCALE_SET_NAME__|'"$pool_name"'|g' \
        -e 's|__EXTRA_NODE_LABELS__|'"$extra_labels"'|g' \
        -e 's|__SSH_PUBLIC_KEY__|'"$SSH_PUBLIC_KEY"'|g' \
        -e 's|__ADMIN_PASSWORD__|'"$ADMIN_PASSWORD"'|g' \
        -e 's|__CUSTOMDATA_GENERATED__|'"$customdata_ts"'|g' \
        -e 's|__HOSTNAME__|'"$vm_hostname"'|g' \
        -e '/^#cloud-config/!{/^[[:space:]]*#/d}' \
        -e '/^$/d' \
        < "$USERDATA_TEMPLATE" \
        > "$output_file"

    # Replace the gzip placeholder with the actual gzipped content (indented 6 spaces)
    local placeholder_line
    placeholder_line="$(grep -n '__BOOTSTRAP_SCRIPT_GZ__' "$output_file" | head -1 | cut -d: -f1)"
    if [[ -n "$placeholder_line" ]]; then
        local indent="      "
        {
            head -n "$((placeholder_line - 1))" "$output_file"
            sed "s/^/${indent}/" "$gz_file"
            tail -n "+$((placeholder_line + 1))" "$output_file"
        } > "${output_file}.tmp" && mv "${output_file}.tmp" "$output_file"
    fi

    echo "$output_file"
}

# create_allocation <vm_name> <public_ip>
# Creates a private IP allocation (and public if needed). Prints allocation IDs.
create_allocation() {
    local name="$1"
    local public_ip="$2"
    local alloc_name="${name}-ip"

    local priv_alloc_id
    priv_alloc_id=$(
        nebius vpc allocation get-by-name --name "$alloc_name" --format jsonpath='.metadata.id' 2>/dev/null \
        || nebius vpc allocation create --name "$alloc_name" --ipv4-private-subnet-id "$SUBNET_ID" --format jsonpath='.metadata.id'
    )
    echo "    Allocation: $alloc_name -> $priv_alloc_id"

    if [[ "$public_ip" == true ]]; then
        local pub_alloc_name="${name}-pub-ip"
        local pub_alloc_id
        pub_alloc_id=$(
            nebius vpc allocation get-by-name --name "$pub_alloc_name" --format jsonpath='.metadata.id' 2>/dev/null \
            || nebius vpc allocation create --name "$pub_alloc_name" --ipv4-public-subnet-id "$SUBNET_ID" --format jsonpath='.metadata.id'
        )
        echo "    Allocation: $pub_alloc_name -> $pub_alloc_id"
    fi
}

# create_disk <vm_name> <image_id>
# Creates or resolves a disk for a VM. Prints the disk ID.
create_disk() {
    local name="$1"
    local image_id="$2"
    local disk_id
    disk_id=$(
        nebius compute disk get-by-name --name "${name}-disk" --format jsonpath='.metadata.id' 2>/dev/null \
        || nebius compute disk create --name "${name}-disk" --size-gibibytes "$DISK_SIZE" --source-image-id "$image_id" --type network_ssd --format jsonpath='.metadata.id'
    ) 2>/dev/stdout | sed -e "s/^/    /"
    echo "    Disk: ${name}-disk -> $disk_id"
}

# create_instance <vm_name> <hostname> <vm_platform> <vm_preset> <userdata_file> <public_ip>
# Creates or resolves a VM instance. Expects the disk and allocations to already exist.
create_instance() {
    local name="$1"
    local vm_hostname="$2"
    local vm_platform="$3"
    local vm_preset="$4"
    local userdata_file="$5"
    local public_ip="$6"

    local disk_id
    disk_id="$(nebius compute disk get-by-name --name "${name}-disk" --format jsonpath='.metadata.id')"

    # Look up the private IP allocation
    local priv_alloc_id
    priv_alloc_id="$(nebius vpc allocation get-by-name --name "${name}-ip" --format jsonpath='.metadata.id')"

    local net_iface
    if [[ "$public_ip" == true ]]; then
        local pub_alloc_id
        pub_alloc_id="$(nebius vpc allocation get-by-name --name "${name}-pub-ip" --format jsonpath='.metadata.id')"
        net_iface="$(jq -cn --arg SUBNET_ID "$SUBNET_ID" --arg PRIV "$priv_alloc_id" --arg PUB "$pub_alloc_id" \
            '[{"name": "eth0", "ip_address": {"allocation_id": $PRIV}, "subnet_id": $SUBNET_ID, "public_ip_address": {"allocation_id": $PUB}}]')"
    else
        net_iface="$(jq -cn --arg SUBNET_ID "$SUBNET_ID" --arg PRIV "$priv_alloc_id" \
            '[{"name": "eth0", "ip_address": {"allocation_id": $PRIV}, "subnet_id": $SUBNET_ID}]')"
    fi

    local userdata
    userdata="$(cat "$userdata_file")"

    local vm_id
    vm_id=$(
        nebius compute instance get-by-name --name "$name" --format jsonpath='.metadata.id' 2>/dev/null \
        || nebius compute instance create \
            --name "$name" --hostname "$vm_hostname" \
            --boot-disk-attach-mode read_write --boot-disk-existing-disk-id "$disk_id" \
            --network-interfaces "$net_iface" \
            --resources-platform "$vm_platform" --resources-preset "$vm_preset" \
            --service-account-id "$SA_ID" \
            --cloud-init-user-data "$userdata" \
            --format jsonpath='.metadata.id'
    ) 2>/dev/stdout | sed -e "s/^/    /"
    echo "    Instance: $name (hostname: $vm_hostname) -> $vm_id"
}
export -f create_allocation create_disk create_instance nebius

# ------------------------------------------------------------------
# Pre-create site and gateway CRDs
# ------------------------------------------------------------------

# Populate gateway pool names before CRD creation
for i in $(seq 1 "$EXT_GATEWAY_POOLS"); do
    GATEWAY_POOL_NAMES+=("${SITE_NAME}extgw${i}")
    GATEWAY_POOL_TYPES+=("External")
done
for i in $(seq 1 "$INT_GATEWAY_POOLS"); do
    GATEWAY_POOL_NAMES+=("${SITE_NAME}intgw${i}")
    GATEWAY_POOL_TYPES+=("Internal")
done

echo ""
echo "==> Pre-creating site and gateway CRDs..."
ensure_site_gateway_resources "$SITE_NAME"

# ------------------------------------------------------------------
# Create VMs
# ------------------------------------------------------------------
echo ""
echo "==> Generating userdata for all VMs..."

# Build a manifest of VMs to create (one line per VM, tab-separated)
VM_MANIFEST="${TEMP_DIR}/vm-manifest.tsv"
: > "$VM_MANIFEST"

# External gateway pools (compute VMs with public IPs)
for i in $(seq 1 "$EXT_GATEWAY_POOLS"); do
    pool_name="${SITE_NAME}extgw${i}"
    echo "  Pool: $pool_name (external gateway, platform=$COMPUTE_PLATFORM, preset=$COMPUTE_PRESET, count=$GATEWAY_COUNT)"
    for j in $(seq 0 "$((GATEWAY_COUNT - 1))"); do
        vm_seq="$(printf '%06d' "$j")"
        vm_hostname="ext-${pool_name}-${vm_seq}"
        vm_name="${USERNAME}-${pool_name}-${vm_seq}"
        userdata_file="$(generate_vm_userdata "$pool_name" "$vm_hostname" ",net.unbounded-cloud.io/gateway=true")"
        printf '%s\t%s\t%s\t%s\t%s\t%s\t%s\n' \
            "$vm_name" "$vm_hostname" "$IMAGE_ID_COMPUTE" "$COMPUTE_PLATFORM" "$COMPUTE_PRESET" "$userdata_file" "true" \
            >> "$VM_MANIFEST"
    done
done

# Internal gateway pools (compute VMs without public IPs)
for i in $(seq 1 "$INT_GATEWAY_POOLS"); do
    pool_name="${SITE_NAME}intgw${i}"
    echo "  Pool: $pool_name (internal gateway, platform=$COMPUTE_PLATFORM, preset=$COMPUTE_PRESET, count=$GATEWAY_COUNT)"
    for j in $(seq 0 "$((GATEWAY_COUNT - 1))"); do
        vm_seq="$(printf '%06d' "$j")"
        vm_hostname="ext-${pool_name}-${vm_seq}"
        vm_name="${USERNAME}-${pool_name}-${vm_seq}"
        userdata_file="$(generate_vm_userdata "$pool_name" "$vm_hostname" ",net.unbounded-cloud.io/gateway=true")"
        printf '%s\t%s\t%s\t%s\t%s\t%s\t%s\n' \
            "$vm_name" "$vm_hostname" "$IMAGE_ID_COMPUTE" "$COMPUTE_PLATFORM" "$COMPUTE_PRESET" "$userdata_file" "false" \
            >> "$VM_MANIFEST"
    done
done

# User pools (compute VMs without public IPs)
for i in $(seq 1 "$USER_POOLS"); do
    pool_name="${SITE_NAME}user${i}"
    echo "  Pool: $pool_name (user, platform=$COMPUTE_PLATFORM, preset=$COMPUTE_PRESET, count=$USER_COUNT)"
    for j in $(seq 0 "$((USER_COUNT - 1))"); do
        vm_seq="$(printf '%06d' "$j")"
        vm_hostname="ext-${pool_name}-${vm_seq}"
        vm_name="${USERNAME}-${pool_name}-${vm_seq}"
        userdata_file="$(generate_vm_userdata "$pool_name" "$vm_hostname")"
        printf '%s\t%s\t%s\t%s\t%s\t%s\t%s\n' \
            "$vm_name" "$vm_hostname" "$IMAGE_ID_COMPUTE" "$COMPUTE_PLATFORM" "$COMPUTE_PRESET" "$userdata_file" "false" \
            >> "$VM_MANIFEST"
    done
done

# GPU pools (GPU VMs without public IPs)
for i in $(seq 1 "$GPU_POOLS"); do
    pool_name="${SITE_NAME}gpu${i}"
    echo "  Pool: $pool_name (GPU, platform=$GPU_PLATFORM, preset=$GPU_PRESET, count=$GPU_COUNT)"
    for j in $(seq 0 "$((GPU_COUNT - 1))"); do
        vm_seq="$(printf '%06d' "$j")"
        vm_hostname="ext-${pool_name}-${vm_seq}"
        vm_name="${USERNAME}-${pool_name}-${vm_seq}"
        userdata_file="$(generate_vm_userdata "$pool_name" "$vm_hostname")"
        printf '%s\t%s\t%s\t%s\t%s\t%s\t%s\n' \
            "$vm_name" "$vm_hostname" "$IMAGE_ID_GPU" "$GPU_PLATFORM" "$GPU_PRESET" "$userdata_file" "false" \
            >> "$VM_MANIFEST"
    done
done

VM_TOTAL="$(wc -l < "$VM_MANIFEST")"
echo ""

# Export variables needed by parallel functions in subshells
export DISK_SIZE SUBNET_ID VERBOSE SA_ID

PUB_IP_COUNT="$(awk -F'\t' '$7 == "true"' "$VM_MANIFEST" | wc -l)"
ALLOC_TOTAL=$((VM_TOTAL + PUB_IP_COUNT))

# Phase 1: Create all IP allocations in parallel
echo "==> Creating $ALLOC_TOTAL IP allocations ($VM_TOTAL private + $PUB_IP_COUNT public, parallelism: 4)..."
xargs -P4 -a "$VM_MANIFEST" -d '\n' -I{} bash -c '
    IFS=$'"'"'\t'"'"' read -r vm_name vm_hostname image_id vm_platform vm_preset userdata_file public_ip <<< "$1"
    create_allocation "$vm_name" "$public_ip"
' _ {}

# Phase 2: Create all disks in parallel
echo ""
echo "==> Creating $VM_TOTAL disks (parallelism: 4)..."
xargs -P4 -a "$VM_MANIFEST" -d '\n' -I{} bash -c '
    IFS=$'"'"'\t'"'"' read -r vm_name vm_hostname image_id vm_platform vm_preset userdata_file public_ip <<< "$1"
    create_disk "$vm_name" "$image_id"
' _ {}

# Phase 3: Create all instances in parallel
echo ""
echo "==> Creating $VM_TOTAL instances (parallelism: 4)..."
xargs -P4 -a "$VM_MANIFEST" -d '\n' -I{} bash -c '
    IFS=$'"'"'\t'"'"' read -r vm_name vm_hostname image_id vm_platform vm_preset userdata_file public_ip <<< "$1"
    create_instance "$vm_name" "$vm_hostname" "$vm_platform" "$vm_preset" "$userdata_file" "$public_ip"
' _ {}

# ------------------------------------------------------------------
# Post-deployment CRD reconciliation
# ------------------------------------------------------------------
echo ""
echo "==> Site '$SITE_NAME' deployed successfully."
echo "    External gateway pools: $EXT_GATEWAY_POOLS"
echo "    Internal gateway pools: $INT_GATEWAY_POOLS"
echo "    User pools:             $USER_POOLS"
echo "    GPU pools:              $GPU_POOLS"
echo "    Nodes will bootstrap into the cluster on first boot."
ensure_unmanaged_kube_proxy_daemonset
ensure_site_gateway_resources "$SITE_NAME"
