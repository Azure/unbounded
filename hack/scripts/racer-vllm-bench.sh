#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0

# racer-vllm-bench.sh -- Measure racer-vllm weight loads over a matrix of
# loader settings.
#
# Run it inside a vLLM container that has racer-vllm installed and the Racer
# client socket mounted, after racer-vllm-fetch has written the model
# directory. For each combination of concurrency, staging_bytes and use_vmm it
# starts `vllm serve <model-dir> --load-format racer`, waits until every rank
# has logged its `racer: loaded ...` line, stops vLLM, and appends one CSV row
# to $OUT. KV cache setup and CUDA graph capture are not measured.
#
# Usage:
#   racer-vllm-bench.sh [-h] <model-dir> [vllm serve args...]
#
# Environment:
#   TP                  Tensor parallel size (default: 1)
#   RANKS               Load lines to wait for (default: $TP). Use TP x PP with
#                       pipeline parallelism, and double it when a draft model
#                       also loads with racer.
#   CONCURRENCY         Loader concurrency values (default: "2 4 8")
#   STAGING_BYTES       Loader staging_bytes values; K, M and G are binary
#                       (default: "256M 512M 1G")
#   USE_VMM             Loader use_vmm values (default: "true false")
#   EXTRA_CONFIG        JSON object merged into every
#                       --model-loader-extra-config (default: {})
#   REPEAT              Runs per combination (default: 1)
#   WARMUP              Runs of the first combination before the matrix,
#                       recorded with phase=warmup (default: 1)
#   TIMEOUT             Seconds to wait for one load (default: 3600)
#   PAUSE               Seconds to wait after stopping vLLM (default: 5)
#   OUT                 CSV file to append to (default: racer-vllm-bench.csv)
#   LOG_DIR             Per-run logs and samples (default: ${OUT%.csv}-logs)
#   RACER_METRICS_URL   Dataplane metrics URL, such as
#                       http://<dataplane-pod-ip>:9090/metrics. When set, rows
#                       get deltas of dataplane counters.
#   LABEL               Free-form label written to every row
#   RANGE_WINDOW_PAGES  The dataplane's RACER_RANGE_WINDOW_PAGES, written to
#                       every row. A label only; it changes nothing.
#   GPU_STAGING_BYTES   The dataplane's RACER_GPU_STAGING_BYTES, written to
#                       every row. A label only; it changes nothing.
#
# Dataplane settings cannot be changed from the vLLM pod. Change them in the
# racer-settings ConfigMap between invocations and set the labels to match.
#
# Example:
#   TP=4 CONCURRENCY="2 4" RANGE_WINDOW_PAGES=4 \
#     racer-vllm-bench.sh /models/llama --max-model-len 4096

set -euo pipefail

TP="${TP:-1}"
RANKS="${RANKS:-$TP}"
CONCURRENCY="${CONCURRENCY:-2 4 8}"
STAGING_BYTES="${STAGING_BYTES:-256M 512M 1G}"
USE_VMM="${USE_VMM:-true false}"
EXTRA_CONFIG="${EXTRA_CONFIG:-}"
REPEAT="${REPEAT:-1}"
WARMUP="${WARMUP:-1}"
TIMEOUT="${TIMEOUT:-3600}"
PAUSE="${PAUSE:-5}"
OUT="${OUT:-racer-vllm-bench.csv}"
LOG_DIR="${LOG_DIR:-${OUT%.csv}-logs}"
RACER_METRICS_URL="${RACER_METRICS_URL:-}"
LABEL="${LABEL:-}"
RANGE_WINDOW_PAGES="${RANGE_WINDOW_PAGES:-}"
GPU_STAGING_BYTES="${GPU_STAGING_BYTES:-}"

HEADER="started,label,phase,range_window_pages,gpu_staging_bytes,concurrency,staging_bytes,use_vmm,status,wall_s"
HEADER+=",ranks,load_s,total_gib,agg_gibps,min_rank_gibps,max_rank_gibps,reads,gpu_gib,host_gib,stream_gib"
HEADER+=",peak_gpu_mib,peak_host_mib"
HEADER+=",dp_host_reads,dp_host_read_ms,dp_host_first_page_ms,dp_gpu_reads,dp_gpu_read_ms,dp_gpu_first_page_ms"
HEADER+=",dp_gpu_read_refused,dp_gpu_pages_gpu_decrypt,dp_gpu_pages_cpu_decrypt,dp_cufile_staged_pages"
HEADER+=",dp_cufile_failures,dp_gpu_staging_exhausted,log_dir"
METRICS_EMPTY=",,,,,,,,,,,"

VLLM_PID=
SAMPLERS=()

