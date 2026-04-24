#!/bin/bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

#
# add-azure-site.sh - Deploy a complete Azure external site for the current AKS cluster
#
# This script deploys networking infrastructure (VNet, LB, optional Bastion) and
# one or more VMSS pools that bootstrap into the currently active Kubernetes cluster.
# It resolves the kubelet identity, NSG, and bootstrap configuration from the cluster's
# system pool automatically, generates per-pool customdata inline, and deploys
# everything in a single Bicep deployment.
#
# Usage:
#   ./add-azure-site.sh --name <site-name> --ipv4-range <cidr> [options]
#
# Required arguments:
#   -n, --name              Site name (a-z0-9 only, used as resource prefix)
#   --ipv4-range            IPv4 address range for the VNet (CIDR notation)
#   --pod-ipv4-cidr         Pod IPv4 CIDR for the site
#
# Optional arguments:
#   -g, --resource-group    Azure resource group (uses az CLI default if not set)
#   -l, --location          Azure region (uses az CLI default if not set)
#   --ipv6-range            IPv6 range ("generate" for auto ULA /48, "" to disable; default: generate)
#   --pod-ipv6-cidr         Pod IPv6 CIDR for the site (required when IPv6 range is enabled)
#   --external-gateway-pools  Number of external gateway VMSS pools with public IPs (default: 1, 0 to skip)
#   --internal-gateway-pools  Number of internal gateway VMSS pools without public IPs (default: 0)
#   --user-pools            Number of user VMSS pools (default: 1, 0 to skip)
#   --gateway-sku           VM SKU for gateway pools (default: Standard_D2ads_v6)
#   --user-sku              VM SKU for user pools (default: Standard_D2ads_v6)
#   --gateway-count         Instances per gateway pool (default: 2)
#   --user-count            Instances per user pool (default: 2)
#   --ports-per-vm          Outbound ports per VM on LB (default: 1024)
#   --outbound-ip-count     Number of outbound IPv4 public IPs on LB (default: 1)
#   --no-bastion            Disable Azure Bastion deployment
#   --peer-primary-vnet     Create bidirectional VNet peering with the primary site VNet
#   --password <pass>       Admin password
#   --ssh-key <key|@path>   SSH public key value, or @<path> to read from file
#   --reimage-existing      After deployment, update and reimage any VMSS that existed before
#   -y, --yes               Skip confirmation prompt
#   -d, --debug             Write all temp files but pause before deploying for inspection
#   --help                  Show this help message
#
# At least one of --password or --ssh-key is required.
#
# usage-end

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"

# Defaults
SITE_NAME=""
RESOURCE_GROUP=""
LOCATION=""
IPV4_RANGE=""
IPV6_RANGE="generate"
POD_IPV4_CIDR=""
POD_IPV6_CIDR=""
EXT_GATEWAY_POOLS=1
INT_GATEWAY_POOLS=0
USER_POOLS=1
GATEWAY_SKU="Standard_D2ads_v6"
USER_SKU="Standard_D2ads_v6"
GATEWAY_COUNT=2
USER_COUNT=2
PORTS_PER_VM=1024
OUTBOUND_IP_COUNT=1
ENABLE_BASTION=true
PEER_PRIMARY_VNET=false
ADMIN_PASSWORD=""
SSH_KEY=""
AUTO_CONFIRM=false
DEBUG_MODE=false
REIMAGE_EXISTING=false
GATEWAY_POOL_NAMES=()
GATEWAY_POOL_TYPES=()
PRIMARY_VNET_HAS_BASTION=false
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

get_site_node_cidrs() {
    local vnet_name="$1"
    local subnet_json=""
    if subnet_json="$(az network vnet subnet show "${RG_FLAG[@]}" --vnet-name "$vnet_name" -n default -o json 2>/dev/null)"; then
        jq -r '.addressPrefixes // [ .addressPrefix ] | .[]' <<<"$subnet_json"
        return
    fi

    python - "$IPV4_RANGE" "$IPV6_RANGE" <<'PY'
import ipaddress
import sys

ipv4 = sys.argv[1]
ipv6 = sys.argv[2]

v4 = ipaddress.ip_network(ipv4, strict=False)
v4_subnets = list(v4.subnets(new_prefix=v4.prefixlen + 1))
print(v4_subnets[1])

if ipv6 and ipv6 != "generate":
    v6 = ipaddress.ip_network(ipv6, strict=False)
    v6_subnets = list(v6.subnets(new_prefix=64))
    print(v6_subnets[0])
PY
}

