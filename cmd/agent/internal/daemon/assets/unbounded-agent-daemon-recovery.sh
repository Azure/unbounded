#!/bin/bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

set -euo pipefail

current="{{ .DaemonBinaryCurrentPath }}"
last_good="$(readlink -f {{ .DaemonBinaryLastGoodPath }} || true)"
pending_upgrade="{{ .DaemonAgentUpgradeOperationPath }}"
failure_signal="{{ .DaemonAgentUpgradeFailurePath }}"

if [ -z "${last_good}" ] || [ ! -x "${last_good}" ]; then
    echo "no valid last-known-good agent binary found" >&2
    exit 1
fi

operation="$(head -n 1 "${pending_upgrade}" 2>/dev/null || true)"
if [ -n "${operation}" ]; then
    mkdir -p "$(dirname "${failure_signal}")"
    {
        printf '%s\n' "${operation}"
        printf 'AgentUpgrade daemon failed after switching binary; rolled back to %s\n' "${last_good}"
    } > "${failure_signal}"
    rm -f "${pending_upgrade}"
fi

ln -sfn "${last_good}" "${current}"
systemctl reset-failed {{ .DaemonUnit }}
systemctl restart {{ .DaemonUnit }}
