#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.
#
# s3-test.sh -- smoke-test the unbounded-storage S3 frontend end-to-end.
#
# Brings up a single `unbounded-storage` daemon on loopback configured
# with an `s3` frontend (`kind = "s3"`) over an ephemeral one-entry
# object catalog, then drives it with `curl` to exercise the S3 request
# surface the native frontend serves:
#
#   * HEAD <object>          -> clean 200 with S3 object headers
#   * GET  /<bucket>/?location -> clean 200 with <LocationConstraint>
#   * PUT  <object>          -> 405 Method Not Allowed
#   * GET  <missing key>     -> 404 NoSuchKey
#   * GET  <object>          -> 200 head, then a *truncated* body
#   * GET  <object> (ranged) -> 206 head, then a *truncated* body
#
# The catalog entry points at a stripe key that is not backed by any
# resident data and whose configured HTTP origin is unreachable, so the
# v0 first-byte pool miss applies: the daemon has already written the
# response head (200/206) when the body read fails, so it drops the
# connection. We therefore assert a *truncated* transfer (curl exit 18,
# fewer than `size` body bytes) rather than a clean 5xx, which is the
# agreed v0 behavior for a body-read miss.
#
# Usage:
#   hack/scripts/s3-test.sh
#
# The daemon pins io_uring fixed buffers, so it needs a raised
# RLIMIT_MEMLOCK. This script raises it via `ulimit -l unlimited`, which
# only succeeds for a privileged caller; run under `sudo -E` (preserving
# the libfabric runtime path) on hosts whose default memlock hard limit
# is small, mirroring `hack/smoke-storage.py`.
#
# Env overrides:
#   BIND               (default 127.0.0.1)
#   PORT               (default 8080)  S3 frontend listen port
#   READY_TIMEOUT_S    (default 15) seconds to wait for the listen socket; min 1
#   SHUTDOWN_TIMEOUT_S (default 5)  seconds to wait for graceful shutdown; min 1
#   LIBFABRIC_VERSION  (default 2.5.1) pinned libfabric under tmp/libfabric/<ver>

set -euo pipefail

repo_root="$(cd "$(dirname "$0")/../.." && pwd)"

# ---- Tunables --------------------------------------------------------------

BIND="${BIND:-127.0.0.1}"
PORT="${PORT:-8080}"
READY_TIMEOUT_S="${READY_TIMEOUT_S:-15}"
SHUTDOWN_TIMEOUT_S="${SHUTDOWN_TIMEOUT_S:-5}"

# The storage daemon links libfabric (the `fabric` module loads
# libfabric.so at runtime), so the pinned install must be on the
# runtime search path. Derived from LIBFABRIC_VERSION (see `make
# libfabric`); a missing install is fatal because, unlike the old
# NullBlockStore frontend, this binary always brings up the fabric.
LIBFABRIC_VERSION="${LIBFABRIC_VERSION:-2.5.1}"
LIBFABRIC_LIB="$repo_root/tmp/libfabric/$LIBFABRIC_VERSION/lib"

BUCKET="demo"
KEY="helloworld.txt"
STRIPE_HEX="aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
SIZE=7   # "foo bar"

DAEMON_BIN="$repo_root/bin/unbounded-storage"
LOG_FILE="$repo_root/tmp/s3-test.log"

# ---- Helpers ---------------------------------------------------------------

die() {
    local rc="$1"; shift
    echo "s3-test: error: $*" >&2
    exit "$rc"
}

probe_port() {
    # Returns 0 if TCP connect to $BIND:$PORT succeeds, non-zero otherwise.
    # Uses bash's /dev/tcp redirection (a builtin); avoids depending on nc.
    (exec 3<>"/dev/tcp/$BIND/$PORT") 2>/dev/null
    local rc=$?
    exec 3<&- 2>/dev/null || true
    exec 3>&- 2>/dev/null || true
    return $rc
}

free_port() {
    # Print a currently-free TCP port on $BIND. Uses python3's socket
    # bind-to-0 trick; the kernel hands back an ephemeral port we then
    # immediately release, racy but adequate for a single backend stub
    # address that nothing ever connects to successfully.
    python3 - "$BIND" <<'PY'
import socket, sys
s = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
s.bind((sys.argv[1], 0))
print(s.getsockname()[1])
s.close()
PY
}

# ---- Preflight -------------------------------------------------------------

[[ -x "$DAEMON_BIN" ]] || die 10 \
    "missing executable $DAEMON_BIN; run 'make unbounded-storage-build' first"

command -v curl >/dev/null 2>&1 || die 11 \
    "curl not found in PATH; install it (e.g. 'apt install curl')"

