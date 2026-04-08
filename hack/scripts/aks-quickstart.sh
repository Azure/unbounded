#!/usr/bin/env bash
# aks-quickstart.sh -- Create or configure an AKS cluster for unbounded-kube
# with gateway node pools and all required networking infrastructure.
#
# The script handles AKS infrastructure (resource group, cluster, gateway node
# pool) and delegates Kubernetes-level setup to `kubectl unbounded site init`,
# which installs unbounded-net, creates site resources, deploys the machina
# controller, and generates bootstrap tokens.
#
# Usage:
#   aks-quickstart.sh create    [options]  Create a new AKS cluster from scratch
#   aks-quickstart.sh setup     [options]  Add gateways to an existing AKS cluster
#   aks-quickstart.sh create-azure-vm [options]  Create a remote-site VM in its own resource group
#
# Create options:
#   --name NAME                  Cluster name (required)
#   --resource-group NAME        Azure resource group (defaults to --name)
#   --location LOCATION          Azure region (default: canadacentral)
#   --k8s-version VERSION        Kubernetes version (default: AKS latest)
#   --system-pool-sku SKU        System pool VM SKU (default: Standard_D2ads_v6)
#   --system-pool-count N        System pool node count (default: 2)
#   --gateway-pool-sku SKU       Gateway pool VM SKU (default: Standard_D2ads_v6)
#   --gateway-pool-count N       Gateway pool node count (default: 2)
#   --service-cidr CIDR          Kubernetes service CIDR (default: 10.0.0.0/16)
#   --site-name NAME             Remote site name (default: remote)
#   --remote-node-cidr CIDR      CIDR for remote site nodes (required)
#   --remote-pod-cidr CIDR       CIDR for remote site pods (required)
#   --ssh-key PATH               Path to SSH public key (auto-generated if omitted)
#   --public-ip-strategy STRAT   node (per-node public IP) or lb (load balancer)
#                                (default: node)
#   -h, --help                   Show this help and exit
#
# Setup options:
#   --name NAME                  Cluster name (auto-detected if omitted)
#   --resource-group NAME        Resource group (auto-detected if omitted)
#   --subscription ID            Azure subscription (auto-detected if omitted)
#   --gateway-pool-sku SKU       Gateway pool VM SKU (default: Standard_D2ads_v6)
#   --gateway-pool-count N       Gateway pool node count (default: 2)
#   --site-name NAME             Remote site name (default: remote)
#   --remote-node-cidr CIDR      CIDR for remote site nodes (required)
#   --remote-pod-cidr CIDR       CIDR for remote site pods (required)
#   --public-ip-strategy STRAT   node or lb (default: node)
#   --context NAME               kubeconfig context (default: current)
#   -h, --help                   Show this help and exit
#
# Create-azure-vm options:
#   --name NAME                  VM name (required)
#   --resource-group NAME        Azure resource group for the VM (defaults to --name)
#   --location LOCATION          Azure region (default: canadacentral)
#   --vm-size SKU                VM SKU (default: Standard_D2ads_v6)
#   --site-name NAME             Remote site name to read node CIDR from (default: remote)
#   --remote-node-cidr CIDR      Override VNet CIDR instead of reading from site
#   --ssh-key PATH               Path to SSH public key (default: ~/.ssh/id_rsa.pub)
#   --admin-username USER        VM admin username (default: azureuser)
#   -h, --help                   Show this help and exit
#
# Public IP strategies:
#   node  Each gateway node gets its own public IP via AKS EnableNodePublicIP.
#         This is the simplest approach and matches what forge does internally.
#   lb    Gateway nodes do NOT get per-node public IPs. Instead you must create
#         a Kubernetes Service type LoadBalancer with externalTrafficPolicy: Local
#         to route WireGuard traffic to gateway pods. Use this when your
#         environment restricts per-node public IPs.

set -euo pipefail

# ── constants ────────────────────────────────────────────────────────────────

GATEWAY_LABEL="unbounded-kube.io/unbounded-net-gateway=true"
GATEWAY_TAINT="CriticalAddonsOnly=true:NoSchedule"
WIREGUARD_PORTS="51820-51899/udp"

# Defaults
DEFAULT_LOCATION="canadacentral"
DEFAULT_SYSTEM_POOL_SKU="Standard_D2ads_v6"
DEFAULT_SYSTEM_POOL_COUNT="2"
DEFAULT_GATEWAY_POOL_SKU="Standard_D2ads_v6"
DEFAULT_GATEWAY_POOL_COUNT="2"
DEFAULT_SERVICE_CIDR="10.0.0.0/16"
DEFAULT_SITE_NAME="remote"
DEFAULT_PUBLIC_IP_STRATEGY="node"
DEFAULT_VM_SIZE="Standard_D2ads_v6"
DEFAULT_VM_IMAGE="Canonical:ubuntu-24_04-lts:server:latest"
DEFAULT_VM_ADMIN_USERNAME="azureuser"

