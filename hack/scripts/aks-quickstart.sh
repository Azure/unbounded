#!/usr/bin/env bash
# aks-quickstart.sh -- Create or configure an AKS cluster for unbounded-kube
# with gateway node pools, unbounded-net CNI, machina controller, and
# bootstrap tokens.
#
# Usage:
#   aks-quickstart.sh create [options]     Create a new AKS cluster from scratch
#   aks-quickstart.sh setup  [options]     Add gateways to an existing AKS cluster
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
#   --cni-version VERSION        unbounded-net version (default: latest release)
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
#   --cni-version VERSION        unbounded-net version (default: latest release)
#   --context NAME               kubeconfig context (default: current)
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
UNBOUNDED_NET_REPO="https://github.com/project-unbounded/unbounded-net"

# Defaults
DEFAULT_LOCATION="canadacentral"
DEFAULT_SYSTEM_POOL_SKU="Standard_D2ads_v6"
DEFAULT_SYSTEM_POOL_COUNT="2"
DEFAULT_GATEWAY_POOL_SKU="Standard_D2ads_v6"
DEFAULT_GATEWAY_POOL_COUNT="2"
DEFAULT_SERVICE_CIDR="10.0.0.0/16"
DEFAULT_SITE_NAME="remote"
DEFAULT_PUBLIC_IP_STRATEGY="node"

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
  require_cmd openssl "Install openssl for generating bootstrap tokens."

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
  local cni_version="${7:-}"

  # Step 1: Verify gateway nodes exist.
  log "Verifying gateway nodes..."
  local gw_node_count
  gw_node_count=$(kubectl "${KUBECTL_CTX_ARGS[@]}" get nodes \
    -l "$GATEWAY_LABEL" \
    -o name 2>/dev/null | wc -l || true)

  (( gw_node_count > 0 )) || die "no nodes with label $GATEWAY_LABEL found. Ensure the gateway node pool is running."
  info "Found $gw_node_count gateway node(s)"

  # Step 2: Install unbounded-net CNI.
  install_unbounded_net "$cni_version"

  # Step 3: Create GatewayPool CR.
  create_gateway_pool_cr

  # Step 4: Create cluster site resources.
  # The controller needs Site CRs to assign nodes to sites and allocate pod
  # CIDRs.  Nodes will not become Ready until their CNI is configured, which
  # requires a pod CIDR from the controller, which requires a matching Site.
  create_site_cr "cluster" "$cluster_node_cidr" "$cluster_pod_cidr"

  # Step 5: Create remote site resources.
  create_site_cr "$site_name" "$remote_node_cidr" "$remote_pod_cidr"

  # Step 6: Wait for nodes to become Ready.
  # Now that the CNI is installed and Site CRs exist, the controller can
  # allocate pod CIDRs and the node agent can configure networking.
  wait_for_nodes_ready "$GATEWAY_LABEL" 300

  # Step 7: Create bootstrap token.
  create_bootstrap_token "$site_name"

  # Step 8: Apply flex agent kubeadm config.
  apply_flex_agent_config "$cluster_service_cidr"

  # Step 9: Install machina controller.
  install_machina

  # Step 10: Print summary.
  print_summary "$site_name"
}