ensure_site_gateway_resources() {
    local site_name="$1"
    local -a node_cidrs=()
    local -a pod_cidrs=()
    local i

    mapfile -t node_cidrs < <(get_site_node_cidrs "${site_name}-vnet")
    [[ "${#node_cidrs[@]}" -gt 0 ]] || die "Failed to resolve node CIDRs for site ${site_name}"
    [[ -n "${POD_IPV4_CIDR}" ]] && pod_cidrs+=("${POD_IPV4_CIDR}")
    [[ -n "${POD_IPV6_CIDR}" ]] && pod_cidrs+=("${POD_IPV6_CIDR}")

    echo "==> Ensuring Site '${site_name}' CRD..."
    {
        echo "apiVersion: net.unbounded-kube.io/v1alpha1"
        echo "kind: Site"
        echo "metadata:"
        echo "  name: ${site_name}"
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

    if [[ "${#GATEWAY_POOL_NAMES[@]}" -eq 0 ]]; then
        echo "  No gateway pools were created; skipping GatewayPool and SiteGatewayPoolAssignment CRDs."
        return
    fi

    echo "==> Ensuring GatewayPool and SiteGatewayPoolAssignment CRDs..."
    for i in "${!GATEWAY_POOL_NAMES[@]}"; do
        local pool_name="${GATEWAY_POOL_NAMES[$i]}"
        local pool_type="${GATEWAY_POOL_TYPES[$i]}"
        cat <<EOF | kubectl apply -f -
apiVersion: net.unbounded-kube.io/v1alpha1
kind: GatewayPool
metadata:
  name: ${pool_name}
spec:
  type: ${pool_type}
  nodeSelector:
    net.unbounded-kube.io/agentpool: "${pool_name}"
EOF

        cat <<EOF | kubectl apply -f -
apiVersion: net.unbounded-kube.io/v1alpha1
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
}

ensure_site_peering_resource() {
    local site_name="$1"
    local primary_vnet_name="$2"
    local primary_site_name
    local peering_name

    if [[ "$PEER_PRIMARY_VNET" != true ]]; then
        return
    fi

    primary_site_name="${primary_vnet_name%-vnet}"
    if [[ -z "$primary_site_name" || "$primary_site_name" == "$site_name" ]]; then
        echo "  Skipping SitePeering creation: unable to resolve distinct primary site name from VNet '${primary_vnet_name}'."
        return
    fi

    peering_name="${primary_site_name}-${site_name}"
    echo "==> Ensuring SitePeering '${peering_name}'..."
    cat <<EOF | kubectl apply -f -
apiVersion: net.unbounded-kube.io/v1alpha1
kind: SitePeering
metadata:
  name: ${peering_name}
spec:
  enabled: true
  sites:
    - "${primary_site_name}"
    - "${site_name}"
EOF
}

ensure_primary_gateway_connectivity() {
    local site_name="$1"
    local primary_gateway_pool_name="$2"
    local pool_name
    local peering_name
    local first_pool
    local second_pool

    if [[ -z "$primary_gateway_pool_name" ]]; then
        return
    fi
    if ! kubectl get gatewaypool "$primary_gateway_pool_name" >/dev/null 2>&1; then
        die "Primary gateway pool ${primary_gateway_pool_name} not found"
    fi

    if [[ "${#GATEWAY_POOL_NAMES[@]}" -gt 0 ]]; then
        echo "==> Ensuring GatewayPoolPeerings to primary pool '${primary_gateway_pool_name}'..."
        for pool_name in "${GATEWAY_POOL_NAMES[@]}"; do
            [[ "$pool_name" == "$primary_gateway_pool_name" ]] && continue
            first_pool="$primary_gateway_pool_name"
            second_pool="$pool_name"
            if [[ "$first_pool" > "$second_pool" ]]; then
                first_pool="$pool_name"
                second_pool="$primary_gateway_pool_name"
            fi
            peering_name="${first_pool}-${second_pool}"
            cat <<EOF | kubectl apply -f -
apiVersion: net.unbounded-kube.io/v1alpha1
kind: GatewayPoolPeering
metadata:
  name: ${peering_name}
spec:
  enabled: true
  gatewayPools:
    - "${primary_gateway_pool_name}"
    - "${pool_name}"
EOF
        done
        return
    fi

    if [[ "$PEER_PRIMARY_VNET" == true ]]; then
        echo "==> Ensuring SiteGatewayPoolAssignment to primary pool '${primary_gateway_pool_name}'..."
        cat <<EOF | kubectl apply -f -
apiVersion: net.unbounded-kube.io/v1alpha1
kind: SiteGatewayPoolAssignment
metadata:
  name: ${site_name}-${primary_gateway_pool_name}
spec:
  enabled: true
  sites:
    - "${site_name}"
  gatewayPools:
    - "${primary_gateway_pool_name}"
EOF
    fi
}

# Parse arguments
while [[ $# -gt 0 ]]; do
    case "$1" in
        -n|--name)           SITE_NAME="$2"; shift 2 ;;
        -g|--resource-group) RESOURCE_GROUP="$2"; shift 2 ;;
        -l|--location)       LOCATION="$2"; shift 2 ;;
        --ipv4-range)        IPV4_RANGE="$2"; shift 2 ;;
        --ipv6-range)        IPV6_RANGE="$2"; shift 2 ;;
        --pod-ipv4-cidr)     POD_IPV4_CIDR="$2"; shift 2 ;;
        --pod-ipv6-cidr)     POD_IPV6_CIDR="$2"; shift 2 ;;
        --external-gateway-pools) EXT_GATEWAY_POOLS="$2"; shift 2 ;;
        --internal-gateway-pools) INT_GATEWAY_POOLS="$2"; shift 2 ;;
        --user-pools)        USER_POOLS="$2"; shift 2 ;;
        --gateway-sku)       GATEWAY_SKU="$2"; shift 2 ;;
        --user-sku)          USER_SKU="$2"; shift 2 ;;
        --gateway-count)     GATEWAY_COUNT="$2"; shift 2 ;;
        --user-count)        USER_COUNT="$2"; shift 2 ;;
        --ports-per-vm)      PORTS_PER_VM="$2"; shift 2 ;;
        --outbound-ip-count) OUTBOUND_IP_COUNT="$2"; shift 2 ;;
        --no-bastion)        ENABLE_BASTION=false; shift ;;
        --peer-primary-vnet) PEER_PRIMARY_VNET=true; shift ;;
        --password)          ADMIN_PASSWORD="$2"; shift 2 ;;
        --ssh-key)           SSH_KEY="$2"; shift 2 ;;
        --reimage-existing)  REIMAGE_EXISTING=true; shift ;;
        -y|--yes)            AUTO_CONFIRM=true; shift ;;
        -d|--debug)          DEBUG_MODE=true; shift ;;
        --help)              usage; exit 0 ;;
        *)                   die "Unknown argument: $1" ;;
    esac
