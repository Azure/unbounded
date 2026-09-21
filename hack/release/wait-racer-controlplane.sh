#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0

# Leader-aware replacement check, called only for deploy/racer-controlplane by
# wait-rollouts.sh after its existence and template-version gates. Never use
# kubectl rollout status here: intentional standbys are not Ready.
set -euo pipefail
: "${KUBECONFIG:?KUBECONFIG must be set}"
: "${EXPECTED_IMAGE_TAG:?EXPECTED_IMAGE_TAG must be set}"
: "${EXPECTED_IMAGE_REGISTRY:?EXPECTED_IMAGE_REGISTRY must be set}"
NS="${NAMESPACE:-unbounded-system}"
registry="${EXPECTED_IMAGE_REGISTRY,,}"
registry="${registry%/}"
# shellcheck source=hack/release/racer-health.sh
source "$(dirname "${BASH_SOURCE[0]}")/racer-health.sh"
KUBECTL=(kubectl --request-timeout=30s -n "$NS")

check_controller() {
  local deployment pods owned healthy
  deployment="$("${KUBECTL[@]}" get deploy/racer-controlplane -o json)" || return 1
  pods="$("${KUBECTL[@]}" get pods -o json)" || return 1
  owned="$(racer_owned_pods "$NS" "$deployment" "$pods")" || return 1
  healthy="$(racer_healthy_pods "$owned")" || return 1
  # No old, terminating, missing, or unhealthy replicas. Check actual Pod
  # images as well as template/status so a settled previous release cannot pass.
  jq -e -L "$RACER_LIB_DIR" --arg registry "$registry" --arg tag "$EXPECTED_IMAGE_TAG" \
    --argjson pods "$owned" --argjson healthy "$healthy" '
      include "racer";
      (.spec.replicas // 0) as $desired
      | $desired > 0 and .metadata.deletionTimestamp == null
        and (.metadata.generation // 0) > 0
        and (.status.observedGeneration // -1) >= .metadata.generation
        and .status.updatedReplicas == $desired and .status.replicas == $desired
        and ($pods | length) == $desired and ($healthy | length) == $desired
        and (.spec.template.spec | release_images($registry; $tag))
        and all($pods[]; .spec | release_images($registry; $tag))
        and any($pods[].status.conditions[]?; .type == "Ready" and .status == "True")
    ' <<<"$deployment" >/dev/null || return 1
  racer_probe_health "$NS" "$healthy"
}

deadline=$((SECONDS + ${RACER_ROLLOUT_TIMEOUT_SECONDS:-300}))
while true; do
  if check_controller; then
    echo "OK: Racer controller replicas replaced with :${EXPECTED_IMAGE_TAG}; leader Service and all processes healthy"
    exit 0
  fi
  if (( SECONDS >= deadline )); then
    echo "::error::Racer controller did not converge to :${EXPECTED_IMAGE_TAG} with a serving leader and healthy processes"
    exit 1
  fi
  echo "Waiting for Racer controller replacement, leader Service, and process health"
  sleep "${POLL_INTERVAL_SECONDS:-5}"
done
