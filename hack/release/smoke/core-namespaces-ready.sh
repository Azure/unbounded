#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

# release-smoke: core-namespaces-ready
#
# Verifies that the core Unbounded namespaces exist on the deployed cluster
# and that every pod on a REACHABLE node in those namespaces is Running and
# Ready. Pods stranded on a node the kubelet has stopped reporting for are
# reported and not counted; see the convention on that below.
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
#     Report them and judge only what is running on a Ready node. The release
#     gate already decides how many NotReady nodes are acceptable, in
#     hack/release/wait-rollouts.sh, and smoke only runs once it has passed, so
#     that policy deliberately lives in exactly one place.

set -euo pipefail

: "${TAG:?TAG must be set}"
: "${KUBECONFIG:?KUBECONFIG must be set}"

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
# 2. Collect nodes that are not Ready. Pods on these are reported but never
#    counted as failures: the kubelet has stopped reporting, so their pods can
#    sit Terminating or unready indefinitely through no fault of the release.
# ---------------------------------------------------------------------------
notready_nodes=""
# Emitted as "<name>\t<ready>" and filtered here rather than with a jsonpath
# predicate on .items: kubectl cannot evaluate a nested filter inside an item
# selector, and silently returns nothing when asked to.
if node_readiness="$("${KUBECTL[@]}" get nodes \
  -o jsonpath='{range .items[*]}{.metadata.name}{"\t"}{.status.conditions[?(@.type=="Ready")].status}{"\n"}{end}' 2>/dev/null)"; then
  notready_nodes="$(awk -F'\t' '$2 != "True" { print $1 }' <<<"${node_readiness}")"
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

# ---------------------------------------------------------------------------
# 3. Every pod on a Ready node is Running and Ready.
#    Completed pods (Job-style workloads in phase Succeeded) are ignored;
#    they are not a failure even though they are not Running.
# ---------------------------------------------------------------------------
failures=0
stranded=0
for ns in "${NAMESPACES[@]}"; do
  echo "Checking pod readiness in ${ns}"

  # Captured before the loop rather than piped into it. A process substitution
  # that fails is invisible to `set -e`, so a query that errored would feed the
  # loop nothing and this check would report success having examined no pods at
  # all - the one outcome a smoke test must never produce.
  if ! pods="$("${KUBECTL[@]}" -n "${ns}" get pods \
      -o jsonpath='{range .items[*]}{.metadata.name}{"\t"}{.status.phase}{"\t"}{.status.conditions[?(@.type=="Ready")].status}{"\t"}{.spec.nodeName}{"\n"}{end}')"; then
    echo "::error::could not list pods in ${ns}"
    exit 1
  fi

  # Emit "<name>\t<phase>\t<ready_status>\t<node>" per pod. ready_status is the
  # status of the Ready condition (True/False/<empty> if missing).
  while IFS=$'\t' read -r name phase ready node; do
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
    echo "::error::pod ${ns}/${name} not ready (phase=${phase} ready=${ready:-<none>} node=${node:-<unscheduled>})"
    failures=$((failures + 1))
  done <<<"${pods}"
done

if (( failures > 0 )); then
  echo "::error::${failures} pod(s) not ready on Ready nodes across ${NAMESPACES[*]}"
  exit 1
fi

if (( stranded > 0 )); then
  echo "::warning::${stranded} pod(s) stranded on NotReady nodes were not validated by this release"
fi

echo "OK: namespaces present and every pod on a Ready node is Running+Ready"
