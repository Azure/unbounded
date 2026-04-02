#!/bin/bash
set -eo pipefail

# Required environment variable:
#   UNBOUNDED_AGENT_CONFIG_FILE - path to the JSON agent config file
#
# The agent binary reads the config file directly via the same environment
# variable, so this script only needs to validate it exists, download the
# agent, and run it.

if [ -z "${UNBOUNDED_AGENT_CONFIG_FILE}" ]; then
    echo "UNBOUNDED_AGENT_CONFIG_FILE is not set" >&2
    exit 1
fi

if [ ! -f "${UNBOUNDED_AGENT_CONFIG_FILE}" ]; then
    echo "config file not found: ${UNBOUNDED_AGENT_CONFIG_FILE}" >&2
    exit 1
fi

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
