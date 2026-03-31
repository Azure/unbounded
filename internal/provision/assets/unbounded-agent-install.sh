#!/bin/bash
set -eo pipefail

# Required environment variables:
#   MACHINA_MACHINE_NAME  - name of the machine
#   KUBE_VERSION          - kubernetes version (e.g. "1.32.3" or "v1.32.3")
#   API_SERVER            - API server endpoint (e.g. "https://my-cluster.hcp.eastus.azmk8s.io:443")
#   BOOTSTRAP_TOKEN       - bootstrap token in <token-id>.<token-secret> format
#   CA_CERT_BASE64        - base64-encoded PEM CA certificate
#   CLUSTER_DNS           - cluster DNS service IP
#   CLUSTER_RG            - cluster resource group
#   MACHINE_NAME          - machine name

KUBE_VERSION="${KUBE_VERSION#v}"
export KUBE_VERSION

_LABELS="kubernetes.azure.com/managed=false"
_LABELS="${_LABELS},kubernetes.azure.com/cluster=${CLUSTER_RG}"
_LABELS="${_LABELS},machina.project-unbounded.io/machine=${MACHINA_MACHINE_NAME}"
if [ -n "${NODE_LABELS}" ]; then
    NODE_LABELS="${NODE_LABELS},${_LABELS}"
else
    NODE_LABELS="${_LABELS}"
fi
export NODE_LABELS

arch="$(uname -m)"
case "$arch" in
    "x86_64") arch="amd64" ;;
    "aarch64") arch="arm64" ;;
    *) echo "unsupported architecture: $arch" >&2; exit 1 ;;
esac

AGENT_URL="https://aksflxcli.z20.web.core.windows.net/unbounded-agent/unbounded-agent-linux-${arch}"
AGENT_BIN="/usr/local/bin/unbounded-agent"

echo "Downloading unbounded-agent for ${arch}..."
curl -fsSL -o "${AGENT_BIN}" "${AGENT_URL}"
chmod +x "${AGENT_BIN}"

echo "Running unbounded-agent start..."
"${AGENT_BIN}" start

echo "Node bootstrap complete."