done

# Validate required arguments
[[ -n "$SITE_NAME" ]] || die "--name is required"
[[ -n "$IPV4_RANGE" ]] || die "--ipv4-range is required"
[[ -n "$POD_IPV4_CIDR" ]] || die "--pod-ipv4-cidr is required"
if [[ -n "$IPV6_RANGE" && -z "$POD_IPV6_CIDR" ]]; then
    die "--pod-ipv6-cidr is required when --ipv6-range is set"
fi
[[ -n "$ADMIN_PASSWORD" || -n "$SSH_KEY" ]] || die "at least one of --password or --ssh-key is required"

if [[ ! "$SITE_NAME" =~ ^[a-z0-9]+$ ]]; then
    die "--name must contain only lowercase letters and digits (a-z0-9)"
fi

# Resolve resource group: explicit arg > az CLI default > error
if [[ -z "$RESOURCE_GROUP" ]]; then
    RESOURCE_GROUP="$(az config get --query 'defaults[?name==`group`].value' -o tsv --only-show-errors)"
    [[ -n "$RESOURCE_GROUP" ]] || die "--resource-group is required (no az CLI default configured)"
fi
RG_FLAG=(-g "$RESOURCE_GROUP")

# Resolve location: explicit arg > az CLI default > resource group location
if [[ -z "$LOCATION" ]]; then
    LOCATION="$(az config get --query 'defaults[?name==`location`].value' -o tsv --only-show-errors)"
fi
if [[ -z "$LOCATION" ]]; then
    LOCATION="$(az group show -g "$RESOURCE_GROUP" --query location -o tsv)"
fi
LOCATION_FLAG=(-l "$LOCATION")

# ------------------------------------------------------------------
# Resolve shared cluster context (done once, reused for all pools)
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
PRIMARY_SUBSCRIPTION_ID="$(echo "$PROVIDER_ID" | awk -F'/+' '{print $3}')"
CLUSTER_RG="$(echo "$PROVIDER_ID" | awk -F'/+' '{print $5}')"
SYSTEM_VMSS="$(echo "$PROVIDER_ID" | awk -F'/+' '{print $9}')"

# Resolve the site (external) subscription from the current az CLI context
SITE_SUBSCRIPTION_ID="$(az account show --query id -o tsv)"
TENANT_ID="$(az account show --query tenantId -o tsv)"

# When the primary cluster is in a different subscription, pass --subscription
# to all az commands that query cluster/primary resources.
PRIMARY_SUB_FLAG=()
CROSS_SUBSCRIPTION=false
if [[ "$PRIMARY_SUBSCRIPTION_ID" != "$SITE_SUBSCRIPTION_ID" ]]; then
    CROSS_SUBSCRIPTION=true
    PRIMARY_SUB_FLAG=(--subscription "$PRIMARY_SUBSCRIPTION_ID")
    echo "==> Cross-subscription deployment detected"
    echo "    Primary (cluster) subscription: $PRIMARY_SUBSCRIPTION_ID"
    echo "    Site (external) subscription:   $SITE_SUBSCRIPTION_ID"
fi

