#!/bin/bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

#
# create-primary-site.sh - Deploy a primary AKS site with networking and cluster resources
#
# Usage:
#   ./create-primary-site.sh --name <site-name> --ipv4-cidr <cidr> --ssh-key <key|@path> [options]
#
# Required arguments:
#   -n, --name              Site name (a-z0-9- only, used as resource prefix)
#   --ipv4-cidr             IPv4 address range for the VNet (CIDR notation)
#   --ssh-key <key|@path>   SSH public key value, or @<path> to read from file
#
# Optional arguments:
#   -g, --resource-group    Azure resource group (uses az CLI default if not set)
#   -l, --location          Azure region (uses az CLI default if not set)
#   --cluster-name          AKS cluster name (a-z0-9-; default: <site-name>-aks)
#   --dns-prefix            AKS DNS prefix (default: <site-name>aks)
#   --kubernetes-version    AKS version (accepts 1.xx.yy or v1.xx.yy; default: provider default)
#   --ipv6-cidr             IPv6 range ("generate" for auto ULA /48, "" to disable; default: generate)
#   --pod-ipv4-cidr         AKS pod IPv4 CIDR
#   --pod-ipv6-cidr         AKS pod IPv6 CIDR
#   --service-ipv4-cidr     AKS service IPv4 CIDR
#   --service-ipv6-cidr     AKS service IPv6 CIDR
#   --network-plugin        AKS networkPlugin value (default: none)
#   --network-plugin-mode   AKS networkPluginMode value (default: none)
#   --system-vm-size        VM size for s1system1 (default: Standard_D2ads_v5)
#   --gateway-vm-size       VM size for s1extgw1 (default: Standard_D2ads_v5)
#   --user-vm-size          VM size for s1user1 (default: Standard_D2ads_v5)
#   --system-count          Node count for s1system1 (default: 2)
#   --gateway-count         Node count for s1extgw1 (default: 2)
#   --user-count            Node count for s1user1 (default: 2)
#   --outbound-ip-count     Number of managed outbound IPv4 public IPs on LB (default: 0, uses existing PIPs)
#   --outbound-ports-per-node  Allocated outbound ports per node on the LB (default: 1024)
#   --no-bastion            Disable Azure Bastion deployment
#   --enable-monitoring     Deploy Azure Managed Prometheus and Grafana
#   --grafana-admin-id      Entra ID object ID to grant Grafana Admin role
#   --infra-only            Deploy Azure resources only; skip in-cluster configuration
#   --skip-infra            Skip deploying Azure resources; only in-cluster configuration
#   --skip-install          Skip installing unbounded-net (deploy infra and fetch credentials only)
#   -y, --yes               Skip confirmation prompt
#   --help                  Show this help message
#
# usage-end

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

SITE_NAME=""
RESOURCE_GROUP=""
LOCATION=""
CLUSTER_NAME=""
DNS_PREFIX=""
KUBERNETES_VERSION=""
IPV4_CIDR=""
IPV6_CIDR="generate"
POD_IPV4_CIDR=""
POD_IPV6_CIDR=""
SERVICE_IPV4_CIDR=""
SERVICE_IPV6_CIDR=""
NETWORK_PLUGIN="none"
NETWORK_PLUGIN_MODE=""
SYSTEM_VM_SIZE="Standard_D2ads_v5"
GATEWAY_VM_SIZE="Standard_D2ads_v5"
USER_VM_SIZE="Standard_D2ads_v5"
SYSTEM_COUNT=2
GATEWAY_COUNT=2
USER_COUNT=2
OUTBOUND_IP_COUNT=0
OUTBOUND_PORTS_PER_NODE=1024
ENABLE_BASTION=true
ENABLE_MONITORING=false
GRAFANA_ADMIN_ID=""
INFRA_ONLY=false
SKIP_INFRA=false
SKIP_INSTALL=false
SSH_KEY=""
AUTO_CONFIRM=false

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

