#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

# wait-rollouts: wait for operator-managed workloads to become available, fail
# fast with a named image when a pod cannot pull the image it needs, and
# tolerate a DaemonSet shortfall that is caused only by unreachable nodes.
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
# Why the node check exists: a DaemonSet counts every node toward
# desiredNumberScheduled, including nodes the kubelet has stopped reporting for.
# A single unreachable node therefore blocks 'rollout status' until its timeout,
# every time, forever. For a project whose premise is nodes in flaky remote
# sites that turns the release gate into a coin toss on hardware liveness.
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
#   MAX_NOTREADY_NODES           How many NotReady nodes may be tolerated before
#                                a DaemonSet shortfall stops being excusable
#                                (default 2). 0 disables tolerance entirely.
#
# Design notes
# ------------
# 'kubectl rollout status' remains the authority on readiness; it evaluates
# observedGeneration and updated/available replicas. The image check only adds
# an early exit, so it is deliberately FAIL-OPEN: anything it cannot determine
# is reported loudly and then ignored, never converted into a failed deploy. It
# is an accelerator for diagnosis, not a second opinion on health.
#
# The node check runs the opposite discipline and is deliberately FAIL-CLOSED.
# It is the only thing here that can turn a failing wait into a passing one, so
# anything it cannot positively verify means "keep waiting" and let rollout
# status deliver the verdict. A broken diagnostic must never manufacture a green
# release. Concretely it tolerates a shortfall only when ALL of these hold:
#
#   - the workload is a DaemonSet (a Deployment reschedules off a dead node, so
#     a shortfall there is a real scheduling problem, not a stranded pod);
#   - between 1 and MAX_NOTREADY_NODES nodes are NotReady;
#   - the DaemonSet controller has observed the current generation;
#   - updated + stranded and ready + stranded both cover desiredNumberScheduled;
#   - no pod on a READY node is unhealthy.
#
# Exceeding MAX_NOTREADY_NODES is reported immediately, grouped by site, so the
# log explains the timeout while it is still counting down. It deliberately does
# NOT abort early: a node may recover inside the window, and inventing a new
# abort path would break the fail-closed rule above.
#
# The image check is also strictly SCOPED to the workload currently being waited
# on. An earlier revision scanned the whole namespace, which meant an unrelated
# tenant (Orca, deployed by a later job) or a leftover pod from a previous
# broken run could abort the very deploy that would have repaired the cluster.

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
MAX_NOTREADY_NODES="${MAX_NOTREADY_NODES:-2}"

if [[ ! "$MAX_NOTREADY_NODES" =~ ^[0-9]+$ ]]; then
  echo "::error::MAX_NOTREADY_NODES must be a non-negative integer (got '${MAX_NOTREADY_NODES}')"
  exit 2
fi

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

