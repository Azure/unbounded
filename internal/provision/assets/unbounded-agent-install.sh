#!/bin/bash
set -eo pipefail

# Required environment variable:
#   UNBOUNDED_AGENT_CONFIG_FILE - path to the JSON agent config file
#
# The script reads the JSON config and exports environment variables that
# the current agent binary expects. This bridges the structured config
# format with the legacy env-var-based interface.

if [ -z "${UNBOUNDED_AGENT_CONFIG_FILE}" ]; then
    echo "UNBOUNDED_AGENT_CONFIG_FILE is not set" >&2
    exit 1
fi

if [ ! -f "${UNBOUNDED_AGENT_CONFIG_FILE}" ]; then
    echo "config file not found: ${UNBOUNDED_AGENT_CONFIG_FILE}" >&2
    exit 1
fi

# ---------------------------------------------------------------------------
# JSON parsing helpers — prefer jq, fall back to python3.
# ---------------------------------------------------------------------------
if command -v jq &>/dev/null; then
    _json_get() { jq -r "$1 // \"\"" "${UNBOUNDED_AGENT_CONFIG_FILE}"; }
    _json_labels() {
        jq -r '
            .Kubelet.Labels // {}
            | to_entries
            | map("\(.key)=\(.value)")
            | join(",")
        ' "${UNBOUNDED_AGENT_CONFIG_FILE}"
    }
elif command -v python3 &>/dev/null; then
    _json_get() {
        python3 -c "
import json, sys, functools, operator
with open('${UNBOUNDED_AGENT_CONFIG_FILE}') as f:
    d = json.load(f)
keys = sys.argv[1].lstrip('.').split('.')
try:
    v = functools.reduce(operator.getitem, keys, d)
except (KeyError, TypeError):
    v = ''
print(v if v is not None else '')
" "$1"
    }
    _json_labels() {
        python3 -c "
import json
with open('${UNBOUNDED_AGENT_CONFIG_FILE}') as f:
    d = json.load(f)
labels = d.get('Kubelet', {}).get('Labels') or {}
print(','.join(f'{k}={v}' for k, v in labels.items()))
"
    }
else
    echo "neither jq nor python3 found; cannot parse config" >&2
    exit 1
fi

# ---------------------------------------------------------------------------
# Map JSON config to environment variables for backwards compatibility.
# ---------------------------------------------------------------------------
export MACHINA_MACHINE_NAME="$(_json_get '.MachineName')"
export API_SERVER="$(_json_get '.Kubelet.ApiServer')"
export BOOTSTRAP_TOKEN="$(_json_get '.Kubelet.BootstrapToken')"
export CA_CERT_BASE64="$(_json_get '.Cluster.CaCertBase64')"
export CLUSTER_DNS="$(_json_get '.Cluster.ClusterDNS')"

KUBE_VERSION="$(_json_get '.Cluster.Version')"
KUBE_VERSION="${KUBE_VERSION#v}"
export KUBE_VERSION

export NODE_LABELS="$(_json_labels)"

# ---------------------------------------------------------------------------
# Download and run the agent.
# ---------------------------------------------------------------------------
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
