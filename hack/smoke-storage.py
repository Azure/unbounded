#!/usr/bin/env python3
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""End-to-end smoke test for the unbounded-storage daemon.

Brings up two `unbounded-storage` processes on loopback, joined into a
two-node Chord ring over TCP RPC (libfabric), each with a file-backed
block device and a frontend whose origin backend points at one shared
stub origin served by this test.

The test then fetches an object through *both* frontends and asserts the
returned body matches the stub origin's payload. Because the object's
single stripe is owned by exactly one of the two nodes, the request to the
non-owning frontend is necessarily routed over the fabric TCP RPC to the
owning node, whose backend issues the outbound GET to the stub origin.
Querying both frontends therefore guarantees the cross-node RPC path is
exercised without reimplementing the stripe-key hashing here.

This whole two-node scenario is run once per protocol pairing so both
frontend/backend implementations are covered against the real fabric:

  - `http`: the plain HTTP frontend backed by the HTTP origin backend.
  - `s3`:   the native S3 frontend backed by the S3 origin backend
            (unsigned/public-bucket mode, path-style `/bucket/key`).

A single `unbounded-storage` process serves exactly one frontend (the
first configured spec wins), so the two kinds cannot share a ring; each
scenario brings up its own fresh two-node ring on new ports and tears it
down before the next.

Pure Python 3 standard library; no pytest. Run directly:

    python3 hack/smoke-storage.py
"""

from __future__ import annotations

import atexit
import http.server
import os
import resource
import signal
import socket
import struct
import subprocess
import sys
import tempfile
import threading
import time
import urllib.error
import urllib.request
from pathlib import Path
from typing import Any

# ============================================================================
# CONFIGURATION CONSTANTS
# ============================================================================

REPO_ROOT = Path(__file__).resolve().parent.parent
TMPDIR = Path(tempfile.mkdtemp(prefix="smoke-storage-"))
BINARY = REPO_ROOT / "bin" / "unbounded-storage"

# The objects served by the stub origin and requested through the
# frontends, one per scenario. The S3 path is path-style (`/bucket/key`)
# because the S3 frontend forwards the request path verbatim to the
# origin as the object id; using a bucket-prefixed key exercises that.
HTTP_OBJECT_PATH = "/smoke-object"
S3_OBJECT_PATH = "/smoke-bucket/smoke-object"
VALID_OBJECTS = frozenset({HTTP_OBJECT_PATH, S3_OBJECT_PATH})

# A 1 GiB object. Built by tiling a fixed pattern so the body is fully
# deterministic and verifiable, but cheap to construct. At this size the
# object spans many stripes (see STRIPE_SIZE), so it can no longer fit in
# a single in-memory buffer-pool: it must be streamed stripe-by-stripe
# through the frontend and cached to disk, exercising the multi-stripe
# read, eviction, and cross-node fetch paths that a single-stripe object
# never touches.
OBJECT_SIZE = 1 << 30  # 1 GiB
# Position-encoded body: every 4096-byte page is filled with its own
# page index as a little-endian u64, repeated. This makes the payload
# fully deterministic *and* self-describing: any reordering, duplication,
# truncation, or cross-stripe mixup in the read path shows up as a page
# whose bytes name a different page, which a periodic/random body would
# hide. Construction is one pass over the 262144 pages, so it stays cheap.
_PAGE = 4096
_PAGES = OBJECT_SIZE // _PAGE
BODY = b"".join(struct.pack("<Q", p) * (_PAGE // 8) for p in range(_PAGES))

STRIPE_SIZE = 4 * 1024 * 1024  # 4 MiB; the 1 GiB object spans 256 stripes
DISK_SIZE = "2G"  # multiple of the 4096-byte page size; holds all stripes of one node

DEVNULL = subprocess.DEVNULL

_procs: list[subprocess.Popen[Any]] = []

# ============================================================================
# LOGGING & UTILITIES
# ============================================================================


def log(msg: str) -> None:
    print(f"==> {msg}", file=sys.stderr)
    sys.stderr.flush()


def die(msg: str) -> None:
    print(f"FAIL: {msg}", file=sys.stderr)
    dump_logs()
    sys.exit(1)


def dump_logs() -> None:
    """Best-effort dump of each spawned process's log on failure."""
    for p in sorted(TMPDIR.glob("*.log")):
        log(f"  --- {p.name} ---")
        try:
            sys.stderr.write(p.read_text())
            sys.stderr.flush()
        except OSError as e:
            log(f"  (failed to read {p.name}: {e})")
    log("  --- end logs ---")