command -v python3 >/dev/null 2>&1 || die 11 \
    "python3 not found in PATH; install it (needed to pick a free port)"

command -v timeout >/dev/null 2>&1 || die 13 \
    "timeout not found in PATH; install coreutils (or set up a substitute)"

[[ -e "$LIBFABRIC_LIB/libfabric.so" ]] || die 15 \
    "pinned libfabric not found at $LIBFABRIC_LIB; run 'make libfabric' first"

# Reject `0` (and anything <1) for the two timeouts. `0` would cause
# `seq 1 0` to emit nothing (no readiness probe at all) and the
# shutdown loop to skip its grace period entirely, both of which are
# silent surprises rather than useful behaviors.
if ! [[ "$READY_TIMEOUT_S" =~ ^[0-9]+$ ]] || (( READY_TIMEOUT_S < 1 )); then
    die 14 "READY_TIMEOUT_S must be a positive integer (got: $READY_TIMEOUT_S)"
fi
if ! [[ "$SHUTDOWN_TIMEOUT_S" =~ ^[0-9]+$ ]] || (( SHUTDOWN_TIMEOUT_S < 1 )); then
    die 14 "SHUTDOWN_TIMEOUT_S must be a positive integer (got: $SHUTDOWN_TIMEOUT_S)"
fi

if probe_port; then
    die 12 "port $BIND:$PORT is already in use; stop the conflicting process or override PORT=<other>"
fi

# io_uring registers fixed buffers, so the daemon needs a raised memlock
# limit. Try to lift the soft limit to the hard limit (and to unlimited
# when privileged); warn but continue if we cannot, so the failure
# surfaces as a daemon bring-up error with its own log rather than an
# opaque preflight abort.
if ! ulimit -l unlimited 2>/dev/null; then
    ulimit -l "$(ulimit -Hl)" 2>/dev/null || true
    echo "s3-test: warning: could not raise RLIMIT_MEMLOCK to unlimited" \
         "(soft=$(ulimit -l)); the daemon may fail to pin io_uring buffers." \
         "Re-run under 'sudo -E' if bring-up fails." >&2
fi

# ---- Workspace + cleanup trap ----------------------------------------------

workdir="$(mktemp -d)"
daemon_pid=""

cleanup() {
    local rc=$?
    if [[ -n "$daemon_pid" ]] && kill -0 "$daemon_pid" 2>/dev/null; then
        kill -TERM "$daemon_pid" 2>/dev/null || true
        # Bounded wait for graceful shutdown.
        local waited=0
        local budget_ms=$(( SHUTDOWN_TIMEOUT_S * 1000 ))
        while kill -0 "$daemon_pid" 2>/dev/null && (( waited < budget_ms )); do
            sleep 0.1
            waited=$(( waited + 100 ))
        done
        if kill -0 "$daemon_pid" 2>/dev/null; then
            echo "s3-test: daemon did not exit within ${SHUTDOWN_TIMEOUT_S}s of SIGTERM; sending SIGKILL" >&2
            kill -KILL "$daemon_pid" 2>/dev/null || true
            wait "$daemon_pid" 2>/dev/null || true
            # Surface a wedged shutdown as a failure if the test would
            # otherwise have passed. If the test already failed, keep
            # the original rc (a real failure dominates a wedge) but
            # warn so the wedge isn't invisible.
            if [[ "$rc" -eq 0 ]]; then
                rc=20
            else
                echo "s3-test: warning: graceful-shutdown wedge masked by prior failure (rc=$rc)" >&2
            fi
        else
            wait "$daemon_pid" 2>/dev/null || true
        fi
    fi
    rm -rf "$workdir"
    # `exit` (not `return`) is required: bash preserves the script's
    # pre-trap exit status if the EXIT trap function just `return`s,
    # so a wedge-only failure (`rc=20`) would otherwise be invisible.
    exit "$rc"
}
trap cleanup EXIT

mkdir -p "$repo_root/tmp"

# ---- Catalog ---------------------------------------------------------------

catalog="$workdir/s3-catalog.yaml"
cat > "$catalog" <<EOF
objects:
  - bucket: $BUCKET
    key: $KEY
    stripe: $STRIPE_HEX
    size: $SIZE
    content_type: text/plain
    last_modified: "1970-01-01T00:00:00Z"
EOF

# ---- Daemon config ---------------------------------------------------------
#
# Single node, no peers. The HTTP backend is required for every frontend
# kind; we point it at an unbound loopback port so a stripe-fetch miss on
# the body read fails to connect, which is exactly the first-byte pool
# miss the truncated-GET assertions below depend on. `disable_rdma`
# forces the libfabric tcp provider so the test does not depend on RDMA
# hardware. A small heap backing keeps the io_uring memlock footprint
# modest.
dead_origin_port="$(free_port)"
config="$workdir/s3.toml"
cat > "$config" <<EOF
[fabric]
listen_addr = "$BIND:0"

