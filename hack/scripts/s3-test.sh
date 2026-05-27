#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.
#
# s3-test.sh -- smoke-test the unbounded-s3 daemon end-to-end.
#
# Spins up the binary against an ephemeral one-entry catalog, then
# runs `s3cmd get` against it. The catalog points at a NullBlockStore
# stripe so the GET is expected to fail (no data to serve), but we
# assert the request reached the daemon and that the miss path fired.
# Tears the daemon down on exit, success or failure.
#
# Usage:
#   hack/scripts/s3-test.sh
#
# Env overrides:
#   BIND               (default 127.0.0.1)
#   PORT               (default 8080)
#   READY_TIMEOUT_S    (default 10) seconds to wait for the listen socket; min 1
#   SHUTDOWN_TIMEOUT_S (default 5)  seconds to wait for graceful shutdown; min 1

set -euo pipefail

repo_root="$(cd "$(dirname "$0")/../.." && pwd)"

# ---- Tunables --------------------------------------------------------------

BIND="${BIND:-127.0.0.1}"
PORT="${PORT:-8080}"
READY_TIMEOUT_S="${READY_TIMEOUT_S:-10}"
SHUTDOWN_TIMEOUT_S="${SHUTDOWN_TIMEOUT_S:-5}"

BUCKET="demo"
KEY="helloworld.txt"
STRIPE_HEX="aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
SIZE=7   # "foo bar"

DAEMON_BIN="$repo_root/bin/unbounded-s3"
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

s3cmd_common=(
    --host="$BIND:$PORT"
    --host-bucket="$BIND:$PORT"
    --no-ssl
    --access_key=test
    --secret_key=test
)

# ---- Preflight -------------------------------------------------------------

[[ -x "$DAEMON_BIN" ]] || die 10 \
    "missing executable $DAEMON_BIN; run 'make unbounded-s3-build' first"

command -v s3cmd >/dev/null 2>&1 || die 11 \
    "s3cmd not found in PATH; install it (e.g. 'apt install s3cmd')"

command -v timeout >/dev/null 2>&1 || die 13 \
    "timeout not found in PATH; install coreutils (or set up a substitute)"

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

# ---- Spawn daemon ----------------------------------------------------------

echo "s3-test: starting daemon at $BIND:$PORT"
# `NO_COLOR=1` keeps the log file ANSI-free so the post-run greps
# below can match structured tracing fields (`bucket="demo"`) without
# being broken up by color escape sequences.
NO_COLOR=1 RUST_LOG=info,unbounded_s3=debug \
    "$DAEMON_BIN" \
        --listen "$BIND:$PORT" \
        --catalog "$catalog" \
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

if ! grep -q "listening on" "$LOG_FILE"; then
    echo "s3-test: missing 'listening on' line in daemon log:" >&2
    tail -n 40 "$LOG_FILE" >&2 || true
    exit 1
fi

if ! grep -q "storage backend up: NullBlockStore" "$LOG_FILE"; then
    echo "s3-test: missing NullBlockStore startup line in daemon log:" >&2
    tail -n 40 "$LOG_FILE" >&2 || true
    exit 1
fi

echo "s3-test: daemon is up; log -> $LOG_FILE"

# ---- GET phase -------------------------------------------------------------
#
# With the `?location` handler in place, `s3cmd get` reaches the
# object GET after a successful bucket-region lookup. The daemon's
# wired BlockStore is `NullBlockStore`, so the read stream errors out
# and `s3cmd` reports a non-zero exit. We assert:
#   1. s3cmd exits non-zero (because the data path failed),
#   2. the daemon log shows the GET request line (structured tracing
#      fields: `object request ... bucket="$BUCKET" key="$KEY"`),
#   3. the daemon log shows the BlockStore miss line.

echo
echo "s3-test: GET via 's3cmd get s3://$BUCKET/$KEY -'"
set +e
get_body="$(timeout 10 s3cmd "${s3cmd_common[@]}" get "s3://$BUCKET/$KEY" - 2>"$workdir/get.stderr")"
get_rc=$?
set -e

get_stderr="$(cat "$workdir/get.stderr")"
get_body_bytes="${#get_body}"

echo "s3-test:   s3cmd exit code: $get_rc"
echo "s3-test:   body bytes received: $get_body_bytes"
if [[ -n "$get_stderr" ]]; then
    echo "s3-test:   s3cmd stderr (truncated):"
    head -n 4 "$workdir/get.stderr" | sed 's/^/    /'
fi

if [[ "$get_rc" -eq 0 ]]; then
    die 3 "s3cmd unexpectedly succeeded; NullBlockStore should have produced a failure"
fi

# Anchor on the message body (`object request`) plus the bucket and
# key structured fields, requiring all three to appear on the same
# log line. Each grep stage filters the previous one's lines, so the
# final `-qF` only succeeds when one line contains all three
# substrings. `-F` treats `.` literally so a `.` in a key (e.g.
# `helloworld.txt`) can't match any character via regex semantics.
if ! grep "object request" "$LOG_FILE" \
        | grep -F "bucket=\"$BUCKET\"" \
        | grep -qF "key=\"$KEY\""; then
    echo "s3-test: missing 'object request ... bucket=\"$BUCKET\" ... key=\"$KEY\"' line in daemon log:" >&2
    tail -n 40 "$LOG_FILE" >&2 || true
    exit 4
fi

if ! grep -q "BlockStore miss" "$LOG_FILE"; then
    echo "s3-test: missing 'BlockStore miss' line in daemon log:" >&2
    tail -n 40 "$LOG_FILE" >&2 || true
    exit 5
fi

# ---- Summary ---------------------------------------------------------------

echo
echo "============================================================"
echo "s3-test summary"
echo "============================================================"
echo "Daemon:  started, served bucket ?location, observed real GET,"
echo "         fired BlockStore miss as expected, shut down cleanly"
echo "s3cmd:   exited non-zero on the GET (NullBlockStore miss)"
echo "Daemon log retained at: $LOG_FILE"
echo "------------------------------------------------------------"
echo "Last 20 lines of daemon log:"
tail -n 20 "$LOG_FILE"
echo "============================================================"

exit 0
