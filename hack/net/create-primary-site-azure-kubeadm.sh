#!/bin/bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

#
# create-primary-site-azure-kubeadm.sh - Deploy a kubeadm-based primary site on Azure VMSSes
#
# Usage:
#   ./create-primary-site-azure-kubeadm.sh --name <site-name> --ipv4-cidr <cidr> --ssh-key <key|@path> [options]
#
# Required arguments:
#   -n, --name              Site name (a-z0-9- only, used as resource prefix)
#   --ipv4-cidr             IPv4 address range for the VNet (CIDR notation)
#   --pod-ipv4-cidr         Pod IPv4 CIDR
#   --service-ipv4-cidr     Service IPv4 CIDR
#   --ssh-key <key|@path>   SSH public key value, or @<path> to read from file
#
# Optional arguments:
#   -g, --resource-group    Azure resource group (uses az CLI default if not set)
#   -l, --location          Azure region (uses az CLI default if not set)
#   --kubernetes-version    Kubernetes version (e.g. v1.30.0; default: latest stable)
#   --ipv6-cidr             IPv6 range ("generate" for auto ULA /48, "" to disable; default: generate)
#   --pod-ipv6-cidr         Pod IPv6 CIDR
#   --service-ipv6-cidr     Service IPv6 CIDR
#   --control-vm-size       VM size for control plane pool (default: Standard_D2ads_v6)
#   --gateway-vm-size       VM size for extgw1 pool (default: Standard_D2ads_v6)
#   --user-vm-size          VM size for user1 pool (default: Standard_D2ads_v6)
#   --control-count         Node count for control plane pool (default: 3)
#   --gateway-count         Node count for extgw1 pool (default: 2)
#   --user-count            Node count for user1 pool (default: 2)
#   --no-bastion            Disable Azure Bastion deployment
#   --outbound-ip-count     Number of outbound IPv4 public IPs on LB (default: 1)
#   --password <pass>       Admin password for VMs
#   --infra-only            Deploy Azure resources only; skip in-cluster configuration
#   --skip-infra            Skip deploying Azure resources; only in-cluster configuration
#   -y, --yes               Skip confirmation prompt
#   -d, --debug             Write all temp files but pause before deploying for inspection
#   --help                  Show this help message
#
# usage-end

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

SITE_NAME=""
RESOURCE_GROUP=""
LOCATION=""
KUBERNETES_VERSION=""
IPV4_CIDR=""
IPV6_CIDR="generate"
POD_IPV4_CIDR=""
POD_IPV6_CIDR=""
SERVICE_IPV4_CIDR=""
SERVICE_IPV6_CIDR=""
CONTROL_VM_SIZE="Standard_D2ads_v6"
GATEWAY_VM_SIZE="Standard_D2ads_v6"
USER_VM_SIZE="Standard_D2ads_v6"
CONTROL_COUNT=3
GATEWAY_COUNT=2
USER_COUNT=2
ENABLE_BASTION=true
OUTBOUND_IP_COUNT=1
ADMIN_PASSWORD=""
SSH_KEY=""
INFRA_ONLY=false
SKIP_INFRA=false
AUTO_CONFIRM=false
DEBUG_MODE=false

usage() {
    awk 'NR >= 3 { if ($0 ~ /usage-end/) exit; print substr($0, 3)}' "$0"
}

die() {
    echo "Error: $1" >&2
    echo "" >&2
    usage >&2
    exit 1
}

get_site_node_cidrs() {
    local vnet_name="$1"
    local subnet_json=""
    if subnet_json="$(az network vnet subnet show "${RG_FLAG[@]}" --vnet-name "$vnet_name" -n default -o json 2>/dev/null)"; then
        jq -r '.addressPrefixes // [ .addressPrefix ] | .[]' <<<"$subnet_json"
        return
    fi

    python3 - "$IPV4_CIDR" "$IPV6_CIDR" <<'PY'
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
    local gateway_pool_name="$2"
    local -a node_cidrs=()
    local -a pod_cidrs=()
    local -a service_cidrs=()

    mapfile -t node_cidrs < <(get_site_node_cidrs "${site_name}-vnet")
    [[ "${#node_cidrs[@]}" -gt 0 ]] || die "Failed to resolve node CIDRs for site ${site_name}"
    [[ -n "${POD_IPV4_CIDR}" ]] && pod_cidrs+=("${POD_IPV4_CIDR}")
    [[ -n "${POD_IPV6_CIDR}" ]] && pod_cidrs+=("${POD_IPV6_CIDR}")
    [[ -n "${SERVICE_IPV4_CIDR}" ]] && service_cidrs+=("${SERVICE_IPV4_CIDR}")
    [[ -n "${SERVICE_IPV6_CIDR}" ]] && service_cidrs+=("${SERVICE_IPV6_CIDR}")

    echo "==> Ensuring Site '${site_name}' and GatewayPool '${gateway_pool_name}' CRDs..."
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

    cat <<EOF | kubectl apply -f -
apiVersion: net.unbounded-kube.io/v1alpha1
kind: GatewayPool
metadata:
  name: ${gateway_pool_name}
spec:
  type: External
  nodeSelector:
    net.unbounded-kube.io/agentpool: "${gateway_pool_name}"
$(if [[ "${#service_cidrs[@]}" -gt 0 ]]; then
    echo "  routedCidrs:"
    for cidr in "${service_cidrs[@]}"; do
        echo "    - \"${cidr}\""
    done
fi)
EOF

    cat <<EOF | kubectl apply -f -
apiVersion: net.unbounded-kube.io/v1alpha1
kind: SiteGatewayPoolAssignment
metadata:
  name: ${site_name}-${gateway_pool_name}
spec:
  sites:
    - "${site_name}"
  gatewayPools:
    - "${gateway_pool_name}"
EOF
}

