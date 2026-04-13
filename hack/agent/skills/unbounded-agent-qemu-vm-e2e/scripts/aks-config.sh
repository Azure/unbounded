#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

# aks-config.sh - Extract unbounded-agent configuration from an AKS kubeconfig
# and write a JSON config file matching the AgentConfig schema.
#
# Usage:
#   ./aks-config.sh <kubeconfig> [machine-name]
#
# Arguments:
#   kubeconfig    - path to the kubeconfig file for the target AKS cluster
#   machine-name  - optional machine name (default: "agent-vm")
#
# Environment variables:
#   REGISTER_WITH_TAINTS - optional comma-separated taints in "key=value:effect"
#                          format (e.g. "dedicated=gpu:NoSchedule,workload=ml:PreferNoSchedule")
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

export KUBECONFIG

# --- API_SERVER ---
api_server=$(kubectl config view --raw -o jsonpath='{.clusters[0].cluster.server}')
[[ -n "$api_server" ]] || die "could not extract API server from kubeconfig"

# --- CA_CERT_BASE64 ---
ca_cert_b64=$(kubectl config view --raw -o jsonpath='{.clusters[0].cluster.certificate-authority-data}')
[[ -n "$ca_cert_b64" ]] || die "could not extract CA certificate from kubeconfig"

# --- KUBE_VERSION ---
# The second "gitVersion" entry in the JSON output is the server version.
kube_version=$(kubectl version -o json 2>/dev/null \
    | grep '"gitVersion"' | tail -1 \
    | tr -d ' ",' | cut -d: -f2) || true
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

# --- Build labels JSON object ---
labels_json="\"kubernetes.azure.com/managed\": \"false\""
if [[ -n "$cluster_rg" ]]; then
    labels_json+=", \"kubernetes.azure.com/cluster\": \"${cluster_rg}\""
fi

# --- Build taints JSON array ---
# REGISTER_WITH_TAINTS is optional; split comma-separated entries into a JSON array.
taints_json="[]"
if [[ -n "${REGISTER_WITH_TAINTS:-}" ]]; then
    taints_json="["
    IFS=',' read -ra taint_parts <<< "$REGISTER_WITH_TAINTS"
    first=true
    for t in "${taint_parts[@]}"; do
        $first || taints_json+=","
        taints_json+="\"${t}\""
        first=false
    done
    taints_json+="]"
fi

# --- Render JSON config ---
printf '{\n' > "$OUTPUT_FILE"
printf '  "MachineName": "%s",\n'  "$MACHINE_NAME"          >> "$OUTPUT_FILE"
printf '  "Cluster": {\n'                                    >> "$OUTPUT_FILE"
printf '    "CaCertBase64": "%s",\n' "$ca_cert_b64"          >> "$OUTPUT_FILE"
printf '    "ClusterDNS": "%s",\n'   "$cluster_dns"          >> "$OUTPUT_FILE"
printf '    "Version": "%s"\n'       "$kube_version"         >> "$OUTPUT_FILE"
printf '  },\n'                                              >> "$OUTPUT_FILE"
printf '  "Kubelet": {\n'                                    >> "$OUTPUT_FILE"
printf '    "ApiServer": "%s",\n'      "$api_server"         >> "$OUTPUT_FILE"
printf '    "BootstrapToken": "%s",\n' "$bootstrap_token"    >> "$OUTPUT_FILE"
printf '    "Labels": {%s},\n'         "$labels_json"        >> "$OUTPUT_FILE"
printf '    "RegisterWithTaints": %s\n' "$taints_json"       >> "$OUTPUT_FILE"
printf '  }\n'                                               >> "$OUTPUT_FILE"
printf '}\n'                                                 >> "$OUTPUT_FILE"

echo "Wrote agent config to ${OUTPUT_FILE}"
