#!/bin/bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

set -eo pipefail

# Required environment variable:
#   UNBOUNDED_AGENT_CONFIG_FILE - path to the JSON agent config file
#
# The agent binary reads the config file directly via the same environment
# variable, so this script only needs to validate it exists, download the
# agent, and run it.
#
# Optional environment variables:
#   AGENT_VERSION         - unbounded-agent release version (default: "v0.0.10")
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
AGENT_VERSION="${AGENT_VERSION:-v0.0.10}"

arch="$(uname -m)"
case "$arch" in
    "x86_64") arch="amd64" ;;
    "aarch64") arch="arm64" ;;
    *) echo "unsupported architecture: $arch" >&2; exit 1 ;;
esac

if [ -z "${AGENT_URL}" ]; then
    AGENT_URL="https://github.com/Azure/unbounded-kube/releases/download/${AGENT_VERSION}/unbounded-agent-linux-${arch}.tar.gz"
fi
AGENT_BIN_BLUE="/usr/local/bin/unbounded-agent-blue"
AGENT_BIN_GREEN="/usr/local/bin/unbounded-agent-green"
AGENT_BIN_CURRENT="/usr/local/bin/unbounded-agent-current"
AGENT_BIN_LAST_GOOD="/usr/local/bin/unbounded-agent-last-good"

echo "Downloading unbounded-agent ${AGENT_VERSION} for ${arch}..."
ACTIVE_BIN="$(readlink -f "${AGENT_BIN_CURRENT}" || true)"
if [ "${ACTIVE_BIN}" = "${AGENT_BIN_BLUE}" ]; then
    NEXT_BIN="${AGENT_BIN_GREEN}"
else
    NEXT_BIN="${AGENT_BIN_BLUE}"
fi

tmp_dir="$(mktemp -d)"
trap 'rm -rf "${tmp_dir}"' EXIT

archive_path="${tmp_dir}/unbounded-agent.tar.gz"
if ! curl -fsSL "${AGENT_URL}" -o "${archive_path}"; then
    echo "failed to download unbounded-agent archive: ${AGENT_URL}" >&2
    exit 1
fi

if ! tar -xzf "${archive_path}" -C "${tmp_dir}" unbounded-agent; then
    echo "failed to extract unbounded-agent from archive: ${archive_path}" >&2
    exit 1
fi

install -m 0755 "${tmp_dir}/unbounded-agent" "${NEXT_BIN}"

if [ -x "${ACTIVE_BIN}" ]; then
    ln -sfn "${ACTIVE_BIN}" "${AGENT_BIN_LAST_GOOD}"
elif [ -x "${NEXT_BIN}" ]; then
    ln -sfn "${NEXT_BIN}" "${AGENT_BIN_LAST_GOOD}"
fi

ln -sfn "${NEXT_BIN}" "${AGENT_BIN_CURRENT}"

_START_ARGS=""
case "${AGENT_DEBUG}" in
    1|true|yes|TRUE|YES|True|Yes) _START_ARGS="--debug" ;;
esac

echo "Running unbounded-agent start..."
"${AGENT_BIN_CURRENT}" start ${_START_ARGS}