# Get the kubelet managed identity from the system VMSS
IDENTITY_ID="$(az vmss show -g "$CLUSTER_RG" -n "$SYSTEM_VMSS" \
    "${PRIMARY_SUB_FLAG[@]}" \
    --query 'identity.userAssignedIdentities | keys(@) | [0]' -o tsv)"
[[ -n "$IDENTITY_ID" ]] || die "Could not resolve kubelet managed identity from system VMSS $SYSTEM_VMSS"

# Resolve the client ID of the kubelet managed identity
KUBELET_CLIENT_ID="$(az identity show --ids "$IDENTITY_ID" --query clientId -o tsv)"
[[ -n "$KUBELET_CLIENT_ID" ]] || die "Could not resolve client ID for kubelet identity $IDENTITY_ID"

# Resolve the principal (object) ID for role assignment naming
IDENTITY_PRINCIPAL_ID="$(az identity show --ids "$IDENTITY_ID" --query principalId -o tsv)"
[[ -n "$IDENTITY_PRINCIPAL_ID" ]] || die "Could not resolve principal ID for kubelet identity $IDENTITY_ID"

# Resolve networking context from the system VMSS subnet
CLUSTER_SUBNET_ID="$(az vmss show -g "$CLUSTER_RG" -n "$SYSTEM_VMSS" \
    "${PRIMARY_SUB_FLAG[@]}" \
    --query 'virtualMachineProfile.networkProfile.networkInterfaceConfigurations[0].ipConfigurations[0].subnet.id' -o tsv)"
CLUSTER_VNET_NAME="$(echo "$CLUSTER_SUBNET_ID" | awk -F'/' '{for(i=1;i<=NF;i++) if($i=="virtualNetworks") print $(i+1)}')"
CLUSTER_VNET_RG="$(echo "$CLUSTER_SUBNET_ID" | awk -F'/' '{for(i=1;i<=NF;i++) if($i=="resourceGroups") print $(i+1)}')"
PRIMARY_SITE_NAME="${CLUSTER_VNET_NAME%-vnet}"

if [[ "$PEER_PRIMARY_VNET" == true && "$ENABLE_BASTION" == true ]]; then
    PRIMARY_VNET_BASTION_COUNT="$(az network bastion list -g "$CLUSTER_VNET_RG" \
        "${PRIMARY_SUB_FLAG[@]}" \
        --query "[?contains(ipConfigurations[0].subnet.id, '/virtualNetworks/${CLUSTER_VNET_NAME}/subnets/AzureBastionSubnet')]|length(@)" -o tsv)"
    if [[ "${PRIMARY_VNET_BASTION_COUNT:-0}" -gt 0 ]]; then
        ENABLE_BASTION=false
        PRIMARY_VNET_HAS_BASTION=true
        echo "==> Primary VNet already has a Bastion host; skipping Bastion deployment for site '$SITE_NAME'."
    fi
fi

# Site VNet and NSG values for customdata (azure.json should reference the site resources, not the cluster)
VNET_NAME="${SITE_NAME}-vnet"
VNET_RG="$RESOURCE_GROUP"
SUBNET_NAME="default"
SECURITY_GROUP_NAME="${SITE_NAME}-nsg"
ROUTE_TABLE_NAME="$(az network vnet subnet show --ids "$CLUSTER_SUBNET_ID" \
    --query 'routeTable.id' -o tsv | awk -F'/' '{print $NF}')"
if [[ -z "$ROUTE_TABLE_NAME" || "$ROUTE_TABLE_NAME" == "None" ]]; then
    ROUTE_TABLE_NAME=""
fi

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
echo "==> Deployment plan for site '$SITE_NAME':"
echo "    Resource group:          $RESOURCE_GROUP"
echo "    Location:                $LOCATION"
echo "    IPv4 range:              $IPV4_RANGE"
echo "    IPv6 range:              $IPV6_RANGE"
echo "    Pod IPv4 CIDR:           $POD_IPV4_CIDR"
echo "    Pod IPv6 CIDR:           ${POD_IPV6_CIDR:-(unset)}"
echo "    Bastion:                 $ENABLE_BASTION"
if [[ "$PRIMARY_VNET_HAS_BASTION" == true ]]; then
    echo "    Bastion skip reason:     primary VNet already has Bastion"
fi
echo "    Peer primary VNet:       $PEER_PRIMARY_VNET"
echo "    LB ports per VM:         $PORTS_PER_VM"
echo "    Outbound IP count:       $OUTBOUND_IP_COUNT"
echo "    External gateway pools:  $EXT_GATEWAY_POOLS (sku=$GATEWAY_SKU, count=$GATEWAY_COUNT each)"
echo "    Internal gateway pools:  $INT_GATEWAY_POOLS (sku=$GATEWAY_SKU, count=$GATEWAY_COUNT each)"
echo "    User pools:              $USER_POOLS (sku=$USER_SKU, count=$USER_COUNT each)"
echo "    API server:              $API_SERVER"
echo "    Kubernetes version:      $KUBERNETES_VERSION"
echo "    Cluster resource group:  $CLUSTER_RG"
if [[ "$CROSS_SUBSCRIPTION" == true ]]; then
    echo "    Primary subscription:    $PRIMARY_SUBSCRIPTION_ID"
    echo "    Site subscription:       $SITE_SUBSCRIPTION_ID"
