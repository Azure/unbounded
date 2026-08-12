#!/usr/bin/env bash
# End-to-end immutable-RACER/local-writes demonstration.
set -euo pipefail

DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
POOL=${POOL:-memsnap-pool}
IMAGE=${IMAGE:-docker.io/library/python:3.12-alpine}
COUNT=${COUNT:-3}
NAMESPACE=${NAMESPACE:-memsnap-demo}
SOCKET=/run/memsnap/memsnap.sock
RUNTIME_DIR=/run/memsnap-demo
STATE_DIR=${STATE_DIR:-/var/lib/memsnap-demo}
RACER_STORE=$STATE_DIR/racer.store
MEMSNAP_ROOT=$STATE_DIR/memsnap
RACER_LOG=$RUNTIME_DIR/racer.log
MEMSNAP_LOG=$RUNTIME_DIR/memsnap.log
SN=(--snapshotter memsnap)
CTR=(ctr --namespace "$NAMESPACE")
RACER_PID=
MEMSNAP_PID=
RACER_DEVICE=

if (( EUID != 0 )); then
  exec sudo -E "$0" "$@"
fi

if [[ ${1:-} == --cleanup-only ]]; then
  for i in $(seq 1 "$COUNT"); do
    "${CTR[@]}" task kill -s SIGKILL "memsnap-demo$i" >/dev/null 2>&1 || true
    "${CTR[@]}" task rm -f "memsnap-demo$i" >/dev/null 2>&1 || true
    "${CTR[@]}" container rm "memsnap-demo$i" >/dev/null 2>&1 || true
  done
  "${CTR[@]}" task kill -s SIGKILL memsnap-python >/dev/null 2>&1 || true
  "${CTR[@]}" task rm -f memsnap-python >/dev/null 2>&1 || true
  "${CTR[@]}" container rm memsnap-python >/dev/null 2>&1 || true
  for pid in $(fuser "$SOCKET" 2>/dev/null || true); do kill -TERM "$pid" 2>/dev/null || true; done
  pkill -TERM -f "$DIR/racer/racer serve $DIR/racer/racer.config.pb" 2>/dev/null || true
  exit 0
fi

rule() { printf '\n\033[1m== %s\033[0m\n' "$1"; }

pool_used() {
  dmsetup status "$POOL" | awk '{split($6, blocks, "/"); printf "%.1f MiB", blocks[1] * 64 / 1024}'
}

