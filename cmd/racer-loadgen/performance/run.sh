#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0
set -euo pipefail
umask 022
root=$(git rev-parse --show-toplevel)
out="$root/tmp/racer-performance"
case "${1:-}" in
  build)
    mkdir -p "$out"
    # Use the caller's Go installation and cache settings when supplied. Fresh
    # checkouts need only go on PATH; all default build state is project-local.
    export GOCACHE="${GOCACHE:-$out/go-cache}"
    export GOPATH="${GOPATH:-$out/go-path}"
    export GOMODCACHE="${GOMODCACHE:-$out/go-modcache}"
    export GOTMPDIR="${GOTMPDIR:-$out/go-tmp}"
    export TMPDIR="${TMPDIR:-$out/tmp}"
    export CARGO_TARGET_DIR="$out/cargo"
    mkdir -p "$GOCACHE" "$GOMODCACHE" "$GOTMPDIR" "$TMPDIR"
    cargo build --locked --release -j 2 --manifest-path "$root/cmd/racer-dataplane/Cargo.toml" --bin racer-dataplane
    cargo build --locked --release -j 2 --manifest-path "$root/cmd/racer-loadgen/performance/Cargo.toml"
    "${GOCMD:-go}" test -c -o "$out/loadgen.test" ./cmd/racer-loadgen
    {
      "${GOCMD:-go}" version
      rustc --version
      sha256sum "$out/loadgen.test" "$out/cargo/release/racer-dataplane"
    } >"$out/build-info.txt"
    ;;
  run)
    if pgrep -x 'compile|rustc|cargo|link|docker-buildx' >/dev/null; then
      printf 'Build processes are active; rerun after compilation finishes.\n' >&2
      exit 1
    fi
    run=$(mktemp -d "$out/run-XXXXXX")
    mkdir -p "$run/runtime/racer/gantry/origin" "$run/runtime/racer/gantry/client"
    # Only the child namespace sees this /run. Every backing file is worktree-local.
    sudo -n unshare --mount --fork bash "$root/cmd/racer-loadgen/performance/run.sh" namespace "$run" "${2:-32}" "${3:-0s}" "${4:-0}"
    ;;
  namespace)
    run=$2
    chown -R root:root "$run/runtime"
    chmod -R go-w "$run/runtime"
    mount --make-rprivate /
    mount --bind "$run/runtime" /run
    export RACER_PERF_ROOT="$run" RACER_PERF_BIN="$out/cargo/release/racer-dataplane"
    export RACER_PERF_CONTROL="$out/cargo/release/racer-performance-control"
    export RACER_PERF_CONCURRENCY="${3:-32}" GOMAXPROCS=8 TMPDIR="$run"
    export RACER_PERF_START_SPACING="${4:-0s}"
    export RACER_PERF_BULK_CONTROL="${5:-0}"
    (
      while true; do
        pgrep -a -x 'compile|rustc|cargo|link|docker-buildx' >>"$run/build-overlap.log" || true
        sleep 1
      done
    ) &
    monitor=$!
    trap 'kill "$monitor" 2>/dev/null || true; chown -R -- "'"$(stat -c %u "$root")"':'"$(stat -c %g "$root")"'" "$run"' EXIT
    "$out/loadgen.test" -test.run '^TestRacerMixedPerformance$' -test.v -test.timeout 6m 2>&1 | tee "$run/test.log"
    ;;
  *)
    printf 'Usage: bash cmd/racer-loadgen/performance/run.sh {build|run [bulk-concurrency] [start-spacing] [bulk-control:0|1]}\n' >&2
    exit 2
    ;;
esac