ensure_unmanaged_cloud_node_manager_daemonset() {
    echo "==> Ensuring cloud-node-manager-unmanaged DaemonSet in kube-system..."
    if ! kubectl get daemonset -n kube-system cloud-node-manager >/dev/null 2>&1; then
        echo "  cloud-node-manager DaemonSet not found in kube-system; skipping cloud-node-manager-unmanaged creation."
        return
    fi

    kubectl get daemonset -n kube-system cloud-node-manager -o json | jq '
      .metadata.name = "cloud-node-manager-unmanaged" |
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
      .spec.selector.matchLabels = {"k8s-app":"cloud-node-manager-unmanaged"} |
      .spec.template.metadata.labels = ((.spec.template.metadata.labels // {}) + {"k8s-app":"cloud-node-manager-unmanaged"}) |
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
              },
              {
                "key": "net.unbounded-cloud.io/provider",
                "operator": "In",
                "values": ["azure"]
              }
            ]
          }
        ]
      }
    ' | kubectl apply -f -
}

get_site_node_cidrs() {
    local vnet_name="$1"
    az network vnet subnet show "${RG_FLAG[@]}" --vnet-name "$vnet_name" -n default -o json \
        | jq -r '.addressPrefixes // [ .addressPrefix ] | .[]'
}

ensure_site_gateway_resources() {
    local site_name="$1"
    local gateway_pool_name="$2"
    local -a node_cidrs=()
    local -a pod_cidrs=()
    local -a service_cidrs=()
    local manage_cni_plugin=true

    mapfile -t node_cidrs < <(get_site_node_cidrs "${site_name}-vnet")
    [[ "${#node_cidrs[@]}" -gt 0 ]] || die "Failed to resolve node CIDRs for site ${site_name}"
    [[ -n "${POD_IPV4_CIDR}" ]] && pod_cidrs+=("${POD_IPV4_CIDR}")
    [[ -n "${POD_IPV6_CIDR}" ]] && pod_cidrs+=("${POD_IPV6_CIDR}")
    [[ -n "${SERVICE_IPV4_CIDR}" ]] && service_cidrs+=("${SERVICE_IPV4_CIDR}")
    [[ -n "${SERVICE_IPV6_CIDR}" ]] && service_cidrs+=("${SERVICE_IPV6_CIDR}")
    if [[ "${NETWORK_PLUGIN}" != "none" ]]; then
        manage_cni_plugin=false
    fi

    echo "==> Ensuring Site '${site_name}' and GatewayPool '${gateway_pool_name}' CRDs..."
    {
        echo "apiVersion: net.unbounded-cloud.io/v1alpha1"
        echo "kind: Site"
        echo "metadata:"
        echo "  name: ${site_name}"
        echo "spec:"
        echo "  nodeCidrs:"
        for cidr in "${node_cidrs[@]}"; do
            echo "    - \"${cidr}\""
        done
        echo "  manageCniPlugin: ${manage_cni_plugin}"
        if [[ "${#pod_cidrs[@]}" -gt 0 ]]; then
            echo "  podCidrAssignments:"
            echo "    - assignmentEnabled: ${manage_cni_plugin}"
            echo "      cidrBlocks:"
            for cidr in "${pod_cidrs[@]}"; do
                echo "        - \"${cidr}\""
            done
        fi
    } | kubectl apply -f -

    cat <<EOF | kubectl apply -f -
apiVersion: net.unbounded-cloud.io/v1alpha1
kind: GatewayPool
metadata:
  name: ${gateway_pool_name}
spec:
  type: External
  nodeSelector:
    agentpool: "${gateway_pool_name}"
$(if [[ "${#service_cidrs[@]}" -gt 0 ]]; then
    echo "  routedCidrs:"
    for cidr in "${service_cidrs[@]}"; do
        echo "    - \"${cidr}\""
    done
fi)
EOF

    cat <<EOF | kubectl apply -f -
apiVersion: net.unbounded-cloud.io/v1alpha1
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

while [[ $# -gt 0 ]]; do
    case "$1" in
        -n|--name) SITE_NAME="$2"; shift 2 ;;
        -g|--resource-group) RESOURCE_GROUP="$2"; shift 2 ;;
        -l|--location) LOCATION="$2"; shift 2 ;;
        --cluster-name) CLUSTER_NAME="$2"; shift 2 ;;
        --dns-prefix) DNS_PREFIX="$2"; shift 2 ;;
        --kubernetes-version) KUBERNETES_VERSION="$2"; shift 2 ;;
        --ipv4-cidr) IPV4_CIDR="$2"; shift 2 ;;
        --ipv6-cidr) IPV6_CIDR="$2"; shift 2 ;;
        --pod-ipv4-cidr) POD_IPV4_CIDR="$2"; shift 2 ;;
        --pod-ipv6-cidr) POD_IPV6_CIDR="$2"; shift 2 ;;
        --service-ipv4-cidr) SERVICE_IPV4_CIDR="$2"; shift 2 ;;
        --service-ipv6-cidr) SERVICE_IPV6_CIDR="$2"; shift 2 ;;
        --network-plugin) NETWORK_PLUGIN="$2"; shift 2 ;;
        --network-plugin-mode) NETWORK_PLUGIN_MODE="$2"; shift 2 ;;
        --system-vm-size) SYSTEM_VM_SIZE="$2"; shift 2 ;;
        --gateway-vm-size) GATEWAY_VM_SIZE="$2"; shift 2 ;;
        --user-vm-size) USER_VM_SIZE="$2"; shift 2 ;;
        --system-count) SYSTEM_COUNT="$2"; shift 2 ;;
        --gateway-count) GATEWAY_COUNT="$2"; shift 2 ;;
        --user-count) USER_COUNT="$2"; shift 2 ;;
        --outbound-ip-count) OUTBOUND_IP_COUNT="$2"; shift 2 ;;
        --outbound-ports-per-node) OUTBOUND_PORTS_PER_NODE="$2"; shift 2 ;;
        --no-bastion) ENABLE_BASTION=false; shift ;;
        --enable-monitoring) ENABLE_MONITORING=true; shift ;;
        --grafana-admin-id) GRAFANA_ADMIN_ID="$2"; shift 2 ;;
        --infra-only) INFRA_ONLY=true; shift ;;
        --skip-infra) SKIP_INFRA=true; shift ;;
        --skip-install) SKIP_INSTALL=true; shift ;;
        --ssh-key) SSH_KEY="$2"; shift 2 ;;
        -y|--yes) AUTO_CONFIRM=true; shift ;;
        --help) usage; exit 0 ;;
        *) die "Unknown argument: $1" ;;
    esac
