#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0

# Print targets, including not-yet-created workloads for enabled Sites. Match
# ControlPlane.Plan's retained installation markers and SiteDaemonSetName's
# encoding (internal/operator/components/racer). An absent installation is a
# valid empty result; failed discovery is never an empty result.
set -euo pipefail
: "${KUBECONFIG:?KUBECONFIG must be set}"
NS="${NAMESPACE:-unbounded-system}"
KUBECTL=(kubectl --request-timeout=30s)

sites_json="$("${KUBECTL[@]}" get sites.unbounded-cloud.io -o json)"
sites="$(jq -r '.items[] | select(.spec.components.racer.enabled == true) | .metadata.name' <<<"$sites_json")"
account="$("${KUBECTL[@]}" -n "$NS" get serviceaccount/racer-controlplane --ignore-not-found -o json)"
deployment="$("${KUBECTL[@]}" -n "$NS" get deploy/racer-controlplane --ignore-not-found -o json)"
if [[ -z "$sites" && -z "$account" && -z "$deployment" ]]; then
  echo "Racer is not enabled or installed; no rollout targets" >&2
  exit 0
fi

echo deploy/racer-controlplane
while IFS= read -r site; do
  [[ -n "$site" ]] || continue
  name="racer-${site}"
  if (( ${#name} > 63 )) || [[ ! "$site" =~ ^[a-z0-9]([-a-z0-9]*[a-z0-9])?$ ]]; then
    digest="$(printf '%s' "$site" | sha256sum)"
    name="racer-site.${digest:0:32}"
  fi
  echo "ds/${name}"
done <<<"$sites"
