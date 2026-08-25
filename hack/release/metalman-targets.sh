#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

# metalman-targets: print the rollout-gate targets for the metalman component.
#
# Called by .github/workflows/release-upgrade.yaml, which appends the output to
# hack/release/wait-rollouts.sh's argument list.
#
# Metalman is a per-SITE component: the operator creates one
# Deployment/metalman-controller-<site> in the operator namespace for every Site
# that enables it, and none at all for a Site that does not. The targets are
# therefore discovered rather than assumed. On unbounded-stable in particular
# they are NOT derived from SITE_NAME: the cluster's own Site is `stable`, while
# metalman runs for a remote site.
#
# Contract (provided by the calling workflow):
#   KUBECONFIG        Path to a kubeconfig for the target cluster.
#
# Optional overrides:
#   REQUIRE_METALMAN  "false" to allow an empty result (default true). The
#                     workflow sets it on a first bootstrap, where no Site has
#                     joined yet; on an existing cluster, nothing enabling
#                     metalman is a regression rather than a configuration.
#   NAMESPACE         Only used in messages; the gate scopes the namespace.
#
# Output contract:
#   stdout  zero or more `deploy/metalman-controller-<site>` lines
#   stderr  notices, warnings and errors
#   exit    0 resolved, or empty and not required
#           1 no Site enables metalman while it is required
#           2 usage: missing tooling or KUBECONFIG
#           3 the query or its evaluation failed
#
# Exit 3 is deliberately distinct from exit 1. A malformed payload, an
# unreachable apiserver or a jq error must never read as "no Site enables
# metalman", which is the answer that would quietly drop metalman out of the
# release gate. Anything this script cannot positively determine is a failure.

set -euo pipefail

REQUIRE_METALMAN="${REQUIRE_METALMAN:-true}"

# The CRD is cluster-scoped, so no namespace flag. --request-timeout keeps a
# stuck apiserver from consuming the job's budget, matching the smoke tests.
SITE_RESOURCE="sites.unbounded-cloud.io"
KUBECTL=(kubectl --request-timeout=30s)

# Deployment name pattern, from internal/operator/components/metalman.
TARGET_PREFIX="deploy/metalman-controller-"

# A Site with no components block, no spec, or `metalman: {}` is simply not
# selected; only an explicit `enabled: true` counts.
SITE_FILTER='
  .items[]
  | select(.spec.components.metalman.enabled == true)
  | .metadata.name
'

if [[ -z "${KUBECONFIG:-}" ]]; then
  echo "::error::metalman-targets.sh requires KUBECONFIG" >&2
  exit 2
fi

for tool in kubectl jq; do
  if ! command -v "$tool" >/dev/null 2>&1; then
    echo "::error::metalman-targets.sh requires ${tool} on PATH" >&2
    exit 2
  fi
done

err="$(mktemp)"
trap 'rm -f "$err"' EXIT

if ! sites_json="$("${KUBECTL[@]}" get "$SITE_RESOURCE" -o json 2>"$err")"; then
  echo "::error::could not list ${SITE_RESOURCE}: $(tr -d '\r' <"$err" | tr '\n' ' ')" >&2
  exit 3
fi

if ! sites="$(printf '%s' "$sites_json" | jq -r "$SITE_FILTER" 2>"$err")"; then
  echo "::error::could not evaluate ${SITE_RESOURCE}: $(tr -d '\r' <"$err" | tr '\n' ' ')" >&2
  exit 3
fi

if [[ -z "$sites" ]]; then
  if [[ "$REQUIRE_METALMAN" == "false" ]]; then
    echo "::warning::no Site enables metalman; not gating on it for this run" >&2
    exit 0
  fi

  echo "::error::no Site enables metalman, but this cluster is expected to run it" >&2
  echo "::error::enable it with 'kubectl patch ${SITE_RESOURCE} <site> --type=merge -p '{\"spec\":{\"components\":{\"metalman\":{\"enabled\":true}}}}''" >&2

  exit 1
fi

while read -r site; do
  [[ -n "$site" ]] || continue

  echo "${TARGET_PREFIX}${site}"
done <<<"$sites"
