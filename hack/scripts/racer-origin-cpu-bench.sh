#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0
#
# racer-origin-cpu-bench.sh: local CPU-time (utime+stime) benchmark of cold
# Racer reads served from an Azure Blob origin, comparing
#   before: racer + `racer-object origin --provider azure` (Go, separate UDS
#           origin process), and
#   after:  racer with the native in-process Azure Blob origin.
# Both read a local fake Azure Blob server (hack/cmd/fake-azure-blob). Reads use
# racersdk over /run/racer/gantry/client/socket from hack/cmd/racer-origin-bench,
# which also launches every process and samples /proc/<pid>/stat around the load
# phase only. Each run gets a fresh slab directory in a private mount namespace
# (sudo -n unshare) where /run is a scratch bind mount, as in
# cmd/racer-loadgen/performance/run.sh.
#
# Usage:
#   # Build binaries for one variant from the current checkout into DIR
#   # (racer, racer-performance-control, racer-object, fake-azure-blob,
#   # racer-origin-bench).
#   hack/scripts/racer-origin-cpu-bench.sh build DIR
#
#   # Run the matrix. Resumable: runs whose JSON already exists are skipped.
#   BEFORE_BIN=DIR1 AFTER_BIN=DIR2 hack/scripts/racer-origin-cpu-bench.sh run
#
#   # Print the markdown summary for OUT.
#   hack/scripts/racer-origin-cpu-bench.sh summarize
#
# Knobs (environment, defaults in brackets):
#   BEFORE_BIN, AFTER_BIN  binary directories per variant (from `build`)
#   CONTROL_BIN   directory holding the mode-aware racer-performance-control used
#                 by every variant [$AFTER_BIN, else $BEFORE_BIN]
#   TOOLS_BIN     fake-azure-blob + racer-origin-bench [$BEFORE_BIN or $AFTER_BIN]
#   OUT           output directory [<repo>/tmp/racer-origin-bench/results-default]
#   VARIANTS      ["before after"], alternated and reversed on odd repetitions
#   MODES         ["cached"]; passed to the control as RACER_BENCH_MODE
#                 (cached|passthrough). The control must print mode=<mode>
#                 after its endpoint; the driver fails the run if the line is
#                 missing or differs, so a base control that ignores the mode
#                 is rejected rather than silently benchmarking cached mode
#   SIZES         object bytes ["4194304 67108864"]
#   CONCURRENCY   ["8 32"]
#   TOTAL_BYTES   bytes per run [4294967296]
#   REPS          [5]
#   SLAB_BYTES    RACER_SLAB_BYTES [1073741824]
#   CPUS_RACER, CPUS_ORIGIN, CPUS_FAKE (fake server and load generator)
#                 taskset lists ["0-3", "4-7", "8-15"]
#   ALLOW_BUILDS=1 run even if cargo/go/rustc builds are active (results skew)
#   Any RACER_BENCH_RACER_<NAME>=v overrides racer's RACER_<NAME> setting.
#
# The after variant's control (cmd/racer-loadgen/performance/control.rs) is
# shared by both variants via CONTROL_BIN. Only the after variant is
# given RACER_BENCH_AZURE_CONTAINER_URL=http://127.0.0.1:PORT/devstoreaccount1/bench
# (anonymous credential) and must publish it as the gantry cache's Azure origin;
# without it the publication omits `origin` and stays wire-compatible with the
# base racer. racer (and racer-object for before) come from the variant's BIN.
# The before variant starts racer-object as UID 65532 (the owner racer gives
# /run/racer/gantry/origin) with the invoking user's primary group so it can
# traverse the binary path.
set -euo pipefail
umask 022

root=$(git -C "$(dirname "$0")" rev-parse --show-toplevel)
script="$root/hack/scripts/racer-origin-cpu-bench.sh"
OUT=${OUT:-$root/tmp/racer-origin-bench/results-default}

busy() {
  pgrep -l -x 'compile|rustc|cargo|link|docker-buildx|go' | awk '{print $2}' | sort | uniq -c | xargs echo -n || true
}

