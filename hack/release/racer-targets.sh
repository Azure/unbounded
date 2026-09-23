#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0

# Print singleton targets using the operator's default-on Site votes and
# retained installation markers. An absent installation is a
# valid empty result; failed discovery is never an empty result.
set -euo pipefail
: "${KUBECONFIG:?KUBECONFIG must be set}"
NS="${NAMESPACE:-unbounded-system}"
KUBECTL=(kubectl --request-timeout=30s)

sites_json="$("${KUBECTL[@]}" get sites.unbounded-cloud.io -o json)"
sites="$(jq -r '.items[] | select(.spec.components.racer.enabled != false) | "enabled"' <<<"$sites_json")"
account="$("${KUBECTL[@]}" -n "$NS" get serviceaccount/racer-controlplane --ignore-not-found -o json)"
deployment="$("${KUBECTL[@]}" -n "$NS" get deploy/racer-controlplane --ignore-not-found -o json)"
dataplane="$("${KUBECTL[@]}" -n "$NS" get ds/racer-dataplane --ignore-not-found -o json)"
if [[ -z "$sites" && -z "$account" && -z "$deployment" && -z "$dataplane" ]]; then
  echo "Racer is not enabled or installed; no rollout targets" >&2
  exit 0
fi

echo deploy/racer-controlplane
echo ds/racer-dataplane