fi
echo "    System VMSS:             $SYSTEM_VMSS"
echo "    Cluster VNet:            $CLUSTER_VNET_NAME (resource group: $CLUSTER_VNET_RG)"
echo "    Site VNet:               $VNET_NAME (resource group: $VNET_RG)"
echo "    Route table:             ${ROUTE_TABLE_NAME:-(none)}"
echo "    Tenant:                  $TENANT_ID"
echo "    Kubelet identity:        $IDENTITY_ID"
echo "    Site NSG:                ${SITE_NAME}-nsg"
if [[ -n "$ADMIN_PASSWORD" && -n "$SSH_KEY" ]]; then
    AUTH_MODE="password + ssh"
elif [[ -n "$ADMIN_PASSWORD" ]]; then
    AUTH_MODE="password"
else
    AUTH_MODE="ssh"
fi
echo "    Auth:                    $AUTH_MODE"
echo ""

if [[ "$AUTO_CONFIRM" != true ]]; then
    read -erp "Proceed with deployment? [y/N] " confirm
    if [[ ! "$confirm" =~ ^[Yy]$ ]]; then
        echo "Aborted."
        exit 0
    fi
fi

# ------------------------------------------------------------------
# Check for existing resources (idempotent redeployment support)
# ------------------------------------------------------------------
echo ""
echo "==> Checking for existing site resources..."

# Check if the VNet already exists and validate address prefixes
EXISTING_VNET_PREFIXES="$(az network vnet show "${RG_FLAG[@]}" -n "${SITE_NAME}-vnet" \
    --query 'addressSpace.addressPrefixes' -o json 2>/dev/null || echo '[]')"
if [[ "$EXISTING_VNET_PREFIXES" != "[]" ]]; then
    echo "  VNet ${SITE_NAME}-vnet already exists, validating address prefixes..."
    EXISTING_V4="$(echo "$EXISTING_VNET_PREFIXES" | jq -r '[.[] | select(contains(":") | not)] | .[0] // empty')"
    EXISTING_V6="$(echo "$EXISTING_VNET_PREFIXES" | jq -r '[.[] | select(contains(":"))] | .[0] // empty')"

    if [[ -n "$EXISTING_V4" && "$EXISTING_V4" != "$IPV4_RANGE" ]]; then
        die "VNet ${SITE_NAME}-vnet exists with IPv4 range $EXISTING_V4 but --ipv4-range is $IPV4_RANGE"
    fi

    if [[ "$IPV6_RANGE" == "generate" && -n "$EXISTING_V6" ]]; then
        echo "  Using existing IPv6 range from VNet: $EXISTING_V6"
        IPV6_RANGE="$EXISTING_V6"
    elif [[ -n "$EXISTING_V6" && -n "$IPV6_RANGE" && "$IPV6_RANGE" != "generate" && "$EXISTING_V6" != "$IPV6_RANGE" ]]; then
        die "VNet ${SITE_NAME}-vnet exists with IPv6 range $EXISTING_V6 but --ipv6-range is $IPV6_RANGE"
    elif [[ -z "$EXISTING_V6" && -n "$IPV6_RANGE" && "$IPV6_RANGE" != "generate" ]]; then
        echo "  Warning: VNet exists without IPv6 but --ipv6-range=$IPV6_RANGE was specified (will be added)" >&2
    fi
else
    echo "  VNet ${SITE_NAME}-vnet does not exist yet (new deployment)"
fi

# Look up existing VMSS computerNamePrefixes for reuse on redeployment.
# Builds an associative array: EXISTING_VMSS_PREFIX[vmssName]=computerNamePrefix
declare -A EXISTING_VMSS_PREFIX
EXISTING_VMSS_COUNT=0
EXISTING_VMSS_JSON="$(az vmss list "${RG_FLAG[@]}" \
    --query "[?starts_with(name, '${SITE_NAME}')].{name:name, prefix:virtualMachineProfile.osProfile.computerNamePrefix}" \
    -o json 2>/dev/null || echo '[]')"
while IFS=$'\t' read -r vmss_name vmss_prefix; do
    [[ -n "$vmss_name" ]] || continue
    EXISTING_VMSS_PREFIX["$vmss_name"]="$vmss_prefix"
    EXISTING_VMSS_COUNT=$((EXISTING_VMSS_COUNT + 1))
    echo "  Found existing VMSS: $vmss_name (prefix: $vmss_prefix)"