log() { printf '== %s\n' "$*" >&2; }
die() {
    printf 'error: %s\n' "$*" >&2
    exit 1
}

usage() {
    sed -n '/^# Usage:/,/^$/{ /^#/!q; s/^# \{0,1\}//; p; }' "$0"
    exit "${1:-0}"
}

require_tools() {
    for bin in "$@"; do
        command -v "$bin" >/dev/null 2>&1 || die "missing required tool: $bin"
    done
}

# to_bytes 256M -> 268435456. K, M and G are binary; a plain number is bytes.
to_bytes() {
    local v=$1 mult=1
    case $v in
        *[Kk]) mult=$((1 << 10)) v=${v%?} ;;
        *[Mm]) mult=$((1 << 20)) v=${v%?} ;;
        *[Gg]) mult=$((1 << 30)) v=${v%?} ;;
    esac
    if [[ ! $v =~ ^[0-9]+$ ]] || ((10#$v == 0)); then
        die "bad size: $1"
    fi
    echo $((10#$v * mult))
}

check_count() { # name value min
    if [[ ! $2 =~ ^[0-9]+$ ]] || ((10#$2 < $3)); then
        die "$1 must be an integer of at least $3, not '$2'"
    fi
}

# loader_config concurrency staging_bytes use_vmm -> the extra config JSON.
loader_config() {
    python3 - "$EXTRA_CONFIG" "$1" "$2" "$3" <<'EOF'
import json, sys

extra = json.loads(sys.argv[1] or "{}")
if not isinstance(extra, dict):
    sys.exit("EXTRA_CONFIG must be a JSON object")
extra.update(concurrency=int(sys.argv[2]), staging_bytes=int(sys.argv[3]), use_vmm=sys.argv[4] == "true")
print(json.dumps(extra))
EOF
}

scrape() { # file
    python3 - "$RACER_METRICS_URL" >"$1" <<'EOF'
import sys, urllib.request

try:
    with urllib.request.urlopen(sys.argv[1], timeout=30) as r:
        sys.stdout.buffer.write(r.read())
except OSError as e:
    sys.exit(f"warning: cannot read {sys.argv[1]}: {e}")
EOF
}

# parse_summary log -> ranks,load_s,total_gib,agg_gibps,min_rank_gibps,
# max_rank_gibps,reads,gpu_gib,host_gib,stream_gib from the first $RANKS
# "racer: loaded N files, X GiB in Ys (Z GiB/s) from ...; reads by delivery
# mode: gpu=.. (.. GiB) host=.. (.. GiB) stream=.. (.. GiB)" lines. A worker
# loads the draft after its target, so lines with the same pid= add up as one
# rank. Lines without a pid count as ranks of their own.
parse_summary() {
    awk -v want="$RANKS" '
        /racer: loaded / && n < want {
            key = "line" n
            if (match($0, /pid=[0-9]+/)) key = substr($0, RSTART, RLENGTH)
            sub(/.*racer: loaded /, "")
            if ($2 != "files,") next
            s = $6; sub(/s$/, "", s)
            n++
            if (!(key in secs)) ranks++
            secs[key] += s
            gib[key] += $3
            total += $3
            for (i = 8; i < NF; i++) {
                if ($i ~ /^(gpu|host|stream)=[0-9]+$/) {
                    split($i, kv, "=")
                    reads += kv[2]
                    b = $(i + 1); sub(/^\(/, "", b)
                    mode[kv[1]] += b
                }
            }
        }
        END {
            if (n == 0) { print ",,,,,,,,,"; exit }
            first = 1
            for (k in secs) {
                if (secs[k] > load) load = secs[k]
                r = secs[k] > 0 ? gib[k] / secs[k] : 0
                if (first || r < lo) lo = r
                if (r > hi) hi = r
                first = 0
            }
            agg = load > 0 ? total / load : 0
            printf "%d,%.2f,%.2f,%.2f,%.2f,%.2f,%d,%.2f,%.2f,%.2f\n", ranks, load, total, agg, lo, hi, reads, mode["gpu"], mode["host"], mode["stream"]
        }' "$1"
}

# metrics_delta before after -> the dp_* columns. Series are summed over
# their labels, except the ones split by outcome or decrypt below.
metrics_delta() {
    awk '
        function ms(p) { return d[p "_count"] > 0 ? sprintf("%.3f", d[p "_sum"] / d[p "_count"] / 1e6) : "" }
        /^#/ || NF < 2 { next }
        {
            split($1, a, "{")
            n = a[1]
            if (n == "racer_gpu_read_requests_total") n = n (index($1, "outcome=\"gpu\"") ? ":gpu" : ":refused")
            if (n == "racer_gpu_read_pages_total") n = n (index($1, "decrypt=\"gpu\"") ? ":gpu" : ":cpu")
            d[n] += FILENAME == ARGV[1] ? -$2 : $2
        }
        END {
            h = "racer_host_read_"
            g = "racer_gpu_read_"
            printf "%.0f,%s,%s,%.0f,%s,%s,%.0f,%.0f,%.0f,%.0f,%.0f,%.0f\n",
                d[h "request_nanoseconds_count"], ms(h "request_nanoseconds"), ms(h "first_page_nanoseconds"),
                d[g "requests_total:gpu"], ms(g "request_nanoseconds"), ms(g "first_page_nanoseconds"),
                d[g "requests_total:refused"], d[g "pages_total:gpu"], d[g "pages_total:cpu"],
                d["racer_gpu_cufile_staged_pages_total"], d["racer_gpu_cufile_failures_total"],
                d["racer_gpu_staging_exhausted_total"]
        }' "$1" "$2"
}

peak() { # file divisor -> the largest sample divided by divisor, or nothing
    awk -v d="$2" '$1 ~ /^[0-9]+$/ { if ($1 + 0 > m) m = $1 + 0; seen = 1 } END { if (seen) printf "%.0f", m / d }' "$1" 2>/dev/null || true
}

start_samplers() { # dir
    if command -v nvidia-smi >/dev/null 2>&1; then
        nvidia-smi --query-gpu=memory.used --format=csv,noheader,nounits -lms 500 >"$1/gpu-mib.txt" 2>/dev/null &
        SAMPLERS+=("$!")
    fi
    if [[ -n $CGROUP_MEM ]]; then
        while cat "$CGROUP_MEM"; do sleep 0.5; done >"$1/host-bytes.txt" 2>/dev/null &
        SAMPLERS+=("$!")
    fi
}

stop_samplers() {
    local p
    for p in "${SAMPLERS[@]}"; do
        kill "$p" 2>/dev/null || true
        wait "$p" 2>/dev/null || true
    done
    SAMPLERS=()
}

# stop_run asks vLLM to exit, kills whatever is left of its session, and stops
# the samplers. It is also the EXIT trap.
stop_run() {
    local i
    if [[ -n $VLLM_PID ]]; then
        kill -TERM "$VLLM_PID" 2>/dev/null || true
        for ((i = 0; i < 60; i++)); do
            kill -0 "$VLLM_PID" 2>/dev/null || break
            sleep 0.5
        done
        kill -KILL -- "-$VLLM_PID" 2>/dev/null || true
        wait "$VLLM_PID" 2>/dev/null || true
        VLLM_PID=
    fi
    stop_samplers
}

run_one() { # phase concurrency staging_bytes use_vmm
    local phase=$1 conc=$2 staging=$3 vmm=$4
    local cfg started dir t0 t1 n status=ok metrics_ok='' metrics summary wall gpu host
    local ranks load total agg lo hi deadline

    cfg=$(loader_config "$conc" "$staging" "$vmm")
    started=$(date -u +%Y-%m-%dT%H:%M:%SZ)
    dir="$LOG_DIR/$(date -u +%Y%m%dT%H%M%S)-c$conc-s$((staging >> 20))M-vmm-$vmm"
    mkdir -p "$dir"
    log "$phase: concurrency=$conc staging_bytes=$staging use_vmm=$vmm ($dir)"

    if [[ -n $RACER_METRICS_URL ]]; then
        if scrape "$dir/metrics-before.txt"; then
            metrics_ok=1
        else
            log "warning: dp_* columns are empty for this run"
        fi
    fi

    start_samplers "$dir"
    t0=$(date +%s.%N)
    deadline=$((SECONDS + TIMEOUT))
    setsid vllm serve "$MODEL" --load-format racer --model-loader-extra-config "$cfg" \
        --tensor-parallel-size "$TP" "${VLLM_ARGS[@]}" </dev/null >"$dir/vllm.log" 2>&1 &
    VLLM_PID=$!

    while :; do
        n=$(grep -cF 'racer: loaded ' "$dir/vllm.log" || true)
        if ((n >= RANKS)); then
            break
        fi
        if ! kill -0 "$VLLM_PID" 2>/dev/null; then
            status=exited
            break
        fi
        if ((SECONDS >= deadline)); then
            status=timeout
            break
        fi
        sleep 0.5
    done
    t1=$(date +%s.%N)
    # Peaks cover the load only, not the KV cache and graph capture after it.
    stop_samplers

    if [[ -n $metrics_ok ]] && ! scrape "$dir/metrics-after.txt"; then
        log "warning: dp_* columns are empty for this run"
        metrics_ok=
    fi
    stop_run

    wall=$(awk -v a="$t0" -v b="$t1" 'BEGIN { printf "%.1f", b - a }')
    summary=$(parse_summary "$dir/vllm.log")
    gpu=$(peak "$dir/gpu-mib.txt" 1)
    host=$(peak "$dir/host-bytes.txt" 1048576)
    metrics=$METRICS_EMPTY
    if [[ -n $metrics_ok ]]; then
        metrics=$(metrics_delta "$dir/metrics-before.txt" "$dir/metrics-after.txt")
    fi
    printf '%s\n' "$started,$LABEL,$phase,$RANGE_WINDOW_PAGES,$GPU_STAGING_BYTES,$conc,$staging,$vmm,$status,$wall,$summary,$gpu,$host,$metrics,$dir" >>"$OUT"

    IFS=, read -r ranks load total agg lo hi _ <<<"$summary"
    if [[ $status == ok ]]; then
        log "ok after ${wall}s: $total GiB in ${load}s on $ranks ranks, $agg GiB/s aggregate, $lo to $hi GiB/s per rank"
    else
        log "$status after ${wall}s with ${ranks:-0} of $RANKS load lines; end of $dir/vllm.log:"
        tail -n 20 "$dir/vllm.log" >&2 || true
    fi
    sleep "$PAUSE"
}

case ${1:-} in
    -h | --help) usage ;;
    "") usage 2 >&2 ;;
esac
MODEL=$1
shift
VLLM_ARGS=("$@")

require_tools vllm python3 setsid
check_count TP "$TP" 1
check_count RANKS "$RANKS" 1
check_count REPEAT "$REPEAT" 1
check_count WARMUP "$WARMUP" 0
check_count TIMEOUT "$TIMEOUT" 1
check_count PAUSE "$PAUSE" 0
LABEL=${LABEL//,/;}
RANGE_WINDOW_PAGES=${RANGE_WINDOW_PAGES//,/;}
GPU_STAGING_BYTES=${GPU_STAGING_BYTES//,/;}

read -r -a CONCURRENCY_LIST <<<"$CONCURRENCY"
read -r -a STAGING_RAW <<<"$STAGING_BYTES"
read -r -a VMM_LIST <<<"$USE_VMM"
((${#CONCURRENCY_LIST[@]} && ${#STAGING_RAW[@]} && ${#VMM_LIST[@]})) || die "CONCURRENCY, STAGING_BYTES and USE_VMM must not be empty"
for c in "${CONCURRENCY_LIST[@]}"; do
    check_count CONCURRENCY "$c" 1
done
STAGING_LIST=()
for s in "${STAGING_RAW[@]}"; do
    b=$(to_bytes "$s")
    STAGING_LIST+=("$b")
done
for v in "${VMM_LIST[@]}"; do
    [[ $v == true || $v == false ]] || die "USE_VMM values must be true or false, not '$v'"
done
loader_config 1 1 true >/dev/null || die "EXTRA_CONFIG must be a JSON object"

if [[ ! -f $MODEL/racer-model.json ]]; then
    log "warning: no racer-model.json in $MODEL; run racer-vllm-fetch first, or set uri in EXTRA_CONFIG"
fi
if [[ -z $RANGE_WINDOW_PAGES || -z $GPU_STAGING_BYTES ]]; then
    log "warning: RANGE_WINDOW_PAGES or GPU_STAGING_BYTES is not set, so rows do not record the dataplane settings"
fi

CGROUP_MEM=
for f in /sys/fs/cgroup/memory.current /sys/fs/cgroup/memory/memory.usage_in_bytes; do
    if [[ -r $f ]]; then
        CGROUP_MEM=$f
        break
    fi
done

mkdir -p "$LOG_DIR" "$(dirname "$OUT")"
if [[ ! -s $OUT ]]; then
    echo "$HEADER" >"$OUT"
fi

trap stop_run EXIT
trap 'exit 130' INT TERM

log "$((${#CONCURRENCY_LIST[@]} * ${#STAGING_LIST[@]} * ${#VMM_LIST[@]} * REPEAT + WARMUP)) runs; appending to $OUT"
for ((i = 0; i < WARMUP; i++)); do
    run_one warmup "${CONCURRENCY_LIST[0]}" "${STAGING_LIST[0]}" "${VMM_LIST[0]}"
done
for ((r = 0; r < REPEAT; r++)); do
    for c in "${CONCURRENCY_LIST[@]}"; do
        for s in "${STAGING_LIST[@]}"; do
            for v in "${VMM_LIST[@]}"; do
                run_one measure "$c" "$s" "$v"
            done
        done
    done
done
log "done; results in $OUT"