# AKS default pod CIDR used when NetworkPlugin=none (BYO CNI) leaves the
# network profile's podCidr empty.  This is the standard Kubernetes default.
DEFAULT_POD_CIDR="10.244.0.0/16"

# ── helpers ──────────────────────────────────────────────────────────────────

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[0;33m'
BLUE='\033[0;34m'
NC='\033[0m' # No color

die() { echo -e "${RED}error:${NC} $*" >&2; exit 1; }
log() { echo -e "${GREEN}==>${NC} $*"; }
warn() { echo -e "${YELLOW}warning:${NC} $*" >&2; }
info() { echo -e "${BLUE}   ${NC} $*"; }

usage() {
  sed -n '/^# Usage:/,/^[^#]/{
    /^[^#]/q
    s/^# \{0,1\}//
    p
  }' "$0"
  exit 0
}

require_cmd() {
  command -v "$1" >/dev/null 2>&1 || die "$1 not found. $2"
}

# is_valid_cidr <cidr> — basic validation of a CIDR notation string.
is_valid_cidr() {
  local cidr="$1"
  [[ "$cidr" =~ ^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+/[0-9]+$ ]] || return 1
  local prefix="${cidr#*/}"
  (( prefix >= 0 && prefix <= 32 )) || return 1
  return 0
}

# dns_ip_from_cidr <service-cidr> — derive the DNS service IP (.10) from a CIDR.
# e.g. 10.0.0.0/16 -> 10.0.0.10
dns_ip_from_cidr() {
  local cidr="$1"
  local base="${cidr%%/*}"
  IFS='.' read -r a b c _ <<< "$base"
  echo "${a}.${b}.${c}.10"
}

# ip4_to_int <a.b.c.d> — print the IPv4 address as a decimal integer.
ip4_to_int() {
  local IFS=.
  read -r a b c d <<< "$1"
  echo $(( (a << 24) | (b << 16) | (c << 8) | d ))
}

# subnet_contains_all <prefix/len> <newline-separated IPs>
# Returns 0 (true) if every IP is within the subnet, 1 otherwise.
subnet_contains_all() {
  local prefix="${1%/*}"
  local len="${1#*/}"
  local mask=$(( 0xFFFFFFFF << (32 - len) & 0xFFFFFFFF ))
  local net_int
  net_int=$(ip4_to_int "$prefix")
  local network=$(( net_int & mask ))
  while IFS= read -r ip; do
    [[ -z "$ip" ]] && continue
    local ip_int
    ip_int=$(ip4_to_int "$ip")
    [[ $(( ip_int & mask )) -eq $network ]] || return 1
  done <<< "$2"
  return 0
}

# wait_for_nodes <label> <expected_count> <timeout_seconds>
# Wait for at least <expected_count> nodes with the given label to be registered.
# NOTE: With NetworkPlugin=none, nodes will NOT reach Ready until a CNI is installed.
# This function only waits for the nodes to appear, not for Ready status.
wait_for_nodes() {
  local label="$1"
  local expected="${2:-1}"
  local timeout="${3:-600}"
  local elapsed=0
  local interval=10

  log "Waiting for $expected node(s) with label $label to appear (timeout: ${timeout}s)..."
  while (( elapsed < timeout )); do
    local node_count
    node_count=$(kubectl "${KUBECTL_CTX_ARGS[@]}" get nodes \
      -l "$label" \
      -o name 2>/dev/null | wc -l || echo 0)

    if (( node_count >= expected )); then
      info "$node_count node(s) with label $label registered"
      return 0
    fi

    sleep "$interval"
    (( elapsed += interval ))
  done

  die "timed out waiting for nodes with label $label after ${timeout}s"
}

# wait_for_nodes_ready <label> <timeout_seconds>
# Wait for at least one node with the given label to be Ready.
# Used after CNI installation when nodes should transition to Ready.
wait_for_nodes_ready() {
  local label="$1"
  local timeout="${2:-300}"
  local elapsed=0
  local interval=10

  log "Waiting for nodes with label $label to be Ready (timeout: ${timeout}s)..."
  while (( elapsed < timeout )); do
    local ready_count
    ready_count=$(kubectl "${KUBECTL_CTX_ARGS[@]}" get nodes \
      -l "$label" \
      -o jsonpath='{range .items[*]}{range .status.conditions[?(@.type=="Ready")]}{.status}{"\n"}{end}{end}' 2>/dev/null \
      | grep -c "True" || true)

    if (( ready_count > 0 )); then
      info "$ready_count node(s) with label $label are Ready"
      return 0
    fi

    sleep "$interval"
    (( elapsed += interval ))
  done

  die "timed out waiting for nodes with label $label to be Ready after ${timeout}s"
}

# ── preflight checks ────────────────────────────────────────────────────────

preflight() {
  require_cmd az      "Install the Azure CLI: https://aka.ms/installazurecli"
  require_cmd kubectl "Install kubectl: https://kubernetes.io/docs/tasks/tools/"
  require_cmd curl    "Install curl for downloading manifests."
  require_cmd jq      "Install jq: https://stedolan.github.io/jq/download/"

  # kubectl-unbounded is required for site init and manual-bootstrap.
  kubectl unbounded --help >/dev/null 2>&1 \
    || die "kubectl-unbounded not found. Install it from: https://github.com/project-unbounded/unbounded-kube/releases/latest"

  az account show --output none 2>/dev/null \
    || die "not logged in to Azure. Run: az login"
}

preflight_vm() {
  require_cmd az      "Install the Azure CLI: https://aka.ms/installazurecli"
  require_cmd kubectl "Install kubectl: https://kubernetes.io/docs/tasks/tools/"

  az account show --output none 2>/dev/null \
    || die "not logged in to Azure. Run: az login"
}

# ── auto-detect cluster identity ─────────────────────────────────────────────

# detect_cluster sets SUB, RG, CLUSTER, NODE_RG from the current kubeconfig.
# Accepts optional overrides via OPT_SUBSCRIPTION, OPT_RESOURCE_GROUP, OPT_CLUSTER_NAME.
detect_cluster() {
  local opt_sub="${1:-}"
  local opt_rg="${2:-}"
  local opt_cluster="${3:-}"

  if [[ -n "$opt_sub" ]] && [[ -n "$opt_rg" ]] && [[ -n "$opt_cluster" ]]; then
    SUB="$opt_sub"
    RG="$opt_rg"
    CLUSTER="$opt_cluster"
    log "Fetching node resource group from AKS..."
    NODE_RG=$(az aks show \
      --subscription "$SUB" \
      --resource-group "$RG" \
      --name "$CLUSTER" \
      --query nodeResourceGroup \
      --output tsv)
    return
  fi

  log "Detecting cluster identity from node spec.providerID..."

  local provider_id
  provider_id=$(kubectl "${KUBECTL_CTX_ARGS[@]}" get nodes \
    -o jsonpath='{.items[0].spec.providerID}' 2>/dev/null || true)

  [[ -z "$provider_id" ]] && die "no nodes found in the current cluster."
  [[ "$provider_id" == azure://* ]] \
    || die "not an AKS cluster (providerID: $provider_id). This script supports AKS only."

  local remainder detected_sub detected_node_rg
  remainder="${provider_id#azure:///subscriptions/}"
  detected_sub="${remainder%%/*}"
  remainder="${remainder#*/resourceGroups/}"
  detected_node_rg="${remainder%%/*}"

  [[ -z "$detected_sub" ]] && die "could not parse subscription from providerID. Pass --subscription explicitly."
  [[ -z "$detected_node_rg" ]] && die "could not parse node resource group from providerID. Pass --resource-group and --name explicitly."

  local detected_rg="" detected_cluster=""

  # Strategy 1: Parse MC_{rg}_{cluster}_{location} convention.
  local node_rg_lower="${detected_node_rg,,}"
  if [[ "$node_rg_lower" == mc_* ]]; then
    local inner="${detected_node_rg:3}"
    IFS='_' read -ra parts <<< "$inner"
    local n="${#parts[@]}"
    if (( n >= 3 )); then
      detected_cluster="${parts[$((n-2))]}"
      detected_rg="${parts[*]:0:$((n-2))}"
      detected_rg="${detected_rg// /_}"
    fi
  fi

  # Strategy 2: Query Azure for the cluster matching the node resource group.
  if [[ -z "$detected_rg" ]] || [[ -z "$detected_cluster" ]]; then
    info "Querying Azure for cluster identity..."
    mapfile -t _cluster_info < <(
      az aks list \
        --subscription "$detected_sub" \
        --query "[?nodeResourceGroup=='${detected_node_rg}'] | [0].[resourceGroup, name]" \
        --output tsv 2>/dev/null || true
    )
    detected_rg="${_cluster_info[0]:-}"
    detected_cluster="${_cluster_info[1]:-}"
  fi

  [[ -z "$detected_rg" ]] || [[ -z "$detected_cluster" ]] && \
    die "could not detect AKS cluster. Pass --subscription, --resource-group, and --name explicitly."

  SUB="${opt_sub:-$detected_sub}"
  RG="${opt_rg:-$detected_rg}"
  CLUSTER="${opt_cluster:-$detected_cluster}"
  NODE_RG="$detected_node_rg"

  info "subscription:        $SUB"
  info "resource group:      $RG"
  info "cluster:             $CLUSTER"
  info "node resource group: $NODE_RG"
}

# ── CIDR auto-detection ──────────────────────────────────────────────────────

# detect_cluster_cidrs sets CLUSTER_NODE_CIDR, CLUSTER_POD_CIDR, CLUSTER_SERVICE_CIDR.
detect_cluster_cidrs() {
  log "Fetching network profile from AKS..."
  mapfile -t _cidrs < <(az aks show \
    --subscription "$SUB" \
    --resource-group "$RG" \
    --name "$CLUSTER" \
    --query "[networkProfile.podCidr, networkProfile.serviceCidr]" \
    --output tsv)

  CLUSTER_POD_CIDR="${_cidrs[0]:-}"
  CLUSTER_SERVICE_CIDR="${_cidrs[1]:-}"

  # Azure CLI returns "None" for null values in tsv output.
  [[ "$CLUSTER_POD_CIDR" == "None" ]] && CLUSTER_POD_CIDR=""
  [[ "$CLUSTER_SERVICE_CIDR" == "None" ]] && CLUSTER_SERVICE_CIDR=""

  # With NetworkPlugin=none (BYO CNI), AKS does not set a pod CIDR in the
  # network profile because the CNI manages pod IP allocation.  Fall back to
  # the standard Kubernetes default.
  if [[ -z "$CLUSTER_POD_CIDR" ]]; then
    CLUSTER_POD_CIDR="$DEFAULT_POD_CIDR"
    info "No pod CIDR in AKS network profile (expected with BYO CNI), using default: $CLUSTER_POD_CIDR"
  fi

  # If the service CIDR is also missing, derive it from the kube-dns ClusterIP.
  if [[ -z "$CLUSTER_SERVICE_CIDR" ]]; then
    info "No service CIDR in AKS network profile, deriving from kube-dns ClusterIP..."
    local dns_ip
    dns_ip=$(kubectl "${KUBECTL_CTX_ARGS[@]}" get svc -n kube-system kube-dns \
      -o jsonpath='{.spec.clusterIP}' 2>/dev/null || true)
    if [[ -n "$dns_ip" ]]; then
      IFS='.' read -r a b _ _ <<< "$dns_ip"
      CLUSTER_SERVICE_CIDR="${a}.${b}.0.0/16"
      info "Derived service CIDR from kube-dns ($dns_ip): $CLUSTER_SERVICE_CIDR"
    else
      die "could not determine service CIDR from AKS network profile or kube-dns."
    fi
  fi

  # Detect node CIDR from VNet subnets.
  log "Detecting cluster node CIDR from VNet subnets..."
  local node_ips
  node_ips=$(kubectl "${KUBECTL_CTX_ARGS[@]}" get nodes \
    -o jsonpath='{range .items[?(@.spec.providerID)]}{range .status.addresses[?(@.type=="InternalIP")]}{.address}{"\n"}{end}{end}' \
    | grep -v '^$')

  [[ -z "$node_ips" ]] && die "could not retrieve node internal IPs."

  CLUSTER_NODE_CIDR=$(az network vnet list \
    --subscription "$SUB" \
    --resource-group "$NODE_RG" \
    --query "[].subnets[].addressPrefix" \
    --output tsv | while IFS= read -r prefix; do
      [[ -z "$prefix" ]] && continue
      subnet_contains_all "$prefix" "$node_ips" && echo "$prefix" && break
    done)

  [[ -z "$CLUSTER_NODE_CIDR" ]] && die "could not find a VNet subnet containing all node IPs."

  info "cluster node CIDR:    $CLUSTER_NODE_CIDR"
  info "cluster pod CIDR:     $CLUSTER_POD_CIDR"
  info "cluster service CIDR: $CLUSTER_SERVICE_CIDR"
}

# ── add gateway node pool ────────────────────────────────────────────────────

add_gateway_pool() {
  local rg="$1"
  local cluster="$2"
  local sku="$3"
  local count="$4"
  local strategy="$5"

  # Check if the gateway pool already exists.
  if az aks nodepool show \
    --resource-group "$rg" \
    --cluster-name "$cluster" \
    --name gwmain \
    --output none 2>/dev/null; then
    info "Gateway node pool 'gwmain' already exists, skipping creation"
    return 0
  fi

  log "Adding gateway node pool 'gwmain'..."

  local extra_args=()
  if [[ "$strategy" == "node" ]]; then
    extra_args+=(--enable-node-public-ip)
    info "Using per-node public IPs (--public-ip-strategy node)"
  else
    info "Skipping per-node public IPs (--public-ip-strategy lb)"
    info "You will need to create a LoadBalancer Service for WireGuard traffic."
    info "See the script header comments for guidance on the lb strategy."
  fi

  az aks nodepool add \
    --resource-group "$rg" \
    --cluster-name "$cluster" \
    --name gwmain \
    --mode User \
    --os-sku AzureLinux \
    --node-vm-size "$sku" \
    --node-count "$count" \
    --allowed-host-ports "$WIREGUARD_PORTS" \
    --labels "$GATEWAY_LABEL" \
    --node-taints "$GATEWAY_TAINT" \
    "${extra_args[@]}" \
    --output none

  info "Gateway node pool 'gwmain' created"
}

# ── site init flow ───────────────────────────────────────────────────────────

do_site_init() {
  local site_name="$1"
  local cluster_node_cidr="$2"
  local cluster_pod_cidr="$3"
  local cluster_service_cidr="$4"
  local remote_node_cidr="$5"
  local remote_pod_cidr="$6"

  log "Running kubectl unbounded site init..."

  local ctx_args=()
  if [[ ${#KUBECTL_CTX_ARGS[@]} -gt 0 ]]; then
    ctx_args=("${KUBECTL_CTX_ARGS[@]}")
  fi

  kubectl "${ctx_args[@]}" unbounded site init \
    --name "$site_name" \
    --cluster-node-cidr "$cluster_node_cidr" \
    --cluster-pod-cidr "$cluster_pod_cidr" \
    --cluster-service-cidr "$cluster_service_cidr" \
    --node-cidr "$remote_node_cidr" \
    --pod-cidr "$remote_pod_cidr"

  print_summary "$site_name"
}

print_summary() {
  local site_name="$1"

  echo
  echo -e "${GREEN}============================================================${NC}"
  echo -e "${GREEN} Unbounded-Kube Quickstart Complete${NC}"
  echo -e "${GREEN}============================================================${NC}"
  echo
  echo "  Cluster:              ${CLUSTER:-unknown}"
  echo "  Resource Group:       ${RG:-unknown}"
  echo "  Site Name:            $site_name"
  echo

  # Print gateway node public IPs.
  echo "  Gateway Node IPs:"
  kubectl "${KUBECTL_CTX_ARGS[@]}" get nodes -l "$GATEWAY_LABEL" \
    -o jsonpath='{range .items[*]}    {.metadata.name}: internal={.status.addresses[?(@.type=="InternalIP")].address} external={.status.addresses[?(@.type=="ExternalIP")].address}{"\n"}{end}' \
    2>/dev/null || true
  echo

  echo -e "${GREEN}------------------------------------------------------------${NC}"
  echo -e "${GREEN} Add a Remote Node${NC}"
  echo -e "${GREEN}------------------------------------------------------------${NC}"
  echo
  echo "  The target host must be Linux (x86_64 or arm64) with internet access."
  echo
  echo -e "  ${YELLOW}Step 1:${NC} Generate and run the bootstrap script on the target host"
  echo
  echo "    kubectl unbounded machine manual-bootstrap <node-name> --site ${site_name} | ssh <host> sudo bash"
  echo
  echo -e "  ${YELLOW}Step 2:${NC} Watch for the node to join the cluster"
  echo
  echo "    kubectl get nodes -w"
  echo
  echo -e "${GREEN}------------------------------------------------------------${NC}"
  echo
}

# ── cmd: create ──────────────────────────────────────────────────────────────

cmd_create() {
  local name="" resource_group="" location="$DEFAULT_LOCATION"
  local k8s_version="" system_pool_sku="$DEFAULT_SYSTEM_POOL_SKU"
  local system_pool_count="$DEFAULT_SYSTEM_POOL_COUNT"
  local gateway_pool_sku="$DEFAULT_GATEWAY_POOL_SKU"
  local gateway_pool_count="$DEFAULT_GATEWAY_POOL_COUNT"
  local service_cidr="$DEFAULT_SERVICE_CIDR"
  local site_name="$DEFAULT_SITE_NAME"
  local remote_node_cidr="" remote_pod_cidr="" ssh_key=""
  local public_ip_strategy="$DEFAULT_PUBLIC_IP_STRATEGY"

  while [[ $# -gt 0 ]]; do
    case "$1" in
      --name)                name="$2";               shift 2 ;;
      --resource-group)      resource_group="$2";      shift 2 ;;
      --location)            location="$2";            shift 2 ;;
      --k8s-version)         k8s_version="$2";         shift 2 ;;
      --system-pool-sku)     system_pool_sku="$2";     shift 2 ;;
      --system-pool-count)   system_pool_count="$2";   shift 2 ;;
      --gateway-pool-sku)    gateway_pool_sku="$2";    shift 2 ;;
      --gateway-pool-count)  gateway_pool_count="$2";  shift 2 ;;
      --service-cidr)        service_cidr="$2";        shift 2 ;;
      --site-name)           site_name="$2";           shift 2 ;;
      --remote-node-cidr)    remote_node_cidr="$2";    shift 2 ;;
      --remote-pod-cidr)     remote_pod_cidr="$2";     shift 2 ;;
      --ssh-key)             ssh_key="$2";             shift 2 ;;
      --public-ip-strategy)  public_ip_strategy="$2";  shift 2 ;;
      -h|--help)             usage ;;
      *) die "unknown option: $1. Use --help for usage." ;;
    esac
  done

  # Validate required parameters.
  [[ -n "$name" ]]            || die "--name is required"
  [[ -n "$remote_node_cidr" ]] || die "--remote-node-cidr is required"
  [[ -n "$remote_pod_cidr" ]]  || die "--remote-pod-cidr is required"

  is_valid_cidr "$service_cidr"     || die "invalid --service-cidr: $service_cidr"
  is_valid_cidr "$remote_node_cidr" || die "invalid --remote-node-cidr: $remote_node_cidr"
  is_valid_cidr "$remote_pod_cidr"  || die "invalid --remote-pod-cidr: $remote_pod_cidr"

  [[ "$public_ip_strategy" == "node" ]] || [[ "$public_ip_strategy" == "lb" ]] \
    || die "--public-ip-strategy must be 'node' or 'lb'"

  resource_group="${resource_group:-$name}"

  # Step 1: Create resource group (idempotent — az group create is a no-op if it exists).
  log "Creating resource group '$resource_group' in '$location'..."
  az group create \
    --name "$resource_group" \
    --location "$location" \
    --output none
  info "Resource group ready"

  # Step 2: Create AKS cluster (skip if it already exists).
  if az aks show \
    --resource-group "$resource_group" \
    --name "$name" \
    --output none 2>/dev/null; then
    info "AKS cluster '$name' already exists, skipping creation"
  else
    # Generate SSH key if not provided.
    local ssh_key_path="$ssh_key"
    local ssh_tmpdir=""
    if [[ -z "$ssh_key_path" ]]; then
      ssh_tmpdir=$(mktemp -d)
      ssh_key_path="$ssh_tmpdir/id_rsa.pub"
      log "Generating SSH key pair..."
      ssh-keygen -t rsa -b 4096 -f "$ssh_tmpdir/id_rsa" -N "" -q
      info "SSH key generated at $ssh_tmpdir/id_rsa"
    fi

    log "Creating AKS cluster '$name' (this may take several minutes)..."

    local k8s_version_args=()
    if [[ -n "$k8s_version" ]]; then
      k8s_version_args=(--kubernetes-version "$k8s_version")
    fi

    local dns_service_ip
    dns_service_ip=$(dns_ip_from_cidr "$service_cidr")

    az aks create \
      --resource-group "$resource_group" \
      --name "$name" \
      --network-plugin none \
      --service-cidr "$service_cidr" \
      --dns-service-ip "$dns_service_ip" \
      --node-resource-group "${name}-nodes" \
      --enable-oidc-issuer \
      --enable-managed-identity \
      --os-sku AzureLinux \
      --admin-username azureuser \
      --ssh-key-value "$ssh_key_path" \
      --nodepool-name system \
      --node-vm-size "$system_pool_sku" \
      --node-count "$system_pool_count" \
      --enable-node-public-ip \
      "${k8s_version_args[@]}" \
      --output none

    info "AKS cluster '$name' created"

    # Note the SSH key location for the user.
    if [[ -n "$ssh_tmpdir" ]]; then
      info "SSH private key saved at $ssh_tmpdir/id_rsa (save this if needed)"
    fi
  fi

  # Step 3: Get kubeconfig (always refresh).
  log "Fetching kubeconfig..."
  az aks get-credentials \
    --resource-group "$resource_group" \
    --name "$name" \
    --overwrite-existing \
    --output none
  info "kubeconfig updated for cluster '$name'"

  # Set globals for downstream functions.
  RG="$resource_group"
  CLUSTER="$name"
  SUB=$(az account show --query id --output tsv)
  NODE_RG="${name}-nodes"
  KUBECTL_CTX_ARGS=()

  # Step 4: Add gateway node pool (idempotent — skips if gwmain already exists).
  add_gateway_pool "$resource_group" "$name" "$gateway_pool_sku" "$gateway_pool_count" "$public_ip_strategy"

  # Step 5: Wait for gateway nodes to be registered (they won't be Ready until CNI is installed).
  wait_for_nodes "$GATEWAY_LABEL" "$gateway_pool_count" 600

  # Step 6: Detect cluster CIDRs.
  detect_cluster_cidrs

  # Step 7: Run site init (installs CNI, creates site resources, deploys machina).
  do_site_init "$site_name" "$CLUSTER_NODE_CIDR" "$CLUSTER_POD_CIDR" "$CLUSTER_SERVICE_CIDR" "$remote_node_cidr" "$remote_pod_cidr"
}