done < <(echo "$EXISTING_VMSS_JSON" | jq -r '.[] | [.name, .prefix] | @tsv')
if [[ "$EXISTING_VMSS_COUNT" -eq 0 ]]; then
    echo "  No existing VMSS pools found"
fi

# Helper: returns existing computerNamePrefix for a pool, or generates a new one
get_hostname_prefix() {
    local pool_name="$1"
    local vmss_name="${SITE_NAME}-${pool_name}"
    if [[ -n "${EXISTING_VMSS_PREFIX[$vmss_name]+x}" ]]; then
        echo "${EXISTING_VMSS_PREFIX[$vmss_name]}"
    else
        echo "ext-${pool_name}-vmss-"
    fi
}

# ------------------------------------------------------------------
# Generate per-pool customdata and build pools JSON array
# ------------------------------------------------------------------

USERDATA_TEMPLATE="${SCRIPT_DIR}/userdata-bootstrap.yaml"
[[ -f "$USERDATA_TEMPLATE" ]] || die "userdata-bootstrap.yaml not found at $USERDATA_TEMPLATE"

umask 077
TEMP_DIR="$(mktemp -d /tmp/site-deploy-XXXXXX)"
if [[ "$DEBUG_MODE" == true ]]; then
    echo "==> Debug mode: temp directory preserved at $TEMP_DIR"
    trap '' EXIT
else
    trap 'rm -rf "$TEMP_DIR"' EXIT
fi

# generate_pool_customdata <pool_name> [extra_labels]
# Outputs base64-encoded customdata to stdout. Status messages go to stderr.
# extra_labels is substituted for __EXTRA_NODE_LABELS__ (e.g. ",net.unbounded-kube.io/gateway=true").
generate_pool_customdata() {
    local pool_name="$1"
    local extra_labels="${2:-}"

    # Check for an existing bootstrap token secret for this pool
    local existing_secret
    existing_secret="$(kubectl get secrets -n kube-system \
        -l net.unbounded-kube.io/pool-name="$pool_name" \
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
            net.unbounded-kube.io/pool-name="$pool_name" >&2
    fi

    # Render the userdata template with all substitutions and base64-encode
    # Skip comment lines (^[[:space:]]*#) to avoid replacing tokens in documentation
    sed -e '/^[[:space:]]*#/!s|__API_SERVER__|'"$API_SERVER"'|g' \
        -e '/^[[:space:]]*#/!s|__BOOTSTRAP_TOKEN__|'"${token_id}.${token_secret}"'|g' \
        -e '/^[[:space:]]*#/!s|__CLUSTER_DNS__|'"$CLUSTER_DNS"'|g' \
        -e '/^[[:space:]]*#/!s|__CLUSTER_RG__|'"$CLUSTER_RG"'|g' \
        -e '/^[[:space:]]*#/!s|__KUBERNETES_VERSION__|'"$KUBERNETES_VERSION"'|g' \
        -e '/^[[:space:]]*#/!s|__CA_CERT_BASE64__|'"$CA_CERT_BASE64"'|g' \
        -e '/^[[:space:]]*#/!s|__TENANT_ID__|'"$TENANT_ID"'|g' \
        -e '/^[[:space:]]*#/!s|__SUBSCRIPTION_ID__|'"$SITE_SUBSCRIPTION_ID"'|g' \
        -e '/^[[:space:]]*#/!s|__SECURITY_GROUP_NAME__|'"$SECURITY_GROUP_NAME"'|g' \
        -e '/^[[:space:]]*#/!s|__VNET_NAME__|'"$VNET_NAME"'|g' \
        -e '/^[[:space:]]*#/!s|__VNET_RG__|'"$VNET_RG"'|g' \
        -e '/^[[:space:]]*#/!s|__SUBNET_NAME__|'"$SUBNET_NAME"'|g' \
        -e '/^[[:space:]]*#/!s|__ROUTE_TABLE_NAME__|'"$ROUTE_TABLE_NAME"'|g' \
        -e '/^[[:space:]]*#/!s|__SCALE_SET_NAME__|'"$pool_name"'|g' \
        -e '/^[[:space:]]*#/!s|__EXTRA_NODE_LABELS__|'"$extra_labels"'|g' \
        -e '/^[[:space:]]*#/!s|__KUBELET_CLIENT_ID__|'"$KUBELET_CLIENT_ID"'|g' \
        -e '/^[[:space:]]*#/!s|__CUSTOMDATA_GENERATED__|'"$(date -u +%Y%m%d%H%M%SZ)"'|g' \
        < "$USERDATA_TEMPLATE" | base64 -w0
}

echo ""
echo "==> Generating customdata for all pools..."

# Use file-based pool JSON to avoid ARG_MAX limits (customdata is large)
POOLS_FILE="${TEMP_DIR}/pools.json"
CUSTOMDATAS_FILE="${TEMP_DIR}/pool-customdatas.json"
echo '[]' > "$POOLS_FILE"
echo '{}' > "$CUSTOMDATAS_FILE"