def free_port() -> int:
    """Return a currently-free TCP port on loopback."""
    s = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
    try:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]
    finally:
        s.close()


# ============================================================================
# PROCESS SPAWNING & MONITORING
# ============================================================================


def _forward_lines(stream: Any, log_file: Any) -> None:
    for line in stream:
        log_file.write(line)
        log_file.flush()
        sys.stderr.write(line)
        sys.stderr.flush()


def spawn(args: list[str], log_path: Path) -> subprocess.Popen[Any]:
    """Start a background process, teeing its output to *log_path* and stderr."""
    log_file = open(log_path, "w")  # noqa: SIM115 - intentionally long-lived
    proc = subprocess.Popen(
        args,
        stdout=subprocess.PIPE,
        stderr=subprocess.STDOUT,
        text=True,
        start_new_session=True,
    )
    threading.Thread(
        target=_forward_lines, args=(proc.stdout, log_file), daemon=True
    ).start()
    _procs.append(proc)
    return proc


def check_procs(procs: list[subprocess.Popen[Any]]) -> None:
    """Die if any of *procs* (the current scenario's processes) has exited."""
    for proc in procs:
        ret = proc.poll()
        if ret is not None:
            die(f"storage process {proc.args} exited early with code {ret}")


def terminate(procs: list[subprocess.Popen[Any]]) -> None:
    """Stop a scenario's processes, escalating SIGTERM to SIGKILL."""
    for proc in procs:
        try:
            os.killpg(proc.pid, signal.SIGTERM)
        except OSError:
            pass
    for proc in procs:
        try:
            proc.wait(timeout=5)
        except (OSError, subprocess.TimeoutExpired):
            try:
                os.killpg(proc.pid, signal.SIGKILL)
                proc.wait(timeout=5)
            except (OSError, subprocess.TimeoutExpired):
                pass


# ============================================================================
# CLEANUP
# ============================================================================

_cleaning_up = False


def cleanup() -> None:
    global _cleaning_up
    if _cleaning_up:
        return
    _cleaning_up = True
    log("Cleaning up...")
    terminate(_procs)
    import shutil

    shutil.rmtree(TMPDIR, ignore_errors=True)


def _sigint_handler(sig: int, frame: Any) -> None:
    cleanup()
    sys.exit(1)


# ============================================================================
# STUB ORIGIN HTTP SERVER
# ============================================================================


