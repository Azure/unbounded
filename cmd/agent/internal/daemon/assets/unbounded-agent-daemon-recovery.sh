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

# Clearing the failure counter is hygiene, not the rollback: it stops systemd
# refusing the restart below for having hit the start limit. It is deliberately
# tolerant of failure, because this script runs with `set -e` and the rollback
# is the part that matters.
#
# Azure Container Linux denies it. Its PID 1 runs in the SELinux kernel_t domain
# rather than init_t, and the policy that grants systemd the service "reload"
# permission does not apply there, so reset-failed is refused for every unit on
# the host, including systemd's own. Aborting here left the daemon down and the
# AgentUpgrade operation stuck, which is the opposite of recovery.
if ! systemctl reset-failed "{{ .DaemonUnit }}"; then
    echo "could not reset the failure counter for {{ .DaemonUnit }}; continuing to roll back" >&2
fi

systemctl restart {{ .DaemonUnit }}