[storage]
backing_kind = "heap"

[topology]
disable_rdma = true

[p2p]
local_node_id = 1

[[disks]]
path = "$workdir/s3.disk"
kind = "file"
size = "64M"
page_size_bytes = 4096
bypass_admission = true
skip_recovery_scan_if_no_meta = true

[[backends]]
id = "origin"
kind = "http"
endpoint = "$BIND:$dead_origin_port"
stripe_size_bytes = 65536

[[frontends]]
id = "s3"
kind = "s3"
bind = "$BIND:$PORT"
backend = "origin"
catalog = "$catalog"
EOF

# ---- Spawn daemon ----------------------------------------------------------

echo "s3-test: starting unbounded-storage with an s3 frontend at $BIND:$PORT"
export LD_LIBRARY_PATH="$LIBFABRIC_LIB${LD_LIBRARY_PATH:+:$LD_LIBRARY_PATH}"
"$DAEMON_BIN" \
    --config "$config" \
    --no-hugepages \
    --bytes-per-shard 8M \
    > "$LOG_FILE" 2>&1 &
daemon_pid=$!

# Wait for the listen socket. Configurable via READY_TIMEOUT_S; 100 ms polling.
ready=0
iters=$(( READY_TIMEOUT_S * 10 ))
for _ in $(seq 1 "$iters"); do
    if probe_port; then
        ready=1
        break
    fi
    if ! kill -0 "$daemon_pid" 2>/dev/null; then
        echo "s3-test: daemon exited before opening port" >&2
        tail -n 40 "$LOG_FILE" >&2 || true
        exit 1
    fi
    sleep 0.1
done

if [[ "$ready" -ne 1 ]]; then
    echo "s3-test: timed out waiting for $BIND:$PORT after ${READY_TIMEOUT_S}s" >&2
    tail -n 40 "$LOG_FILE" >&2 || true
    exit 1
fi

# ---- Startup log assertions ------------------------------------------------

if ! grep -q "frontend s3 (s3) driver registered" "$LOG_FILE"; then
    echo "s3-test: missing s3 driver registration line in daemon log:" >&2
    tail -n 40 "$LOG_FILE" >&2 || true
    exit 1
fi

echo "s3-test: daemon is up; log -> $LOG_FILE"

url="http://$BIND:$PORT"

# ---- HEAD: clean 200 with S3 object headers --------------------------------
#
# HEAD never reads the body stripe, so the response is complete and
# curl succeeds. Assert the status line and the content-length the
# catalog size implies.

echo
echo "s3-test: HEAD /$BUCKET/$KEY"
head_out="$(timeout 10 curl -sS -I "$url/$BUCKET/$KEY" 2>"$workdir/head.err")" || \
    die 3 "HEAD request failed: $(cat "$workdir/head.err")"
echo "$head_out" | grep -qi "^HTTP/1.1 200" || {
    echo "$head_out" >&2
    die 3 "HEAD: expected 200, got the above"
}
echo "$head_out" | grep -qi "^content-length: $SIZE" || {
    echo "$head_out" >&2
    die 3 "HEAD: expected 'content-length: $SIZE', got the above"
}
echo "s3-test:   HEAD returned 200 with content-length: $SIZE"

# ---- ?location: clean 200 with <LocationConstraint> ------------------------

echo
echo "s3-test: GET /$BUCKET/?location"
loc_out="$(timeout 10 curl -sS "$url/$BUCKET/?location" 2>"$workdir/loc.err")" || \
    die 3 "?location request failed: $(cat "$workdir/loc.err")"
echo "$loc_out" | grep -q "<LocationConstraint" || {
    echo "$loc_out" >&2
    die 3 "?location: expected <LocationConstraint> body, got the above"
}
echo "s3-test:   ?location returned a LocationConstraint document"

# ---- PUT: 405 Method Not Allowed -------------------------------------------

echo
echo "s3-test: PUT /$BUCKET/$KEY (expect 405)"
put_code="$(timeout 10 curl -sS -o /dev/null -w '%{http_code}' -X PUT "$url/$BUCKET/$KEY" 2>"$workdir/put.err")" || \
    die 3 "PUT request failed: $(cat "$workdir/put.err")"
[[ "$put_code" == "405" ]] || die 3 "PUT: expected 405, got $put_code"
echo "s3-test:   PUT returned 405"

# ---- GET missing key: 404 NoSuchKey ----------------------------------------