# NOTREADY_NODE_FILTER emits "<name>\t<site>" per node whose Ready condition is
# not True. A missing Ready condition counts as NotReady. The site label is only
# used for the human-facing message; both the canonical and the deprecated key
# are consulted because the net controllers still dual-write them.
NOTREADY_NODE_FILTER='
  .items[]
  | select((((.status.conditions // []) | map(select(.type == "Ready")) | .[0].status) // "Unknown") != "True")
  | [ .metadata.name,
      (.metadata.labels["unbounded-cloud.io/site"]
        // .metadata.labels["net.unbounded-cloud.io/site"]
        // "no-site")
    ]
  | @tsv
'

# DAEMONSET_SHORTFALL_FILTER classifies a DaemonSet's pods against the NotReady
# node names, emitting "<stranded>\t<unhealthy_on_ready>".
#
#   stranded            pods on a NotReady node. The controller can neither
#                       update nor reap these; they are why the rollout stalls.
#   unhealthy_on_ready  pods NOT on a NotReady node that are not Running+Ready.
#                       A pod with no nodeName counts here: a DaemonSet pod is
#                       assigned at creation, so an unassigned one is a real
#                       scheduling failure rather than a stranded pod.
DAEMONSET_SHORTFALL_FILTER='
  [ .items[]
    | (.spec.nodeName // "") as $node
    | { stranded: ($notready | any(. == $node)),
        ready: ((.status.phase == "Running")
                 and ((((.status.conditions // [])
                         | map(select(.type == "Ready")) | .[0].status) // "False") == "True"))
      }
  ]
  | [ (map(select(.stranded)) | length),
      (map(select((.stranded | not) and (.ready | not))) | length)
    ]
  | @tsv
'

# notready_nodes prints one TSV record per NotReady node. Empty output means
# every node is Ready.
#
# Exit codes: 0 succeeded, 1 query or evaluation failed.
notready_nodes() {
  local nodes_json

  nodes_json="$(kubectl_json get nodes -o json)" || return 1

  printf '%s' "$nodes_json" | jq -r "$NOTREADY_NODE_FILTER" 2>"${WORKDIR}/jq.err" || return 1
}

# describe_nodes collapses the TSV into one annotation-safe line grouped by
# site, for example "boulderlab[spark-3d37] edge[node-9, node-10]".
describe_nodes() {
  awk -F'\t' '
    { order[$2] = ($2 in order) ? order[$2] : ++n; group[$2] = group[$2] (group[$2] ? ", " : "") $1 }
    END { for (s in group) printf "%s[%s] ", s, group[s] }
  ' <<<"$1" | sed 's/ $//'
}

# node_tolerance decides whether a stalled DaemonSet rollout is explained
# entirely by nodes the cluster has lost contact with.
#
# Returns 0 to tolerate (the caller stops waiting and succeeds), 1 to keep
# waiting. EVERY uncertain path returns 1: this is the one check that can turn a
# failing wait into a passing one, so it must never guess. See the FAIL-CLOSED
# note at the top of this file.
node_tolerance() {
  local target="$1" selector="$2"
  local obj_json pods_json nodes_tsv notready_json counts status_tsv
  local kind desired ready updated generation observed stranded unhealthy
  local notready_count

  # No selector means pods cannot be scoped to this workload.
  [[ -n "$selector" ]] || return 1

  obj_json="$(kubectl_json get "$target" -o json)" || {
    warn_once "could not read ${target} while evaluating node tolerance: $(flatten "${WORKDIR}/kubectl.err")"

    return 1
  }

  # A Deployment reschedules off a dead node, so a shortfall there is a real
  # scheduling problem and must not be excused.
  kind="$(printf '%s' "$obj_json" | jq -r '.kind // ""' 2>"${WORKDIR}/jq.err")" || return 1
  [[ "$kind" == "DaemonSet" ]] || return 1

  nodes_tsv="$(notready_nodes)" || {
    warn_once "could not evaluate node readiness for ${target}: $(flatten "${WORKDIR}/kubectl.err")"

    return 1
  }

  # Every node is Ready, so whatever is stalling this rollout is a real problem.
  [[ -n "$nodes_tsv" ]] || return 1

  notready_count="$(wc -l <<<"$nodes_tsv" | tr -d ' ')"

  if (( notready_count > MAX_NOTREADY_NODES )); then
    warn_once "too many NotReady nodes for the ${target} shortfall to be excused (${notready_count} > MAX_NOTREADY_NODES=${MAX_NOTREADY_NODES}): $(describe_nodes "$nodes_tsv")"

    return 1
  fi

  notready_json="$(cut -f1 <<<"$nodes_tsv" | jq -R -s -c 'split("\n") | map(select(length > 0))' 2>"${WORKDIR}/jq.err")" || return 1

  pods_json="$(kubectl_json get pods --selector "$selector" -o json)" || {
    warn_once "could not list pods for ${target} while evaluating node tolerance: $(flatten "${WORKDIR}/kubectl.err")"

    return 1
  }

  counts="$(printf '%s' "$pods_json" | jq -r --argjson notready "$notready_json" \
    "$DAEMONSET_SHORTFALL_FILTER" 2>"${WORKDIR}/jq.err")" || return 1

  IFS=$'\t' read -r stranded unhealthy <<<"$counts" || return 1

  status_tsv="$(printf '%s' "$obj_json" | jq -r '
    [ (.status.desiredNumberScheduled // 0),
      (.status.numberReady // 0),
      (.status.updatedNumberScheduled // 0),
      (.metadata.generation // 0),
      (.status.observedGeneration // -1)
    ] | @tsv' 2>"${WORKDIR}/jq.err")" || return 1

  IFS=$'\t' read -r desired ready updated generation observed <<<"$status_tsv" || return 1

  # Guard against every way this could be evaluated against stale or partial
  # data. Any of these failing simply means "not yet", never "close enough".
  (( desired > 0 )) || return 1
  (( observed >= generation )) || return 1
  (( stranded > 0 )) || return 1
  (( unhealthy == 0 )) || return 1
  (( updated + stranded >= desired )) || return 1
  (( ready + stranded >= desired )) || return 1

  echo "::warning::${target} is short ${stranded} of ${desired} pods, entirely on NotReady nodes: $(describe_nodes "$nodes_tsv")"
  echo "::warning::tolerating the ${target} shortfall (MAX_NOTREADY_NODES=${MAX_NOTREADY_NODES}); this release was NOT validated on those nodes"

  if [[ -n "${GITHUB_STEP_SUMMARY:-}" ]]; then
    {
      echo "### Degraded rollout tolerated: ${target}"
      echo
      echo "- Ready pods: ${ready}/${desired} (${stranded} stranded on NotReady nodes)"
      echo "- NotReady nodes: $(describe_nodes "$nodes_tsv")"
      echo "- Tolerated because \`MAX_NOTREADY_NODES=${MAX_NOTREADY_NODES}\`"
    } >> "$GITHUB_STEP_SUMMARY"
  fi

  return 0
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

# wait_rollout runs 'kubectl rollout status' in the background and polls
# alongside it. Delegating to kubectl keeps the real rollout semantics
# (observedGeneration, updated/available replicas) rather than reimplementing
# them; the polls only add early exits, one in each direction.
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

    # Checked after the image guard so a missing image is still reported as
    # such: an unreachable node cannot mask a pipeline that forgot to build
    # something, because a stranded pod's image never gets pulled at all.
    if node_tolerance "$target" "$selector"; then
      kill "$ROLLOUT_PID" 2>/dev/null || true
      wait "$ROLLOUT_PID" 2>/dev/null || true
      ROLLOUT_PID=""

      return 0
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
