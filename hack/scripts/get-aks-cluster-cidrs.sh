#!/usr/bin/env bash
# get-aks-cluster-cidrs.sh -- Detect CIDRs for an AKS cluster and print a
# ready-to-paste "kubectl unbounded site init" command.
#
# All options are optional for a standard AKS cluster connected to the current
# kubeconfig context. Subscription, resource group, and cluster name are
# auto-detected from node spec.providerID.
#
# Usage:
#   hack/scripts/get-aks-cluster-cidrs.sh [options]
#
# Options:
#   -s, --subscription ID      Azure subscription ID
#                              (auto-detected from node spec.providerID)
#   -g, --resource-group NAME  AKS resource group
#                              (auto-detected from node spec.providerID)
#   -n, --cluster-name NAME    AKS cluster name
#                              (auto-detected from node spec.providerID)
#       --context NAME         kubeconfig context (defaults to current context)
#   -h, --help                 Show this help and exit

set -euo pipefail

# ── helpers ──────────────────────────────────────────────────────────────────

die() { echo "error: $*" >&2; exit 1; }

usage() {
  sed -n '/^# Usage:/,/^[^#]/{ /^#/{ s/^# \{0,1\}//; p }; /^[^#]/q }' "$0"
  exit 0
}

require_cmd() {
  command -v "$1" >/dev/null 2>&1 || die "$1 not found. $2"
}

# ── argument parsing ──────────────────────────────────────────────────────────

OPT_SUBSCRIPTION=""
OPT_RESOURCE_GROUP=""
OPT_CLUSTER_NAME=""
OPT_CONTEXT=""

while [[ $# -gt 0 ]]; do
  case "$1" in
    -s|--subscription)    OPT_SUBSCRIPTION="$2";   shift 2 ;;
    -g|--resource-group)  OPT_RESOURCE_GROUP="$2"; shift 2 ;;
    -n|--cluster-name)    OPT_CLUSTER_NAME="$2";   shift 2 ;;
    --context)            OPT_CONTEXT="$2";        shift 2 ;;
    -h|--help)            usage ;;
    *) die "unknown option: $1. Use --help for usage." ;;
  esac
done

# ── preflight checks ──────────────────────────────────────────────────────────

require_cmd kubectl "Install kubectl: https://kubernetes.io/docs/tasks/tools/"
require_cmd az      "Install the Azure CLI: https://aka.ms/installazurecli"

az account show --output none 2>/dev/null \
  || die "not logged in to Azure. Run: az login"

# ── kubectl context args ───────────────────────────────────────────────────────

KUBECTL_CTX_ARGS=()
if [[ -n "$OPT_CONTEXT" ]]; then
  KUBECTL_CTX_ARGS=(--context "$OPT_CONTEXT")
fi

# ── auto-detect from node providerID ─────────────────────────────────────────