done

[[ -n "$SITE_NAME" ]] || die "--name is required"
[[ -n "$IPV4_CIDR" ]] || die "--ipv4-cidr is required"
[[ -n "$SSH_KEY" ]] || die "--ssh-key is required"

if [[ ! "$SITE_NAME" =~ ^[a-z0-9-]+$ ]]; then
    die "--name must contain only lowercase letters, digits, and hyphens (a-z0-9-)"
fi

if [[ -z "$RESOURCE_GROUP" ]]; then
    RESOURCE_GROUP="$(az config get --query 'defaults[?name==`group`].value' -o tsv --only-show-errors)"
    [[ -n "$RESOURCE_GROUP" ]] || die "--resource-group is required (no az CLI default configured)"
fi
RG_FLAG=(-g "$RESOURCE_GROUP")

if [[ -z "$LOCATION" ]]; then
    LOCATION="$(az config get --query 'defaults[?name==`location`].value' -o tsv --only-show-errors)"
fi
if [[ -z "$LOCATION" ]]; then
    LOCATION="$(az group show -g "$RESOURCE_GROUP" --query location -o tsv)"
fi

if [[ -z "$CLUSTER_NAME" ]]; then
    CLUSTER_NAME="${SITE_NAME}-aks"
fi
if [[ ! "$CLUSTER_NAME" =~ ^[a-z0-9-]+$ ]]; then
    die "--cluster-name must contain only lowercase letters, digits, and hyphens (a-z0-9-)"
fi
if [[ -z "$DNS_PREFIX" ]]; then
    DNS_PREFIX="${SITE_NAME}aks"
fi
if [[ -n "$KUBERNETES_VERSION" ]]; then
    KUBERNETES_VERSION="${KUBERNETES_VERSION#v}"
fi

if [[ "$ENABLE_MONITORING" == true && -z "$GRAFANA_ADMIN_ID" ]]; then
    GRAFANA_ADMIN_ID="$(az ad signed-in-user show --query id -o tsv 2>/dev/null)" || true
    if [[ -n "$GRAFANA_ADMIN_ID" ]]; then
        echo "==> Resolved Grafana Admin to current Azure CLI user: $GRAFANA_ADMIN_ID"
    fi
