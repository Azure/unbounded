#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0
#
# Prints one line summarizing the cachemesh state of every gantry pod:
#   <epoch> pods=<n> ready=<n> leaders=<n> members=<distinct values> term=<distinct> seq=<distinct>
# Reads each pod's :9095/metrics through the API server proxy in parallel.
set -euo pipefail

# shellcheck source-path=SCRIPTDIR
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

pods="$(k -n "${GANTRY_NS}" get pods -l app.kubernetes.io/name=gantry \
  --field-selector=status.phase=Running -o name 2>/dev/null | sed 's|pod/||')"
echo "${pods}" | grep . | xargs -P 64 -I{} sh -c \
  "timeout 20 kubectl get --raw /api/v1/namespaces/${GANTRY_NS}/pods/{}:9095/proxy/metrics 2>/dev/null |
   awk '/^gantry_cachemesh_(ready|leading|members|view_term|view_seq) /{v[\$1]=\$2} END{printf \"%s %s %s %s %s %s\\n\", \"{}\", v[\"gantry_cachemesh_ready\"]+0, v[\"gantry_cachemesh_leading\"]+0, v[\"gantry_cachemesh_members\"]+0, v[\"gantry_cachemesh_view_term\"]+0, v[\"gantry_cachemesh_view_seq\"]+0}'" |
  awk -v now="$(date +%s.%N)" '{n++; r+=$2; l+=$3; if($3==1) ld=$1; m[$4]++; t[$5]++; s[$6]++}
    END{printf "%s pods=%d ready=%d leaders=%d leader=%s members=", now, n, r, l, ld;
      for(k in m) printf "%s:%d,", k, m[k]; printf " term="; for(k in t) printf "%s:%d,", k, t[k];
      printf " seq="; for(k in s) printf "%s:%d,", k, s[k]; print ""}'
