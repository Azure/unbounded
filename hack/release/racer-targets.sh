#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0

# Live ClusterCaches require both singleton targets. Without caches, retain only
# existing nonterminating workloads independently. An absent installation is a
# valid empty result; failed discovery is never an empty result.
set -euo pipefail
: "${KUBECONFIG:?KUBECONFIG must be set}"
NS="${NAMESPACE:-unbounded-system}"
KUBECTL=(kubectl --request-timeout=30s)

caches_json="$("${KUBECTL[@]}" get clustercaches.racer.unbounded-cloud.io -o json)"
caches="$(jq -r '.items[] | select(.metadata.deletionTimestamp == null) | "live"' <<<"$caches_json")"
deployment="$("${KUBECTL[@]}" -n "$NS" get deploy/racer-controlplane --ignore-not-found -o json)"
dataplane="$("${KUBECTL[@]}" -n "$NS" get ds/racer-dataplane --ignore-not-found -o json)"
deployment="$(jq -r 'select(.metadata.deletionTimestamp == null) | "live"' <<<"$deployment")"
dataplane="$(jq -r 'select(.metadata.deletionTimestamp == null) | "live"' <<<"$dataplane")"

targets=()
if [[ -n "$caches" || -n "$deployment" ]]; then
  targets+=(deploy/racer-controlplane)
fi
if [[ -n "$caches" || -n "$dataplane" ]]; then
  targets+=(ds/racer-dataplane)
fi
if (( ${#targets[@]} == 0 )); then
  echo "Racer has no live ClusterCaches or retained workloads; no rollout targets" >&2
  exit 0
fi

printf '%s\n' "${targets[@]}"
