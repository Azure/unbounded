#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

# wait-rollouts: wait for operator-managed workloads to become available, and
# fail fast with a named image when a pod cannot pull its image.
#
# Shared by .github/workflows/nightly.yaml and .github/workflows/release-upgrade.yaml.
# Both deploy gates previously carried a copy of this logic and had already
# drifted (the gantry DaemonSet was gated in one and not the other), so the
# single copy lives here next to the smoke tests those workflows also share.
#
# Why the image check exists: the operator resolves every component image as
# <registry>/<repository>:<operator version>. When a pipeline forgets to build
# one of them the pods sit in ImagePullBackOff, 'kubectl rollout status' simply
# blocks for its full timeout, and the run reports a bare "pod not ready". That
# failure mode kept the nightly red for 15 consecutive nights. Detecting the
# pull error names the missing image in seconds instead.
#
# Usage:
#   hack/release/wait-rollouts.sh deploy/unbounded-operator ds/gantry ...
#
# Arguments are kubectl <kind>/<name> references, waited on in order.
#
# Contract (provided by the calling workflow):
#   KUBECONFIG   Path to a kubeconfig for the target cluster.
#
# Optional overrides:
#   NAMESPACE                Namespace to watch (default unbounded-system).
#   ROLLOUT_TIMEOUT          Per-workload rollout timeout (default 5m).
#   CREATE_TIMEOUT_SECONDS   How long to wait for a workload to be created by
#                            the operator before giving up (default 300).
#   POLL_INTERVAL_SECONDS    Poll cadence for the image check (default 5).

set -euo pipefail

: "${KUBECONFIG:?KUBECONFIG must be set}"

NS="${NAMESPACE:-unbounded-system}"
ROLLOUT_TIMEOUT="${ROLLOUT_TIMEOUT:-5m}"
CREATE_TIMEOUT_SECONDS="${CREATE_TIMEOUT_SECONDS:-300}"
POLL_INTERVAL_SECONDS="${POLL_INTERVAL_SECONDS:-5}"

KUBECTL=(kubectl --request-timeout=30s -n "$NS")

if (( $# == 0 )); then
  echo "::error::wait-rollouts.sh requires at least one <kind>/<name> argument"
  exit 2
fi

# Waiting reasons treated as terminal. ImagePullBackOff means the kubelet has
# already retried and backed off; InvalidImageName means the reference does not
# parse at all. Both are unambiguous, so neither produces false positives on a
# merely slow pull. A bare ErrImagePull is the first transient attempt and is
# deliberately NOT fatal, though it is still reported in the diagnostics below.
FATAL_WAITING_REASONS='^(ImagePullBackOff|InvalidImageName)$'

# image_pull_failures prints one tab-separated record per container stuck on a
# fatal image error, and returns 1 when there are none. Init containers are
# included: a failed init image never lets the main container start.
image_pull_failures() {
  local out
  out="$("${KUBECTL[@]}" get pods -o json 2>/dev/null | jq -r --arg re "$FATAL_WAITING_REASONS" '
    .items[]
    | .metadata.name as $pod
    | ((.status.containerStatuses // []) + (.status.initContainerStatuses // []))[]
    | select((.state.waiting.reason // "") | test($re))
    | [$pod, .name, .image, .state.waiting.reason, ((.state.waiting.message // "") | gsub("\t"; " "))]
    | @tsv
  ' 2>/dev/null || true)"

  [[ -z "$out" ]] && return 1

  printf '%s\n' "$out"
}

# report_image_failures turns the records into GitHub error annotations so the
# missing image is visible in the run summary without opening the log.
report_image_failures() {
  local records="$1" pod container image reason message

  while IFS=$'\t' read -r pod container image reason message; do
    [[ -z "$pod" ]] && continue
    echo "::error::pod ${NS}/${pod} container ${container} cannot pull ${image} (${reason})"
    [[ -n "$message" ]] && echo "  ${message}"
  done <<< "$records"

  echo "::error::image pull failure in ${NS}; the pipeline is missing an image the operator references"
}

# fail_on_image_pull aborts the whole script when any container is wedged on a
# fatal image error.
fail_on_image_pull() {
  local records
  if records="$(image_pull_failures)"; then
    report_image_failures "$records"
    return 1
  fi

  return 0
}

# wait_exists blocks until the operator materializes the workload. Component
# workloads are created asynchronously after the operator reconciles the Site,
# so they do not exist the moment the deploy step finishes.
wait_exists() {
  local target="$1" deadline=$(( SECONDS + CREATE_TIMEOUT_SECONDS ))

  until "${KUBECTL[@]}" get "$target" >/dev/null 2>&1; do
    if (( SECONDS >= deadline )); then
      echo "::error::timed out waiting for ${target} in ${NS} to be created"
      return 1
    fi

    fail_on_image_pull || return 1
    sleep "$POLL_INTERVAL_SECONDS"
  done
}

# wait_rollout runs 'kubectl rollout status' in the background and polls for
# image failures alongside it. Delegating to kubectl keeps the real rollout
# semantics (observedGeneration, updated/available replicas) rather than
# reimplementing them; the poll only adds an early exit.
wait_rollout() {
  local target="$1" pid rc=0

  "${KUBECTL[@]}" rollout status "$target" --timeout="$ROLLOUT_TIMEOUT" &
  pid=$!

  while kill -0 "$pid" 2>/dev/null; do
    if ! fail_on_image_pull; then
      kill "$pid" 2>/dev/null || true
      wait "$pid" 2>/dev/null || true
      echo "::error::aborted waiting for ${target}: image pull failure in ${NS}"

      return 1
    fi

    sleep "$POLL_INTERVAL_SECONDS"
  done

  wait "$pid" || rc=$?

  return "$rc"
}

echo "Waiting for rollouts in ${NS}: $*"

for target in "$@"; do
  echo "::group::${target}"
  wait_exists "$target"
  wait_rollout "$target"
  echo "::endgroup::"
done

echo "OK: all workloads rolled out in ${NS}"
