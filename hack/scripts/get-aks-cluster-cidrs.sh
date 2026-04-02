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
require_cmd python3 "Install Python 3.x: https://www.python.org/downloads/"

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

  read -r DETECTED_SUB DETECTED_NODE_RG DETECTED_RG DETECTED_CLUSTER DETECTED_LOCATION < <(
    python3 - "$PROVIDER_ID" <<'PYEOF'
import re, sys

pid = sys.argv[1]
m = re.match(r'azure:///subscriptions/([^/]+)/resourceGroups/([^/]+)/', pid, re.IGNORECASE)
if not m:
    print("", "", "", "", "", flush=True)
    sys.exit(0)

sub     = m.group(1)
node_rg = m.group(2)

# AKS node resource group convention: MC_{rg}_{cluster}_{location}
# The prefix is case-insensitive; Azure lowercases it.
if not re.match(r'mc_', node_rg, re.IGNORECASE):
    print("", "", "", "", "", flush=True)
    sys.exit(0)

# Strip the "mc_" prefix then split; cluster=second-to-last, location=last,
# rg=everything in between (handles underscores in the RG name).
inner  = node_rg[3:]                    # jawilder-test_test_canadacentral
parts  = inner.split('_')
if len(parts) < 3:
    print("", "", "", "", "", flush=True)
    sys.exit(0)

location = parts[-1]
cluster  = parts[-2]
rg       = '_'.join(parts[:-2])

print(sub, node_rg, rg, cluster, location, flush=True)
PYEOF
  )

  [[ -z "$DETECTED_SUB" ]] && die "could not parse providerID '$PROVIDER_ID'. Pass --subscription, --resource-group, and --cluster-name explicitly."

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
AKS_JSON=$(az aks show \
  --subscription "$SUB" \
  --resource-group "$RG" \
  --name "$CLUSTER" \
  --output json)

read -r POD_CIDR SERVICE_CIDR < <(python3 - "$AKS_JSON" <<'PYEOF'
import json, sys
d = json.loads(sys.argv[1])
np = d.get("networkProfile", {})
pod_cidr     = np.get("podCidr") or ""
service_cidr = np.get("serviceCidr") or ""
print(pod_cidr, service_cidr, flush=True)
PYEOF
)

[[ -z "$POD_CIDR" ]]     && die "could not determine pod CIDR from AKS network profile. Pass --cluster-pod-cidr explicitly to kubectl unbounded site init."
[[ -z "$SERVICE_CIDR" ]] && die "could not determine service CIDR from AKS network profile. Pass --cluster-service-cidr explicitly to kubectl unbounded site init."

# ── find node subnet CIDR from VNet ──────────────────────────────────────────

echo "Fetching VNet subnets from node resource group..."

NODE_IPS=$(kubectl "${KUBECTL_CTX_ARGS[@]}" get nodes \
  -o jsonpath='{range .items[*]}{range .status.addresses[?(@.type=="InternalIP")]}{.address}{"\n"}{end}{end}')

[[ -z "$NODE_IPS" ]] && die "could not retrieve node internal IPs."

VNET_JSON=$(az network vnet list \
  --subscription "$SUB" \
  --resource-group "$NODE_RG" \
  --output json)

NODE_CIDR=$(python3 - "$VNET_JSON" "$NODE_IPS" <<'PYEOF'
import ipaddress, json, sys

vnets    = json.loads(sys.argv[1])
node_ips = [ipaddress.ip_address(ip.strip()) for ip in sys.argv[2].strip().splitlines() if ip.strip()]

candidates = []
for vnet in vnets:
    for subnet in vnet.get("subnets", []):
        prefix = subnet.get("addressPrefix") or ""
        if not prefix:
            continue
        try:
            net = ipaddress.ip_network(prefix, strict=False)
        except ValueError:
            continue
        if all(ip in net for ip in node_ips):
            candidates.append(net)

if not candidates:
    print("", flush=True)
    sys.exit(0)

# Pick the most specific (smallest) subnet that contains all node IPs.
candidates.sort(key=lambda n: n.prefixlen, reverse=True)
print(str(candidates[0]), flush=True)
PYEOF
)

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