# Parse arguments
while [[ $# -gt 0 ]]; do
    case "$1" in
        -n|--name)            SITE_NAME="$2"; shift 2 ;;
        -g|--resource-group)  RESOURCE_GROUP="$2"; shift 2 ;;
        -l|--location)        LOCATION="$2"; shift 2 ;;
        --kubernetes-version) KUBERNETES_VERSION="$2"; shift 2 ;;
        --ipv4-cidr)          IPV4_CIDR="$2"; shift 2 ;;
        --ipv6-cidr)          IPV6_CIDR="$2"; shift 2 ;;
        --pod-ipv4-cidr)      POD_IPV4_CIDR="$2"; shift 2 ;;
        --pod-ipv6-cidr)      POD_IPV6_CIDR="$2"; shift 2 ;;
        --service-ipv4-cidr)  SERVICE_IPV4_CIDR="$2"; shift 2 ;;
        --service-ipv6-cidr)  SERVICE_IPV6_CIDR="$2"; shift 2 ;;
        --control-vm-size)    CONTROL_VM_SIZE="$2"; shift 2 ;;
        --gateway-vm-size)    GATEWAY_VM_SIZE="$2"; shift 2 ;;
        --user-vm-size)       USER_VM_SIZE="$2"; shift 2 ;;
        --control-count)      CONTROL_COUNT="$2"; shift 2 ;;
        --gateway-count)      GATEWAY_COUNT="$2"; shift 2 ;;
        --user-count)         USER_COUNT="$2"; shift 2 ;;
        --no-bastion)         ENABLE_BASTION=false; shift ;;
        --outbound-ip-count) OUTBOUND_IP_COUNT="$2"; shift 2 ;;
        --password)           ADMIN_PASSWORD="$2"; shift 2 ;;
        --ssh-key)            SSH_KEY="$2"; shift 2 ;;
        --infra-only)         INFRA_ONLY=true; shift ;;
        --skip-infra)         SKIP_INFRA=true; shift ;;
        -y|--yes)             AUTO_CONFIRM=true; shift ;;
        -d|--debug)           DEBUG_MODE=true; shift ;;
        --help)               usage; exit 0 ;;
        *)                    die "Unknown argument: $1" ;;
    esac
done

# Validate required arguments
[[ -n "$SITE_NAME" ]] || die "--name is required"
[[ -n "$IPV4_CIDR" ]] || die "--ipv4-cidr is required"
[[ -n "$POD_IPV4_CIDR" ]] || die "--pod-ipv4-cidr is required"
[[ -n "$SERVICE_IPV4_CIDR" ]] || die "--service-ipv4-cidr is required"
[[ -n "$SSH_KEY" || -n "$ADMIN_PASSWORD" ]] || die "at least one of --ssh-key or --password is required"

if [[ ! "$SITE_NAME" =~ ^[a-z0-9-]+$ ]]; then
    die "--name must contain only lowercase letters, digits, and hyphens (a-z0-9-)"
fi

# Resolve resource group
if [[ -z "$RESOURCE_GROUP" ]]; then
    RESOURCE_GROUP="$(az config get --query 'defaults[?name==`group`].value' -o tsv --only-show-errors)"
    [[ -n "$RESOURCE_GROUP" ]] || die "--resource-group is required (no az CLI default configured)"
fi
RG_FLAG=(-g "$RESOURCE_GROUP")

# Resolve location
if [[ -z "$LOCATION" ]]; then
    LOCATION="$(az config get --query 'defaults[?name==`location`].value' -o tsv --only-show-errors)"
fi
if [[ -z "$LOCATION" ]]; then
    LOCATION="$(az group show -g "$RESOURCE_GROUP" --query location -o tsv)"
fi

# Resolve Kubernetes version
if [[ -z "$KUBERNETES_VERSION" ]]; then
    echo "==> Detecting latest stable Kubernetes version..."
    KUBERNETES_VERSION="$(curl -fsSL https://dl.k8s.io/release/stable.txt)"
    echo "    Using: $KUBERNETES_VERSION"
fi
# Normalize: ensure leading 'v'
if [[ ! "$KUBERNETES_VERSION" =~ ^v ]]; then
    KUBERNETES_VERSION="v${KUBERNETES_VERSION}"
fi
# If only major.minor given (e.g. v1.34), resolve the latest patch release
if [[ "$KUBERNETES_VERSION" =~ ^v[0-9]+\.[0-9]+$ ]]; then
    echo "==> Resolving latest patch version for ${KUBERNETES_VERSION}..."
    RESOLVED_VERSION="$(curl -fsSL "https://dl.k8s.io/release/stable-${KUBERNETES_VERSION#v}.txt" 2>/dev/null)" || true
    if [[ -n "$RESOLVED_VERSION" ]]; then
        KUBERNETES_VERSION="$RESOLVED_VERSION"
        echo "    Resolved to: $KUBERNETES_VERSION"
    else
        # Fallback: append .0
        KUBERNETES_VERSION="${KUBERNETES_VERSION}.0"
        echo "    Could not resolve; using: $KUBERNETES_VERSION"
    fi
fi