racer_mutating_io() {
  local device=${RACER_DEVICE##*/}
  # writes completed/sectors, discards completed/sectors, flushes completed
  awk '{print $5 ":" $7 ":" $12 ":" $14 ":" $16}' "/sys/class/block/$device/stat"
}

remove_container() {
  local id=$1
  "${CTR[@]}" task kill -s SIGKILL "$id" >/dev/null 2>&1 || true
  "${CTR[@]}" task rm -f "$id" >/dev/null 2>&1 || true
  "${CTR[@]}" container rm "$id" >/dev/null 2>&1 || true
}

cleanup() {
  local status=$?
  trap - EXIT INT TERM
  set +e

  for i in $(seq 1 "$COUNT"); do
    remove_container "memsnap-demo$i"
  done
  remove_container memsnap-python

  if [[ -n $MEMSNAP_PID ]]; then
    kill -TERM "$MEMSNAP_PID" 2>/dev/null
    wait "$MEMSNAP_PID" 2>/dev/null
  fi
  if [[ -n $RACER_PID ]]; then
    kill -TERM "$RACER_PID" 2>/dev/null
    wait "$RACER_PID" 2>/dev/null
  fi
  rm -rf "$RUNTIME_DIR"
  exit "$status"
}
trap cleanup EXIT INT TERM

require() {
  command -v "$1" >/dev/null || {
    echo "required command not found: $1" >&2
    exit 1
  }
}

for command in blockdev ctr dmsetup go losetup mkfs.ext4; do
  require "$command"
done
[[ -x $DIR/racer/racer ]] || { echo "missing RACER binary: $DIR/racer/racer" >&2; exit 1; }
[[ -r $DIR/racer/racer.config.pb ]] || { echo "missing RACER config: $DIR/racer/racer.config.pb" >&2; exit 1; }

rule "Build"
mkdir -p "$DIR/bin"
go build -o "$DIR/bin/memsnap" "$DIR"
echo "Built $DIR/bin/memsnap"

# Stop an earlier development instance cleanly before replacing its pool.
if [[ -S $SOCKET ]]; then
  for pid in $(fuser "$SOCKET" 2>/dev/null || true); do
    kill -TERM "$pid" 2>/dev/null || true
  done
  for _ in $(seq 1 50); do
    [[ ! -S $SOCKET ]] && break
    sleep 0.1
  done
fi

rule "Start RACER"
rm -rf "$RUNTIME_DIR"
mkdir -p "$RUNTIME_DIR" "$STATE_DIR"
RACER_STORE=$RACER_STORE "$DIR/racer/racer" serve "$DIR/racer/racer.config.pb" >"$RACER_LOG" 2>&1 &
RACER_PID=$!

for _ in $(seq 1 100); do
  RACER_DEVICE=$(awk '/^device 1 -> / {print $4; exit}' "$RACER_LOG" 2>/dev/null || true)
  [[ -n $RACER_DEVICE && -b $RACER_DEVICE ]] && break
  if ! kill -0 "$RACER_PID" 2>/dev/null; then
    echo "RACER exited before exporting a block device:" >&2
    cat "$RACER_LOG" >&2
    exit 1
  fi
  sleep 0.1
done
[[ -n $RACER_DEVICE && -b $RACER_DEVICE ]] || { echo "timed out waiting for RACER block device" >&2; exit 1; }
printf 'RACER device: %s (%s GiB)\n' "$RACER_DEVICE" "$(awk -v bytes="$(blockdev --getsize64 "$RACER_DEVICE")" 'BEGIN {printf "%.1f", bytes / 1073741824}')"

rule "Start memsnap"
"$DIR/bin/memsnap" -device "$RACER_DEVICE" -pool "$POOL" -root "$MEMSNAP_ROOT" >"$MEMSNAP_LOG" 2>&1 &
MEMSNAP_PID=$!
for _ in $(seq 1 100); do
  if [[ -S $SOCKET ]] && "${CTR[@]}" snapshots "${SN[@]}" ls >/dev/null 2>&1; then
    break
  fi
  if ! kill -0 "$MEMSNAP_PID" 2>/dev/null; then
    echo "memsnap exited during startup:" >&2
    cat "$MEMSNAP_LOG" >&2
    exit 1
  fi
  sleep 0.1
done
[[ -S $SOCKET ]] || { echo "timed out waiting for memsnap" >&2; exit 1; }

echo "RACER backs image data; thin metadata and container upper layers are local:"
dmsetup table "$POOL-data" "$POOL" | sed 's/^/  /'
printf '  local state: %s\n' "$MEMSNAP_ROOT"

rule "Download and unpack Python"
pull_start=$(date +%s%N)
if ! "${CTR[@]}" images pull "${SN[@]}" --platform linux/amd64 "$IMAGE" >"$RUNTIME_DIR/pull.log" 2>&1; then
  cat "$RUNTIME_DIR/pull.log" >&2
  exit 1
fi
pull_ms=$(( ($(date +%s%N) - pull_start) / 1000000 ))
printf 'Downloaded and unpacked %s in %d ms\n' "$IMAGE" "$pull_ms"
BASE=$(pool_used)
echo "RACER allocated after image pull: $BASE"
sync
RACER_IO_BASE=$(racer_mutating_io)

rule "Layer chain"
echo "Each image layer is a copy-on-write thin snapshot of the layer below it:"
"${CTR[@]}" snapshots "${SN[@]}" ls | sed 's/^/  /'

rule "Start $COUNT isolated containers"
for i in $(seq 1 "$COUNT"); do
  "${CTR[@]}" run -d "${SN[@]}" "$IMAGE" "memsnap-demo$i" \
    python3 -c 'import time; time.sleep(300)'
done
for i in $(seq 1 "$COUNT"); do
  "${CTR[@]}" task exec --exec-id "check$i" "memsnap-demo$i" python3 -c \
    "from pathlib import Path; p = Path('/tmp/marker'); p.write_text('written by container $i'); print('  container $i reads back:', p.read_text())"
done
AFTER=$(pool_used)
sync
RACER_IO_AFTER=$(racer_mutating_io)
echo "  image, unpacked once:       $BASE"
echo "  after $COUNT container writes: $AFTER"
[[ $RACER_IO_AFTER == "$RACER_IO_BASE" ]] || { echo "container writes issued mutating I/O to RACER" >&2; exit 1; }
echo "  RACER write/discard/flush counters unchanged; writes are under $MEMSNAP_ROOT/snapshots"

rule "Python hello-world latency"
python_start=$(date +%s%N)
python_output=$("${CTR[@]}" run --rm "${SN[@]}" "$IMAGE" memsnap-python \
  python3 -c 'print("Hello from Python with local writes")')
python_ms=$(( ($(date +%s%N) - python_start) / 1000000 ))
echo "  $python_output"
printf '  End-to-end container + Python latency: %d ms\n' "$python_ms"

rule "Teardown"
for i in $(seq 1 "$COUNT"); do
  remove_container "memsnap-demo$i"
done
for _ in $(seq 1 50); do
  active=$(dmsetup ls --target thin 2>/dev/null | awk -v prefix="$POOL-" '$1 ~ "^" prefix {count++} END {print count+0}')
  (( active == 0 )) && break
  sleep 0.1
done
echo "Container volumes remaining: $active"
echo "RACER allocated after containers: $(pool_used)"
echo "Demo completed successfully; durable state remains in $STATE_DIR."
