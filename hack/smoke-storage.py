#!/usr/bin/env python3
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""End-to-end smoke test for the unbounded-storage daemon.

Brings up two `unbounded-storage` processes on loopback, joined into a
two-node Chord ring over TCP RPC (libfabric), each with a file-backed
block device and an HTTP frontend whose HTTP backend points at one shared
stub origin served by this test.

The test then fetches an object through *both* frontends and asserts the
returned body matches the stub origin's payload. Because the object's
single stripe is owned by exactly one of the two nodes, the request to the
non-owning frontend is necessarily routed over the fabric TCP RPC to the
owning node, whose HTTP backend issues the outbound GET to the stub
origin. Querying both frontends therefore guarantees the cross-node RPC
path is exercised without reimplementing the stripe-key hashing here.

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

# The object served by the stub origin and requested through the frontends.
OBJECT_PATH = "/smoke-object"
BODY = b"unbounded-storage smoke test payload :: " * 50  # ~2000 bytes, one stripe

STRIPE_SIZE = 65536  # power of two, larger than BODY so the object is one stripe
DISK_SIZE = "64M"  # multiple of the 4096-byte page size

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
    for name in ("node1.log", "node2.log"):
        p = TMPDIR / name
        if not p.exists():
            continue
        log(f"  --- {name} ---")
        try:
            sys.stderr.write(p.read_text())
            sys.stderr.flush()
        except OSError as e:
            log(f"  (failed to read {name}: {e})")
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


def check_procs() -> None:
    """Die if any spawned storage process has exited."""
    for proc in _procs:
        ret = proc.poll()
        if ret is not None:
            die(f"storage process {proc.args} exited early with code {ret}")


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
    for proc in _procs:
        try:
            os.killpg(proc.pid, signal.SIGTERM)
        except OSError:
            pass
    for proc in _procs:
        try:
            proc.wait(timeout=5)
        except (OSError, subprocess.TimeoutExpired):
            try:
                os.killpg(proc.pid, signal.SIGKILL)
                proc.wait(timeout=5)
            except (OSError, subprocess.TimeoutExpired):
                pass
    import shutil

    shutil.rmtree(TMPDIR, ignore_errors=True)


def _sigint_handler(sig: int, frame: Any) -> None:
    cleanup()
    sys.exit(1)


# ============================================================================
# STUB ORIGIN HTTP SERVER
# ============================================================================


class _OriginHandler(http.server.BaseHTTPRequestHandler):
    """Serves the single in-memory object `BODY` at `OBJECT_PATH`.

    Implements exactly what the storage stack requires of an origin:
    `HEAD` returns 200 with a Content-Length (used by the frontend to
    resolve object length), and ranged `GET` returns 206 with a matching
    Content-Range and body slice (used by the HTTP backend on cache miss).
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
        if self._path() != OBJECT_PATH:
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
        if self._path() != OBJECT_PATH:
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
kind = "http"
endpoint = "{origin_addr}"
stripe_size_bytes = {STRIPE_SIZE}

[[frontends]]
id = "fe"
kind = "http"
bind = "{frontend_bind}"
backend = "origin"
"""
    )


# ============================================================================
# READINESS & CLIENT
# ============================================================================


def wait_port(host: str, port: int, timeout: int = 60) -> None:
    """Wait until a TCP connect to host:port succeeds."""
    log(f"  Waiting for {host}:{port} to accept connections...")
    for elapsed in range(timeout):
        check_procs()
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


def fetch(url: str, timeout: int = 30) -> tuple[int, bytes]:
    """GET *url*, retrying briefly while the frontend warms up."""
    deadline = time.monotonic() + timeout
    last_err: Exception | None = None
    while time.monotonic() < deadline:
        check_procs()
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
    fab_a, fab_b = free_port(), free_port()
    fe_a, fe_b = free_port(), free_port()
    origin_addr = f"127.0.0.1:{origin_port}"

    log(f"Working directory: {TMPDIR}")
    log(
        f"Ports: origin={origin_port} fabric=({fab_a},{fab_b}) "
        f"frontends=({fe_a},{fe_b})"
    )

    log("Starting stub origin HTTP server")
    origin = start_origin(origin_port)

    log("Writing node configs")
    cfg1, cfg2 = TMPDIR / "node1.toml", TMPDIR / "node2.toml"
    write_config(
        cfg1,
        fabric_addr=f"127.0.0.1:{fab_a}",
        local_id=1,
        peer_id=2,
        peer_addr=f"127.0.0.1:{fab_b}",
        disk_path=TMPDIR / "node1.disk",
        origin_addr=origin_addr,
        frontend_bind=f"127.0.0.1:{fe_a}",
    )
    write_config(
        cfg2,
        fabric_addr=f"127.0.0.1:{fab_b}",
        local_id=2,
        peer_id=1,
        peer_addr=f"127.0.0.1:{fab_a}",
        disk_path=TMPDIR / "node2.disk",
        origin_addr=origin_addr,
        frontend_bind=f"127.0.0.1:{fe_b}",
    )

    log("Spawning two unbounded-storage processes")
    spawn([str(BINARY), "--config", str(cfg1), "--no-hugepages"], TMPDIR / "node1.log")
    spawn([str(BINARY), "--config", str(cfg2), "--no-hugepages"], TMPDIR / "node2.log")

    wait_port("127.0.0.1", fe_a)
    wait_port("127.0.0.1", fe_b)
    # Give the fabric peers a moment to dial each other before routing.
    log("  Letting fabric peers establish...")
    time.sleep(3)
    check_procs()

    log("Fetching object through frontend A")
    status_a, body_a = fetch(f"http://127.0.0.1:{fe_a}{OBJECT_PATH}")
    if status_a != 200:
        die(f"frontend A returned status {status_a}, expected 200")
    if body_a != BODY:
        die(
            f"frontend A body mismatch: got {len(body_a)} bytes, "
            f"expected {len(BODY)}"
        )
    log("  frontend A returned correct body")

    log("Fetching object through frontend B")
    status_b, body_b = fetch(f"http://127.0.0.1:{fe_b}{OBJECT_PATH}")
    if status_b != 200:
        die(f"frontend B returned status {status_b}, expected 200")
    if body_b != BODY:
        die(
            f"frontend B body mismatch: got {len(body_b)} bytes, "
            f"expected {len(BODY)}"
        )
    log("  frontend B returned correct body")

    # The stub origin must have been hit, proving traffic traversed
    # frontend -> storage stack -> HTTP backend -> origin.
    gets = [r for r in origin.requests if r[0] == "GET" and r[1] == OBJECT_PATH]  # type: ignore[attr-defined]
    if not gets:
        die("stub origin received no GET for the object; backend was not exercised")
    log(f"  stub origin served {len(gets)} backend GET(s)")

    log("")
    log("Smoke test PASSED")


if __name__ == "__main__":
    main()