# Compute cluster DNS IP: 10th IP in service CIDR
CLUSTER_DNS="$(python3 -c "import ipaddress,sys; n=ipaddress.ip_network(sys.argv[1],strict=False); print(n[10])" "$SERVICE_IPV4_CIDR")"

# Build combined pod/service CIDRs for kubeadm
POD_CIDR="$POD_IPV4_CIDR"
SERVICE_CIDR="$SERVICE_IPV4_CIDR"
if [[ -n "$POD_IPV6_CIDR" ]]; then
    POD_CIDR="${POD_IPV4_CIDR},${POD_IPV6_CIDR}"
fi
if [[ -n "$SERVICE_IPV6_CIDR" ]]; then
    SERVICE_CIDR="${SERVICE_IPV4_CIDR},${SERVICE_IPV6_CIDR}"
fi

# Resolve SSH key
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
        [[ "$SSH_KEY" =~ $re ]] || die "SSH key value does not start with ssh-rsa or ssh-ed25519"
        SSH_PUBLIC_KEY="$SSH_KEY"
    fi
fi

SUBSCRIPTION_ID="$(az account show --query id -o tsv)"
TENANT_ID="$(az account show --query tenantId -o tsv)"

# Network resource names (matching external-site-networking.bicep conventions)
VNET_NAME="${SITE_NAME}-vnet"
VNET_RG="$RESOURCE_GROUP"
SUBNET_NAME="default"
SECURITY_GROUP_NAME="${SITE_NAME}-nsg"
ROUTE_TABLE_NAME=""

# Pool names (following add-azure-site.sh patterns)
CONTROL_POOL_NAME="${SITE_NAME}control1"
GATEWAY_POOL_NAME="${SITE_NAME}extgw1"
USER_POOL_NAME="${SITE_NAME}user1"

echo ""
echo "==> Deployment plan for kubeadm primary site '$SITE_NAME':"
echo "    Resource group:        $RESOURCE_GROUP"
echo "    Location:              $LOCATION"
echo "    Kubernetes version:    $KUBERNETES_VERSION"
echo "    IPv4 CIDR:             $IPV4_CIDR"
echo "    IPv6 CIDR:             $IPV6_CIDR"
echo "    Pod CIDR:              $POD_CIDR"
echo "    Service CIDR:          $SERVICE_CIDR"
echo "    Cluster DNS:           $CLUSTER_DNS"
echo "    Bastion:               $ENABLE_BASTION"
echo "    Outbound IP count:     $OUTBOUND_IP_COUNT"
echo "    ${CONTROL_POOL_NAME}:           size=$CONTROL_VM_SIZE count=$CONTROL_COUNT (control plane, API server LB)"
echo "    ${GATEWAY_POOL_NAME}:           size=$GATEWAY_VM_SIZE count=$GATEWAY_COUNT (external gateway, per-VM public IPs)"
echo "    ${USER_POOL_NAME}:           size=$USER_VM_SIZE count=$USER_COUNT (worker)"
echo ""

if [[ "$AUTO_CONFIRM" != true ]]; then
    read -erp "Proceed with deployment? [y/N] " confirm
    if [[ ! "$confirm" =~ ^[Yy]$ ]]; then
        echo "Aborted."
        exit 0
    fi
fi

# ------------------------------------------------------------------
# Check for existing running cluster
# ------------------------------------------------------------------
# If the API server public IP already exists and is healthy, skip
# infrastructure deployment and PKI generation to avoid invalidating
# the running cluster's certificates.
API_SERVER_PORT=6443
EXISTING_CLUSTER=false
if [[ "$SKIP_INFRA" != true ]]; then
    EXISTING_API_IP="$(az network public-ip show "${RG_FLAG[@]}" -n "${SITE_NAME}-apiserver-pip" --query ipAddress -o tsv 2>/dev/null)" || true
    if [[ -n "$EXISTING_API_IP" ]]; then
        EXISTING_HEALTH="$(curl -sk --connect-timeout 5 "https://${EXISTING_API_IP}:${API_SERVER_PORT}/healthz" 2>/dev/null)" || true
        if [[ "$EXISTING_HEALTH" == "ok" ]]; then
            echo ""
            echo "==> Existing cluster detected at https://${EXISTING_API_IP}:${API_SERVER_PORT} (healthy)"
            echo "    Skipping infrastructure and PKI generation to preserve running cluster."
            EXISTING_CLUSTER=true
            API_SERVER_IP="$EXISTING_API_IP"
        fi
    fi
fi

# ------------------------------------------------------------------
# Pre-generate PKI and bootstrap token
# ------------------------------------------------------------------

umask 077
TEMP_DIR="$(mktemp -d "${SCRIPT_DIR}/create-kubeadm-site.XXXXXX.tmp")"
if [[ "$DEBUG_MODE" == true ]]; then
    echo "==> Debug mode: temp directory preserved at $TEMP_DIR"
    trap '' EXIT
else
    trap 'rm -rf "$TEMP_DIR"' EXIT
fi