# ── cmd: setup ───────────────────────────────────────────────────────────────

cmd_setup() {
  local name="" resource_group="" subscription=""
  local gateway_pool_sku="$DEFAULT_GATEWAY_POOL_SKU"
  local gateway_pool_count="$DEFAULT_GATEWAY_POOL_COUNT"
  local site_name="$DEFAULT_SITE_NAME"
  local remote_node_cidr="" remote_pod_cidr=""
  local public_ip_strategy="$DEFAULT_PUBLIC_IP_STRATEGY"
  local context=""

  while [[ $# -gt 0 ]]; do
    case "$1" in
      --name)                name="$2";                shift 2 ;;
      --resource-group)      resource_group="$2";      shift 2 ;;
      --subscription)        subscription="$2";        shift 2 ;;
      --gateway-pool-sku)    gateway_pool_sku="$2";    shift 2 ;;
      --gateway-pool-count)  gateway_pool_count="$2";  shift 2 ;;
      --site-name)           site_name="$2";           shift 2 ;;
      --remote-node-cidr)    remote_node_cidr="$2";    shift 2 ;;
      --remote-pod-cidr)     remote_pod_cidr="$2";     shift 2 ;;
      --public-ip-strategy)  public_ip_strategy="$2";  shift 2 ;;
      --context)             context="$2";             shift 2 ;;
      -h|--help)             usage ;;
      *) die "unknown option: $1. Use --help for usage." ;;
    esac
  done

  # Validate required parameters.
  [[ -n "$remote_node_cidr" ]] || die "--remote-node-cidr is required"
  [[ -n "$remote_pod_cidr" ]]  || die "--remote-pod-cidr is required"

  is_valid_cidr "$remote_node_cidr" || die "invalid --remote-node-cidr: $remote_node_cidr"
  is_valid_cidr "$remote_pod_cidr"  || die "invalid --remote-pod-cidr: $remote_pod_cidr"

  [[ "$public_ip_strategy" == "node" ]] || [[ "$public_ip_strategy" == "lb" ]] \
    || die "--public-ip-strategy must be 'node' or 'lb'"

  # Setup kubectl context args.
  KUBECTL_CTX_ARGS=()
  if [[ -n "$context" ]]; then
    KUBECTL_CTX_ARGS=(--context "$context")
  fi

  # Step 1: Detect cluster identity.
  detect_cluster "$subscription" "$resource_group" "$name"

  # Step 2: Check network plugin.
  log "Checking AKS network plugin..."
  local network_plugin
  network_plugin=$(az aks show \
    --subscription "$SUB" \
    --resource-group "$RG" \
    --name "$CLUSTER" \
    --query "networkProfile.networkPlugin" \
    --output tsv 2>/dev/null || true)

  if [[ "$network_plugin" == "none" ]] || [[ "$network_plugin" == "None" ]]; then
    info "Network plugin: none (BYO CNI) -- compatible"
  elif [[ -z "$network_plugin" ]] || [[ "$network_plugin" == "null" ]]; then
    info "Network plugin: not detected (assuming BYO CNI)"
  else
    echo
    warn "This AKS cluster uses network plugin '$network_plugin'."
    warn "unbounded-net requires NetworkPlugin=none (BYO CNI mode)."
    warn ""
    warn "To use unbounded-kube, recreate the cluster with:"
    warn "  az aks create ... --network-plugin none"
    warn ""
    warn "Or use 'aks-quickstart.sh create' to create a new cluster."
    echo
    die "incompatible network plugin: $network_plugin"
  fi

  # Step 3: Add gateway node pool.
  add_gateway_pool "$RG" "$CLUSTER" "$gateway_pool_sku" "$gateway_pool_count" "$public_ip_strategy"

  # Step 4: Wait for gateway nodes to be registered (they won't be Ready until CNI is installed).
  wait_for_nodes "$GATEWAY_LABEL" "$gateway_pool_count" 600

  # Step 5: Detect cluster CIDRs.
  detect_cluster_cidrs

  # Step 6: Run site init (installs CNI, creates site resources, deploys machina).
  do_site_init "$site_name" "$CLUSTER_NODE_CIDR" "$CLUSTER_POD_CIDR" "$CLUSTER_SERVICE_CIDR" "$remote_node_cidr" "$remote_pod_cidr"
}

