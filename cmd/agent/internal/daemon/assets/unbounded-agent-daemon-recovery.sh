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

if [ -f "${pending_upgrade}" ]; then
    message="AgentUpgrade daemon failed after switching binary; rolled back to ${last_good}"
    if ! "${last_good}" record-agent-upgrade-failure-signal \
        --operation-path "${pending_upgrade}" \
        --failure-path "${failure_signal}" \
        --message "${message}"; then
        echo "failed to record AgentUpgrade recovery signal" >&2
        rm -f "${pending_upgrade}"
    fi
fi

ln -sfn "${last_good}" "${current}"
systemctl reset-failed {{ .DaemonUnit }}
systemctl restart {{ .DaemonUnit }}