if [[ "$EXISTING_CLUSTER" != true ]]; then
    echo ""
    echo "==> Generating PKI material and bootstrap token..."

    # CA key pair
    openssl genrsa -out "${TEMP_DIR}/ca.key" 4096 2>/dev/null
    openssl req -x509 -new -nodes -key "${TEMP_DIR}/ca.key" \
        -sha256 -days 3650 -out "${TEMP_DIR}/ca.crt" \
        -subj "/CN=kubernetes" 2>/dev/null
    CA_CERT_BASE64="$(base64 -w0 < "${TEMP_DIR}/ca.crt")"
    CA_KEY_BASE64="$(base64 -w0 < "${TEMP_DIR}/ca.key")"
    echo "    CA certificate generated"

    # Service account key pair
    openssl genrsa -out "${TEMP_DIR}/sa.key" 4096 2>/dev/null
    openssl rsa -in "${TEMP_DIR}/sa.key" -pubout -out "${TEMP_DIR}/sa.pub" 2>/dev/null
    SA_KEY_BASE64="$(base64 -w0 < "${TEMP_DIR}/sa.key")"
    SA_PUB_BASE64="$(base64 -w0 < "${TEMP_DIR}/sa.pub")"
    echo "    Service account key pair generated"

    # Bootstrap token (format: [a-z0-9]{6}.[a-z0-9]{16})
    TOKEN_ID="$(openssl rand -hex 16 | cut -c1-6)"
    TOKEN_SECRET="$(openssl rand -hex 16 | cut -c1-16)"
    BOOTSTRAP_TOKEN="${TOKEN_ID}.${TOKEN_SECRET}"
    echo "    Bootstrap token generated"

    # Certificate key for kubeadm upload-certs (32-byte hex string)
    CERTIFICATE_KEY="$(openssl rand -hex 32)"
    echo "    Certificate key generated"
fi

# ------------------------------------------------------------------
# Generate customdata for all pools
# ------------------------------------------------------------------
echo ""
echo "==> Generating customdata for all pools..."

KUBEADM_INIT_TEMPLATE="${SCRIPT_DIR}/userdata-kubeadm-init.yaml"
BOOTSTRAP_TEMPLATE="${SCRIPT_DIR}/userdata-bootstrap.yaml"

[[ -f "$KUBEADM_INIT_TEMPLATE" ]] || die "userdata-kubeadm-init.yaml not found at $KUBEADM_INIT_TEMPLATE"
[[ -f "$BOOTSTRAP_TEMPLATE" ]] || die "userdata-bootstrap.yaml not found at $BOOTSTRAP_TEMPLATE"

# The API server IP is not known until after Bicep deploys the public IP.
# We use a placeholder that we will replace after the Bicep deployment outputs it.
# For the control plane nodes, we use the placeholder directly.
# For worker/gateway nodes, we need the IP for the bootstrap template's __API_SERVER__ field.
API_SERVER_PORT=6443

# write_common_sed_script <output-file> <scale_set_name> [extra_labels]
# Writes a sed script file with common substitutions. Avoids word-splitting
# issues that arise when passing large base64 values through shell expansion.
write_common_sed_script() {
    local script_file="$1"
    local scale_set_name="$2"
    local extra_labels="${3:-}"
    cat > "$script_file" <<SEDEOF
/^[[:space:]]*#/!s|__KUBERNETES_VERSION__|${KUBERNETES_VERSION}|g
/^[[:space:]]*#/!s|__BOOTSTRAP_TOKEN__|${BOOTSTRAP_TOKEN}|g
/^[[:space:]]*#/!s|__CLUSTER_DNS__|${CLUSTER_DNS}|g
/^[[:space:]]*#/!s|__CLUSTER_RG__|${RESOURCE_GROUP}|g
/^[[:space:]]*#/!s|__CA_CERT_BASE64__|${CA_CERT_BASE64}|g
/^[[:space:]]*#/!s|__TENANT_ID__|${TENANT_ID}|g
/^[[:space:]]*#/!s|__SUBSCRIPTION_ID__|${SUBSCRIPTION_ID}|g
/^[[:space:]]*#/!s|__SECURITY_GROUP_NAME__|${SECURITY_GROUP_NAME}|g
/^[[:space:]]*#/!s|__VNET_NAME__|${VNET_NAME}|g
/^[[:space:]]*#/!s|__VNET_RG__|${VNET_RG}|g
/^[[:space:]]*#/!s|__SUBNET_NAME__|${SUBNET_NAME}|g
/^[[:space:]]*#/!s|__ROUTE_TABLE_NAME__|${ROUTE_TABLE_NAME}|g
/^[[:space:]]*#/!s|__SCALE_SET_NAME__|${scale_set_name}|g
/^[[:space:]]*#/!s|__EXTRA_NODE_LABELS__|${extra_labels}|g
/^[[:space:]]*#/!s|__CUSTOMDATA_GENERATED__|$(date -u +%Y%m%d%H%M%SZ)|g
SEDEOF
}

# Customdata is generated in Phase 2 after the identity is created.
echo "  Customdata will be generated after identity is created (Phase 2)"

# ------------------------------------------------------------------
# Build pools JSON (without customdata initially)
# ------------------------------------------------------------------

POOLS_FILE="${TEMP_DIR}/pools.json"
CUSTOMDATAS_FILE="${TEMP_DIR}/pool-customdatas.json"

get_hostname_prefix() {
    local pool_name="$1"
    echo "ext-${pool_name}-vmss-"
}

# ------------------------------------------------------------------
# Phase 1: Deploy infrastructure (identity, networking, API server LB)
# ------------------------------------------------------------------

