#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

# wait-rollouts: wait for operator-managed workloads to become the release under
# test and then become available, fail fast with a named image when a pod cannot
# pull the image it needs, and tolerate a DaemonSet shortfall that is caused
# only by unreachable nodes.
#
# Each workload is waited on in three phases: it must exist, it must reference
# EXPECTED_IMAGE_TAG, and only then is its rollout judged - and the tag is
# confirmed again once kubectl reports success. The middle phase matters because
# 'kubectl rollout status' answers "is this workload settled", and a workload
# the operator has not updated yet is perfectly settled, so asking first would
# certify the PREVIOUS release. The confirmation matters because a rollout takes
# minutes, and whatever rewrites the template in that window is what kubectl
# ends up reporting success for.
#
# Shared by .github/workflows/nightly.yaml and release-upgrade.yaml. Both gates
# previously carried a copy of this logic and had already drifted (gantry was
# gated in one and not the other). Both invoke it from a sparse checkout of the
# workflow's own commit, so a rollback or backfill onto an older tag still runs
# the current gate.
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
#   POLL_INTERVAL_SECONDS        Cadence of both polls, image and node
#                                (default 5).
#   IMAGE_FAILURE_GRACE_SECONDS  How long a retryable image failure must persist
#                                before it is treated as fatal (default 90).
#   MAX_NOTREADY_NODES           How many NotReady nodes may be tolerated before
#                                a DaemonSet shortfall stops being excusable
#                                (default 2). 0 disables tolerance entirely.
#   EXPECTED_IMAGE_TAG           The release being deployed, e.g. v0.2.5 or
#                                nightly-abc1234. No workload is judged until
#                                every image we publish carries it. Unset skips
#                                that wait and disables tolerance.
#   EXPECTED_IMAGE_REGISTRY      Where we publish, e.g. ghcr.io/azure. Required
#                                with EXPECTED_IMAGE_TAG, and the only way to
#                                tell our images from third-party pins.
#
# Design notes
# ------------
# 'kubectl rollout status' remains the authority on readiness. The two polls
# beside it only add early exits, and they run opposite disciplines.
#
# The image check is FAIL-OPEN: anything it cannot determine is reported loudly
# and then ignored, never converted into a failed deploy. It is an accelerator
# for diagnosis, not a second opinion on health. It exists because a pipeline
# that forgets to build one component leaves its pods in ImagePullBackOff and
# 'rollout status' just blocks for its full timeout, reporting a bare "pod not
# ready" - a failure mode that kept the nightly red for 15 consecutive nights.
# It is also strictly SCOPED to the workload being waited on: an earlier
# revision scanned the whole namespace, so an unrelated tenant or a leftover pod
# could abort the very deploy that would have repaired the cluster.
#
# The node check is FAIL-CLOSED. It is the only thing here that can turn a
# failing wait into a passing one, so anything it cannot positively verify means
# "keep waiting" and let rollout status deliver the verdict. It exists because a
# DaemonSet counts every node toward desiredNumberScheduled, including nodes the
# kubelet has stopped reporting for, so one unreachable node blocks the gate
# until its timeout, every time, forever.
#
# There are two shapes of that problem, and each has its own set of conditions.
#
# A DAEMONSET is tolerated only when ALL of:
#
#   - it is short of pods at all;
#   - its CURRENT pod template references EXPECTED_IMAGE_TAG;
#   - between 1 and MAX_NOTREADY_NODES nodes are NotReady;
#   - the DaemonSet controller has observed the current generation, and nothing
#     is misscheduled onto a node it no longer selects;
#   - no pod on a READY node is unhealthy or still on the previous release;
#   - Ready nodes carrying a healthy current pod, plus the unreachable ones,
#     cover the desired count. Counted in NODES: the invariant is one pod per
#     node, and counting pods let a node with two cover for a node with none.
#
# A DEPLOYMENT is ordinarily NOT tolerated: it reschedules off a dead node, so a
# shortfall is a real scheduling problem. The exception is a SITE-SCOPED one,
# whose required node affinity pins it to a site whose nodes are ALL
# unreachable. It has nowhere to reschedule to, so it stalls exactly the way a
# DaemonSet does, and the operator creates one per Site that enables a component
# (metalman is the current example). Tolerated only when ALL of:
#
#   - it is short of available replicas;
#   - its CURRENT pod template references EXPECTED_IMAGE_TAG;
#   - the Deployment controller has observed the current generation;
#   - it is pinned, by required affinity or nodeSelector, to at least one site;
#   - EVERY site it is pinned to has at least one node and NO Ready node. One
#     Ready node anywhere in the set means it could run there, so the shortfall
#     is a real problem;
#   - between 1 and MAX_NOTREADY_NODES nodes are NotReady;
#   - none of its pods is on a Ready node, which would contradict the above.
#
# Tolerance repeats the EXPECTED_IMAGE_TAG condition rather than trusting the
# earlier phase: the template can arrive while a pod on a reachable node is
# still running the old one.
#
# Exceeding MAX_NOTREADY_NODES is reported immediately, grouped by site, so the
# log explains the timeout while it is still counting down. It does NOT abort
# early: a node may recover inside the window.

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
EXPECTED_IMAGE_TAG="${EXPECTED_IMAGE_TAG:-}"
# Lowercased because image references are, while the GitHub owner that usually
# supplies this is not: ghcr.io/Azure never appears in a pod spec.
EXPECTED_IMAGE_REGISTRY="${EXPECTED_IMAGE_REGISTRY:-}"
EXPECTED_IMAGE_REGISTRY="${EXPECTED_IMAGE_REGISTRY,,}"
EXPECTED_IMAGE_REGISTRY="${EXPECTED_IMAGE_REGISTRY%/}"