install_unbounded_net() {
  local cni_version="${1:-}"

  # Resolve "latest" or empty to the actual latest release tag.
  if [[ -z "$cni_version" ]] || [[ "$cni_version" == "latest" ]]; then
    log "Resolving latest unbounded-net release..."
    cni_version=$(curl -sI -o /dev/null -w '%{url_effective}' -L "${UNBOUNDED_NET_REPO}/releases/latest" \
      | grep -oP 'tag/\K.*')
    [[ -n "$cni_version" ]] || die "could not resolve latest unbounded-net release"
    info "Latest version: $cni_version"
  fi

  local release_url="${UNBOUNDED_NET_REPO}/releases/download/${cni_version}/unbounded-net-manifests-${cni_version}.tar.gz"

  log "Installing unbounded-net CNI ${cni_version}..."

  local tmpdir
  tmpdir=$(mktemp -d)
  trap "rm -rf '$tmpdir'" RETURN

  info "Downloading $release_url"
  curl -fsSL "$release_url" | tar xz -C "$tmpdir"

  # Apply all manifests recursively — the tarball contains subdirectories
  # (crds/, controller/, node/) alongside top-level namespace/configmap YAMLs.
  # --force-conflicts is needed on re-runs because the unbounded-net controller
  # takes ownership of caBundle fields on its webhooks and API service at runtime.
  kubectl "${KUBECTL_CTX_ARGS[@]}" apply -R -f "$tmpdir/" --server-side --force-conflicts 2>&1 | \
    while IFS= read -r line; do info "$line"; done

  log "Waiting for unbounded-net controller to be ready..."
  kubectl "${KUBECTL_CTX_ARGS[@]}" -n unbounded-net rollout status deployment/unbounded-net-controller \
    --timeout=120s 2>&1 | while IFS= read -r line; do info "$line"; done

  info "unbounded-net CNI installed"
}

create_gateway_pool_cr() {
  log "Creating GatewayPool 'gw-main'..."
  kubectl "${KUBECTL_CTX_ARGS[@]}" apply --server-side -f - <<'EOF'
---
apiVersion: net.unbounded-kube.io/v1alpha1
kind: GatewayPool
metadata:
  name: gw-main
spec:
  nodeSelector:
    unbounded-kube.io/unbounded-net-gateway: "true"
  type: External
EOF
  info "GatewayPool 'gw-main' created"
}

create_site_cr() {
  local site_name="$1"
  local node_cidr="$2"
  local pod_cidr="$3"

  log "Creating Site and SiteGatewayPoolAssignment for '$site_name'..."

  kubectl "${KUBECTL_CTX_ARGS[@]}" apply --server-side -f - <<EOF
---
apiVersion: net.unbounded-kube.io/v1alpha1
kind: Site
metadata:
  name: ${site_name}
  labels:
    unbounded-kube.io/site: "${site_name}"
spec:
  nodeCidrs:
    - ${node_cidr}
  podCidrAssignments:
    - cidrBlocks:
        - ${pod_cidr}
---
apiVersion: net.unbounded-kube.io/v1alpha1
kind: SiteGatewayPoolAssignment
metadata:
  name: ${site_name}
  labels:
    unbounded-kube.io/site: "${site_name}"
spec:
  sites:
    - ${site_name}
  gatewayPools:
    - gw-main
EOF
  info "Site '$site_name' created"
}

create_bootstrap_token() {
  local site_name="$1"

  log "Creating bootstrap token for site '$site_name'..."

  # Check if a token already exists for this site.
  local existing
  existing=$(kubectl "${KUBECTL_CTX_ARGS[@]}" get secrets -n kube-system \
    -l "unbounded-kube.io/site=${site_name}" \
    --field-selector type=bootstrap.kubernetes.io/token \
    -o name 2>/dev/null || true)

  if [[ -n "$existing" ]]; then
    info "Bootstrap token already exists for site '$site_name', skipping"
    # Extract the existing token for the summary.
    BOOTSTRAP_TOKEN=$(kubectl "${KUBECTL_CTX_ARGS[@]}" get "$existing" -n kube-system \
      -o jsonpath='{.data.token-id}' 2>/dev/null | base64 -d 2>/dev/null || true)
    local secret
    secret=$(kubectl "${KUBECTL_CTX_ARGS[@]}" get "$existing" -n kube-system \
      -o jsonpath='{.data.token-secret}' 2>/dev/null | base64 -d 2>/dev/null || true)
    if [[ -n "$BOOTSTRAP_TOKEN" ]] && [[ -n "$secret" ]]; then
      BOOTSTRAP_TOKEN="${BOOTSTRAP_TOKEN}.${secret}"
    else
      BOOTSTRAP_TOKEN="(existing — retrieve from secret $existing)"
    fi
    return 0
  fi

  local token_id token_secret
  token_id=$(openssl rand -hex 3)
  token_secret=$(openssl rand -hex 8)

  kubectl "${KUBECTL_CTX_ARGS[@]}" apply --server-side -f - <<EOF
apiVersion: v1
kind: Secret
metadata:
  name: bootstrap-token-${token_id}
  namespace: kube-system
  labels:
    unbounded-kube.io/default-bootstrap-token: "true"
    unbounded-kube.io/site: "${site_name}"
type: bootstrap.kubernetes.io/token
stringData:
  token-id: "${token_id}"
  token-secret: "${token_secret}"
  usage-bootstrap-authentication: "true"
  usage-bootstrap-signing: "true"
  auth-extra-groups: "system:bootstrappers:kubeadm:default-node-token"
EOF

  BOOTSTRAP_TOKEN="${token_id}.${token_secret}"
  info "Bootstrap token created: ${BOOTSTRAP_TOKEN}"
}