if [[ "$SKIP_INFRA" != true && "$EXISTING_CLUSTER" != true ]]; then
    echo ""
    echo "==> Phase 1: Deploying infrastructure (identity, networking, LB)..."

    # Build the parameters JSON
    SSH_JQ_FLAG=(--arg sshPublicKey "$SSH_PUBLIC_KEY")
    if [[ -n "$SSH_KEY_FILE" ]]; then
        SSH_JQ_FLAG=(--rawfile sshPublicKey "$SSH_KEY_FILE")
    fi

    # For phase 1, we deploy with empty pools to create identity + networking + API LB
    echo '[]' > "$POOLS_FILE"
    echo '{}' > "$CUSTOMDATAS_FILE"

    jq -n \
        --arg siteName "$SITE_NAME" \
        --arg ipv4Range "$IPV4_CIDR" \
        --arg ipv6Range "$IPV6_CIDR" \
        --argjson enableBastion "$ENABLE_BASTION" \
        --argjson outboundIpCount "$OUTBOUND_IP_COUNT" \
        --slurpfile pools "$POOLS_FILE" \
        --slurpfile poolCustomDatas "$CUSTOMDATAS_FILE" \
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
                "outboundIpCount": {"value": $outboundIpCount},
                "pools": {"value": $pools[0]},
                "poolCustomDatas": {"value": $poolCustomDatas[0]},
                "adminPassword": {"value": $adminPassword},
                "sshPublicKey": {"value": $sshPublicKey}
            }
        }' > "${TEMP_DIR}/phase1-params.json"

    BICEP_TEMPLATE="${SCRIPT_DIR}/templates/primary-site-kubeadm.bicep"
    AZ_OUTPUT_FORMAT="json"
    if [[ -t 1 ]]; then
        AZ_OUTPUT_FORMAT="jsonc"
    fi

    az deployment group create \
        "${RG_FLAG[@]}" \
        --name "deploy-kubeadm-${SITE_NAME}-phase1-$(date +%Y%m%d%H%M%S)" \
        --template-file "$BICEP_TEMPLATE" \
        --parameters "@${TEMP_DIR}/phase1-params.json" \
        --output "$AZ_OUTPUT_FORMAT"

    echo ""
    echo "==> Phase 1 complete: infrastructure deployed."
fi

# ------------------------------------------------------------------
# Resolve identity and API server IP from deployed resources
# ------------------------------------------------------------------
echo ""
echo "==> Resolving deployed resource properties..."

IDENTITY_ID="$(az identity show "${RG_FLAG[@]}" -n "${SITE_NAME}-identity" --query id -o tsv)"
IDENTITY_CLIENT_ID="$(az identity show "${RG_FLAG[@]}" -n "${SITE_NAME}-identity" --query clientId -o tsv)"
[[ -n "$IDENTITY_ID" ]] || die "Could not resolve identity ${SITE_NAME}-identity"
echo "    Identity: $IDENTITY_ID"
echo "    Identity client ID: $IDENTITY_CLIENT_ID"

API_SERVER_IP="$(az network public-ip show "${RG_FLAG[@]}" -n "${SITE_NAME}-apiserver-pip" --query ipAddress -o tsv)"
[[ -n "$API_SERVER_IP" ]] || die "Could not resolve API server public IP"
echo "    API server IP: $API_SERVER_IP"

# ------------------------------------------------------------------
# Phase 2: Generate customdata and deploy VMSSes
# ------------------------------------------------------------------
if [[ "$SKIP_INFRA" != true && "$EXISTING_CLUSTER" != true ]]; then
    echo ""
    echo "==> Phase 2: Generating customdata and deploying VMSSes..."

    # Generate control plane customdata
    echo "  Pool: ${CONTROL_POOL_NAME} (control plane, sku=$CONTROL_VM_SIZE, count=$CONTROL_COUNT)"
    write_common_sed_script "${TEMP_DIR}/control.sed" "${SITE_NAME}-${CONTROL_POOL_NAME}" ""
    cat >> "${TEMP_DIR}/control.sed" <<SEDEOF
/^[[:space:]]*#/!s|__CA_KEY_BASE64__|${CA_KEY_BASE64}|g
/^[[:space:]]*#/!s|__SA_KEY_BASE64__|${SA_KEY_BASE64}|g
/^[[:space:]]*#/!s|__SA_PUB_BASE64__|${SA_PUB_BASE64}|g
/^[[:space:]]*#/!s|__API_SERVER_IP__|${API_SERVER_IP}|g
/^[[:space:]]*#/!s|__API_SERVER_PORT__|${API_SERVER_PORT}|g
/^[[:space:]]*#/!s|__POD_CIDR__|${POD_CIDR}|g
/^[[:space:]]*#/!s|__SERVICE_CIDR__|${SERVICE_CIDR}|g
/^[[:space:]]*#/!s|__CERTIFICATE_KEY__|${CERTIFICATE_KEY}|g
/^[[:space:]]*#/!s|__KUBELET_CLIENT_ID__|${IDENTITY_CLIENT_ID}|g
SEDEOF
    sed -f "${TEMP_DIR}/control.sed" < "$KUBEADM_INIT_TEMPLATE" | base64 -w0 > "${TEMP_DIR}/${CONTROL_POOL_NAME}.cd"

    # Generate gateway customdata (uses standard bootstrap template)
    echo "  Pool: ${GATEWAY_POOL_NAME} (external gateway, sku=$GATEWAY_VM_SIZE, count=$GATEWAY_COUNT)"
    write_common_sed_script "${TEMP_DIR}/gateway.sed" "${SITE_NAME}-${GATEWAY_POOL_NAME}" ",net.unbounded-kube.io/gateway=true"
    cat >> "${TEMP_DIR}/gateway.sed" <<SEDEOF
