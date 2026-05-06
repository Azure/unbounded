#!/bin/bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

set -euo pipefail

current="{{ .DaemonBinaryCurrentPath }}"
last_good="$(readlink -f {{ .DaemonBinaryLastGoodPath }} || true)"

if [ -z "${last_good}" ] || [ ! -x "${last_good}" ]; then
    echo "no valid last-known-good agent binary found" >&2
    exit 1
fi

ln -sfn "${last_good}" "${current}"
systemctl reset-failed {{ .DaemonUnit }}
systemctl restart {{ .DaemonUnit }}