fi

SSH_PUBLIC_KEY=""
SSH_KEY_FILE=""
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

echo ""
echo "==> Deployment plan for primary site '$SITE_NAME':"
echo "    Resource group:        $RESOURCE_GROUP"
echo "    Location:              $LOCATION"
echo "    Cluster name:          $CLUSTER_NAME"
echo "    DNS prefix:            $DNS_PREFIX"
echo "    Kubernetes version:    ${KUBERNETES_VERSION:-(provider default)}"
echo "    IPv4 CIDR:             $IPV4_CIDR"
echo "    IPv6 CIDR:             $IPV6_CIDR"
echo "    Pod IPv4 CIDR:         ${POD_IPV4_CIDR:-(unset)}"
echo "    Pod IPv6 CIDR:         ${POD_IPV6_CIDR:-(unset)}"
echo "    Service IPv4 CIDR:     ${SERVICE_IPV4_CIDR:-(unset)}"
echo "    Service IPv6 CIDR:     ${SERVICE_IPV6_CIDR:-(unset)}"
echo "    Bastion:               $ENABLE_BASTION"
echo "    Monitoring:            $ENABLE_MONITORING"
if [[ -n "$GRAFANA_ADMIN_ID" ]]; then
echo "    Grafana Admin:         $GRAFANA_ADMIN_ID"
fi
echo "    networkPlugin:         $NETWORK_PLUGIN"
echo "    networkPluginMode:     $NETWORK_PLUGIN_MODE"
echo "    s1system1:             size=$SYSTEM_VM_SIZE count=$SYSTEM_COUNT"
echo "    s1extgw1:              size=$GATEWAY_VM_SIZE count=$GATEWAY_COUNT nodePublicIP=true allowedHostPorts=51820-51999"
echo "    s1user1:               size=$USER_VM_SIZE count=$USER_COUNT"
echo "    Outbound IPs:          ${OUTBOUND_IP_COUNT} (0=existing PIPs)"
echo "    Outbound ports/node:   $OUTBOUND_PORTS_PER_NODE"
echo ""

if [[ "$AUTO_CONFIRM" != true ]]; then
    read -erp "Proceed with deployment? [y/N] " confirm
    if [[ ! "$confirm" =~ ^[Yy]$ ]]; then
        echo "Aborted."
        exit 0
    fi
fi

umask 077
TEMP_DIR="$(mktemp -d "${SCRIPT_DIR}/create-primary-site.XXXXXX.tmp")"
trap 'rm -rf "$TEMP_DIR"' EXIT

PARAMS_FILE="${TEMP_DIR}/parameters.json"
BICEP_TEMPLATE="${SCRIPT_DIR}/templates/primary-site.bicep"

SSH_JQ_FLAG=(--arg sshPublicKey "$SSH_PUBLIC_KEY")
if [[ -n "$SSH_KEY_FILE" ]]; then
    SSH_JQ_FLAG=(--rawfile sshPublicKey "$SSH_KEY_FILE")
fi

