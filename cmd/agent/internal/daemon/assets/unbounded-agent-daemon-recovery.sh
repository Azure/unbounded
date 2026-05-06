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

operation="$(
    python3 - "${pending_upgrade}" 2>/dev/null <<'PY' || true
import json
import sys

try:
    with open(sys.argv[1], encoding="utf-8") as f:
        data = f.read()
except FileNotFoundError:
    sys.exit(0)

try:
    print(json.loads(data).get("operationName", ""))
except json.JSONDecodeError:
    print(data.splitlines()[0] if data.splitlines() else "")
PY
)"
if [ -z "${operation}" ] && ! command -v python3 >/dev/null 2>&1; then
    operation="$(head -n 1 "${pending_upgrade}" 2>/dev/null || true)"
fi
if [ -n "${operation}" ]; then
    mkdir -p "$(dirname "${failure_signal}")"
    message="AgentUpgrade daemon failed after switching binary; rolled back to ${last_good}"
    if command -v python3 >/dev/null 2>&1; then
        python3 - "${failure_signal}" "${operation}" "${message}" <<'PY'
import json
import sys

with open(sys.argv[1], "w", encoding="utf-8") as f:
    json.dump({"operationName": sys.argv[2], "message": sys.argv[3]}, f)
    f.write("\n")
PY
    else
        safe_operation="$(printf '%s' "${operation}" | tr -cd '[:alnum:]._-')"
        printf '{"operationName":"%s","message":"AgentUpgrade daemon failed after switching binary"}\n' "${safe_operation}" > "${failure_signal}"
    fi
    rm -f "${pending_upgrade}"
fi

ln -sfn "${last_good}" "${current}"
systemctl reset-failed {{ .DaemonUnit }}
systemctl restart {{ .DaemonUnit }}