if [[ ! "$MAX_NOTREADY_NODES" =~ ^[0-9]+$ ]]; then
  echo "::error::MAX_NOTREADY_NODES must be a non-negative integer (got '${MAX_NOTREADY_NODES}')"
  exit 2
fi

# Reported once, up front, rather than per poll: with no tag to check against
# there is no way to tell the release being deployed from the one it replaces,
# so tolerance is unavailable and a shortfall will run the rollout timeout down.
# Both callers set it; this fires when a third one forgets to.
if (( MAX_NOTREADY_NODES > 0 )) && [[ -z "$EXPECTED_IMAGE_TAG" ]]; then
  echo "::notice::EXPECTED_IMAGE_TAG is not set; degraded-node tolerance is unavailable for this run"
fi

# The two travel together. Deciding whether a workload is on this release means
# telling the images we publish from the third-party ones pinned beside them,
# and that is what the registry supplies. Falling back to a weaker rule when
# only one is set would be a silent downgrade of the check.
if [[ -n "$EXPECTED_IMAGE_TAG" && -z "$EXPECTED_IMAGE_REGISTRY" ]]; then
  echo "::error::EXPECTED_IMAGE_TAG is set without EXPECTED_IMAGE_REGISTRY; both are needed to tell our images from pinned third-party ones"
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

# ImagePullBackOff is RETRYABLE: registry throttling, a brief outage or delayed
# credentials all land here and then recover, so it is only fatal once it has
# persisted for IMAGE_FAILURE_GRACE_SECONDS. A bare ErrImagePull is the first
# attempt and is never fatal.
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

# disable_guard turns the image check off for the rest of the run, for when it
# cannot be evaluated at all. Reported as an error so it cannot pass unnoticed,
# but it does not fail the deploy: rollout status still returns the
# authoritative verdict, and a broken diagnostic must not become an outage.
disable_guard() {
  GUARD_DISABLED=1

  echo "::error::wait-rollouts.sh image guard disabled: $1"
  echo "::error::rollout status still gates this deploy, but a missing image will no longer be named; fix wait-rollouts.sh"
}

# kubectl_json runs a read-only query and prints stdout on success. stderr goes
# to a file rather than 2>&1, because kubectl's deprecation and warning lines
# would corrupt the JSON jq has to parse. Returns 1 on failure, leaving the
# message in ${WORKDIR}/kubectl.err.
kubectl_json() {
  local out

  if out="$("${KUBECTL[@]}" "$@" 2>"${WORKDIR}/kubectl.err")"; then
    printf '%s' "$out"

    return 0
  fi

  return 1
}

