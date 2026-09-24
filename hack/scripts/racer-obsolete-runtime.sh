#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0

# Read-only inventory for the coordinated stateless dev/test cutover.
# This script never deletes objects. See designs/racer-stateless-cutover.md.
set -euo pipefail

if [[ $# != 2 || -z $1 || -z $2 || $1 == -* || $2 == -* ]]; then
    printf 'Usage: bash %s CONTEXT STATE_NAMESPACE\n' "$0" >&2
    exit 2
fi

kubectl --context="$1" --namespace="$2" get configmaps -o json | jq -r '
  .items[]
  | .metadata as $m
  | ($m.labels // {}) as $labels
  | select(
      (($m.name | test("^racer-v4-(topology|storage)-[0-9a-f]{64}$"))
        and $labels["racer.unbounded-cloud.io/rust-state"] == "pointer")
      or (($m.name | test("^racer-v4-chunk-[0-9a-f]{64}$"))
        and $labels["racer.unbounded-cloud.io/rust-state"] == "chunk")
      or ($m.name == "racer-v4-store-gate"
        and $labels["racer.unbounded-cloud.io/rust-state"] == "gate")
      or (($m.name | test("^racer-pki-[0-9a-f]{16}-[0-9a-f]{64}$"))
        and $labels["racer.unbounded-cloud.io/pki-participants"] == "v4")
      or any($m.ownerReferences[]?;
        .apiVersion == "v1" and .kind == "Pod" and .controller == true
        and (.uid | type) == "string" and (.uid | length) > 0
        and $m.name == ("racer-replica-" + .uid))
    )
  | ["configmap/" + $m.name, $m.uid, $m.resourceVersion]
  | @tsv
'
