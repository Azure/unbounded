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

# ResetFailed requires SELinux service:reload, which the tested ACL policy
# denies to our caller context. Keep it as a fast-path optimization, not a
# prerequisite. Neither the policy nor the daemon's start limit is changed.
if ! systemctl reset-failed "{{ .DaemonUnit }}"; then
    echo "could not reset the failure counter for {{ .DaemonUnit }}; continuing to roll back" >&2
fi

if ! systemctl restart "{{ .DaemonUnit }}"; then
    # Some systemd builds retain Result=exit-code when rate-limited, so that
    # property cannot reliably identify start-limit exhaustion. Allow ONE
    # delayed retry after an ordinary restart failure. This can also retry a
    # different startup error; persistent errors remain failures.
    interval=$(systemctl show "{{ .DaemonUnit }}" --property=StartLimitIntervalUSec --value)

    # systemctl formats this property as a timespan, e.g. "1min 500ms", not
    # necessarily a numeric microsecond count. Parse its integer components
    # without eval. Reject unknown, infinite, zero, and excessive intervals
    # rather than making recovery wait forever or silently disabling limits.
    total_us=0
    read -r -a components <<< "$interval"
    for component in "${components[@]}"; do
        if [[ ! "$component" =~ ^([0-9]{1,9})(us|ms|s|min|h|d|w|month|y)$ ]]; then
            echo "unsupported recovery start-limit interval: $interval" >&2
            exit 1
        fi
        value=$((10#${BASH_REMATCH[1]}))
        case "${BASH_REMATCH[2]}" in
            us) factor=1 ;;
            ms) factor=1000 ;;
            s) factor=1000000 ;;
            min) factor=60000000 ;;
            # Any nonzero component of an hour or longer exceeds our cap.
            *) factor=300000001 ;;
        esac
        total_us=$((total_us + value * factor))
        if (( total_us > 300000000 )); then
            echo "recovery start-limit interval exceeds the supported 5min maximum: $interval" >&2
            exit 1
        fi
    done
    if (( total_us == 0 )); then
        echo "no finite positive start-limit interval for delayed recovery: $interval" >&2
        exit 1
    fi

    # Wait a full window from the failed attempt, plus a five-second margin.
    # Expiration does not itself enqueue a start; this service must do so.
    delay=$(((total_us + 999999) / 1000000 + 5))
    echo "daemon restart failed; retrying last-known-good activation once after ${delay}s" >&2
    sleep "$delay"

    # Do not overwrite a newer selection made while recovery was sleeping.
    if [ "$(readlink -f "$current")" != "$last_good" ]; then
        echo "daemon selection changed during recovery; refusing stale restart" >&2
        exit 1
    fi
    systemctl start "{{ .DaemonUnit }}"
fi

# Type=simple can report startup success before the process immediately exits.
# Check that the selected executable is still running after a settling period.
# This is a local liveness check; the recovered agent reports the upgrade result.
sleep 5
systemctl is-active --quiet "{{ .DaemonUnit }}"
pid=$(systemctl show "{{ .DaemonUnit }}" --property=MainPID --value)
if [[ ! "$pid" =~ ^[1-9][0-9]*$ ]] || [ "$(readlink -f "/proc/$pid/exe")" != "$last_good" ]; then
    echo "recovered daemon is not running the expected last-known-good executable" >&2
    exit 1
fi