/^[[:space:]]*#/!s|__API_SERVER__|${API_SERVER_IP}:${API_SERVER_PORT}|g
/^[[:space:]]*#/!s|__KUBELET_CLIENT_ID__|${IDENTITY_CLIENT_ID}|g
SEDEOF
    sed -f "${TEMP_DIR}/gateway.sed" < "$BOOTSTRAP_TEMPLATE" | base64 -w0 > "${TEMP_DIR}/${GATEWAY_POOL_NAME}.cd"

    # Generate worker customdata (uses standard bootstrap template)
    echo "  Pool: ${USER_POOL_NAME} (worker, sku=$USER_VM_SIZE, count=$USER_COUNT)"
    write_common_sed_script "${TEMP_DIR}/user.sed" "${SITE_NAME}-${USER_POOL_NAME}" ""
    cat >> "${TEMP_DIR}/user.sed" <<SEDEOF
/^[[:space:]]*#/!s|__API_SERVER__|${API_SERVER_IP}:${API_SERVER_PORT}|g
/^[[:space:]]*#/!s|__KUBELET_CLIENT_ID__|${IDENTITY_CLIENT_ID}|g
SEDEOF
    sed -f "${TEMP_DIR}/user.sed" < "$BOOTSTRAP_TEMPLATE" | base64 -w0 > "${TEMP_DIR}/${USER_POOL_NAME}.cd"

    # Build pools JSON
    CONTROL_PREFIX="$(get_hostname_prefix "$CONTROL_POOL_NAME")"
    GATEWAY_PREFIX="$(get_hostname_prefix "$GATEWAY_POOL_NAME")"
    USER_PREFIX="$(get_hostname_prefix "$USER_POOL_NAME")"

    jq -n \
        --arg cpName "$CONTROL_POOL_NAME" \
        --arg cpSku "$CONTROL_VM_SIZE" \
        --argjson cpCount "$CONTROL_COUNT" \
        --arg cpPrefix "$CONTROL_PREFIX" \
        --arg gwName "$GATEWAY_POOL_NAME" \
        --arg gwSku "$GATEWAY_VM_SIZE" \
        --argjson gwCount "$GATEWAY_COUNT" \
        --arg gwPrefix "$GATEWAY_PREFIX" \
        --arg usName "$USER_POOL_NAME" \
        --arg usSku "$USER_VM_SIZE" \
        --argjson usCount "$USER_COUNT" \
        --arg usPrefix "$USER_PREFIX" \
        '[
            {
                "name": $cpName,
                "sku": $cpSku,
                "instanceCount": $cpCount,
                "computerNamePrefix": $cpPrefix,
                "enablePublicIPPerVM": false,
                "isControlPlane": true,
                "allowedHostPorts": "6443/tcp",
                "allowedHostPortsPriority": 199
            },
            {
                "name": $gwName,
                "sku": $gwSku,
                "instanceCount": $gwCount,
                "computerNamePrefix": $gwPrefix,
                "enablePublicIPPerVM": true,
                "isControlPlane": false,
                "allowedHostPorts": "51820-51999/udp",
                "allowedHostPortsPriority": 200
            },
            {
                "name": $usName,
                "sku": $usSku,
                "instanceCount": $usCount,
                "computerNamePrefix": $usPrefix,
                "enablePublicIPPerVM": false,
                "isControlPlane": false
            }
        ]' > "$POOLS_FILE"

    # Build customdata map
    jq -n \
        --rawfile cp "${TEMP_DIR}/${CONTROL_POOL_NAME}.cd" \
        --rawfile gw "${TEMP_DIR}/${GATEWAY_POOL_NAME}.cd" \
        --rawfile us "${TEMP_DIR}/${USER_POOL_NAME}.cd" \
        --arg cpName "$CONTROL_POOL_NAME" \
        --arg gwName "$GATEWAY_POOL_NAME" \
        --arg usName "$USER_POOL_NAME" \
        '{($cpName): $cp, ($gwName): $gw, ($usName): $us}' > "$CUSTOMDATAS_FILE"

    SSH_JQ_FLAG=(--arg sshPublicKey "$SSH_PUBLIC_KEY")
    if [[ -n "$SSH_KEY_FILE" ]]; then
        SSH_JQ_FLAG=(--rawfile sshPublicKey "$SSH_KEY_FILE")
    fi

    jq -n \
        --arg siteName "$SITE_NAME" \
        --arg ipv4Range "$IPV4_CIDR" \
        --arg ipv6Range "$IPV6_CIDR" \
        --argjson enableBastion "$ENABLE_BASTION" \
        --argjson outboundIpCount "$OUTBOUND_IP_COUNT" \
        --slurpfile pools "$POOLS_FILE" \
        --slurpfile poolCustomDatas "$CUSTOMDATAS_FILE" \
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
                "outboundIpCount": {"value": $outboundIpCount},
                "pools": {"value": $pools[0]},
                "poolCustomDatas": {"value": $poolCustomDatas[0]},
                "adminPassword": {"value": $adminPassword},
                "sshPublicKey": {"value": $sshPublicKey}
            }
        }' > "${TEMP_DIR}/phase2-params.json"

    BICEP_TEMPLATE="${SCRIPT_DIR}/templates/primary-site-kubeadm.bicep"
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
        --name "deploy-kubeadm-${SITE_NAME}-phase2-$(date +%Y%m%d%H%M%S)" \
        --template-file "$BICEP_TEMPLATE" \
        --parameters "@${TEMP_DIR}/phase2-params.json" \
        --output "$AZ_OUTPUT_FORMAT"

    echo ""
    echo "==> Phase 2 complete: VMSSes deployed."

    if [[ "$INFRA_ONLY" == true ]]; then
        echo "==> --infra-only: skipping in-cluster configuration."
        echo "    API server endpoint: https://${API_SERVER_IP}:${API_SERVER_PORT}"
        exit 0
    fi