class _OriginHandler(http.server.BaseHTTPRequestHandler):
    """Serves the in-memory object `BODY` at any path in `VALID_OBJECTS`.

    Implements exactly what the storage stack requires of an origin:
    `HEAD` returns 200 with a Content-Length (the backend issues it to
    fill an object's content-addressed length entry), and ranged `GET`
    returns 206 with a matching Content-Range and body slice (used by the
    backend on a data-stripe cache miss). Both the HTTP and the S3 origin
    backends speak this same plaintext HTTP/1.1 shape; the S3 backend in
    unsigned (public-bucket) mode adds no headers the origin must honor,
    so one handler serves both scenarios.
    """

    protocol_version = "HTTP/1.0"  # close per request, matching Connection: close

    def _path(self) -> str:
        return self.path.split("?", 1)[0]

    def log_message(self, fmt: str, *args: Any) -> None:  # noqa: A002
        log(f"  [origin] {self.command} {self.path}")

    def _record(self, method: str) -> None:
        self.server.requests.append((method, self._path(), self.headers.get("Range")))  # type: ignore[attr-defined]

    def do_HEAD(self) -> None:  # noqa: N802
        self._record("HEAD")
        if self._path() not in VALID_OBJECTS:
            self.send_response(404)
            self.send_header("Content-Length", "0")
            self.end_headers()
            return
        self.send_response(200)
        self.send_header("Content-Length", str(len(BODY)))
        self.send_header("Accept-Ranges", "bytes")
        self.send_header("Connection", "close")
        self.end_headers()

    def do_GET(self) -> None:  # noqa: N802
        self._record("GET")
        if self._path() not in VALID_OBJECTS:
            self.send_response(404)
            self.send_header("Content-Length", "0")
            self.end_headers()
            return
        total = len(BODY)
        rng = self.headers.get("Range")
        if rng:
            # Format: "bytes=start-end" (end inclusive).
            spec = rng.split("=", 1)[1]
            start_s, end_s = spec.split("-", 1)
            start = int(start_s)
            end = int(end_s) if end_s else total - 1
            end = min(end, total - 1)
            chunk = BODY[start : end + 1]
            self.send_response(206)
            self.send_header("Content-Range", f"bytes {start}-{end}/{total}")
            self.send_header("Content-Length", str(len(chunk)))
            self.send_header("Accept-Ranges", "bytes")
            self.send_header("Connection", "close")
            self.end_headers()
            self.wfile.write(chunk)
        else:
            self.send_response(200)
            self.send_header("Content-Length", str(total))
            self.send_header("Accept-Ranges", "bytes")
            self.send_header("Connection", "close")
            self.end_headers()
            self.wfile.write(BODY)


def start_origin(port: int) -> http.server.ThreadingHTTPServer:
    srv = http.server.ThreadingHTTPServer(("127.0.0.1", port), _OriginHandler)
    srv.requests = []  # type: ignore[attr-defined]
    threading.Thread(target=srv.serve_forever, daemon=True).start()
    return srv


# ============================================================================
# CONFIG GENERATION
# ============================================================================


def write_config(
    path: Path,
    *,
    kind: str,
    fabric_addr: str,
    local_id: int,
    peer_id: int,
    peer_addr: str,
    disk_path: Path,
    origin_addr: str,
    frontend_bind: str,
) -> None:
    path.write_text(
        f"""\
[fabric]
listen_addr = "{fabric_addr}"

[storage]
backing_kind = "heap"

[topology]
# Force the libfabric tcp provider even on hosts that expose an RDMA
# HCA in sysfs (e.g. cloud VMs with an mlx5 device but no usable verbs
# provider). This keeps the smoke test on the TCP RPC path.
disable_rdma = true

[p2p]
local_node_id = {local_id}

[[peers]]
id = {peer_id}
transport = "tcp"
address = "{peer_addr}"

[[disks]]
path = "{disk_path}"
kind = "file"
size = "{DISK_SIZE}"
page_size_bytes = 4096
bypass_admission = true
skip_recovery_scan_if_no_meta = true

[[backends]]
id = "origin"
kind = "{kind}"
endpoint = "{origin_addr}"
stripe_size_bytes = {STRIPE_SIZE}

[[frontends]]
id = "fe"
kind = "{kind}"
bind = "{frontend_bind}"
backend = "origin"
"""
    )


# ============================================================================
# READINESS & CLIENT
# ============================================================================


def wait_port(
    host: str, port: int, procs: list[subprocess.Popen[Any]], timeout: int = 60
) -> None:
    """Wait until a TCP connect to host:port succeeds."""
    log(f"  Waiting for {host}:{port} to accept connections...")
    for elapsed in range(timeout):
        check_procs(procs)
        s = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
        s.settimeout(1)
        try:
            s.connect((host, port))
            s.close()
            log(f"  {host}:{port} is accepting")
            return
        except OSError:
            s.close()
        if elapsed > 0 and elapsed % 10 == 0:
            log(f"    ({elapsed}s) still waiting for {host}:{port}")
        time.sleep(1)
    die(f"Timed out waiting for {host}:{port}")