apply_flex_agent_config() {
  local service_cidr="$1"

  log "Applying flex agent kubeadm config..."

  # Extract CA certificate from kubeconfig.
  local ca_data
  ca_data=$(kubectl "${KUBECTL_CTX_ARGS[@]}" config view --raw \
    -o jsonpath='{.clusters[0].cluster.certificate-authority-data}')
  [[ -n "$ca_data" ]] || die "could not extract CA certificate from kubeconfig"

  # Extract API server URL.
  local server_url
  server_url=$(kubectl "${KUBECTL_CTX_ARGS[@]}" config view --raw \
    -o jsonpath='{.clusters[0].cluster.server}')
  [[ -n "$server_url" ]] || die "could not extract API server URL from kubeconfig"

  # Get Kubernetes version.
  local k8s_version
  k8s_version=$(kubectl "${KUBECTL_CTX_ARGS[@]}" version -o json 2>/dev/null \
    | jq -r '.serverVersion.gitVersion')
  [[ -n "$k8s_version" ]] && [[ "$k8s_version" != "null" ]] \
    || die "could not determine Kubernetes version"

  info "server:     $server_url"
  info "k8s:        $k8s_version"
  info "service:    $service_cidr"

  kubectl "${KUBECTL_CTX_ARGS[@]}" apply --server-side -f - <<EOF
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  namespace: kube-system
  name: kubeadm:nodes-kubeadm-config
rules:
- verbs: ["get"]
  apiGroups: [""]
  resources: ["configmaps"]
  resourceNames: ["kubeadm-config"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  namespace: kube-system
  name: kubeadm:nodes-kubeadm-config
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: kubeadm:nodes-kubeadm-config
subjects:
- kind: Group
  name: system:bootstrappers:kubeadm:default-node-token
---
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  namespace: kube-system
  name: kubeadm:kubelet-config
rules:
- verbs: ["get"]
  apiGroups: [""]
  resources: ["configmaps"]
  resourceNames: ["kubelet-config"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  namespace: kube-system
  name: kubeadm:kubelet-config
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: kubeadm:kubelet-config
subjects:
- kind: Group
  name: system:bootstrappers:kubeadm:default-node-token
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: kubeadm:get-nodes
rules:
- verbs: ["get"]
  apiGroups: [""]
  resources: ["nodes"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: kubeadm:get-nodes
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: kubeadm:get-nodes
subjects:
- kind: Group
  name: system:bootstrappers:kubeadm:default-node-token
---
apiVersion: v1
kind: ConfigMap
metadata:
  namespace: kube-public
  name: cluster-info
data:
  kubeconfig: |
    apiVersion: v1
    kind: Config
    clusters:
    - cluster:
        certificate-authority-data: ${ca_data}
        server: ${server_url}
---
apiVersion: v1
kind: ConfigMap
metadata:
  namespace: kube-system
  name: kubeadm-config
data:
  ClusterConfiguration: |
    apiVersion: kubeadm.k8s.io/v1beta4
    kind: ClusterConfiguration
    kubernetesVersion: ${k8s_version}
    networking:
      serviceSubnet: ${service_cidr}
---
apiVersion: v1
kind: ConfigMap
metadata:
  namespace: kube-system
  name: kubelet-config
data:
  kubelet: |
    apiVersion: kubelet.config.k8s.io/v1beta1
    kind: KubeletConfiguration
EOF

  info "Flex agent kubeadm config applied"
}

install_machina() {
  log "Installing machina controller..."

  local script_dir repo_root
  script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
  repo_root="$(cd "$script_dir/../.." && pwd)"

  local machina_dir="$repo_root/deploy/machina"
  [[ -d "$machina_dir" ]] || die "machina manifests not found at $machina_dir. Are you running from the unbounded-kube repository?"

  # Apply namespace first so the ConfigMap can be created.
  kubectl "${KUBECTL_CTX_ARGS[@]}" apply --server-side -f "$machina_dir/01-namespace.yaml" 2>&1 | \
    while IFS= read -r line; do info "$line"; done

  # Apply CRD.
  kubectl "${KUBECTL_CTX_ARGS[@]}" apply --server-side -f "$machina_dir/crd/unbounded-kube.io_machines.yaml" 2>&1 | \
    while IFS= read -r line; do info "$line"; done

  # Apply RBAC.
  kubectl "${KUBECTL_CTX_ARGS[@]}" apply --server-side -f "$machina_dir/02-rbac.yaml" 2>&1 | \
    while IFS= read -r line; do info "$line"; done

  # Generate custom config with API server endpoint (skip the default 03-config.yaml).
  local server_url
  server_url=$(kubectl "${KUBECTL_CTX_ARGS[@]}" config view --raw \
    -o jsonpath='{.clusters[0].cluster.server}')

  kubectl "${KUBECTL_CTX_ARGS[@]}" apply --server-side -f - <<EOF
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: machina-config
  namespace: machina-system
data:
  config.yaml: |
    # Machina Controller Configuration
    apiServerEndpoint: "${server_url}"
    metricsAddr: ":8080"
    probeAddr: ":8081"
    enableLeaderElection: false
    maxConcurrentReconciles: 50
EOF

  # Apply deployment and service.
  kubectl "${KUBECTL_CTX_ARGS[@]}" apply --server-side -f "$machina_dir/04-deployment.yaml" 2>&1 | \
    while IFS= read -r line; do info "$line"; done
  kubectl "${KUBECTL_CTX_ARGS[@]}" apply --server-side -f "$machina_dir/05-service.yaml" 2>&1 | \
    while IFS= read -r line; do info "$line"; done

  log "Waiting for machina controller to be ready..."
  kubectl "${KUBECTL_CTX_ARGS[@]}" -n machina-system rollout status deployment/machina-controller \
    --timeout=120s 2>&1 | while IFS= read -r line; do info "$line"; done

  info "Machina controller installed"
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
  local cni_version=""

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
      --cni-version)         cni_version="$2";         shift 2 ;;
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

  # Step 7: Run site init (installs CNI, which makes nodes Ready).
  do_site_init "$site_name" "$CLUSTER_NODE_CIDR" "$CLUSTER_POD_CIDR" "$CLUSTER_SERVICE_CIDR" "$remote_node_cidr" "$remote_pod_cidr" "$cni_version"
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
  local cni_version=""

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
      --cni-version)         cni_version="$2";         shift 2 ;;
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

  # Step 6: Run site init (installs CNI, which makes nodes Ready).
  do_site_init "$site_name" "$CLUSTER_NODE_CIDR" "$CLUSTER_POD_CIDR" "$CLUSTER_SERVICE_CIDR" "$remote_node_cidr" "$remote_pod_cidr" "$cni_version"
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
BOOTSTRAP_TOKEN=""
KUBECTL_CTX_ARGS=()

main() {
  if [[ $# -eq 0 ]]; then
    usage
  fi

  local subcommand="$1"
  shift

  preflight

  case "$subcommand" in
    create) cmd_create "$@" ;;
    setup)  cmd_setup "$@" ;;
    -h|--help) usage ;;
    *) die "unknown subcommand: $subcommand. Use 'create' or 'setup'." ;;
  esac
}

main "$@"
