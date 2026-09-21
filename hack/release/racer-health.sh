#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0

# Shared, fail-closed Racer health checks. Source this file; it is not a smoke
# task. API proxy requests exercise the Service and each process from the API
# server, without requiring runner access to cluster Pod/Service IPs.
RACER_LIB_DIR="$(dirname "${BASH_SOURCE[0]}")"

racer_owned_pods() {
  local ns="$1" deployment="$2" pods="$3" replicasets
  replicasets="$(kubectl --request-timeout=30s -n "$ns" get replicasets -o json)" || return 1
  jq -ce --argjson deployment "$deployment" --argjson replicasets "$replicasets" '
    ($deployment.metadata.uid // "") as $uid
    | if $uid == "" then error("Racer Deployment has no UID") else . end
    | [$replicasets.items[]
       | select(any(.metadata.ownerReferences[]?;
           .controller == true and .kind == "Deployment" and .uid == $uid))
       | .metadata.uid] as $owners
    | [.items[] | select(any(.metadata.ownerReferences[]?;
        .controller == true and .kind == "ReplicaSet" and (.uid as $id | $owners | index($id)) != null))]
  ' <<<"$pods"
}

racer_healthy_pods() {
  jq -c -L "$RACER_LIB_DIR" 'include "racer"; map(select(healthy_controller))' <<<"$1"
}

racer_probe_health() {
  local ns="$1" pods="$2" names name
  names="$(jq -r '.[].metadata.name' <<<"$pods")" || return 1
  [[ -n "$names" ]] || return 1
  # /readyz on port 8080 is only served by the elected leader. A Deployment's
  # Available condition or a Lease alone cannot prove this Service works.
  kubectl --request-timeout=30s get --raw "/api/v1/namespaces/${ns}/services/http:racer-controlplane:8080/proxy/readyz" >/dev/null || return 1
  while IFS= read -r name; do
    kubectl --request-timeout=30s get --raw "/api/v1/namespaces/${ns}/pods/http:${name}:8081/proxy/healthz" >/dev/null || return 1
  done <<<"$names"
}
