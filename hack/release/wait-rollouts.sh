#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

# wait-rollouts: wait for operator-managed workloads to become available, and
# fail fast with a named image when a pod cannot pull the image it needs.
#
# Shared by .github/workflows/nightly.yaml and .github/workflows/release-upgrade.yaml.
# Both deploy gates previously carried a copy of this logic and had already
# drifted (the gantry DaemonSet was gated in one and not the other), so the
# single copy lives here next to the smoke tests those workflows also share.
# Both invoke it from a sparse checkout of the workflow's own commit, so a
# rollback or backfill onto an older tag still runs the current gate.
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
#   NAMESPACE                    Namespace to watch (default unbounded-system).
#   ROLLOUT_TIMEOUT              Per-workload rollout timeout (default 5m).
#   CREATE_TIMEOUT_SECONDS       How long to wait for a workload to be created
#                                by the operator before giving up (default 300).
#   POLL_INTERVAL_SECONDS        Poll cadence for the image check (default 5).
#   IMAGE_FAILURE_GRACE_SECONDS  How long a retryable image failure must persist
#                                before it is treated as fatal (default 90).
#
# Design notes
# ------------
# 'kubectl rollout status' remains the authority on readiness; it evaluates
# observedGeneration and updated/available replicas. The image check only adds
# an early exit, so it is deliberately FAIL-OPEN: anything it cannot determine
# is reported loudly and then ignored, never converted into a failed deploy. It
# is an accelerator for diagnosis, not a second opinion on health.
#
# It is also strictly SCOPED to the workload currently being waited on. An
# earlier revision scanned the whole namespace, which meant an unrelated tenant
# (Orca, deployed by a later job) or a leftover pod from a previous broken run
# could abort the very deploy that would have repaired the cluster.

set -euo pipefail

# Associative arrays and ${BASH_VERSINFO[@]} need bash 4+. Checked explicitly so
# a bash 3.2 host (notably macOS) fails with a clear message instead of silently
# mis-evaluating the grace-period bookkeeping.
if (( BASH_VERSINFO[0] < 4 )); then
  echo "::error::wait-rollouts.sh requires bash 4 or newer (found ${BASH_VERSION:-unknown})"
  exit 2
fi

: "${KUBECONFIG:?KUBECONFIG must be set}"

NS="${NAMESPACE:-unbounded-system}"
ROLLOUT_TIMEOUT="${ROLLOUT_TIMEOUT:-5m}"
CREATE_TIMEOUT_SECONDS="${CREATE_TIMEOUT_SECONDS:-300}"
POLL_INTERVAL_SECONDS="${POLL_INTERVAL_SECONDS:-5}"
IMAGE_FAILURE_GRACE_SECONDS="${IMAGE_FAILURE_GRACE_SECONDS:-90}"

# jq is preinstalled on GitHub-hosted runners and is already used by the
# smoke-discover jobs, but assert it rather than letting a missing binary
# quietly reduce the guard to a no-op.
for tool in kubectl jq; do
  if ! command -v "$tool" >/dev/null 2>&1; then
    echo "::error::wait-rollouts.sh requires ${tool} on PATH"
    exit 2
  fi
done