echo
echo "s3-test: GET /$BUCKET/does-not-exist (expect 404)"
miss_body="$(timeout 10 curl -sS -w '\n%{http_code}' "$url/$BUCKET/does-not-exist" 2>"$workdir/404.err")" || \
    die 3 "404 request failed: $(cat "$workdir/404.err")"
echo "$miss_body" | tail -n1 | grep -qx "404" || {
    echo "$miss_body" >&2
    die 3 "missing key: expected 404, got the above"
}
echo "$miss_body" | grep -q "<Code>NoSuchKey</Code>" || {
    echo "$miss_body" >&2
    die 3 "missing key: expected NoSuchKey body, got the above"
}
echo "s3-test:   unknown key returned 404 NoSuchKey"

# ---- GET object: 200 head, truncated body (first-byte pool miss) -----------
#
# The catalog stripe is not resident and its origin is unreachable, so
# the body read misses after the 200 head has already been written. The
# daemon drops the connection, so curl reports a truncated transfer
# (exit 18) and writes fewer than `SIZE` bytes. We assert the 200 head
# *and* the truncation, distinguishing the agreed v0 behavior from a
# clean 5xx (which would mean the head was withheld).

echo
echo "s3-test: GET /$BUCKET/$KEY (expect 200 head, truncated body)"
set +e
timeout 10 curl -sS -D "$workdir/get.head" -o "$workdir/get.body" "$url/$BUCKET/$KEY" 2>"$workdir/get.err"
get_rc=$?
set -e
get_head="$(cat "$workdir/get.head" 2>/dev/null || true)"
get_bytes="$(wc -c < "$workdir/get.body" 2>/dev/null || echo 0)"
echo "s3-test:   curl exit=$get_rc body_bytes=$get_bytes"

echo "$get_head" | grep -qi "^HTTP/1.1 200" || {
    echo "$get_head" >&2
    die 4 "GET: expected a 200 response head before truncation, got the above"
}
if [[ "$get_rc" -eq 0 ]]; then
    die 4 "GET unexpectedly completed; a first-byte pool miss must drop the connection (truncated transfer)"
fi
if (( get_bytes >= SIZE )); then
    die 4 "GET returned $get_bytes bytes (>= $SIZE); expected a truncated body from the dropped connection"
fi
echo "s3-test:   GET produced a 200 head then a truncated/dropped body, as expected"

# ---- Ranged GET: 206 head, truncated body ----------------------------------

echo
echo "s3-test: GET /$BUCKET/$KEY Range: bytes=2-4 (expect 206 head, truncated body)"
set +e
timeout 10 curl -sS -r 2-4 -D "$workdir/range.head" -o "$workdir/range.body" "$url/$BUCKET/$KEY" 2>"$workdir/range.err"
range_rc=$?
set -e
range_head="$(cat "$workdir/range.head" 2>/dev/null || true)"
range_bytes="$(wc -c < "$workdir/range.body" 2>/dev/null || echo 0)"
echo "s3-test:   curl exit=$range_rc body_bytes=$range_bytes"

echo "$range_head" | grep -qi "^HTTP/1.1 206" || {
    echo "$range_head" >&2
    die 4 "ranged GET: expected a 206 response head, got the above"
}
echo "$range_head" | grep -qi "^content-range: bytes 2-4/$SIZE" || {
    echo "$range_head" >&2
    die 4 "ranged GET: expected 'content-range: bytes 2-4/$SIZE', got the above"
}
if [[ "$range_rc" -eq 0 ]]; then
    die 4 "ranged GET unexpectedly completed; a first-byte pool miss must drop the connection"
fi
if (( range_bytes >= 3 )); then
    die 4 "ranged GET returned $range_bytes bytes (>= 3); expected a truncated body"
fi
echo "s3-test:   ranged GET produced a 206 head then a truncated/dropped body, as expected"

# ---- Summary ---------------------------------------------------------------

echo
echo "============================================================"
echo "s3-test summary"
echo "============================================================"
echo "Daemon:  unbounded-storage with a native s3 frontend, up and"
echo "         shut down cleanly."
echo "S3 surface:"
echo "  HEAD          -> 200 with content-length: $SIZE"
echo "  ?location     -> 200 LocationConstraint"
echo "  PUT           -> 405 Method Not Allowed"
echo "  GET (missing) -> 404 NoSuchKey"
echo "  GET           -> 200 head, body truncated on first-byte miss"
echo "  GET (range)   -> 206 head, body truncated on first-byte miss"
echo "Daemon log retained at: $LOG_FILE"
echo "------------------------------------------------------------"
echo "Last 20 lines of daemon log:"
tail -n 20 "$LOG_FILE"
echo "============================================================"

exit 0
