#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

# release-smoke: core-namespaces-ready
#
# Verifies that the core Unbounded namespaces exist on the deployed cluster
# and that every pod the release can be judged on is Running and Ready. Pods
# stranded by a node the kubelet has stopped reporting for, whether they are on
# it or merely pinned to it, are reported and not counted; see the convention on
# that below.
#
# This script is the canonical TEMPLATE for release smoke tests. Copy it
# as the starting point for new smoke checks under hack/release/smoke/.
#
# Contract (provided by .github/workflows/release-upgrade.yaml):
#   TAG          The deployed release tag, e.g. v0.1.12.
#   KUBECONFIG   Path to a kubeconfig pointed at unbounded-stable.
#   SITE_NAME    Site name configured for the cluster.
#
# Conventions for new smoke tests:
#   - set -euo pipefail at the top so any unchecked failure aborts.
#   - Print what you are checking; the run log is the primary debugging
#     surface.
#   - Keep the check focused. One concept per script.
#   - Cap your kubectl calls with --request-timeout so a stuck apiserver
#     fails fast instead of consuming the per-task 15-minute budget.
#   - Exit 0 on pass, non-zero on fail.
#   - Use GitHub Actions workflow commands ("::error::msg") to surface
#     failures as red annotations in the run summary. See
#     https://docs.github.com/en/actions/reference/workflow-commands-for-github-actions
#   - Do not fail on workloads stranded by an unreachable node. Sites go
#     offline; that is the premise of the project, not a release regression.
#     How many NotReady nodes are acceptable is decided once, by the deploy
#     gate in hack/release/wait-rollouts.sh, which has already passed by the
#     time smoke runs.

set -euo pipefail

: "${TAG:?TAG must be set}"
: "${KUBECONFIG:?KUBECONFIG must be set}"

command -v jq >/dev/null 2>&1 || {
  echo "::error::core-namespaces-ready requires jq on PATH"
  exit 1
}

NAMESPACES=(unbounded-system)
KUBECTL=(kubectl --request-timeout=30s)

echo "Smoke: validate core namespaces and pod readiness for ${TAG}"

# ---------------------------------------------------------------------------
# 1. Namespaces exist.
# ---------------------------------------------------------------------------
for ns in "${NAMESPACES[@]}"; do
  echo "Checking namespace ${ns} exists"
  if ! "${KUBECTL[@]}" get namespace "${ns}" >/dev/null; then
    echo "::error::namespace ${ns} not found on cluster"
    exit 1
  fi
done

# ---------------------------------------------------------------------------
# 2. Collect node readiness. Pods on a node that is not Ready are reported,
#    never counted: with the kubelet gone they can sit Terminating indefinitely
#    through no fault of the release.
#
#    Per-SITE readiness is collected from the same payload, for the unscheduled
#    case in section 3. Read once because two queries could disagree.
# ---------------------------------------------------------------------------
notready_nodes=""
site_readiness=""