jq -n \
    --arg siteName "$SITE_NAME" \
    --arg clusterName "$CLUSTER_NAME" \
    --arg dnsPrefix "$DNS_PREFIX" \
    --arg location "$LOCATION" \
    --arg kubernetesVersion "$KUBERNETES_VERSION" \
    --arg ipv4Range "$IPV4_CIDR" \
    --arg ipv6Range "$IPV6_CIDR" \
    --arg podIpv4Cidr "$POD_IPV4_CIDR" \
    --arg podIpv6Cidr "$POD_IPV6_CIDR" \
    --arg serviceIpv4Cidr "$SERVICE_IPV4_CIDR" \
    --arg serviceIpv6Cidr "$SERVICE_IPV6_CIDR" \
    --arg networkPlugin "$NETWORK_PLUGIN" \
    --arg networkPluginMode "$NETWORK_PLUGIN_MODE" \
    --arg systemPoolVmSize "$SYSTEM_VM_SIZE" \
    --arg gatewayPoolVmSize "$GATEWAY_VM_SIZE" \
    --arg userPoolVmSize "$USER_VM_SIZE" \
    --argjson systemPoolCount "$SYSTEM_COUNT" \
    --argjson gatewayPoolCount "$GATEWAY_COUNT" \
    --argjson userPoolCount "$USER_COUNT" \
    --argjson managedOutboundIPCount "$OUTBOUND_IP_COUNT" \
    --argjson allocatedOutboundPorts "$OUTBOUND_PORTS_PER_NODE" \
    --argjson enableBastion "$ENABLE_BASTION" \
    --argjson enableMonitoring "$ENABLE_MONITORING" \
    --arg grafanaAdminObjectId "$GRAFANA_ADMIN_ID" \
    "${SSH_JQ_FLAG[@]}" \
    '{
        "$schema": "https://schema.management.azure.com/schemas/2019-04-01/deploymentParameters.json#",
        "contentVersion": "1.0.0.0",
        "parameters": {
            "siteName": {"value": $siteName},
            "clusterName": {"value": $clusterName},
            "dnsPrefix": {"value": $dnsPrefix},
            "location": {"value": $location},
            "kubernetesVersion": {"value": $kubernetesVersion},
            "ipv4Range": {"value": $ipv4Range},
            "ipv6Range": {"value": $ipv6Range},
            "podIpv4Cidr": {"value": $podIpv4Cidr},
            "podIpv6Cidr": {"value": $podIpv6Cidr},
            "serviceIpv4Cidr": {"value": $serviceIpv4Cidr},
            "serviceIpv6Cidr": {"value": $serviceIpv6Cidr},
            "networkPlugin": {"value": $networkPlugin},
            "networkPluginMode": {"value": $networkPluginMode},
            "systemPoolVmSize": {"value": $systemPoolVmSize},
            "gatewayPoolVmSize": {"value": $gatewayPoolVmSize},
            "userPoolVmSize": {"value": $userPoolVmSize},
            "systemPoolCount": {"value": $systemPoolCount},
            "gatewayPoolCount": {"value": $gatewayPoolCount},
            "userPoolCount": {"value": $userPoolCount},
            "managedOutboundIPCount": {"value": $managedOutboundIPCount},
            "allocatedOutboundPorts": {"value": $allocatedOutboundPorts},
            "enableBastion": {"value": $enableBastion},
            "enableMonitoring": {"value": $enableMonitoring},
            "grafanaAdminObjectId": {"value": $grafanaAdminObjectId},
            "sshPublicKey": {"value": $sshPublicKey}
        }
    }' > "$PARAMS_FILE"

echo ""

if [[ "$SKIP_INFRA" != true ]]; then
    echo "==> Deploying primary site '$SITE_NAME'..."
    az deployment group create \
        "${RG_FLAG[@]}" \
        --name "deploy-primary-${SITE_NAME}-$(date +%Y%m%d%H%M%S)" \
        --template-file "$BICEP_TEMPLATE" \
        --parameters "@${PARAMS_FILE}"
    
    echo ""
    echo "==> Primary site '$SITE_NAME' deployed successfully."
    
    if [[ "$INFRA_ONLY" == true ]]; then
        echo "==> --infra-only: skipping in-cluster configuration."
        exit 0
    fi
fi

echo "==> Fetching admin cluster credentials..."
az aks get-credentials --admin "${RG_FLAG[@]}" -n "$CLUSTER_NAME" --overwrite-existing
ensure_unmanaged_kube_proxy_daemonset
ensure_unmanaged_cloud_node_manager_daemonset

if [[ "$SKIP_INSTALL" == true ]]; then
    echo "==> --skip-install: skipping unbounded-net installation."
    echo "==> Cluster is ready. Install unbounded-net manually with:"
    echo "    make -C hack/net deploy"
    exit 0
fi

echo "==> Running make -C hack/net deploy-crds..."
(cd "$REPO_ROOT" && make -C hack/net deploy-crds)
echo "==> Deploying site resources..."
ensure_site_gateway_resources "$SITE_NAME" "${SITE_NAME}extgw1"
echo "==> Running make build && make -C hack/net deploy..."
(cd "$REPO_ROOT" && make build && make -C hack/net deploy)
