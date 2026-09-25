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
echo "Downloading unbounded-agent ${_version_desc} for ${arch} from ${AGENT_URL}..."
# Staged under /var/lib rather than the default temporary directory because the
# staged binary is executed, not just copied: admission runs from it below.
# Hardened hosts commonly mount /tmp noexec, which would fail the run outright,
# and image-based hosts are the ones most likely to do so.
staging_root="/var/lib/unbounded"
mkdir -p "${staging_root}"
tmp_dir="$(mktemp -d "${staging_root}/install.XXXXXX")"
trap 'rm -rf "${tmp_dir}"' EXIT
curl -fsSL "${AGENT_URL}" | tar -xz -C "${tmp_dir}" unbounded-agent
# Run admission from the staged executable. Bootstrap installs the daemon binary
# only after acquiring installation ownership; retries cannot overwrite a live
# current/compatibility binary link before their intent has been accepted.
AGENT_BIN="${tmp_dir}/unbounded-agent"
chmod 0755 "${AGENT_BIN}"

# Seed the daemon binary path for an agent released before the host root. The
# agent version is selected independently of this script - by AGENT_VERSION, by
# AGENT_URL, or by the default of tracking the latest published release - so it
# may be one that never writes its own binary and looks for it at
# /usr/local/bin. An agent that answers host-root installs itself under the host
# root, and seeding /usr/local/bin for it would make a fresh host look like one
# installed by an older agent.
#
# The test follows symlinks on purpose. On a host this installation already owns
# the path resolves through the compatibility symlink to a live blue-green slot,
# so it is left untouched and admission still runs from the staged executable
# above. A dangling link resolves to nothing and is replaced, because install
# would otherwise write through it to a stale location.
if ! "${AGENT_BIN}" host-root >/dev/null 2>&1; then
    AGENT_BIN_TARGET="/usr/local/bin/unbounded-agent"
    if [ ! -x "${AGENT_BIN_TARGET}" ]; then
        rm -f "${AGENT_BIN_TARGET}"
        if ! install -m 0755 "${AGENT_BIN}" "${AGENT_BIN_TARGET}"; then
            echo "unbounded-agent ${_version_desc} predates /opt/unbounded and needs a writable /usr/local/bin; use a newer release" >&2
            exit 1
        fi
    fi
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