if nodes_json="$("${KUBECTL[@]}" get nodes -o json 2>/dev/null)"; then
  notready_nodes="$(jq -r '
    .items[]
    | select(((((.status.conditions // []) | map(select(.type == "Ready")) | .[0].status) // "Unknown")) != "True")
    | .metadata.name
  ' <<<"${nodes_json}")"

  # "<site>\t<total>\t<ready>" per site any node claims. Both label keys are
  # consulted because the net controllers still dual-write them.
  site_readiness="$(jq -r '
    [ .items[]
      | { site: (.metadata.labels["unbounded-cloud.io/site"]
                  // .metadata.labels["net.unbounded-cloud.io/site"]
                  // ""),
          ready: (((((.status.conditions // [])
                      | map(select(.type == "Ready")) | .[0].status) // "Unknown")) == "True")
        }
      | select(.site != "")
    ]
    | group_by(.site)[]
    | [ .[0].site, (. | length), ([ .[] | select(.ready) ] | length) ]
    | @tsv
  ' <<<"${nodes_json}")"
else
  # Fail closed: if node readiness cannot be established, judge every pod.
  echo "::warning::could not list node readiness; every unready pod will be treated as a failure"
fi

if [[ -n "${notready_nodes}" ]]; then
  echo "::warning::NotReady nodes (pods on these are excluded from this check): $(tr '\n' ' ' <<<"${notready_nodes}")"
fi

is_notready_node() {
  [[ -n "$1" && -n "${notready_nodes}" ]] || return 1

  grep -qxF "$1" <<<"${notready_nodes}"
}

# site_entirely_unreachable answers whether a site exists and has no Ready node.
# A site no node claims returns 1: that is a label matching nothing, which is
# indistinguishable from a typo and must not excuse anything.
#
# This mirrors the site check in hack/release/wait-rollouts.sh. The two are
# deliberately separate copies, because a smoke test is meant to stand alone -
# but they answer the same question, so change them together.
site_entirely_unreachable() {
  local want="$1" site total ready

  [[ -n "${want}" && -n "${site_readiness}" ]] || return 1

  while IFS=$'\t' read -r site total ready; do
    [[ "${site}" == "${want}" ]] || continue

    (( total > 0 )) || return 1
    (( ready == 0 )) || return 1

    return 0
  done <<<"${site_readiness}"

  # No node claims this site at all.
  return 1
}

# pod_is_stranded_unscheduled answers whether an unscheduled pod is explained by
# every site it is pinned to being unreachable. Only REQUIRED affinity counts: a
# preferred term is a hint the scheduler may ignore, so a pod carrying one could
# have run elsewhere and its being unscheduled is a real failure.
#
# $1 is the comma-separated pin list extracted by the pod query below. Empty
# means the pod is not site-scoped, so nothing here excuses it.
pod_is_stranded_unscheduled() {
  local pins="$1" site

  [[ -n "${pins}" ]] || return 1

  while IFS= read -r site; do
    [[ -n "${site}" ]] || continue

    site_entirely_unreachable "${site}" || return 1
  done <<<"$(tr ',' '\n' <<<"${pins}")"

  return 0
}

# ---------------------------------------------------------------------------
# 3. Every pod the release can be judged on is Running and Ready.
#
#    Three kinds are reported rather than counted, because none of them is
#    something a release can fix:
#      - Completed pods (Job-style workloads in phase Succeeded);
#      - pods on a node that is not Ready;
#      - UNSCHEDULED pods pinned to a site with no reachable node. These have no
#        nodeName, so they cannot be matched against the NotReady list at all,
#        and used to be counted as failures. The operator creates one Deployment
#        per Site for some components, and when that site is entirely
#        unreachable its pod has nowhere to go. The deploy gate has already
#        tolerated exactly this before smoke runs; see wait-rollouts.sh.
# ---------------------------------------------------------------------------
failures=0
stranded=0
for ns in "${NAMESPACES[@]}"; do
  echo "Checking pod readiness in ${ns}"

  # Captured before the loop, not piped into it: a failing process substitution
  # is invisible to `set -e`, so an errored query would feed the loop nothing
  # and this check would pass having examined no pods at all.
  #
  # -o json rather than jsonpath because the site pins need a nested filter,
  # and kubectl silently returns nothing when asked for one.
  if ! pods_json="$("${KUBECTL[@]}" -n "${ns}" get pods -o json)"; then
    echo "::error::could not list pods in ${ns}"
    exit 1
  fi

  # "<name>|<phase>|<ready_status>|<node>|<site_pins>" per pod, joined on the
  # ASCII unit separator rather than a tab. Tab is IFS whitespace, so bash
  # collapses runs of it and an EMPTY field silently disappears: an unscheduled
  # pod has neither a Ready condition nor a nodeName, so two fields in a row are
  # empty and every later field shifts left. The separator below cannot appear
  # in a Kubernetes name or label value.
  #
  # ready_status is the status of the Ready condition (True/False/empty if
  # missing). site_pins is comma-separated, and empty for a pod that is not
  # site-scoped.
  if ! pods="$(jq -r '
    .items[]
    | [ .metadata.name,
        (.status.phase // ""),
        ((((.status.conditions // []) | map(select(.type == "Ready")) | .[0].status) // "")),
        (.spec.nodeName // ""),
        ([ (.spec.affinity.nodeAffinity
              .requiredDuringSchedulingIgnoredDuringExecution.nodeSelectorTerms // [])[]
            | (.matchExpressions // [])[]
            | select(.key == "unbounded-cloud.io/site" or .key == "net.unbounded-cloud.io/site")
            | select(.operator == "In")
            | (.values // [])[]
          ] + [ ((.spec.nodeSelector // {}) | to_entries)[]
                | select(.key == "unbounded-cloud.io/site" or .key == "net.unbounded-cloud.io/site")
                | .value
              ]
          | unique | join(","))
      ]
    | join("\u001f")
  ' <<<"${pods_json}")"; then
    echo "::error::could not evaluate pods in ${ns}"
    exit 1
  fi

  while IFS=$'\037' read -r name phase ready node pins; do
    [[ -z "${name}" ]] && continue
    if [[ "${phase}" == "Succeeded" ]]; then
      continue
    fi
    if [[ "${phase}" == "Running" && "${ready}" == "True" ]]; then
      continue
    fi
    if is_notready_node "${node}"; then
      echo "  stranded: ${ns}/${name} on NotReady node ${node} (phase=${phase})"
      stranded=$((stranded + 1))
      continue
    fi
    if [[ -z "${node}" ]] && pod_is_stranded_unscheduled "${pins}"; then
      echo "  stranded: ${ns}/${name} unscheduled; pinned to unreachable site(s) ${pins} (phase=${phase})"
      stranded=$((stranded + 1))
      continue
    fi
    echo "::error::pod ${ns}/${name} not ready (phase=${phase} ready=${ready:-<none>} node=${node:-<unscheduled>})"
    failures=$((failures + 1))
  done <<<"${pods}"
done

if (( failures > 0 )); then
  echo "::error::${failures} pod(s) not ready on Ready nodes across ${NAMESPACES[*]}"
  exit 1
fi

if (( stranded > 0 )); then
  echo "::warning::${stranded} pod(s) stranded by unreachable nodes were not validated by this release"
fi

echo "OK: namespaces present and every pod the release can be judged on is Running+Ready"
