#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0
set -euo pipefail

# Run from the repository root. Binaries are explicit so the campaign cannot
# silently exercise an unrelated daemon.
: "${RACER_CONTROLPLANE_BINARY:?set the absolute Rust control-plane executable}"
: "${RACER_DATAPLANE_BINARY:?set the absolute production dataplane executable}"
: "${KUBEBUILDER_ASSETS:?set the envtest kube-apiserver/etcd directory}"
: "${TMPDIR:?set workspace-local ext4 scratch space}"
: "${RACER_LIVE_SOCKET_ROOT:?set an existing workspace directory with an absolute path at most 36 bytes}"
export RACER_REQUIRE_LIVE=1
log=$(mktemp "$TMPDIR/racer-live-run-XXXXXX.log")
printf 'Campaign output: %s\n' "$log"
# Select both tests once; a reduced-regression failure must not suppress the
# independent production campaign or require rerunning that campaign twice.
timeout --signal=INT --kill-after=30s 15m \
    go test -mod=readonly -tags=e2e ./e2e/racer-controlplane -run '^(TestColdObjectMultiPeer|TestProductionBinaryCampaign)$' -count=1 -v -timeout=12m "$@" 2>&1 | tee "$log"
