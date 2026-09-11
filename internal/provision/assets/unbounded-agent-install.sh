#!/bin/bash
# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0

set -eo pipefail

# Required environment variable:
#   UNBOUNDED_AGENT_CONFIG_FILE - path to the JSON agent config file
#
# The agent binary reads the config file directly via the same environment
# variable, so this script only needs to validate it exists, download the
# agent, and run it.
#
# Optional environment variables (download customization):
#   AGENT_VERSION         - pin to a specific unbounded-agent release tag
#                           (e.g. "v0.0.10"). When unset (default) the script
#                           downloads the latest published GitHub release so
#                           new releases are picked up automatically.
#   AGENT_BASE_URL        - base URL for release downloads. Defaults to
#                           "https://github.com/Azure/unbounded/releases".
#                           Set this to self-host or mirror the release assets
#                           (the layout under the base URL must match the
#                           GitHub releases layout:
#                           <base>/latest/download/<asset> and
#                           <base>/download/<tag>/<asset>).
#   AGENT_URL             - fully qualified download URL for the agent tarball.
#                           When set it overrides AGENT_VERSION and
#                           AGENT_BASE_URL entirely.
#   AGENT_DEBUG           - enable debug mode for unbounded-agent
#                           (e.g. "1", "true", "yes").
#   AGENT_PREFLIGHT       - run unbounded-agent preflight before start.
#                           Defaults to enabled. Set to "0", "false", or
#                           "no" to skip preflight.

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
AGENT_VERSION="${AGENT_VERSION:-}"
AGENT_BASE_URL="${AGENT_BASE_URL:-https://github.com/Azure/unbounded/releases}"

arch="$(uname -m)"
case "$arch" in
    "x86_64") arch="amd64" ;;
    "aarch64") arch="arm64" ;;
    *) echo "unsupported architecture: $arch" >&2; exit 1 ;;
esac

if [ -z "${AGENT_URL}" ]; then
    if [ -z "${AGENT_VERSION}" ]; then
        # Track the latest published release. GitHub's "latest/download"
        # endpoint auto-redirects to the newest release asset, so a new
        # release is picked up without editing this script.
        AGENT_URL="${AGENT_BASE_URL}/latest/download/unbounded-agent-linux-${arch}.tar.gz"
        _version_desc="latest"
    else
        AGENT_URL="${AGENT_BASE_URL}/download/${AGENT_VERSION}/unbounded-agent-linux-${arch}.tar.gz"
        _version_desc="${AGENT_VERSION}"
    fi
else
    _version_desc="${AGENT_VERSION:-custom}"
fi
AGENT_BIN_DIR="${AGENT_HOST_PREFIX:-/usr/local}/bin"
AGENT_BIN="${AGENT_BIN_DIR}/unbounded-agent"

echo "Downloading unbounded-agent ${_version_desc} for ${arch} from ${AGENT_URL}..."
tmp_dir="$(mktemp -d)"
trap 'rm -rf "${tmp_dir}"' EXIT
curl -fsSL "${AGENT_URL}" | tar -xz -C "${tmp_dir}" unbounded-agent
if ! mkdir -p "${AGENT_BIN_DIR}" 2>/dev/null || ! install -m 0755 "${tmp_dir}/unbounded-agent" "${AGENT_BIN}"; then
    echo "error: cannot install the agent to ${AGENT_BIN}." >&2
    echo "Set AGENT_HOST_PREFIX (Machine spec.agent.hostPrefix) to a writable prefix." >&2
    echo "Hosts with a read-only /usr, such as Azure Container Linux, require this." >&2
    exit 1
fi

_START_ARGS=""
case "${AGENT_DEBUG}" in
    1|true|yes|TRUE|YES|True|Yes) _START_ARGS="--debug" ;;
esac

case "${AGENT_PREFLIGHT:-true}" in
    0|false|no|FALSE|NO|False|No)
        ;;
    *)
        echo "Running unbounded-agent preflight..."
        "${AGENT_BIN}" preflight ${_START_ARGS}
        ;;
esac

echo "Running unbounded-agent start..."
"${AGENT_BIN}" start ${_START_ARGS}
