#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0
#
# Reset target nodes between benchmark runs: remove bench images pulled from
# the origin, delete gantry-* containerd leases (gantry pins blobs for 60m),
# prune unreferenced content, and report free disk per node.
#
# Usage: clean.sh            (NODES=... limits the node set)
# Env:   DROP_CACHES=1       also drop the page cache (sync; echo 3 >
#                            /proc/sys/vm/drop_caches) so the next run does
#                            not serve blobs from memory.
#        CLEAN_PATTERN=re    image name regex (default ^<origin>/bench/)
#        MIN_FREE_GB=150     warn about nodes with less free disk
set -euo pipefail

# shellcheck source-path=SCRIPTDIR
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

origin="$(origin_addr)"
envy="$(env_entry ORIGIN "${origin}")
$(env_entry DROP_CACHES "${DROP_CACHES:-0}")"
if [[ -n "${CLEAN_PATTERN:-}" ]]; then
  envy+=$'\n'"$(env_entry CLEAN_PATTERN "${CLEAN_PATTERN}")"
fi

out="$(ds_oneshot bench-clean clean "${envy}")"
min=$(( ${MIN_FREE_GB:-150} * 1000000000 ))
awk -v min="${min}" '
  $1 == "CLEAN" && $3 ~ /^remaining_bench_images=/ && $3 != "remaining_bench_images=0" {print "WARN", $0}
  $1 == "CLEAN" && $3 ~ /failed/ {print "WARN", $0}
  $1 == "DISK" {n++; if ($3 < min) print "LOW-DISK", $2, $3 / 1e9 " GB"; if (lo == "" || $3 < lo) lo = $3; tot += $3}
  END {if (n) printf "nodes=%d min_free=%.1fGB avg_free=%.1fGB\n", n, lo / 1e9, tot / n / 1e9}
' "${out}"/*.log