add_pool() {
    local name="$1" sku="$2" count="$3" prefix="$4" cd_file="$5" pub="$6"
    local host_ports="${7:-}" host_ports_priority="${8:-0}"
    local tmp="${TEMP_DIR}/pools.tmp.json"
    # Add pool metadata (without customdata) to pools array
    jq --arg name "$name" \
        --arg sku "$sku" \
        --argjson count "$count" \
        --arg prefix "$prefix" \
        --argjson pub "$pub" \
        --arg hostPorts "$host_ports" \
        --argjson hostPortsPriority "$host_ports_priority" \
        '. + [{
            name: $name,
            sku: $sku,
            instanceCount: $count,
            computerNamePrefix: $prefix,
            enablePublicIPPerVM: $pub
        } + (if $hostPorts != "" then {
            allowedHostPorts: $hostPorts,
            allowedHostPortsPriority: $hostPortsPriority
        } else {} end)]' \
        "$POOLS_FILE" > "$tmp" && mv "$tmp" "$POOLS_FILE"
    # Add customdata to separate secure object keyed by pool name
    local cd_tmp="${TEMP_DIR}/customdatas.tmp.json"
    jq --arg name "$name" --rawfile cd "$cd_file" '. + {($name): $cd}' \
        "$CUSTOMDATAS_FILE" > "$cd_tmp" && mv "$cd_tmp" "$CUSTOMDATAS_FILE"
}

# External gateway pools (with public IPs)
NSG_RULE_PRIORITY=200
for i in $(seq 1 "$EXT_GATEWAY_POOLS"); do
    pool_name="${SITE_NAME}extgw${i}"
    hostname_prefix="$(get_hostname_prefix "$pool_name")"
    echo "  Pool: $pool_name (external gateway, sku=$GATEWAY_SKU, count=$GATEWAY_COUNT)"
    cd_file="${TEMP_DIR}/${pool_name}.cd"
    generate_pool_customdata "$pool_name" ",net.unbounded-kube.io/gateway=true" > "$cd_file"
    add_pool "$pool_name" "$GATEWAY_SKU" "$GATEWAY_COUNT" "$hostname_prefix" "$cd_file" true "51820-51999/udp" "$NSG_RULE_PRIORITY"
    GATEWAY_POOL_NAMES+=("$pool_name")
    GATEWAY_POOL_TYPES+=("External")
    NSG_RULE_PRIORITY=$((NSG_RULE_PRIORITY + 1))
done

# Internal gateway pools (without public IPs)
for i in $(seq 1 "$INT_GATEWAY_POOLS"); do
    pool_name="${SITE_NAME}intgw${i}"
    hostname_prefix="$(get_hostname_prefix "$pool_name")"
    echo "  Pool: $pool_name (internal gateway, sku=$GATEWAY_SKU, count=$GATEWAY_COUNT)"
    cd_file="${TEMP_DIR}/${pool_name}.cd"
    generate_pool_customdata "$pool_name" ",net.unbounded-kube.io/gateway=true" > "$cd_file"
    add_pool "$pool_name" "$GATEWAY_SKU" "$GATEWAY_COUNT" "$hostname_prefix" "$cd_file" false
    GATEWAY_POOL_NAMES+=("$pool_name")
    GATEWAY_POOL_TYPES+=("Internal")
done

# User pools (without public IPs)
for i in $(seq 1 "$USER_POOLS"); do
    pool_name="${SITE_NAME}user${i}"
    hostname_prefix="$(get_hostname_prefix "$pool_name")"
    echo "  Pool: $pool_name (user, sku=$USER_SKU, count=$USER_COUNT)"
    cd_file="${TEMP_DIR}/${pool_name}.cd"
    generate_pool_customdata "$pool_name" > "$cd_file"
    add_pool "$pool_name" "$USER_SKU" "$USER_COUNT" "$hostname_prefix" "$cd_file" false
done

POOL_COUNT="$(jq 'length' "$POOLS_FILE")"
echo "  Total pools: $POOL_COUNT"

if [[ "$POOL_COUNT" -eq 0 ]]; then
    die "No pools to deploy (--external-gateway-pools, --internal-gateway-pools, and --user-pools are all 0)"
fi

echo ""
echo "==> Pre-creating site and peering CRDs..."
ensure_site_gateway_resources "$SITE_NAME"
ensure_site_peering_resource "$SITE_NAME" "$CLUSTER_VNET_NAME"
ensure_primary_gateway_connectivity "$SITE_NAME" "${PRIMARY_SITE_NAME}extgw1"

# ------------------------------------------------------------------
# Build parameters file and deploy
# ------------------------------------------------------------------
echo ""
echo "==> Deploying site '$SITE_NAME'..."

PARAMS_FILE="${TEMP_DIR}/parameters.json"