fi

# ------------------------------------------------------------------
# Wait for API server
# ------------------------------------------------------------------
echo ""
echo "==> Waiting for API server at https://${API_SERVER_IP}:${API_SERVER_PORT}/healthz ..."

MAX_WAIT=600
WAIT_INTERVAL=10
elapsed=0
while (( elapsed < MAX_WAIT )); do
    if curl -sk --connect-timeout 5 "https://${API_SERVER_IP}:${API_SERVER_PORT}/healthz" 2>/dev/null | grep -q "ok"; then
        echo "    API server is healthy (waited ${elapsed}s)"
        break
    fi
    echo "    Not ready yet (${elapsed}s elapsed)..."
    sleep "$WAIT_INTERVAL"
    elapsed=$((elapsed + WAIT_INTERVAL))
done

if (( elapsed >= MAX_WAIT )); then
    die "API server did not become healthy within ${MAX_WAIT}s"
fi

# ------------------------------------------------------------------
# Build kubeconfig from pre-generated CA
# ------------------------------------------------------------------
echo ""
echo "==> Building kubeconfig..."

KUBECONFIG_FILE="${TEMP_DIR}/kubeconfig"
if [[ "$EXISTING_CLUSTER" == true ]]; then
    # Extract the cluster CA certificate from the running control plane.
    # The full admin.conf is too large for run-command output, so we extract
    # just the CA cert and generate a fresh admin kubeconfig locally.
    echo "    Extracting CA certificate from running control plane..."
    CA_PEM="$(az vmss run-command invoke "${RG_FLAG[@]}" -n "${SITE_NAME}-${CONTROL_POOL_NAME}" --instance-id 0 \
        --command-id RunShellScript \
        --scripts "cat /etc/kubernetes/pki/ca.crt" \
        --query 'value[0].message' -o tsv 2>/dev/null \
        | sed -n '/BEGIN CERTIFICATE/,/END CERTIFICATE/p')"
    echo "$CA_PEM" > "${TEMP_DIR}/ca.crt"
    CA_CERT_BASE64="$(base64 -w0 < "${TEMP_DIR}/ca.crt")"

    CA_KEY_PEM="$(az vmss run-command invoke "${RG_FLAG[@]}" -n "${SITE_NAME}-${CONTROL_POOL_NAME}" --instance-id 0 \
        --command-id RunShellScript \
        --scripts "cat /etc/kubernetes/pki/ca.key" \
        --query 'value[0].message' -o tsv 2>/dev/null \
        | sed -n '/BEGIN.*KEY/,/END.*KEY/p')"
    echo "$CA_KEY_PEM" > "${TEMP_DIR}/ca.key"

    # Generate admin client cert signed by the cluster's CA
    openssl genrsa -out "${TEMP_DIR}/admin.key" 4096 2>/dev/null
    openssl req -new -key "${TEMP_DIR}/admin.key" \
        -subj "/O=system:masters/CN=kubernetes-admin" \
        -out "${TEMP_DIR}/admin.csr" 2>/dev/null
    openssl x509 -req -in "${TEMP_DIR}/admin.csr" \
        -CA "${TEMP_DIR}/ca.crt" -CAkey "${TEMP_DIR}/ca.key" \
        -CAcreateserial -out "${TEMP_DIR}/admin.crt" \
        -days 365 -sha256 2>/dev/null

    ADMIN_CERT_BASE64="$(base64 -w0 < "${TEMP_DIR}/admin.crt")"
    ADMIN_KEY_BASE64="$(base64 -w0 < "${TEMP_DIR}/admin.key")"

    cat > "$KUBECONFIG_FILE" <<EOF
apiVersion: v1
kind: Config
clusters:
- cluster:
    certificate-authority-data: ${CA_CERT_BASE64}
    server: https://${API_SERVER_IP}:${API_SERVER_PORT}
  name: ${SITE_NAME}
contexts:
- context:
    cluster: ${SITE_NAME}
    user: ${SITE_NAME}-admin
  name: ${SITE_NAME}
current-context: ${SITE_NAME}
users:
- name: ${SITE_NAME}-admin
  user:
    client-certificate-data: ${ADMIN_CERT_BASE64}
    client-key-data: ${ADMIN_KEY_BASE64}
EOF
else
    # Generate an admin client certificate signed by the pre-generated CA
    openssl genrsa -out "${TEMP_DIR}/admin.key" 4096 2>/dev/null
    openssl req -new -key "${TEMP_DIR}/admin.key" \
        -subj "/O=system:masters/CN=kubernetes-admin" \
        -out "${TEMP_DIR}/admin.csr" 2>/dev/null
    openssl x509 -req -in "${TEMP_DIR}/admin.csr" \
        -CA "${TEMP_DIR}/ca.crt" -CAkey "${TEMP_DIR}/ca.key" \
        -CAcreateserial -out "${TEMP_DIR}/admin.crt" \
        -days 365 -sha256 2>/dev/null

    ADMIN_CERT_BASE64="$(base64 -w0 < "${TEMP_DIR}/admin.crt")"
    ADMIN_KEY_BASE64="$(base64 -w0 < "${TEMP_DIR}/admin.key")"

    cat > "$KUBECONFIG_FILE" <<EOF
