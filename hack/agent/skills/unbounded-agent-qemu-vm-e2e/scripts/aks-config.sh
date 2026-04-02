#!/usr/bin/env bash
# aks-config.sh — Extract unbounded-agent configuration from an AKS kubeconfig.
#
# Usage:
#   ./aks-config.sh <kubeconfig> [machine-name]
#
# Arguments:
#   kubeconfig    - path to the kubeconfig file for the target AKS cluster
#   machine-name  - optional machine name (default: "agent-vm")
#
# The script prints shell variable assignments that can be eval'd or sourced:
#   eval "$(./aks-config.sh /path/to/kubeconfig)"
#
# Outputs:
#   MACHINA_MACHINE_NAME  - machine name
#   KUBE_VERSION          - kubernetes version without the "v" prefix
#   API_SERVER            - API server endpoint
#   BOOTSTRAP_TOKEN       - bootstrap token (<token-id>.<token-secret>)
#   CA_CERT_BASE64        - base64-encoded CA certificate
#   CLUSTER_DNS           - cluster DNS service IP
#   NODE_LABELS           - node labels including managed=false and cluster RG
set -euo pipefail

usage() {
    echo "Usage: $0 <kubeconfig> [machine-name]" >&2
    exit 1
}

die() { echo "error: $*" >&2; exit 1; }

[[ $# -ge 1 ]] || usage

KUBECONFIG="$1"
MACHINE_NAME="${2:-agent-vm}"

[[ -f "$KUBECONFIG" ]] || die "kubeconfig not found: $KUBECONFIG"
command -v kubectl >/dev/null 2>&1 || die "kubectl is required but not found in PATH"

export KUBECONFIG

# --- API_SERVER ---
api_server=$(kubectl config view --raw -o jsonpath='{.clusters[0].cluster.server}')
[[ -n "$api_server" ]] || die "could not extract API server from kubeconfig"

# --- CA_CERT_BASE64 ---
ca_cert_b64=$(kubectl config view --raw -o jsonpath='{.clusters[0].cluster.certificate-authority-data}')
[[ -n "$ca_cert_b64" ]] || die "could not extract CA certificate from kubeconfig"

# --- KUBE_VERSION (without "v" prefix) ---
kube_version=$(kubectl version -o json 2>/dev/null \
    | python3 -c "import json,sys; print(json.load(sys.stdin)['serverVersion']['gitVersion'].lstrip('v'))" 2>/dev/null) || true
[[ -n "$kube_version" ]] || die "could not determine Kubernetes version from cluster"

# --- CLUSTER_DNS ---
# Detect from kube-dns / coredns service ClusterIP.
cluster_dns=$(kubectl get svc -n kube-system kube-dns -o jsonpath='{.spec.clusterIP}' 2>/dev/null) || true
if [[ -z "$cluster_dns" ]]; then
    cluster_dns=$(kubectl get svc -n kube-system coredns -o jsonpath='{.spec.clusterIP}' 2>/dev/null) || true
fi
[[ -n "$cluster_dns" ]] || die "could not determine cluster DNS IP (looked for kube-dns and coredns services)"

# --- BOOTSTRAP_TOKEN ---
# Find the first bootstrap token secret with usage-bootstrap-authentication=true.
token_id=""
token_secret=""

token_names=$(kubectl get secrets -n kube-system \
    -o jsonpath='{range .items[?(@.type=="bootstrap.kubernetes.io/token")]}{.metadata.name}{"\n"}{end}' 2>/dev/null)

while IFS= read -r name; do
    [[ -n "$name" ]] || continue

    auth=$(kubectl get secret -n kube-system "$name" \
        -o jsonpath='{.data.usage-bootstrap-authentication}' 2>/dev/null | base64 -d 2>/dev/null) || true
    [[ "$auth" == "true" ]] || continue

    token_id=$(kubectl get secret -n kube-system "$name" \
        -o jsonpath='{.data.token-id}' 2>/dev/null | base64 -d 2>/dev/null) || true
    token_secret=$(kubectl get secret -n kube-system "$name" \
        -o jsonpath='{.data.token-secret}' 2>/dev/null | base64 -d 2>/dev/null) || true
    [[ -n "$token_id" && -n "$token_secret" ]] && break

    token_id=""
    token_secret=""
done <<< "$token_names"

[[ -n "$token_id" && -n "$token_secret" ]] || die "no valid bootstrap token found in kube-system secrets"
bootstrap_token="${token_id}.${token_secret}"

# --- NODE RESOURCE GROUP (for NODE_LABELS) ---
# Extract from existing node labels.
cluster_rg=$(kubectl get nodes -o jsonpath='{.items[0].metadata.labels.kubernetes\.azure\.com/cluster}' 2>/dev/null) || true

node_labels="kubernetes.azure.com/managed=false"
if [[ -n "$cluster_rg" ]]; then
    node_labels="${node_labels},kubernetes.azure.com/cluster=${cluster_rg}"
fi

# --- Output ---
cat <<EOF
export MACHINA_MACHINE_NAME='${MACHINE_NAME}'
export KUBE_VERSION='${kube_version}'
export API_SERVER='${api_server}'
export BOOTSTRAP_TOKEN='${bootstrap_token}'
export CA_CERT_BASE64='${ca_cert_b64}'
export CLUSTER_DNS='${cluster_dns}'
export NODE_LABELS='${node_labels}'
EOF