if (( $# == 0 )); then
  echo "::error::wait-rollouts.sh requires at least one <kind>/<name> argument"
  exit 2
fi

KUBECTL=(kubectl --request-timeout=30s -n "$NS")

# InvalidImageName is terminal: the reference does not parse, so no retry can
# ever succeed and there is nothing to wait for.
TERMINAL_WAITING_REASONS='^InvalidImageName$'

# ImagePullBackOff is RETRYABLE. The kubelet has backed off, but registry
# throttling, a brief outage, or delayed credentials all land here and then
# recover, and the gantry DaemonSet additionally pulls a pinned third-party
# init image. It therefore only becomes fatal once it has persisted for
# IMAGE_FAILURE_GRACE_SECONDS. A bare ErrImagePull is the first attempt and is
# never fatal.
BACKOFF_WAITING_REASONS='^ImagePullBackOff$'

WORKDIR="$(mktemp -d)"
ROLLOUT_PID=""
OPEN_GROUP=""
LAST_WARNING=""
GUARD_DISABLED=0
declare -A IMAGE_FIRST_SEEN=()

# cleanup reaps the background rollout watcher and closes any open log group.
# Without it a cancelled run (nightly sets cancel-in-progress) leaves
# 'kubectl rollout status' running until its own timeout, and a failure exits
# with the group still open, which hides the failure in a collapsed section.
cleanup() {
  if [[ -n "$ROLLOUT_PID" ]] && kill -0 "$ROLLOUT_PID" 2>/dev/null; then
    kill "$ROLLOUT_PID" 2>/dev/null || true
    wait "$ROLLOUT_PID" 2>/dev/null || true
  fi

  close_group
  rm -rf "$WORKDIR" 2>/dev/null || true
}

trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

open_group() {
  OPEN_GROUP="$1"

  echo "::group::$1"
}

close_group() {
  [[ -n "$OPEN_GROUP" ]] || return 0

  OPEN_GROUP=""

  echo "::endgroup::"
}

# warn_once suppresses consecutive duplicates. A sustained apiserver problem
# polls every few seconds for the whole rollout timeout, and 60 identical
# annotations would bury the real failure.
warn_once() {
  local message="$1"

  [[ "$message" == "$LAST_WARNING" ]] && return 0

  LAST_WARNING="$message"

  echo "::warning::${message}"
}

# flatten collapses captured stderr into a single line for use in an annotation.
flatten() {
  tr -d '\r' <"$1" | tr '\n' ' ' | sed 's/[[:space:]]\{1,\}/ /g; s/^ //; s/ $//'
}

# disable_guard turns the image check off for the rest of the run. Used when the
# check cannot be evaluated at all, which is a bug in this script or an
# unexpected kubectl payload. It is reported as an error annotation so it cannot
# pass unnoticed, but it does not fail the deploy: rollout status still returns
# the authoritative verdict, and a broken diagnostic must not become an outage.
disable_guard() {
  GUARD_DISABLED=1

  echo "::error::wait-rollouts.sh image guard disabled: $1"
  echo "::error::rollout status still gates this deploy, but a missing image will no longer be named; fix wait-rollouts.sh"
}

# kubectl_json runs a read-only query and prints stdout on success. stderr is
# captured to a file instead of being folded into stdout with 2>&1, because
# kubectl writes deprecation and warning lines there and merging them would
# corrupt the JSON that jq has to parse. Returns 1 on failure, leaving the
# message in ${WORKDIR}/kubectl.err.
kubectl_json() {
  local out

  if out="$("${KUBECTL[@]}" "$@" 2>"${WORKDIR}/kubectl.err")"; then
    printf '%s' "$out"

    return 0
  fi

  return 1
}

# resolve_target prints the workload's pod selector on the first line and its
# desired container images as a JSON array on the second.
#
# Exit codes: 0 resolved, 2 query failed, 3 evaluation failed.
resolve_target() {
  local target="$1" json

  json="$(kubectl_json get "$target" -o json)" || return 2

  printf '%s' "$json" | jq -r '
    ((.spec.selector.matchLabels // {}) | to_entries | map("\(.key)=\(.value)") | join(",")),
    ([ (.spec.template.spec.containers // [])[], (.spec.template.spec.initContainers // [])[] ]
      | map(.image) | unique | tojson)
  ' 2>"${WORKDIR}/jq.err" || return 3
}

# IMAGE_FAILURE_FILTER emits one TSV record per container wedged on an image
# error that this rollout is actually responsible for.
#
# Three filters keep it scoped:
#   - the caller restricts the pod list to the workload's own selector;
#   - pods with a deletionTimestamp are skipped, since a terminating pod is
#     already on its way out and cannot be repaired;
#   - the failing container's image is matched against the images the workload
#     currently WANTS, which drops pods left over from a previous revision that
#     referenced a different (already superseded) image.
#
# The image comes from the POD SPEC rather than from containerStatuses, because
# the runtime normalizes status images (adding a registry or a docker.io/library
# prefix) and that would defeat the comparison against the workload template.
#
# Init containers are included: a failed init image never lets the main
# container start.
IMAGE_FAILURE_FILTER='
  .items[]
  | select(.metadata.deletionTimestamp == null)
  | . as $pod
  | ([ (.spec.containers // [])[], (.spec.initContainers // [])[] ]
      | map({ (.name): .image }) | add // {}) as $spec_images
  | [ (.status.containerStatuses // [])[], (.status.initContainerStatuses // [])[] ][]
  | . as $status
  | ($status.state.waiting.reason // "") as $reason
  | select(($reason | test($terminal)) or ($reason | test($backoff)))
  | ($spec_images[$status.name] // "") as $image
  | select($image != "" and ($desired | index($image)) != null)
  | [ $pod.metadata.name,
      $status.name,
      $image,
      $reason,
      (if ($reason | test($terminal)) then "terminal" else "backoff" end),
      (($status.state.waiting.message // "") | gsub("[\t\n]"; " "))
    ]
  | @tsv
'

# image_failures prints one TSV record per offending container.
#
# Exit codes: 0 records found, 1 none, 2 query failed, 3 evaluation failed.
image_failures() {
  local selector="$1" desired="$2" pods_json records

  pods_json="$(kubectl_json get pods --selector "$selector" -o json)" || return 2

  records="$(printf '%s' "$pods_json" | jq -r \
    --argjson desired "$desired" \
    --arg terminal "$TERMINAL_WAITING_REASONS" \
    --arg backoff "$BACKOFF_WAITING_REASONS" \
    "$IMAGE_FAILURE_FILTER" 2>"${WORKDIR}/jq.err")" || return 3

  [[ -n "$records" ]] || return 1

  printf '%s\n' "$records"
}

# check_images decides whether to abort the wait for one workload.
#
# Returns 0 to keep waiting, 1 to abort.
check_images() {
  local target="$1" selector="$2" desired="$3"
  local records rc=0 abort=0 key age
  local pod container image reason class message
  local -A seen_now=()

  records="$(image_failures "$selector" "$desired")" || rc=$?

  case "$rc" in
    1)
      # Everything healthy right now, so any earlier backoff recovered and its
      # grace clock must not carry over.
      IMAGE_FIRST_SEEN=()

      return 0
      ;;
    2)
      warn_once "could not list pods for ${target} in ${NS}: $(flatten "${WORKDIR}/kubectl.err")"

      return 0
      ;;
    3)
      disable_guard "evaluating pod status for ${target} failed: $(flatten "${WORKDIR}/jq.err")"

      return 0
      ;;
  esac

  while IFS=$'\t' read -r pod container image reason class message; do
    [[ -n "$pod" ]] || continue

    key="${pod}/${container}/${image}"
    seen_now["$key"]=1

    if [[ "$class" == "terminal" ]]; then
      echo "::error::pod ${NS}/${pod} container ${container} cannot pull ${image} (${reason})"
      [[ -n "$message" ]] && echo "  ${message}"

      abort=1

      continue
    fi

    if [[ -z "${IMAGE_FIRST_SEEN[$key]:-}" ]]; then
      IMAGE_FIRST_SEEN["$key"]="$SECONDS"

      echo "pod ${NS}/${pod} container ${container} cannot pull ${image} (${reason}); allowing ${IMAGE_FAILURE_GRACE_SECONDS}s for it to recover"
      [[ -n "$message" ]] && echo "  ${message}"
    fi

    age=$(( SECONDS - IMAGE_FIRST_SEEN[$key] ))

    if (( age >= IMAGE_FAILURE_GRACE_SECONDS )); then
      echo "::error::pod ${NS}/${pod} container ${container} cannot pull ${image} (${reason} for ${age}s)"
      [[ -n "$message" ]] && echo "  ${message}"

      abort=1
    fi
  done <<<"$records"

  # Drop clocks for containers that recovered, so an intermittent failure never
  # accumulates across unrelated occurrences.
  for key in "${!IMAGE_FIRST_SEEN[@]}"; do
    [[ -n "${seen_now[$key]:-}" ]] || unset 'IMAGE_FIRST_SEEN[$key]'
  done

  (( abort == 1 )) || return 0

  echo "::error::image pull failure for ${target} in ${NS}; the pipeline is missing an image the operator references"

  return 1
}

# wait_exists blocks until the operator materializes the workload. Component
# workloads are created asynchronously after the operator reconciles the Site,
# so they do not exist the moment the deploy step finishes.
wait_exists() {
  local target="$1" deadline=$(( SECONDS + CREATE_TIMEOUT_SECONDS )) last_error=""

  while true; do
    if "${KUBECTL[@]}" get "$target" >/dev/null 2>"${WORKDIR}/get.err"; then
      return 0
    fi

    last_error="$(flatten "${WORKDIR}/get.err")"

    # NotFound is the expected state while the operator is still reconciling.
    # Anything else (RBAC denial, expired credentials, an unreachable
    # apiserver, an unknown resource type) is surfaced as it happens and
    # carried into the timeout message, so it can no longer masquerade as a
    # workload the operator simply never created.
    if [[ "$last_error" != *NotFound* && "$last_error" != *"not found"* ]]; then
      warn_once "querying ${target} in ${NS} failed: ${last_error}"
    else
      last_error=""
    fi

    if (( SECONDS >= deadline )); then
      if [[ -n "$last_error" ]]; then
        echo "::error::timed out waiting for ${target} in ${NS} to be created; last error: ${last_error}"
      else
        echo "::error::timed out waiting for ${target} in ${NS} to be created"
      fi

      return 1
    fi

    sleep "$POLL_INTERVAL_SECONDS"
  done
}

# wait_rollout runs 'kubectl rollout status' in the background and polls for
# image failures alongside it. Delegating to kubectl keeps the real rollout
# semantics (observedGeneration, updated/available replicas) rather than
# reimplementing them; the poll only adds an early exit.
wait_rollout() {
  local target="$1" selector="$2" desired="$3" rc=0

  IMAGE_FIRST_SEEN=()

  "${KUBECTL[@]}" rollout status "$target" --timeout="$ROLLOUT_TIMEOUT" &
  ROLLOUT_PID=$!

  while kill -0 "$ROLLOUT_PID" 2>/dev/null; do
    if [[ -n "$selector" ]] && (( GUARD_DISABLED == 0 )); then
      if ! check_images "$target" "$selector" "$desired"; then
        kill "$ROLLOUT_PID" 2>/dev/null || true
        wait "$ROLLOUT_PID" 2>/dev/null || true
        ROLLOUT_PID=""

        echo "::error::aborted waiting for ${target}: image pull failure in ${NS}"

        return 1
      fi
    fi

    sleep "$POLL_INTERVAL_SECONDS"
  done

  wait "$ROLLOUT_PID" || rc=$?
  ROLLOUT_PID=""

  return "$rc"
}

# wait_target waits for one workload to exist and then to roll out.
wait_target() {
  local target="$1" selector="" desired="[]" meta rc=0
  local -a lines=()

  LAST_WARNING=""

  wait_exists "$target" || return 1

  if (( GUARD_DISABLED == 0 )); then
    meta="$(resolve_target "$target")" || rc=$?

    case "$rc" in
      0)
        mapfile -t lines <<<"$meta"
        selector="${lines[0]:-}"
        desired="${lines[1]:-[]}"

        if [[ -z "$selector" ]]; then
          # Only matchExpressions, which this guard does not translate. Fail
          # open: rollout status still gates the deploy.
          echo "::notice::${target} has no matchLabels selector; skipping the image check for it"
        fi
        ;;
      2)
        warn_once "could not read ${target} in ${NS}: $(flatten "${WORKDIR}/kubectl.err")"
        ;;
      3)
        disable_guard "reading the spec of ${target} failed: $(flatten "${WORKDIR}/jq.err")"
        ;;
    esac
  fi

  wait_rollout "$target" "$selector" "$desired"
}

echo "Waiting for rollouts in ${NS}: $*"

for target in "$@"; do
  open_group "$target"

  status=0
  wait_target "$target" || status=$?

  # Closed explicitly rather than after the loop body so a failure cannot exit
  # with the group still open, which would render the failing target collapsed.
  close_group

  if (( status != 0 )); then
    exit "$status"
  fi
done

echo "OK: all workloads rolled out in ${NS}"
