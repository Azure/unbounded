#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0
#
# Remove the gantry bench release and wait for the agent pods to go away.
set -euo pipefail

# shellcheck source-path=SCRIPTDIR
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

timeout -s TERM -k 10 600 helm uninstall gantry --namespace "${GANTRY_NS}" --wait --timeout 9m || true
k -n "${GANTRY_NS}" wait --for=delete pod -l app.kubernetes.io/component=agent --timeout=280s || true
k delete namespace "${GANTRY_NS}" --ignore-not-found --wait=true --timeout=280s
