#!/bin/bash
# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0

set -euo pipefail

current="{{ .DaemonBinaryCurrentPath }}"
last_good="$(readlink -f {{ .DaemonBinaryLastGoodPath }} || true)"
upgrade_signal="{{ .DaemonAgentUpgradeSignalPath }}"

if [ -z "${last_good}" ] || [ ! -x "${last_good}" ]; then
    echo "no valid last-known-good agent binary found" >&2
    exit 1
fi

if [ -f "${upgrade_signal}" ]; then
    message="AgentUpgrade daemon failed after switching binary; rolled back to ${last_good}"
    if ! "${last_good}" record-agent-upgrade-failure-signal --message "${message}"; then
        echo "failed to record AgentUpgrade recovery signal" >&2
    fi
fi

ln -sfn "${last_good}" "${current}"
systemctl reset-failed {{ .DaemonUnit }}
systemctl restart {{ .DaemonUnit }}
