#!/bin/bash
set -eo pipefail

# Required environment variable:
#   UNBOUNDED_AGENT_CONFIG_FILE - path to the JSON agent config file
#
# The agent binary reads the config file directly via the same environment
# variable, so this script only needs to validate it exists, download the
# agent, and run it.
#
# Optional environment variables:
#   AGENT_VERSION         - unbounded-agent release version (default: "v0.0.5")
#   AGENT_URL             - override the download URL for the unbounded-agent tarball
#   AGENT_DEBUG           - enable debug mode for unbounded-agent (e.g. "1", "true", "yes")

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
AGENT_VERSION="${AGENT_VERSION:-v0.0.5}"

arch="$(uname -m)"
case "$arch" in
    "x86_64") arch="amd64" ;;
    "aarch64") arch="arm64" ;;
    *) echo "unsupported architecture: $arch" >&2; exit 1 ;;
esac

if [ -z "${AGENT_URL}" ]; then
    AGENT_URL="https://github.com/project-unbounded/unbounded-kube/releases/download/${AGENT_VERSION}/unbounded-agent-linux-${arch}.tar.gz"
fi
AGENT_BIN="/usr/local/bin/unbounded-agent"

echo "Downloading unbounded-agent ${AGENT_VERSION} for ${arch}..."
curl -fsSL "${AGENT_URL}" | tar -xz -C /usr/local/bin unbounded-agent
chmod +x "${AGENT_BIN}"

_START_ARGS=""
case "${AGENT_DEBUG}" in
    1|true|yes|TRUE|YES|True|Yes) _START_ARGS="--debug" ;;
esac

echo "Running unbounded-agent start..."
"${AGENT_BIN}" start ${_START_ARGS}