# Build the parameters JSON object
# Use --rawfile for SSH key when provided via file to avoid ARG_MAX issues
SSH_JQ_FLAG=(--arg sshPublicKey "$SSH_PUBLIC_KEY")
if [[ -n "$SSH_KEY_FILE" ]]; then
    SSH_JQ_FLAG=(--rawfile sshPublicKey "$SSH_KEY_FILE")
fi

jq -n \
    --arg siteName "$SITE_NAME" \
    --arg ipv4Range "$IPV4_RANGE" \
    --arg ipv6Range "$IPV6_RANGE" \
    --argjson enableBastion "$ENABLE_BASTION" \
    --argjson peerPrimaryVnet "$PEER_PRIMARY_VNET" \
    --argjson portsPerVM "$PORTS_PER_VM" \
    --argjson outboundIpCount "$OUTBOUND_IP_COUNT" \
    --arg primaryVnetName "$CLUSTER_VNET_NAME" \
    --arg primaryVnetResourceGroup "$CLUSTER_VNET_RG" \
    --arg primaryVnetSubscriptionId "$(if [[ "$CROSS_SUBSCRIPTION" == true ]]; then echo "$PRIMARY_SUBSCRIPTION_ID"; fi)" \
    --slurpfile pools "$POOLS_FILE" \
    --slurpfile poolCustomDatas "$CUSTOMDATAS_FILE" \
    --arg identityId "$IDENTITY_ID" \
    --arg identityPrincipalId "$IDENTITY_PRINCIPAL_ID" \
    --arg adminPassword "$ADMIN_PASSWORD" \
    "${SSH_JQ_FLAG[@]}" \
    '{
        "$schema": "https://schema.management.azure.com/schemas/2019-04-01/deploymentParameters.json#",
        "contentVersion": "1.0.0.0",
        "parameters": {
            "siteName": {"value": $siteName},
            "ipv4Range": {"value": $ipv4Range},
            "ipv6Range": {"value": $ipv6Range},
            "enableBastion": {"value": $enableBastion},
            "peerPrimaryVnet": {"value": $peerPrimaryVnet},
            "portsPerVM": {"value": $portsPerVM},
            "outboundIpCount": {"value": $outboundIpCount},
            "primaryVnetName": {"value": $primaryVnetName},
            "primaryVnetResourceGroup": {"value": $primaryVnetResourceGroup},
            "primaryVnetSubscriptionId": {"value": $primaryVnetSubscriptionId},
            "pools": {"value": $pools[0]},
            "poolCustomDatas": {"value": $poolCustomDatas[0]},
            "identityId": {"value": $identityId},
            "identityPrincipalId": {"value": $identityPrincipalId},
            "adminPassword": {"value": $adminPassword},
            "sshPublicKey": {"value": $sshPublicKey}
        }
    }' > "$PARAMS_FILE"

BICEP_TEMPLATE="${SCRIPT_DIR}/templates/external-site.bicep"
AZ_OUTPUT_FORMAT="json"
if [[ -t 1 ]]; then
    AZ_OUTPUT_FORMAT="jsonc"
fi

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
    --name "deploy-${SITE_NAME}-$(date +%Y%m%d%H%M%S)" \
    --template-file "$BICEP_TEMPLATE" \
    --parameters "@${PARAMS_FILE}" \
    --output "$AZ_OUTPUT_FORMAT"

echo ""
echo "==> Site '$SITE_NAME' deployed successfully."
echo "    External gateway pools: $EXT_GATEWAY_POOLS"
echo "    Internal gateway pools: $INT_GATEWAY_POOLS"
echo "    User pools: $USER_POOLS"
echo "    Nodes will bootstrap into the cluster on first boot."
ensure_unmanaged_kube_proxy_daemonset
ensure_site_gateway_resources "$SITE_NAME"
ensure_site_peering_resource "$SITE_NAME" "$CLUSTER_VNET_NAME"
ensure_primary_gateway_connectivity "$SITE_NAME" "${PRIMARY_SITE_NAME}extgw1"

# ------------------------------------------------------------------
# Reimage pre-existing VMSS if requested
# ------------------------------------------------------------------
if [[ "$REIMAGE_EXISTING" == true && "$EXISTING_VMSS_COUNT" -gt 0 ]]; then
    echo ""
    echo "==> Reimaging pre-existing VMSS pools..."
    for vmss_name in "${!EXISTING_VMSS_PREFIX[@]}"; do
        echo "  Updating instances for $vmss_name..."
        az vmss update-instances "${RG_FLAG[@]}" -n "$vmss_name" --instance-ids '*'
        echo "  Reimaging $vmss_name (no-wait)..."
        az vmss reimage "${RG_FLAG[@]}" -n "$vmss_name" --instance-ids '*' --no-wait
    done
    echo "==> Reimage initiated for $EXISTING_VMSS_COUNT existing VMSS pool(s)."
fi