apiVersion: v1
kind: Config
clusters:
- cluster:
    certificate-authority-data: ${CA_CERT_BASE64}
    server: https://${API_SERVER_IP}:${API_SERVER_PORT}
  name: ${SITE_NAME}
contexts:
- context:
    cluster: ${SITE_NAME}
    user: ${SITE_NAME}-admin
  name: ${SITE_NAME}
current-context: ${SITE_NAME}
users:
- name: ${SITE_NAME}-admin
  user:
    client-certificate-data: ${ADMIN_CERT_BASE64}
    client-key-data: ${ADMIN_KEY_BASE64}
EOF
fi

# Merge into the user's kubeconfig
KUBECONFIG_DEST="${HOME}/.kube/config"
if [[ -f "$KUBECONFIG_DEST" ]]; then
    KUBECONFIG="${KUBECONFIG_FILE}:${KUBECONFIG_DEST}" kubectl config view --flatten > "${TEMP_DIR}/merged-kubeconfig"
    cp "${TEMP_DIR}/merged-kubeconfig" "$KUBECONFIG_DEST"
    chmod 600 "$KUBECONFIG_DEST"
else
    mkdir -p "$(dirname "$KUBECONFIG_DEST")"
    cp "$KUBECONFIG_FILE" "$KUBECONFIG_DEST"
    chmod 600 "$KUBECONFIG_DEST"
fi
kubectl config use-context "${SITE_NAME}"
echo "    kubeconfig context '${SITE_NAME}' set as current"

# ------------------------------------------------------------------
# Wait for nodes and approve CSRs
# ------------------------------------------------------------------
echo ""
echo "==> Waiting for nodes to register..."

NODES_EXPECTED=$((CONTROL_COUNT + GATEWAY_COUNT + USER_COUNT))
NODE_WAIT=600
NODE_INTERVAL=15
elapsed=0
while (( elapsed < NODE_WAIT )); do
    READY_NODES="$(kubectl get nodes --no-headers 2>/dev/null | wc -l)" || true
    if (( READY_NODES >= NODES_EXPECTED )); then
        echo "    All $NODES_EXPECTED nodes registered"
        break
    fi

    # Approve any pending CSRs
    kubectl get csr -o jsonpath='{range .items[?(@.status.conditions==null)]}{.metadata.name}{"\n"}{end}' 2>/dev/null | while read -r csr; do
        [[ -n "$csr" ]] && kubectl certificate approve "$csr" 2>/dev/null && echo "    Approved CSR: $csr" || true
    done

    echo "    ${READY_NODES}/${NODES_EXPECTED} nodes registered (${elapsed}s elapsed)..."
    sleep "$NODE_INTERVAL"
    elapsed=$((elapsed + NODE_INTERVAL))
done

echo "==> Current nodes:"
kubectl get nodes -o wide

# ------------------------------------------------------------------
# In-cluster configuration
# ------------------------------------------------------------------
echo ""
echo "==> Running in-cluster configuration..."

echo "==> Verifying kube-proxy..."
# kube-proxy is deployed by kubeadm init and configured for nftables mode
# in the init script. Verify it's running here.
if kubectl get daemonset -n kube-system kube-proxy >/dev/null 2>&1; then
    echo "  kube-proxy DaemonSet found"
    # Ensure nftables mode is set (idempotent for re-runs)
    kubectl get cm -n kube-system kube-proxy -o json \
        | jq '.data["config.conf"] |= sub("mode: \"\""; "mode: \"nftables\"")' \
        | kubectl apply -f - 2>/dev/null || true
    kubectl rollout restart ds/kube-proxy -n kube-system 2>/dev/null || true
else
    echo "  kube-proxy DaemonSet not found; deploying via kubeadm on control plane..."
    az vmss run-command invoke "${RG_FLAG[@]}" -n "${SITE_NAME}-${CONTROL_POOL_NAME}" --instance-id 0 \
        --command-id RunShellScript \
        --scripts "kubeadm init phase addon kube-proxy --kubeconfig /etc/kubernetes/admin.conf 2>&1" \
        --query 'value[0].message' -o tsv 2>/dev/null || true
    # Configure nftables mode
    kubectl get cm -n kube-system kube-proxy -o json \
        | jq '.data["config.conf"] |= sub("mode: \"\""; "mode: \"nftables\"")' \
        | kubectl apply -f - 2>/dev/null || true
    kubectl rollout restart ds/kube-proxy -n kube-system 2>/dev/null || true
fi

echo "==> Running make net-deploy-crds..."
(cd "$REPO_ROOT" && make net-deploy-crds)

echo "==> Deploying site resources..."
ensure_site_gateway_resources "$SITE_NAME" "${GATEWAY_POOL_NAME}"

echo "==> Running make build net-deploy..."
(cd "$REPO_ROOT" && make build net-deploy)

echo ""
echo "==> Kubeadm primary site '$SITE_NAME' deployment complete!"
echo "    API server:    https://${API_SERVER_IP}:${API_SERVER_PORT}"
echo "    Kubeconfig:    kubectl config use-context ${SITE_NAME}"
echo "    Control plane: ${SITE_NAME}-${CONTROL_POOL_NAME} (${CONTROL_COUNT} nodes)"
echo "    Gateway:       ${SITE_NAME}-${GATEWAY_POOL_NAME} (${GATEWAY_COUNT} nodes)"
echo "    Workers:       ${SITE_NAME}-${USER_POOL_NAME} (${USER_COUNT} nodes)"
