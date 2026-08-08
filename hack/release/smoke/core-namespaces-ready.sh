#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

# release-smoke: core-namespaces-ready
#
# Verifies that the core Unbounded namespaces exist on the deployed cluster
# and that every pod in those namespaces is Running and Ready.
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
# 2. Every pod in the target namespaces is Running and Ready.
#    Completed pods (Job-style workloads in phase Succeeded) are ignored;
#    they are not a failure even though they are not Running.
# ---------------------------------------------------------------------------
failures=0
for ns in "${NAMESPACES[@]}"; do
  echo "Checking pod readiness in ${ns}"
  # Emit "<name>\t<phase>\t<ready_status>" per pod. ready_status is the
  # status of the Ready condition (True/False/<empty> if missing).
  while IFS=$'\t' read -r name phase ready; do
    [[ -z "${name}" ]] && continue
    if [[ "${phase}" == "Succeeded" ]]; then
      continue
    fi
    if [[ "${phase}" != "Running" || "${ready}" != "True" ]]; then
      echo "::error::pod ${ns}/${name} not ready (phase=${phase} ready=${ready:-<none>})"
      failures=$((failures + 1))
    fi
  done < <("${KUBECTL[@]}" -n "${ns}" get pods \
            -o jsonpath='{range .items[*]}{.metadata.name}{"\t"}{.status.phase}{"\t"}{.status.conditions[?(@.type=="Ready")].status}{"\n"}{end}')
done

if (( failures > 0 )); then
  echo "::error::${failures} pod(s) not ready across ${NAMESPACES[*]}"
  exit 1
fi

echo "OK: namespaces present and all pods Running+Ready"