if [[ -z "$OPT_SUBSCRIPTION" ]] || [[ -z "$OPT_RESOURCE_GROUP" ]] || [[ -z "$OPT_CLUSTER_NAME" ]]; then
  echo "Detecting cluster identity from node spec.providerID..."

  PROVIDER_ID=$(kubectl "${KUBECTL_CTX_ARGS[@]}" get nodes \
    -o jsonpath='{.items[0].spec.providerID}' 2>/dev/null || true)

  [[ -z "$PROVIDER_ID" ]] && die "no nodes found in the current cluster."

  [[ "$PROVIDER_ID" == azure://* ]] \
    || die "not an AKS cluster (providerID: $PROVIDER_ID). This script supports AKS only."

  # Extract subscription and node resource group from the providerID path.
  # Format: azure:///subscriptions/{sub}/resourceGroups/{nodeRG}/...
  remainder="${PROVIDER_ID#azure:///subscriptions/}"
  DETECTED_SUB="${remainder%%/*}"
  remainder="${remainder#*/resourceGroups/}"
  DETECTED_NODE_RG="${remainder%%/*}"

  [[ -z "$DETECTED_SUB" ]] && die "could not parse subscription from providerID '$PROVIDER_ID'. Pass --subscription explicitly."
  [[ -z "$DETECTED_NODE_RG" ]] && die "could not parse node resource group from providerID '$PROVIDER_ID'. Pass --resource-group and --cluster-name explicitly."

  DETECTED_RG=""
  DETECTED_CLUSTER=""
  DETECTED_LOCATION=""

  # Strategy 1: Try the AKS node resource group convention MC_{rg}_{cluster}_{location}.
  node_rg_lower="${DETECTED_NODE_RG,,}"
  if [[ "$node_rg_lower" == mc_* ]]; then
    inner="${DETECTED_NODE_RG:3}"   # strip leading "MC_" or "mc_"
    # Split on '_'; location=last, cluster=second-to-last, rg=everything before.
    IFS='_' read -ra parts <<< "$inner"
    n="${#parts[@]}"
    if [[ $n -ge 3 ]]; then
      DETECTED_LOCATION="${parts[$((n-1))]}"
      DETECTED_CLUSTER="${parts[$((n-2))]}"
      DETECTED_RG="${parts[*]:0:$((n-2))}"
      DETECTED_RG="${DETECTED_RG// /_}"
    fi
  fi

  # Strategy 2: If MC_ parsing didn't work, query Azure for the AKS cluster
  # whose nodeResourceGroup matches what we extracted from the providerID.
  if [[ -z "$DETECTED_RG" ]] || [[ -z "$DETECTED_CLUSTER" ]]; then
    echo "Node resource group '$DETECTED_NODE_RG' does not follow the MC_ convention. Querying Azure for cluster identity..."
    mapfile -t _cluster_info < <(
      az aks list \
        --subscription "$DETECTED_SUB" \
        --query "[?nodeResourceGroup=='${DETECTED_NODE_RG}'] | [0].[resourceGroup, name, location]" \
        --output tsv 2>/dev/null || true
    )
    DETECTED_RG="${_cluster_info[0]:-}"
    DETECTED_CLUSTER="${_cluster_info[1]:-}"
    DETECTED_LOCATION="${_cluster_info[2]:-}"
  fi

  [[ -z "$DETECTED_RG" ]] || [[ -z "$DETECTED_CLUSTER" ]] && \
    die "could not detect AKS cluster for node resource group '$DETECTED_NODE_RG'. Pass --subscription, --resource-group, and --cluster-name explicitly."

  SUB="${OPT_SUBSCRIPTION:-$DETECTED_SUB}"
  NODE_RG="$DETECTED_NODE_RG"
  RG="${OPT_RESOURCE_GROUP:-$DETECTED_RG}"
  CLUSTER="${OPT_CLUSTER_NAME:-$DETECTED_CLUSTER}"
  LOCATION="$DETECTED_LOCATION"

  echo "  subscription:       $SUB"
  echo "  resource group:     $RG"
  echo "  cluster:            $CLUSTER"
  echo "  location:           $LOCATION"
  echo "  node resource group: $NODE_RG"
  echo
else
  SUB="$OPT_SUBSCRIPTION"
  RG="$OPT_RESOURCE_GROUP"
  CLUSTER="$OPT_CLUSTER_NAME"

  # Still need the node resource group for VNet lookup.
  echo "Fetching node resource group from AKS..."
  NODE_RG=$(az aks show \
    --subscription "$SUB" \
    --resource-group "$RG" \
    --name "$CLUSTER" \
    --query nodeResourceGroup \
    --output tsv)
fi

# ── fetch pod and service CIDRs from az aks show ──────────────────────────────

echo "Fetching network profile from AKS..."
mapfile -t _cidrs < <(az aks show \
  --subscription "$SUB" \
  --resource-group "$RG" \
  --name "$CLUSTER" \
  --query "[networkProfile.podCidr, networkProfile.serviceCidr]" \
  --output tsv)
POD_CIDR="${_cidrs[0]:-}"
SERVICE_CIDR="${_cidrs[1]:-}"

# Azure CLI returns the literal string "None" for null values in tsv output.
[[ "$POD_CIDR" == "None" ]]     && POD_CIDR=""
[[ "$SERVICE_CIDR" == "None" ]] && SERVICE_CIDR=""

[[ -z "$POD_CIDR" ]]     && die "cluster has no pod CIDR in its network profile (possibly BYO CNI). Pass --cluster-pod-cidr explicitly to kubectl unbounded site init."
[[ -z "$SERVICE_CIDR" ]] && die "could not determine service CIDR from AKS network profile. Pass --cluster-service-cidr explicitly to kubectl unbounded site init."

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

echo "Fetching VNet subnets from node resource group..."

NODE_IPS=$(kubectl "${KUBECTL_CTX_ARGS[@]}" get nodes \
  -o jsonpath='{range .items[?(@.spec.providerID)]}{range .status.addresses[?(@.type=="InternalIP")]}{.address}{"\n"}{end}{end}' \
  | grep -v '^$')

[[ -z "$NODE_IPS" ]] && die "could not retrieve node internal IPs."

NODE_CIDR=$(az network vnet list \
  --subscription "$SUB" \
  --resource-group "$NODE_RG" \
  --query "[].subnets[].addressPrefix" \
  --output tsv | while IFS= read -r prefix; do
    [[ -z "$prefix" ]] && continue
    subnet_contains_all "$prefix" "$NODE_IPS" && echo "$prefix" && break
  done)

[[ -z "$NODE_CIDR" ]] && die "could not find a VNet subnet containing all node IPs. Pass --cluster-node-cidr explicitly to kubectl unbounded site init."

# ── print results ─────────────────────────────────────────────────────────────

echo
echo "Detected CIDRs for AKS cluster '$CLUSTER' ($RG / $LOCATION):"
echo
printf "  %-26s %s\n" "Cluster node CIDR:"    "$NODE_CIDR"
printf "  %-26s %s\n" "Cluster pod CIDR:"     "$POD_CIDR"
printf "  %-26s %s\n" "Cluster service CIDR:" "$SERVICE_CIDR"
echo
echo "Run the following to initialize an unbounded site."
echo "Edit <SITE-NAME>, <REMOTE-NODE-CIDR>, and <REMOTE-POD-CIDR> before running:"
echo
echo "  kubectl unbounded site init \\"
echo "    --name <SITE-NAME> \\"
printf "    --cluster-node-cidr    %-20s\\\\\n" "$NODE_CIDR"
printf "    --cluster-pod-cidr     %-20s\\\\\n" "$POD_CIDR"
printf "    --cluster-service-cidr %-20s\\\\\n" "$SERVICE_CIDR"
echo "    --node-cidr <REMOTE-NODE-CIDR> \\"
echo "    --pod-cidr  <REMOTE-POD-CIDR>"
echo