# resolve_target prints three lines: the workload's kind, its pod selector, and
# its desired container images as a JSON array. Kind is taken from this one read
# rather than per poll, so a Deployment costs no API calls in the poll loop.
#
# Exit codes: 0 resolved, 2 query failed, 3 evaluation failed.
resolve_target() {
  local target="$1" json

  json="$(kubectl_json get "$target" -o json)" || return 2

  printf '%s' "$json" | jq -r '
    (.kind // ""),
    ((.spec.selector.matchLabels // {}) | to_entries | map("\(.key)=\(.value)") | join(",")),
    ([ (.spec.template.spec.containers // [])[], (.spec.template.spec.initContainers // [])[] ]
      | map(.image) | unique | tojson)
  ' 2>"${WORKDIR}/jq.err" || return 3
}

# IMAGE_FAILURE_FILTER emits one TSV record per container wedged on an image
# error that this rollout is actually responsible for. Three filters keep it
# scoped: the caller restricts the pod list to the workload's selector,
# terminating pods are skipped as unrepairable, and the failing image must be
# one the workload currently WANTS, which drops superseded revisions.
#
# The image comes from the POD SPEC, not containerStatuses: the runtime
# normalizes status images, which would defeat that last comparison. Init
# containers count, since a failed init image never lets the main one start.
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
# node names, emitting
# "<stranded>\t<unhealthy_on_ready>\t<outdated_on_ready>\t<healthy_on_ready>".
#
#   stranded            NODES the cluster cannot reach that hold a pod, which
#                       the controller can neither update nor reap; they are why
#                       the rollout stalls.
#   unhealthy_on_ready  pods elsewhere that are not Running+Ready. A pod with no
#                       nodeName counts here: DaemonSet pods are assigned at
#                       creation, so an unassigned one is a scheduling failure.
#   outdated_on_ready   pods elsewhere still running something other than $tag.
#   healthy_on_ready    NODES elsewhere carrying a Running+Ready pod on $tag.
#
# The two coverage numbers count NODES, not pods, because the invariant being
# checked is one pod per node. Counting pods let a node with two of them - a
# terminating pod and its replacement - cover for a node with none, so the
# fleet looked complete while a reachable node had nothing running on it.
# Terminating pods are excluded from the reachable counts for the same reason:
# a pod on its way out is not coverage. They still count toward stranded, which
# is exactly what a pod on an unreachable node usually is.
#
# $owner scopes the list to pods this workload actually owns. The selector is
# only a label match, and anything else wearing those labels is not evidence
# about this rollout either way.
#
# Every number is derived from the pod list, never from .status counters. An
# earlier revision compared updatedNumberScheduled + stranded against desired,
# which double-counts: a stranded pod the controller had already updated is in
# BOTH terms, so the sum reached desired while a reachable node was still
# running the previous release. That is not a corner case - with maxUnavailable
# 1 a stranded pod counts as unavailable, so the controller stops updating the
# remaining nodes, making it the steady state of a stalled rollout.
DAEMONSET_SHORTFALL_FILTER='
  [ .items[]
    | select((.metadata.ownerReferences // []) | any(.uid == $owner))
    | (.spec.nodeName // "") as $node
    | { node: $node,
        terminating: (.metadata.deletionTimestamp != null),
        stranded: ($notready | any(. == $node)),
        ready: ((.status.phase == "Running")
                 and ((((.status.conditions // [])
                         | map(select(.type == "Ready")) | .[0].status) // "False") == "True")),
        current: ([ (.spec.containers // [])[], (.spec.initContainers // [])[] ]
                    | map(.image)
                    | map(select(startswith($registry + "/"))) as $ours
                    | ($ours | length) > 0 and ($ours | all(endswith($tag))))
      }
  ] as $pods
  | [ ($pods | map(select(.stranded)) | map(.node) | unique | length),
      ($pods | map(select((.stranded | not) and (.terminating | not) and (.ready | not))) | length),
      ($pods | map(select((.stranded | not) and (.terminating | not) and (.current | not))) | length),
      ($pods | map(select((.stranded | not) and (.terminating | not) and .ready and .current and (.node != "")))
             | map(.node) | unique | length)
    ]
  | @tsv
'

# DAEMONSET_STATUS_FILTER emits what only the controller can tell us:
# "<desired>\t<ready>\t<misscheduled>\t<generation>\t<observedGeneration>".
# desired is the node count no pod list can supply, ready detects the shortfall,
# misscheduled says whether any pod is running where it should not be, and the
# generations say whether the controller has caught up. Whether the fleet is
# UPDATED is decided from the pods; see DAEMONSET_SHORTFALL_FILTER.
DAEMONSET_STATUS_FILTER='
  [ (.status.desiredNumberScheduled // 0),
    (.status.numberReady // 0),
    (.status.numberMisscheduled // 0),
    (.metadata.generation // 0),
    (.status.observedGeneration // -1)
  ]
  | @tsv
'

# TEMPLATE_IMAGE_FILTER reports whether a pod template is on this release:
# every image we publish carries $tag, and there is at least one of them.
#
# Scoped to $registry because a component's images sit beside pinned
# third-party ones - gantry's busybox init never carries a release tag - and
# those must not be judged. An earlier revision asked whether ANY image carried
# the tag, which meant a current sidecar excused a stale primary.
#
# `all` is true for an empty list, hence the length check: a workload with none
# of our images is not on this release, it is something else entirely.
TEMPLATE_IMAGE_FILTER='
  [ (.spec.template.spec.containers // [])[], (.spec.template.spec.initContainers // [])[] ]
  | map(.image)
  | map(select(startswith($registry + "/"))) as $ours
  | ($ours | length) > 0 and ($ours | all(endswith($tag)))
'

# SITE_PIN_FILTER emits, as a JSON array, the sites a workload is pinned to.
#
# Only REQUIRED affinity counts: a preferred term is a hint the scheduler may
# ignore, so a workload carrying one can still run elsewhere and its shortfall
# is not explained by a dead site. nodeSelector is included because it is the
# older spelling of the same constraint, and both keys are consulted because the
# net controllers still dual-write them.
#
# nodeSelectorTerms are OR-ed, nodeSelector entries AND-ed. Collapsing both into
# one set and later demanding that EVERY site in it be unreachable is exact for
# the OR and conservative for the AND, which is the right direction: the worst
# it does is decline to tolerate.
SITE_PIN_FILTER='
  [ (.spec.template.spec.affinity.nodeAffinity
       .requiredDuringSchedulingIgnoredDuringExecution.nodeSelectorTerms // [])[]
    | (.matchExpressions // [])[]
    | select(.key == "unbounded-cloud.io/site" or .key == "net.unbounded-cloud.io/site")
    | select(.operator == "In")
    | (.values // [])[]
  ]
  + [ ((.spec.template.spec.nodeSelector // {}) | to_entries)[]
      | select(.key == "unbounded-cloud.io/site" or .key == "net.unbounded-cloud.io/site")
      | .value
    ]
  | unique
'

# SITE_NODE_FILTER emits "<site>\t<total>\t<ready>" for each site in $sites.
# A site with no nodes at all reports total 0, which the caller treats as
# unverifiable rather than as "entirely unreachable".
SITE_NODE_FILTER='
  [ .items[]
    | { site: (.metadata.labels["unbounded-cloud.io/site"]
                // .metadata.labels["net.unbounded-cloud.io/site"]
                // ""),
        ready: (((((.status.conditions // [])
                    | map(select(.type == "Ready")) | .[0].status) // "Unknown")) == "True")
      }
  ] as $nodes
  | $sites[]
  | . as $site
  | [ $site,
      ([ $nodes[] | select(.site == $site) ] | length),
      ([ $nodes[] | select(.site == $site and .ready) ] | length)
    ]
  | @tsv
'

# DEPLOYMENT_STATUS_FILTER emits
# "<desired>\t<available>\t<generation>\t<observedGeneration>".
DEPLOYMENT_STATUS_FILTER='
  [ (.spec.replicas // 0),
    (.status.availableReplicas // 0),
    (.metadata.generation // 0),
    (.status.observedGeneration // -1)
  ]
  | @tsv
'

# DEPLOYMENT_PLACEMENT_FILTER counts this Deployment's pods that sit on a node
# which is NOT NotReady. The answer must be zero: a pod on a reachable node
# means the workload can run somewhere after all, so its shortfall is not
# explained by the dead site.
#
# $owners holds the uids of the ReplicaSets this Deployment owns, because a
# Deployment does not own its pods directly. Terminating pods are excluded for
# the same reason the DaemonSet filter excludes them: a pod on its way out is
# not evidence either way.
DEPLOYMENT_PLACEMENT_FILTER='
  [ .items[]
    | select(.metadata.deletionTimestamp == null)
    | select(any((.metadata.ownerReferences // [])[]; .uid as $uid | ($owners | index($uid)) != null))
    | (.spec.nodeName // "") as $node
    | select($node != "" and (($notready | index($node)) == null))
  ]
  | length
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
# site, for example "boulderlab[spark-3d37] edge[node-9, node-10]". Sites are
# emitted in first-seen order; awk iterates an associative array in an
# unspecified one, which would reshuffle the message between identical polls.
describe_nodes() {
  awk -F'\t' '
    !($2 in group) { order[++n] = $2 }
    { group[$2] = group[$2] (group[$2] ? ", " : "") $1 }
    END {
      for (i = 1; i <= n; i++) {
        printf "%s%s[%s]", (i > 1 ? " " : ""), order[i], group[order[i]]
      }
    }
  ' <<<"$1"
}

# template_has_release reports, quietly, whether a workload's CURRENT pod
# template references EXPECTED_IMAGE_TAG. Read from the object re-fetched on
# each poll, never the spec read before the wait began: observing the operator
# rewrite that template mid-wait is the entire mechanism.
#
# Returns 1 when the tag is unset or jq fails, like every other uncertain path
# in node_tolerance.
template_has_release() {
  local obj_json="$1" matched

  [[ -n "$EXPECTED_IMAGE_TAG" ]] || return 1

  matched="$(printf '%s' "$obj_json" | jq -r --arg tag ":${EXPECTED_IMAGE_TAG}" \
    --arg registry "$EXPECTED_IMAGE_REGISTRY" \
    "$TEMPLATE_IMAGE_FILTER" 2>"${WORKDIR}/jq.err")" || return 1

  [[ "$matched" == "true" ]]
}

# running_expected_release is template_has_release plus the explanation the
# waiting paths want when it is not yet true.
running_expected_release() {
  local target="$1" obj_json="$2"

  if template_has_release "$obj_json"; then
    return 0
  fi

  # Normal early in a deploy, and the reason the shortfall is not excused yet.
  # An image pinned by an overrides entry looks the same but never clears.
  warn_once "${target} does not reference :${EXPECTED_IMAGE_TAG} yet, so its shortfall cannot be excused; waiting for the operator to update it"

  return 1
}

# confirm_expected_release re-reads a workload after its rollout succeeded and
# fails if it is no longer the release under test.
#
# The tag was checked before rollout status was asked, and a rollout takes
# minutes. Anything that rewrites the template in between - another release
# deploying to the same cluster, a hand-applied manifest - and kubectl reports
# THAT rollout as this one's success. The concurrency group now serializes
# deploys per cluster, which removes the pipeline's own version of this; the
# check remains because a shared cluster has other writers.
confirm_expected_release() {
  local target="$1" obj_json

  [[ -n "$EXPECTED_IMAGE_TAG" ]] || return 0

  obj_json="$(kubectl_json get "$target" -o json)" || {
    echo "::error::${target} rolled out but could not be re-read to confirm it is still :${EXPECTED_IMAGE_TAG}: $(flatten "${WORKDIR}/kubectl.err")"

    return 1
  }

  if template_has_release "$obj_json"; then
    return 0
  fi

  echo "::error::${target} rolled out, but no longer references :${EXPECTED_IMAGE_TAG}; something replaced it mid-wait and this rollout is not the one being gated"

  return 1
}

# node_tolerance decides whether a stalled rollout is explained entirely by
# nodes the cluster has lost contact with, and dispatches to the check for the
# workload's kind.
#
# Returns 0 to tolerate (the caller stops waiting and succeeds), 1 to keep
# waiting. EVERY uncertain path returns 1; see the FAIL-CLOSED note at the top.
#
# Conditions are ordered cheapest-first, and everything decidable without the
# apiserver is decided before anything is queried: this runs on every poll of
# every workload, so reading the node list before noticing the workload was
# neither kind would cost tens of megabytes per wait to reach the same answer.
node_tolerance() {
  local target="$1" kind="$2" selector="$3"

  # No selector means pods cannot be scoped to this workload. Also the state
  # when the image guard has been disabled, which takes tolerance down with it:
  # a script that could not read a workload's spec has no business excusing that
  # workload's shortfall.
  [[ -n "$selector" ]] || return 1

  # Tolerance switched off, or nothing to compare a template against.
  (( MAX_NOTREADY_NODES > 0 )) || return 1
  [[ -n "$EXPECTED_IMAGE_TAG" ]] || return 1

  # Free: read once, before the wait, by resolve_target.
  case "$kind" in
    DaemonSet)  daemonset_tolerance "$target" "$selector" ;;
    Deployment) site_deployment_tolerance "$target" "$selector" ;;
    *)          return 1 ;;
  esac
}

# daemonset_tolerance decides whether a stalled DaemonSet rollout is explained
# entirely by nodes the cluster has lost contact with.
daemonset_tolerance() {
  local target="$1" selector="$2"
  local obj_json pods_json nodes_tsv notready_json counts status_tsv
  local desired ready misscheduled generation observed owner_uid
  local stranded unhealthy outdated healthy
  local notready_count

  # One read serves both the live status numbers and the image check, and both
  # have to be evaluated against the CURRENT object.
  obj_json="$(kubectl_json get "$target" -o json)" || {
    warn_once "could not read ${target} while evaluating node tolerance: $(flatten "${WORKDIR}/kubectl.err")"

    return 1
  }

  status_tsv="$(printf '%s' "$obj_json" | jq -r "$DAEMONSET_STATUS_FILTER" 2>"${WORKDIR}/jq.err")" || return 1

  IFS=$'\t' read -r desired ready misscheduled generation observed <<<"$status_tsv" || return 1

  # Stale or partial data. Any of these failing means "not yet", never "close
  # enough".
  (( desired > 0 )) || return 1
  (( observed >= generation )) || return 1

  # A pod running on a node the DaemonSet no longer selects. Coverage is counted
  # in nodes, and this one is not a node the fleet is supposed to cover, so it
  # could stand in for a desired node that has nothing. The controller reaps
  # these, so it clears on its own.
  if (( misscheduled != 0 )); then
    warn_once "${target} has ${misscheduled} misscheduled pod(s); a shortfall cannot be excused while any pod is running where it should not be"

    return 1
  fi

  # No shortfall to excuse; rollout status will return on its own.
  (( ready < desired )) || return 1

  # Checked before the node and pod queries: it is the condition most likely to
  # be false early in a deploy, and it needs no further API calls.
  running_expected_release "$target" "$obj_json" || return 1

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

  # Scopes the pod list to what this workload owns. Without a uid there is no
  # way to tell its pods from anything else wearing the same labels, which is
  # not a judgement this check may make on a guess.
  owner_uid="$(printf '%s' "$obj_json" | jq -r '.metadata.uid // ""' 2>"${WORKDIR}/jq.err")" || return 1
  [[ -n "$owner_uid" ]] || return 1

  counts="$(printf '%s' "$pods_json" | jq -r --argjson notready "$notready_json" \
    --arg tag ":${EXPECTED_IMAGE_TAG}" --arg owner "$owner_uid" \
    --arg registry "$EXPECTED_IMAGE_REGISTRY" \
    "$DAEMONSET_SHORTFALL_FILTER" 2>"${WORKDIR}/jq.err")" || return 1

  IFS=$'\t' read -r stranded unhealthy outdated healthy <<<"$counts" || return 1

  # The shortfall must be the unreachable nodes and nothing else: something is
  # stranded, and everything reachable is both healthy and on this release.
  (( stranded > 0 )) || return 1
  (( unhealthy == 0 )) || return 1
  (( outdated == 0 )) || return 1

  # Coverage counted in NODES, so a node carrying two pods cannot cover for a
  # node carrying none.
  (( healthy + stranded >= desired )) || return 1

  echo "::warning::${target} is short ${stranded} of ${desired} pods, entirely on NotReady nodes: $(describe_nodes "$nodes_tsv")"
  echo "::warning::tolerating the ${target} shortfall (MAX_NOTREADY_NODES=${MAX_NOTREADY_NODES}); this release was NOT validated on those nodes"

  if [[ -n "${GITHUB_STEP_SUMMARY:-}" ]]; then
    {
      echo "### Degraded rollout tolerated: ${target}"
      echo
      echo "- Ready pods: ${healthy}/${desired} (${stranded} stranded on NotReady nodes)"
      echo "- NotReady nodes: $(describe_nodes "$nodes_tsv")"
      echo "- Deployed image tag: \`${EXPECTED_IMAGE_TAG}\`"
      echo "- Tolerated because \`MAX_NOTREADY_NODES=${MAX_NOTREADY_NODES}\`"
    } >> "$GITHUB_STEP_SUMMARY"
  fi

  return 0
}

# site_deployment_tolerance decides whether a stalled Deployment rollout is
# explained entirely by its target site being unreachable.
#
# The nodes are read ONCE here and both questions answered from that one
# payload: which nodes are NotReady, and how many nodes each pinned site has.
# Asking twice would cost a second full node list on every poll.
site_deployment_tolerance() {
  local target="$1" selector="$2"
  local obj_json nodes_json nodes_tsv notready_json sites_json site_tsv status_tsv
  local rs_json owners_json pods_json
  local desired available generation observed owner_uid
  local notready_count on_ready site total ready_count

  obj_json="$(kubectl_json get "$target" -o json)" || {
    warn_once "could not read ${target} while evaluating node tolerance: $(flatten "${WORKDIR}/kubectl.err")"

    return 1
  }

  # The sites this Deployment is pinned to. Checked before anything is queried:
  # a Deployment that is not site-scoped can reschedule, so the ordinary rule
  # applies and its shortfall is a real scheduling problem.
  sites_json="$(printf '%s' "$obj_json" | jq -c "$SITE_PIN_FILTER" 2>"${WORKDIR}/jq.err")" || return 1
  [[ -n "$sites_json" && "$sites_json" != "[]" ]] || return 1

  status_tsv="$(printf '%s' "$obj_json" | jq -r "$DEPLOYMENT_STATUS_FILTER" 2>"${WORKDIR}/jq.err")" || return 1

  IFS=$'\t' read -r desired available generation observed <<<"$status_tsv" || return 1

  # Stale or partial data. Any of these failing means "not yet", never "close
  # enough".
  (( desired > 0 )) || return 1
  (( observed >= generation )) || return 1

  # No shortfall to excuse; rollout status will return on its own.
  (( available < desired )) || return 1

  # Checked before the node and pod queries: it is the condition most likely to
  # be false early in a deploy, and it needs no further API calls.
  running_expected_release "$target" "$obj_json" || return 1

  nodes_json="$(kubectl_json get nodes -o json)" || {
    warn_once "could not evaluate node readiness for ${target}: $(flatten "${WORKDIR}/kubectl.err")"

    return 1
  }

  nodes_tsv="$(printf '%s' "$nodes_json" | jq -r "$NOTREADY_NODE_FILTER" 2>"${WORKDIR}/jq.err")" || return 1

  # Every node is Ready, so whatever is stalling this rollout is a real problem.
  [[ -n "$nodes_tsv" ]] || return 1

  notready_count="$(wc -l <<<"$nodes_tsv" | tr -d ' ')"

  if (( notready_count > MAX_NOTREADY_NODES )); then
    warn_once "too many NotReady nodes for the ${target} shortfall to be excused (${notready_count} > MAX_NOTREADY_NODES=${MAX_NOTREADY_NODES}): $(describe_nodes "$nodes_tsv")"

    return 1
  fi

  site_tsv="$(printf '%s' "$nodes_json" | jq -r --argjson sites "$sites_json" \
    "$SITE_NODE_FILTER" 2>"${WORKDIR}/jq.err")" || return 1

  [[ -n "$site_tsv" ]] || return 1

  # Every pinned site must be populated and entirely unreachable. A site with no
  # nodes proves nothing, and one Ready node anywhere in the set means the
  # workload could be running there.
  while IFS=$'\t' read -r site total ready_count; do
    [[ -n "$site" ]] || continue

    if (( total == 0 )); then
      warn_once "${target} is pinned to site ${site}, which has no nodes; its shortfall cannot be excused"

      return 1
    fi

    (( ready_count == 0 )) || return 1
  done <<<"$site_tsv"

  notready_json="$(cut -f1 <<<"$nodes_tsv" | jq -R -s -c 'split("\n") | map(select(length > 0))' 2>"${WORKDIR}/jq.err")" || return 1

  # A Deployment does not own its pods directly, so the ReplicaSets it owns are
  # resolved first. Without them there is no way to tell its pods from anything
  # else wearing the same labels, which is not a judgement this check may make
  # on a guess.
  owner_uid="$(printf '%s' "$obj_json" | jq -r '.metadata.uid // ""' 2>"${WORKDIR}/jq.err")" || return 1
  [[ -n "$owner_uid" ]] || return 1

  rs_json="$(kubectl_json get replicasets --selector "$selector" -o json)" || {
    warn_once "could not list replicasets for ${target} while evaluating node tolerance: $(flatten "${WORKDIR}/kubectl.err")"

    return 1
  }

  owners_json="$(printf '%s' "$rs_json" | jq -c --arg owner "$owner_uid" \
    '[ .items[] | select((.metadata.ownerReferences // []) | any(.uid == $owner)) | .metadata.uid ]' \
    2>"${WORKDIR}/jq.err")" || return 1

  [[ -n "$owners_json" && "$owners_json" != "[]" ]] || return 1

  pods_json="$(kubectl_json get pods --selector "$selector" -o json)" || {
    warn_once "could not list pods for ${target} while evaluating node tolerance: $(flatten "${WORKDIR}/kubectl.err")"

    return 1
  }

  on_ready="$(printf '%s' "$pods_json" | jq -r --argjson notready "$notready_json" \
    --argjson owners "$owners_json" "$DEPLOYMENT_PLACEMENT_FILTER" 2>"${WORKDIR}/jq.err")" || return 1

  # Contradicts the site check above: something of this workload is running on a
  # reachable node, so the shortfall is not the dead site.
  (( on_ready == 0 )) || return 1

  echo "::warning::${target} is short $(( desired - available )) of ${desired} replicas; every site it is pinned to is unreachable: $(describe_nodes "$nodes_tsv")"
  echo "::warning::tolerating the ${target} shortfall (MAX_NOTREADY_NODES=${MAX_NOTREADY_NODES}); this release was NOT validated on those nodes"

  if [[ -n "${GITHUB_STEP_SUMMARY:-}" ]]; then
    {
      echo "### Degraded rollout tolerated: ${target}"
      echo
      echo "- Available replicas: ${available}/${desired}"
      echo "- Pinned to site(s) with no reachable node: $(cut -f1 <<<"$site_tsv" | paste -sd', ' -)"
      echo "- NotReady nodes: $(describe_nodes "$nodes_tsv")"
      echo "- Deployed image tag: \`${EXPECTED_IMAGE_TAG}\`"
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

    # NotFound is expected while the operator reconciles. Anything else (RBAC,
    # expired credentials, an unreachable apiserver) is surfaced as it happens
    # and carried into the timeout message, so it cannot masquerade as a
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

# wait_for_release blocks until the workload's pod template references
# EXPECTED_IMAGE_TAG, i.e. until it IS the release under test.
#
# Without this the gate can pass against the PREVIOUS release. Component
# workloads are updated asynchronously: the operator reconciles the Site some
# time after its own Deployment reports available, so for the first seconds of a
# wait every component still carries the template it had before - fully healthy,
# fully rolled out. 'kubectl rollout status' answers "is this workload settled",
# and a workload nobody has touched yet is settled, so it returns success
# immediately and the deploy is declared good having validated nothing.
#
# This is the same condition node_tolerance applies, hoisted to cover every
# path rather than only the degraded one.
#
# A workload whose image has been pinned elsewhere by an
# unbounded-component-overrides entry never satisfies this and will time out.
# That is deliberate: the gate cannot certify a release against a workload that
# is not running it, and going quiet instead would be the failure this check
# exists to prevent.
wait_for_release() {
  local target="$1" deadline=$(( SECONDS + CREATE_TIMEOUT_SECONDS )) obj_json

  # Reported once at startup; tolerance is off too, so there is nothing to wait
  # for and rollout status remains the only verdict.
  [[ -n "$EXPECTED_IMAGE_TAG" ]] || return 0

  while true; do
    if obj_json="$(kubectl_json get "$target" -o json)"; then
      if running_expected_release "$target" "$obj_json"; then
        return 0
      fi
    else
      warn_once "could not read ${target} while waiting for :${EXPECTED_IMAGE_TAG}: $(flatten "${WORKDIR}/kubectl.err")"
    fi

    if (( SECONDS >= deadline )); then
      echo "::error::${target} never referenced :${EXPECTED_IMAGE_TAG} within ${CREATE_TIMEOUT_SECONDS}s; the operator has not rolled this release out to it"
      echo "::error::if its image is pinned by an unbounded-component-overrides entry, that override has to go before this release can be gated"

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
  local target="$1" kind="$2" selector="$3" desired="$4" rc=0

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
    if node_tolerance "$target" "$kind" "$selector"; then
      kill "$ROLLOUT_PID" 2>/dev/null || true
      wait "$ROLLOUT_PID" 2>/dev/null || true
      ROLLOUT_PID=""

      # Same confirmation the rollout path gets. node_tolerance checked the tag
      # on the object it read, but then went on to query nodes and pods, and
      # this is the one place a tolerated shortfall becomes a passing gate.
      confirm_expected_release "$target" || return 1

      return 0
    fi

    sleep "$POLL_INTERVAL_SECONDS"
  done

  wait "$ROLLOUT_PID" || rc=$?
  ROLLOUT_PID=""

  (( rc == 0 )) || return "$rc"

  # kubectl reported success. Confirm it reported it about the right thing.
  confirm_expected_release "$target"
}

# wait_target waits for one workload to exist and then to roll out.
wait_target() {
  local target="$1" kind="" selector="" desired="[]" meta rc=0
  local -a lines=()

  LAST_WARNING=""

  wait_exists "$target" || return 1

  # Before rollout status, not after: a workload the operator has not updated
  # yet is already "rolled out", so asking kubectl first would accept the
  # previous release.
  wait_for_release "$target" || return 1

  if (( GUARD_DISABLED == 0 )); then
    meta="$(resolve_target "$target")" || rc=$?

    case "$rc" in
      0)
        mapfile -t lines <<<"$meta"
        kind="${lines[0]:-}"
        selector="${lines[1]:-}"
        desired="${lines[2]:-[]}"

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

  wait_rollout "$target" "$kind" "$selector" "$desired"
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
