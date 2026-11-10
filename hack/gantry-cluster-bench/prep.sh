#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0
#
# Write (or remove) /etc/containerd/certs.d/<origin>/hosts.toml on the target
# nodes: gantry mirror 127.0.0.1:5000 first, then the plain-HTTP origin. When
# gantry is not running the mirror dial fails fast (200ms) and containerd
# falls back to the origin, so the gantry image itself can be pulled.
# Idempotent. Runs as a one-shot DaemonSet that is deleted afterwards.
#
# Usage: prep.sh [apply|remove]    (NODES=... limits the node set)
set -euo pipefail

# shellcheck source-path=SCRIPTDIR
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

action="${1:-apply}"
[[ "${action}" == apply || "${action}" == remove ]] || die "usage: prep.sh [apply|remove]"

origin="$(origin_addr)"
out="$(ds_oneshot bench-prep "prep-${action}" "$(env_entry ORIGIN "${origin}")")"
grep -h '^PREP' "${out}"/*.log | sort | uniq -c -f2 | head -5 || true
log "prep ${action}: $(grep -l "^PREP .* ${action/apply/applied}" "${out}"/*.log | wc -l) nodes"