# ── cmd: create-azure-vm ───────────────────────────────────────────────────────────

cmd_create_vm() {
  local name="" resource_group="" location="$DEFAULT_LOCATION"
  local vm_size="$DEFAULT_VM_SIZE"
  local site_name="$DEFAULT_SITE_NAME"
  local remote_node_cidr=""
  local ssh_key="$HOME/.ssh/id_rsa.pub"
  local admin_username="$DEFAULT_VM_ADMIN_USERNAME"

  while [[ $# -gt 0 ]]; do
    case "$1" in
      --name)                name="$2";               shift 2 ;;
      --resource-group)      resource_group="$2";      shift 2 ;;
      --location)            location="$2";            shift 2 ;;
      --vm-size)             vm_size="$2";             shift 2 ;;
      --site-name)           site_name="$2";           shift 2 ;;
      --remote-node-cidr)    remote_node_cidr="$2";    shift 2 ;;
      --ssh-key)             ssh_key="$2";             shift 2 ;;
      --admin-username)      admin_username="$2";      shift 2 ;;
      -h|--help)             usage ;;
      *) die "unknown option: $1. Use --help for usage." ;;
    esac
  done

  # Validate required parameters.
  [[ -n "$name" ]] || die "--name is required"

  [[ -f "$ssh_key" ]] || die "SSH public key not found at $ssh_key. Provide --ssh-key or generate one with ssh-keygen."

  # Infer remote node CIDR from the site resource, unless provided via --remote-node-cidr.
  if [[ -z "$remote_node_cidr" ]]; then
    log "Fetching remote node CIDR from site '$site_name'..."
    remote_node_cidr=$(kubectl get site "$site_name" \
      -o jsonpath='{.spec.nodeCidrs[0]}' 2>/dev/null || true)

    [[ -n "$remote_node_cidr" ]] \
      || die "could not read node CIDR from site '$site_name'. Ensure the site exists (kubectl get site) or pass --remote-node-cidr CIDR manually."

    info "Using node CIDR from site '$site_name': $remote_node_cidr"
  fi

  is_valid_cidr "$remote_node_cidr" \
    || die "invalid remote node CIDR: $remote_node_cidr"

  resource_group="${resource_group:-${name}-rg}"

  local vnet_name="${name}-vnet"
  local subnet_name="${name}-subnet"

  # Step 1: Create resource group.
  log "Creating resource group '$resource_group' in '$location'..."
  az group create \
    --name "$resource_group" \
    --location "$location" \
    --output none
  info "Resource group ready"

  # Step 2: Create VNet + subnet using the remote-node-cidr.
  log "Creating VNet '$vnet_name' with address space '$remote_node_cidr'..."
  az network vnet create \
    --resource-group "$resource_group" \
    --name "$vnet_name" \
    --address-prefixes "$remote_node_cidr" \
    --subnet-name "$subnet_name" \
    --subnet-prefixes "$remote_node_cidr" \
    --location "$location" \
    --output none
  info "VNet '$vnet_name' with subnet '$subnet_name' ($remote_node_cidr) created"

  # Step 3: Create the VM with Ubuntu 24.04 LTS and the local SSH key.
  log "Creating VM '$name' (image: $DEFAULT_VM_IMAGE, size: $vm_size)..."
  az vm create \
    --resource-group "$resource_group" \
    --name "$name" \
    --image "$DEFAULT_VM_IMAGE" \
    --size "$vm_size" \
    --vnet-name "$vnet_name" \
    --subnet "$subnet_name" \
    --admin-username "$admin_username" \
    --ssh-key-values "$ssh_key" \
    --public-ip-sku Standard \
    --output table
  info "VM '$name' created"

  # Print summary.
  local public_ip
  public_ip=$(az vm show \
    --resource-group "$resource_group" \
    --name "$name" \
    --show-details \
    --query publicIps \
    --output tsv 2>/dev/null || true)

  local private_ip
  private_ip=$(az vm show \
    --resource-group "$resource_group" \
    --name "$name" \
    --show-details \
    --query privateIps \
    --output tsv 2>/dev/null || true)

  echo
  echo -e "${GREEN}============================================================${NC}"
  echo -e "${GREEN} Remote-Site VM Created${NC}"
  echo -e "${GREEN}============================================================${NC}"
  echo
  echo "  VM Name:           $name"
  echo "  Resource Group:    $resource_group"
  echo "  Location:          $location"
  echo "  VM Size:           $vm_size"
  echo "  Image:             $DEFAULT_VM_IMAGE"
  echo "  VNet / Subnet:     $vnet_name / $subnet_name ($remote_node_cidr)"
  echo "  Admin User:        $admin_username"
  echo "  Public IP:         ${public_ip:-n/a}"
  echo "  Private IP:        ${private_ip:-n/a}"
  echo
  echo -e "${GREEN}------------------------------------------------------------${NC}"
  echo -e "${GREEN} Bootstrap the VM as a Remote Node${NC}"
  echo -e "${GREEN}------------------------------------------------------------${NC}"
  echo
  echo "  kubectl unbounded machine manual-bootstrap ${name} --site ${site_name} \\"
  echo "      | ssh ${admin_username}@${public_ip:-<public-ip>} sudo bash"
  echo
  echo -e "${GREEN}------------------------------------------------------------${NC}"
  echo
}

# ── main ─────────────────────────────────────────────────────────────────────

# Global state set by various functions.
SUB=""
RG=""
CLUSTER=""
NODE_RG=""
CLUSTER_NODE_CIDR=""
CLUSTER_POD_CIDR=""
CLUSTER_SERVICE_CIDR=""
KUBECTL_CTX_ARGS=()

main() {
  if [[ $# -eq 0 ]]; then
    usage
  fi

  local subcommand="$1"
  shift

  case "$subcommand" in
    create-azure-vm) preflight_vm ;;
    create|setup) preflight ;;
    -h|--help) usage ;;
    *) die "unknown subcommand: $subcommand. Use 'create', 'setup', or 'create-azure-vm'." ;;
  esac

  case "$subcommand" in
    create)    cmd_create "$@" ;;
    setup)     cmd_setup "$@" ;;
    create-azure-vm) cmd_create_vm "$@" ;;
  esac
}

main "$@"