case "${1:-}" in
  build)
    dir=${2:?build DIR}
    mkdir -p "$dir"
    dir=$(cd "$dir" && pwd)
    export CARGO_TARGET_DIR=${CARGO_TARGET_DIR:-$root/tmp/racer-origin-bench/cargo}
    timeout -s TERM 1800 cargo build --locked --release -j "${JOBS:-4}" --manifest-path "$root/cmd/racer/Cargo.toml" --bin racer
    timeout -s TERM 1800 cargo build --locked --release -j "${JOBS:-4}" --manifest-path "$root/cmd/racer-loadgen/performance/Cargo.toml"
    cp "$CARGO_TARGET_DIR/release/racer" "$CARGO_TARGET_DIR/release/racer-performance-control" "$dir/"
    for cmd in cmd/racer-object hack/cmd/fake-azure-blob hack/cmd/racer-origin-bench; do
      if [ -d "$root/$cmd" ]; then
        (cd "$root" && timeout -s TERM 600 go build -o "$dir/" "./$cmd")
      fi
    done
    {
      git -C "$root" rev-parse HEAD
      git -C "$root" status --short
      go version
      rustc --version
      sha256sum "$dir"/*
    } >"$dir/build-info.txt" 2>&1 || true
    ls -l "$dir"
    ;;
  run)
    if [ -n "$(busy)" ] && [ "${ALLOW_BUILDS:-0}" != 1 ]; then
      printf 'Build processes are active; rerun after they finish (or ALLOW_BUILDS=1):\n%s\n' "$(busy)" >&2
      exit 1
    fi
    tools=${TOOLS_BIN:-${BEFORE_BIN:-${AFTER_BIN:-}}}
    [ -n "$tools" ] || { echo 'set BEFORE_BIN/AFTER_BIN (and optionally TOOLS_BIN)' >&2; exit 2; }
    control=${CONTROL_BIN:-${AFTER_BIN:-${BEFORE_BIN:-}}}
    mkdir -p "$OUT/results" "$OUT/work"
    reps=${REPS:-5}
    for rep in $(seq 0 $((reps - 1))); do
      variants=${VARIANTS:-before after}
      if [ $((rep % 2)) = 1 ]; then
        # shellcheck disable=SC2086 # word list
        variants=$(printf '%s\n' $variants | tac | xargs)
      fi
      for size in ${SIZES:-4194304 67108864}; do
        for c in ${CONCURRENCY:-8 32}; do
          for mode in ${MODES:-cached}; do
            for variant in $variants; do
              name="$variant-$mode-s$size-c$c-r$rep"
              result="$OUT/results/$name.json"
              if [ -e "$result" ]; then
                echo "skip $name (done)"
                continue
              fi
              if [ -n "$(busy)" ]; then
                printf 'WARNING: build processes active during %s:\n%s\n' "$name" "$(busy)" | tee -a "$OUT/build-overlap.log" >&2
              fi
              bin_var=$(echo "$variant" | tr '[:lower:]' '[:upper:]')_BIN
              bin=${!bin_var:?$bin_var is required for variant $variant}
              work="$OUT/work/$name"
              sudo -n rm -rf "$work"
              mkdir -p "$work/runtime/racer/gantry/origin" "$work/runtime/racer/gantry/client"
              echo "=== $name"
              # sudo resets the environment; forward the knobs explicitly.
              mapfile -t knobs < <(env | grep -E '^(TOTAL_BYTES|SLAB_BYTES|CPUS_[A-Z]+|LOADGEN_GOMAXPROCS|BENCH_ARGS|RACER_BENCH_[A-Z0-9_]+)=' || true)
              if ! timeout -s TERM --kill-after=30s 1800 sudo -n env "${knobs[@]}" unshare --mount --fork bash "$script" namespace \
                "$work" "$result" "$variant" "$mode" "$size" "$c" "$rep" "$bin" "$tools" "$control" "$(id -g)" "$(id -u):$(id -g)"; then
                echo "FAILED $name; logs in $work" >&2
                exit 1
              fi
              # Keep logs, drop slabs (disk is tight).
              rm -rf "$work/slabs"
            done
          done
        done
      done
    done
    "$script" summarize | tee "$OUT/summary.md"
    ;;
  namespace)
    work=$2 result=$3 variant=$4 mode=$5 size=$6 c=$7 rep=$8 bin=$9 tools=${10} control=${11} group=${12} owner=${13}
    trap 'chown -R "$owner" "$work" "$result" 2>/dev/null || true' EXIT
    chown -R root:root "$work/runtime"
    chmod -R go-w "$work/runtime"
    mount --make-rprivate /
    mount --bind "$work/runtime" /run
    # shellcheck disable=SC2086 # BENCH_ARGS is a flag list
    taskset -c "${CPUS_FAKE:-8-15}" env GOMAXPROCS="${LOADGEN_GOMAXPROCS:-8}" "$tools/racer-origin-bench" run \
      -variant "$variant" -mode "$mode" -object-size "$size" -concurrency "$c" -rep "$rep" \
      -total-bytes "${TOTAL_BYTES:-4294967296}" -slab-bytes "${SLAB_BYTES:-1073741824}" \
      -racer "$bin/racer" -control "$control/racer-performance-control" -racer-object "$bin/racer-object" \
      -fake "$tools/fake-azure-blob" -work "$work" -out "$result" -origin-groups "$group" \
      -cpus-racer "${CPUS_RACER:-0-3}" -cpus-origin "${CPUS_ORIGIN:-4-7}" -cpus-fake "${CPUS_FAKE:-8-15}" \
      ${BENCH_ARGS:-} 2>&1 | tee "$work/driver.log"
    exit "${PIPESTATUS[0]}"
    ;;
  summarize)
    tools=${TOOLS_BIN:-${BEFORE_BIN:-${AFTER_BIN:-}}}
    if [ -n "$tools" ] && [ -x "$tools/racer-origin-bench" ]; then
      "$tools/racer-origin-bench" summarize -dir "$OUT/results"
    else
      (cd "$root" && go run ./hack/cmd/racer-origin-bench summarize -dir "$OUT/results")
    fi
    ;;
  *)
    sed -n '4,59p' "$0" >&2
    exit 2
    ;;
esac
