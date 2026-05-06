#!/bin/bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

set -euo pipefail

current="{{ .DaemonBinaryCurrentPath }}"
last_good="$(readlink -f {{ .DaemonBinaryLastGoodPath }} || true)"
pending_upgrade="{{ .DaemonAgentUpgradeOperationPath }}"
failure_signal="{{ .DaemonAgentUpgradeFailurePath }}"

json_escape() {
    printf '%s' "$1" | sed 's/\\/\\\\/g; s/"/\\"/g'
}

if [ -z "${last_good}" ] || [ ! -x "${last_good}" ]; then
    echo "no valid last-known-good agent binary found" >&2
    exit 1
fi

operation="$(
    sed -n 's/.*"operationName"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "${pending_upgrade}" 2>/dev/null | head -n 1 || true
)"
if [ -z "${operation}" ]; then
    operation="$(head -n 1 "${pending_upgrade}" 2>/dev/null || true)"
fi
if [ -n "${operation}" ]; then
    mkdir -p "$(dirname "${failure_signal}")"
    message="AgentUpgrade daemon failed after switching binary; rolled back to ${last_good}"
    printf '{"operationName":"%s","message":"%s"}\n' "$(json_escape "${operation}")" "$(json_escape "${message}")" > "${failure_signal}"
    rm -f "${pending_upgrade}"
fi

ln -sfn "${last_good}" "${current}"
systemctl reset-failed {{ .DaemonUnit }}
systemctl restart {{ .DaemonUnit }}
