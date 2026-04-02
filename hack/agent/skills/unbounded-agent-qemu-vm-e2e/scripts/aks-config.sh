#!/usr/bin/env bash
# aks-config.sh — Extract unbounded-agent configuration from an AKS kubeconfig
# and write a JSON config file matching the AgentConfig schema.
#
# Usage:
#   ./aks-config.sh <kubeconfig> [machine-name]
#
# Arguments:
#   kubeconfig    - path to the kubeconfig file for the target AKS cluster
#   machine-name  - optional machine name (default: "agent-vm")
#
# The script writes unbounded-agent-config-dev.json at the repo root.
# Inside the qemu VM the repo is mounted at /agent, so the agent can read it
# directly:
#
#   UNBOUNDED_AGENT_CONFIG_FILE=/agent/unbounded-agent-config-dev.json unbounded-agent start
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../../../../.." && pwd)"

OUTPUT_FILE="${REPO_ROOT}/unbounded-agent-config-dev.json"

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
command -v jq >/dev/null 2>&1 || die "jq is required but not found in PATH"

export KUBECONFIG

# --- API_SERVER ---
api_server=$(kubectl config view --raw -o jsonpath='{.clusters[0].cluster.server}')
[[ -n "$api_server" ]] || die "could not extract API server from kubeconfig"

# --- CA_CERT_BASE64 ---
ca_cert_b64=$(kubectl config view --raw -o jsonpath='{.clusters[0].cluster.certificate-authority-data}')
[[ -n "$ca_cert_b64" ]] || die "could not extract CA certificate from kubeconfig"

# --- KUBE_VERSION ---
kube_version=$(kubectl version -o json 2>/dev/null | jq -r '.serverVersion.gitVersion') || true
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

# --- NODE RESOURCE GROUP (for Labels) ---
cluster_rg=$(kubectl get nodes -o jsonpath='{.items[0].metadata.labels.kubernetes\.azure\.com/cluster}' 2>/dev/null) || true

# --- Build labels object ---
labels=$(jq -n --arg rg "$cluster_rg" '
    {"kubernetes.azure.com/managed": "false"}
    | if $rg != "" then . + {"kubernetes.azure.com/cluster": $rg} else . end
')

# --- Render JSON config ---
jq -n \
    --arg machineName "$MACHINE_NAME" \
    --arg caCert      "$ca_cert_b64" \
    --arg clusterDNS  "$cluster_dns" \
    --arg version     "$kube_version" \
    --arg apiServer   "$api_server" \
    --arg token       "$bootstrap_token" \
    --argjson labels  "$labels" \
'{
    MachineName: $machineName,
    Cluster: {
        CaCertBase64: $caCert,
        ClusterDNS:   $clusterDNS,
        Version:      $version
    },
    Kubelet: {
        ApiServer:      $apiServer,
        BootstrapToken: $token,
        Labels:         $labels
    }
}' > "$OUTPUT_FILE"

echo "Wrote agent config to ${OUTPUT_FILE}"