def fetch(
    url: str, procs: list[subprocess.Popen[Any]], timeout: int = 30
) -> tuple[int, bytes]:
    """GET *url*, retrying briefly while the frontend warms up."""
    deadline = time.monotonic() + timeout
    last_err: Exception | None = None
    while time.monotonic() < deadline:
        check_procs(procs)
        try:
            with urllib.request.urlopen(url, timeout=10) as resp:
                return resp.status, resp.read()
        except urllib.error.HTTPError as e:
            return e.code, e.read()
        except (urllib.error.URLError, OSError) as e:
            last_err = e
            time.sleep(1)
    die(f"GET {url} did not succeed within {timeout}s: {last_err}")
    raise AssertionError("unreachable")


# ============================================================================
# SCENARIO
# ============================================================================


def _analyze_corruption(label: str, body: bytes) -> set[int]:
    """Per-page diff of *body* vs BODY; return the set of corrupted stripes.

    Each 4096-byte page of BODY is filled with its own LE-u64 page index, so
    a mismatching page can be decoded back to whichever page's data actually
    landed there. We log a histogram of (got_page - expected_page) deltas and
    the corrupted-stripe set so local-vs-cross-node displacement is visible.
    """
    pages_per_stripe = STRIPE_SIZE // _PAGE
    bad_stripes: set[int] = set()
    deltas: dict[int, int] = {}
    n_bad = 0
    first_bad = -1
    for p in range(min(len(body), len(BODY)) // _PAGE):
        off = p * _PAGE
        exp = BODY[off : off + _PAGE]
        got = body[off : off + _PAGE]
        if got == exp:
            continue
        n_bad += 1
        if first_bad < 0:
            first_bad = p
        bad_stripes.add(p // pages_per_stripe)
        got_page = struct.unpack("<Q", got[:8])[0]
        delta = got_page - p
        deltas[delta] = deltas.get(delta, 0) + 1
    log(f"  [{label}] {n_bad} corrupted pages, first at page {first_bad}")
    log(f"  [{label}] {len(bad_stripes)} corrupted stripes: {sorted(bad_stripes)[:16]}")
    top = sorted(deltas.items(), key=lambda kv: -kv[1])[:8]
    log(f"  [{label}] page-delta histogram (got_page - expected_page): {top}")
    return bad_stripes


def _report_corruption(corrupt: dict[str, set[int]]) -> None:
    if "A" in corrupt and "B" in corrupt:
        both = corrupt["A"] & corrupt["B"]
        only_a = corrupt["A"] - corrupt["B"]
        only_b = corrupt["B"] - corrupt["A"]
        log(f"  stripes corrupt in BOTH A and B: {len(both)} -> {sorted(both)[:16]}")
        log(f"  stripes corrupt only via A: {len(only_a)} -> {sorted(only_a)[:16]}")
        log(f"  stripes corrupt only via B: {len(only_b)} -> {sorted(only_b)[:16]}")


def run_scenario(kind: str, origin_addr: str, object_path: str, origin: Any) -> None:
    """Bring up a fresh two-node ring of frontend/backend *kind* and fetch.

    Spawns its own two `unbounded-storage` processes on new ports, fetches
    *object_path* through both frontends (asserting body and a cross-node
    origin GET), then tears the ring down. *origin* is the shared stub
    origin; its recorded requests are reset so the GET assertion is scoped
    to this scenario.
    """
    log("")
    log(f"=== Scenario: {kind} frontend + {kind} backend ===")

    procs: list[subprocess.Popen[Any]] = []
    fab_a, fab_b = free_port(), free_port()
    fe_a, fe_b = free_port(), free_port()
    log(f"Ports: fabric=({fab_a},{fab_b}) frontends=({fe_a},{fe_b})")

    log("Writing node configs")
    cfg1 = TMPDIR / f"{kind}-node1.toml"
    cfg2 = TMPDIR / f"{kind}-node2.toml"
    write_config(
        cfg1,
        kind=kind,
        fabric_addr=f"127.0.0.1:{fab_a}",
        local_id=1,
        peer_id=2,
        peer_addr=f"127.0.0.1:{fab_b}",
        disk_path=TMPDIR / f"{kind}-node1.disk",
        origin_addr=origin_addr,
        frontend_bind=f"127.0.0.1:{fe_a}",
    )
    write_config(
        cfg2,
        kind=kind,
        fabric_addr=f"127.0.0.1:{fab_b}",
        local_id=2,
        peer_id=1,
        peer_addr=f"127.0.0.1:{fab_a}",
        disk_path=TMPDIR / f"{kind}-node2.disk",
        origin_addr=origin_addr,
        frontend_bind=f"127.0.0.1:{fe_b}",
    )

    try:
        log("Spawning two unbounded-storage processes")
        procs.append(
            spawn(
                [str(BINARY), "--config", str(cfg1), "--no-hugepages"],
                TMPDIR / f"{kind}-node1.log",
            )
        )
        procs.append(
            spawn(
                [str(BINARY), "--config", str(cfg2), "--no-hugepages"],
                TMPDIR / f"{kind}-node2.log",
            )
        )

        wait_port("127.0.0.1", fe_a, procs)
        wait_port("127.0.0.1", fe_b, procs)
        # Give the fabric peers a moment to dial each other before routing.
        log("  Letting fabric peers establish...")
        time.sleep(3)
        check_procs(procs)

        # Scope the origin GET assertion to this scenario's requests.
        origin.requests = []

        corrupt: dict[str, set[int]] = {}
        for label, fe_port in (("A", fe_a), ("B", fe_b)):
            log(f"Fetching object through frontend {label}")
            status, body = fetch(f"http://127.0.0.1:{fe_port}{object_path}", procs)
            if status != 200:
                die(f"frontend {label} returned status {status}, expected 200")
            if body != BODY:
                bad_stripes = _analyze_corruption(label, body)
                corrupt[label] = bad_stripes
            else:
                log(f"  frontend {label} returned correct body")

        if corrupt:
            _report_corruption(corrupt)
            die("body mismatch (see corruption analysis above)")

        # The stub origin must have been hit, proving traffic traversed
        # frontend -> storage stack -> {kind} backend -> origin.
        gets = [
            r for r in origin.requests if r[0] == "GET" and r[1] == object_path
        ]
        if not gets:
            die(
                f"stub origin received no GET for {object_path}; "
                f"the {kind} backend was not exercised"
            )
        log(f"  stub origin served {len(gets)} backend GET(s)")
        log(f"  {kind} scenario PASSED")
    finally:
        log(f"  Tearing down {kind} ring")
        terminate(procs)


# ============================================================================
# MAIN
# ============================================================================


def main() -> None:
    signal.signal(signal.SIGINT, _sigint_handler)
    atexit.register(cleanup)

    if not BINARY.exists():
        die(
            f"{BINARY} not found; build it first with "
            "`make unbounded-storage-build`"
        )

    # io_uring registers fixed buffers; raise the memlock limit so the
    # storage processes (which inherit our limits) can pin their pages.
    try:
        resource.setrlimit(
            resource.RLIMIT_MEMLOCK, (resource.RLIM_INFINITY, resource.RLIM_INFINITY)
        )
    except (ValueError, OSError) as e:
        log(f"  (could not raise RLIMIT_MEMLOCK: {e}; continuing)")

    origin_port = free_port()
    origin_addr = f"127.0.0.1:{origin_port}"

    log(f"Working directory: {TMPDIR}")
    log(f"Stub origin on {origin_addr}")
    log("Starting stub origin HTTP server")
    origin = start_origin(origin_port)

    # Run the full two-node ring once per protocol pairing. Each scenario
    # brings up and tears down its own ring on fresh ports, so the two
    # frontend/backend kinds never share a process (only the first
    # configured frontend is served per process).
    run_scenario("http", origin_addr, HTTP_OBJECT_PATH, origin)
    run_scenario("s3", origin_addr, S3_OBJECT_PATH, origin)

    log("")
    log("Smoke test PASSED")


if __name__ == "__main__":
    main()
