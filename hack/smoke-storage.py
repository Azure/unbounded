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

    sudo python3 hack/smoke-storage.py

Root is required: the harness reserves 2 MiB hugepages on the host (the
daemon's default shard backing) and raises RLIMIT_MEMLOCK so the storage
processes can pin their io_uring buffers.

By default the two `unbounded-storage` processes per scenario are spawned
directly as child processes (the local-development path, unchanged).
"""

from __future__ import annotations

import atexit
import http.server
import os
import resource
import shutil
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
BINARY = Path(
    os.environ.get("SMOKE_STORAGE_BINARY", str(REPO_ROOT / "bin" / "unbounded-storage"))
)

USE_SYSTEMD = os.environ.get("SMOKE_STORAGE_SYSTEMD", "0") == "1"
INSTALL_SCRIPT = REPO_ROOT / "hack" / "scripts" / "install-unbounded-storage.sh"
STORAGE_PREFIX = os.environ.get("SMOKE_STORAGE_PREFIX", "/opt/unbounded-storage")
STORAGE_TARBALL = os.environ.get("SMOKE_STORAGE_TARBALL", "")

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
# plain bytes; 2 GiB, multiple of the 4096-byte page size; holds all stripes of one node
DISK_SIZE = 2 * 1024 * 1024 * 1024

# Hugepage backing. The daemon defaults to `backing_kind = "hugepage2_mb"`,
# so the smoke test exercises that real path by reserving 2 MiB hugepages on
# the host up front (rather than passing `--no-hugepages` to fall back to the
# heap). `memory_total_bytes` is pinned so the reservation below is exact.
HUGEPAGE_SIZE = 2 * 1024 * 1024  # 2 MiB; matches memory::HUGEPAGE_2MB
MEMORY_TOTAL_BYTES = 128 * 1024 * 1024  # matches StorageCfg default; pinned for exactness
RPC_SCRATCH_PAGES = 8  # matches main.rs RPC_SCRATCH_PAGES
NODES_PER_SCENARIO = 2

# Hugepages the per-node pool backing needs: the whole memory_total_bytes
# pool (split across the node's serving shards) plus scratch, each rounded up.
_HP_PER_SHARD = (MEMORY_TOTAL_BYTES + HUGEPAGE_SIZE - 1) // HUGEPAGE_SIZE + RPC_SCRATCH_PAGES
# Total for a scenario's two concurrent nodes, plus 50% headroom for any
# allocator rounding / transient double-counting during teardown overlap.
HUGEPAGES_NEEDED = _HP_PER_SHARD * NODES_PER_SCENARIO
HUGEPAGES_RESERVE = HUGEPAGES_NEEDED + HUGEPAGES_NEEDED // 2

NR_HUGEPAGES_PATH = Path("/sys/kernel/mm/hugepages/hugepages-2048kB/nr_hugepages")
FREE_HUGEPAGES_PATH = Path("/sys/kernel/mm/hugepages/hugepages-2048kB/free_hugepages")

DEVNULL = subprocess.DEVNULL

# Every node brought up across all scenarios, for global teardown/log dump.
_nodes: list[_Node] = []

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
    """Best-effort dump of each node's log on failure."""
    for node in _nodes:
        node.dump_log()
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
# NODE MANAGEMENT (subprocess or systemd)
# ============================================================================


class _Node:
    """A single running storage node.

    Two implementations back this interface: `_LocalNode` spawns the binary
    directly as a child process (default, local development), and
    `_SystemdNode` installs and runs it as a systemd unit through the
    installer script (CI). The rest of the harness only uses this interface.
    """

    label: str

    def stop(self) -> None:
        raise NotImplementedError

    def dump_log(self) -> None:
        raise NotImplementedError


def _forward_lines(stream: Any, log_file: Any) -> None:
    for line in stream:
        log_file.write(line)
        log_file.flush()
        sys.stderr.write(line)
        sys.stderr.flush()


class _LocalNode(_Node):
    """A node spawned as a direct child process, teed to a log file + stderr."""

    def __init__(self, args: list[str], log_path: Path) -> None:
        self.label = f"storage process {args}"
        self.log_path = log_path
        self._stopped = False
        # Owned by this node so stop() can close it; the forwarding thread
        # writes to it for the life of the process.
        self.log_file = open(log_path, "w")  # noqa: SIM115 - intentionally long-lived
        self.proc: subprocess.Popen[Any] = subprocess.Popen(
            args,
            stdout=subprocess.PIPE,
            stderr=subprocess.STDOUT,
            text=True,
            start_new_session=True,
        )
        threading.Thread(
            target=_forward_lines, args=(self.proc.stdout, self.log_file), daemon=True
        ).start()

    def stop(self) -> None:
        # Idempotent: the scenario's `finally` and the atexit `cleanup` both
        # tear nodes down, so stop() can be called more than once per node.
        if self._stopped:
            return
        self._stopped = True
        try:
            os.killpg(self.proc.pid, signal.SIGTERM)
        except OSError:
            pass
        try:
            self.proc.wait(timeout=5)
        except (OSError, subprocess.TimeoutExpired):
            try:
                os.killpg(self.proc.pid, signal.SIGKILL)
                self.proc.wait(timeout=5)
            except (OSError, subprocess.TimeoutExpired):
                pass
        # The process is gone (or unkillable); the forwarding thread is a
        # daemon reading a now-closed pipe, so flush best-effort and release
        # the log file handle.
        try:
            self.log_file.flush()
            os.fsync(self.log_file.fileno())
        except OSError:
            pass
        try:
            self.log_file.close()
        except OSError:
            pass

    def dump_log(self) -> None:
        log(f"  --- {self.log_path.name} ---")
        try:
            sys.stderr.write(self.log_path.read_text())
            sys.stderr.flush()
        except OSError as e:
            log(f"  (failed to read {self.log_path.name}: {e})")


class _SystemdNode(_Node):
    """A node installed and run as a systemd unit via the installer script.

    Installing the unit (and starting it via `systemctl enable --now`) is the
    installer's job; this class drives the same script CI ships, then manages
    the resulting unit's teardown and log capture. Output goes to the journal,
    so `dump_log` shells out to journalctl.
    """

    def __init__(self, unit: str, cfg: Path) -> None:
        self.unit = unit
        self.label = f"systemd unit {unit}"
        env = dict(os.environ)
        env.update(
            {
                "SERVICE_NAME": unit,
                # The installer puts `--config <CONFIG_PATH>` on the ExecStart
                # line itself, so point it at this node's config rather than
                # passing a second `--config` via STORAGE_ARGS (which the
                # daemon rejects as a repeated argument).
                "CONFIG_PATH": str(cfg),
                "LOCAL_TARBALL": STORAGE_TARBALL,
                "VERSION": "local",
                # Give each node its own prefix so concurrent installs do not
                # race on a shared releases/ dir and `current` symlink (each
                # install does rm -rf + recreate of that directory).
                "PREFIX": f"{STORAGE_PREFIX}/{unit}",
            }
        )
        log(f"  Installing {unit} via {INSTALL_SCRIPT.name}")
        try:
            subprocess.run(["bash", str(INSTALL_SCRIPT)], env=env, check=True)
        except subprocess.CalledProcessError as e:
            die(f"installer failed for {unit} (exit {e.returncode})")

    def stop(self) -> None:
        # Stop, disable, and remove the transient unit so repeated/local runs
        # do not accumulate leftover services. The journal is retained, so
        # dump_log still works after removal.
        subprocess.run(
            ["systemctl", "stop", self.unit],
            stdout=DEVNULL,
            stderr=DEVNULL,
        )
        subprocess.run(
            ["systemctl", "disable", self.unit],
            stdout=DEVNULL,
            stderr=DEVNULL,
        )
        unit_path = f"/etc/systemd/system/{self.unit}.service"
        try:
            os.remove(unit_path)
        except FileNotFoundError:
            # Already removed (or never written); nothing to clean up.
            pass
        except OSError as e:
            log(f"  (failed to remove unit file {unit_path}: {e})")
        subprocess.run(
            ["systemctl", "daemon-reload"],
            stdout=DEVNULL,
            stderr=DEVNULL,
        )

    def dump_log(self) -> None:
        log(f"  --- journalctl -u {self.unit} ---")
        subprocess.run(
            ["journalctl", "--no-pager", "-u", self.unit],
            stdout=sys.stderr,
            stderr=sys.stderr,
        )


def start_node(kind: str, idx: int, cfg: Path, log_path: Path) -> _Node:
    """Bring up node *idx* of scenario *kind*, registering it for teardown.

    Dispatches to systemd or a direct subprocess based on USE_SYSTEMD; both
    run `unbounded-storage --config <cfg>`, letting the config's hugepage
    backing take effect (the harness reserves the hugepages up front).
    """
    if USE_SYSTEMD:
        node: _Node = _SystemdNode(f"unbounded-storage-smoke-{kind}-{idx}", cfg)
    else:
        node = _LocalNode([str(BINARY), "--config", str(cfg)], log_path)
    _nodes.append(node)
    return node


def terminate(nodes: list[_Node]) -> None:
    """Stop a scenario's nodes."""
    for node in nodes:
        node.stop()


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
    terminate(_nodes)
    restore_hugepages()
    shutil.rmtree(TMPDIR, ignore_errors=True)


def _sigint_handler(sig: int, frame: Any) -> None:
    cleanup()
    sys.exit(1)


# ============================================================================
# HUGEPAGE RESERVATION
# ============================================================================
_orig_nr_hugepages: int | None = None


def _read_int(path: Path) -> int | None:
    try:
        return int(path.read_text().strip())
    except (OSError, ValueError):
        return None


def reserve_hugepages() -> None:
    """Ensure at least `HUGEPAGES_RESERVE` free 2 MiB hugepages.

    The daemon backs its shards with 2 MiB hugepages (`backing_kind =
    "hugepage2_mb"`), so reserve them on the host before any storage process
    starts. Reads the current `nr_hugepages` (saved for restore), bumps the
    pool if needed, then asks the kernel to compact memory and re-checks the
    free count. Dies with an actionable message if the host cannot back the
    pages, since the storage processes would otherwise fail their hugetlb
    mmap one by one with a less obvious error.
    """
    global _orig_nr_hugepages

    if not NR_HUGEPAGES_PATH.exists():
        die(
            "host does not expose 2 MiB hugepages "
            f"({NR_HUGEPAGES_PATH} missing); cannot run the hugepage smoke test"
        )

    current = _read_int(NR_HUGEPAGES_PATH)
    if current is None:
        die(f"could not read {NR_HUGEPAGES_PATH}")
    _orig_nr_hugepages = current

    free = _read_int(FREE_HUGEPAGES_PATH) or 0
    log(
        f"Hugepages: need {HUGEPAGES_NEEDED} (reserving {HUGEPAGES_RESERVE}); "
        f"host has nr={current} free={free}"
    )
    if free >= HUGEPAGES_NEEDED:
        log("  enough free hugepages already; leaving the pool as-is")
        # Nothing to restore: we did not change the pool.
        _orig_nr_hugepages = None
        return

    target = max(current, HUGEPAGES_RESERVE)
    log(f"  raising nr_hugepages {current} -> {target}")
    try:
        NR_HUGEPAGES_PATH.write_text(f"{target}\n")
    except OSError as e:
        die(
            f"failed to set nr_hugepages={target} ({e}); "
            "run under sudo so the harness can reserve hugepages"
        )

    # The kernel may not satisfy the full request from fragmented memory on
    # the first try. Nudge it with a compaction pass and re-read.
    free = _read_int(FREE_HUGEPAGES_PATH) or 0
    if free < HUGEPAGES_NEEDED:
        compact = Path("/proc/sys/vm/compact_memory")
        if compact.exists():
            try:
                compact.write_text("1\n")
                time.sleep(1)
            except OSError as e:
                # Compaction is a best-effort nudge; if it fails we still fall
                # through to the retry write and the final free-page check.
                log(f"  (memory compaction failed, continuing: {e})")
            NR_HUGEPAGES_PATH.write_text(f"{target}\n")
            free = _read_int(FREE_HUGEPAGES_PATH) or 0

    got = _read_int(NR_HUGEPAGES_PATH) or 0
    log(f"  nr_hugepages now {got}, free {free}")
    if free < HUGEPAGES_NEEDED:
        die(
            f"only {free} free 2 MiB hugepages after reserving (need "
            f"{HUGEPAGES_NEEDED}); host memory may be too fragmented. "
            "Free memory or reserve hugepages at boot, then retry."
        )


def restore_hugepages() -> None:
    """Restore `nr_hugepages` to the value captured by `reserve_hugepages`.

    Only reads `_orig_nr_hugepages`; `cleanup()` is guarded by `_cleaning_up`
    so this runs at most once per process, and a no-op when the pool was left
    untouched (`_orig_nr_hugepages is None`).
    """
    if _orig_nr_hugepages is None:
        return
    try:
        NR_HUGEPAGES_PATH.write_text(f"{_orig_nr_hugepages}\n")
        log(f"Restored nr_hugepages -> {_orig_nr_hugepages}")
    except OSError as e:
        log(f"  (could not restore nr_hugepages to {_orig_nr_hugepages}: {e})")


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
    local_id: int,
    peer_id: int,
    peer_addr: str,
    disk_path: Path,
    origin_addr: str,
    frontend_addr: str,
    fabric_addr: str,
    metrics_addr: str,
) -> None:
    # The config schema is proto3-native: byte sizes are plain integer
    # byte counts (see api/unbounded-storage/config.proto).
    # Backend and frontend implementations are selected by the oneof
    # config table name.
    #
    # Startup-fixed knobs live in the `[startup]` section of the config:
    # the fabric bind address, the per-shard hugepage backing size
    # (memory_total_bytes, leaving the daemon's hugepage default in place), and
    # forcing the libfabric tcp provider (disable_rdma) even on hosts that
    # expose an unusable RDMA HCA in sysfs. They only take effect at process
    # start and are intentionally not part of the dynamic reload path.
    path.write_text(
        f"""\
[[backends]]
name = "origin"

[backends.config.{kind}]
url = "{origin_addr}"
stripe_size_bytes = {STRIPE_SIZE}

[[keyspaces]]
name = "objects"

[[keyspaces.routes]]
key_prefix = "/"
backend = "origin"
origin_prefix = "/"

[[neighborhoods]]
name = "p2p"
source = "objects"
local_node_id = {local_id}

[[neighborhoods.peers]]
id = {peer_id}

[neighborhoods.peers.config.tcp]
addr = "{peer_addr}"

[[caches]]
name = "cache"
source = "p2p"

[[caches.disks]]
page_size_bytes = 4096
skip_recovery_scan = true

[caches.disks.config.file]
path = "{disk_path}"
size = {DISK_SIZE}

[[frontends]]
name = "fe"

[[frontends.mounts]]
public_prefix = "/"
source = "cache"
key_prefix = "/"

[frontends.config.{kind}]
addr = "{frontend_addr}"

[startup.memory]
# Back shards with 2 MiB hugepages (the daemon default, so no_hugepages is
# left unset) and exercise the real hugetlb path. The harness reserves these
# on the host before any node starts; memory_total_bytes is pinned to match that
# reservation.
memory_total_bytes = {MEMORY_TOTAL_BYTES}

[startup.fabric.binds.tcp]
addr = "{fabric_addr}"

[startup.metrics]
# Expose the Prometheus exporter on a dedicated control-plane port so the
# smoke test can scrape /metrics and assert request counters advanced.
addr = "{metrics_addr}"

[startup.topology]
disable_rdma = true
# Cap the smoke test at two serving shards so it exercises the shared
# per-node endpoint with more than one shard. A node advertises one static
# fabric address to its peers, so it binds exactly one inbound
# fabric endpoint on that fixed port; `plan_fabric_units` maps every serving
# shard onto that single shared endpoint (per-node for tcp, per-HCA for
# verbs). Binding one endpoint per shard instead would make every shard past
# the first collide on the port (`fi_endpoint` -> EADDRINUSE). The cap is a
# ceiling, not a floor: a runner with only one usable serving core degrades
# to a single shard and still passes.
serving_cores = 2
"""
    )


# ============================================================================
# READINESS & CLIENT
# ============================================================================


def wait_port(host: str, port: int, timeout: int = 60) -> None:
    """Wait until a TCP connect to host:port succeeds."""
    log(f"  Waiting for {host}:{port} to accept connections...")
    for elapsed in range(timeout):
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


def scrape_metric_sum(url: str, name: str, timeout: int = 30) -> float:
    """Scrape Prometheus text from *url* and sum all samples of *name*.

    Sums across every label-set series whose metric name matches *name*
    (ignoring comment/HELP/TYPE lines), so a counter split across labels
    like `frontend_requests_total{frontend="fe",method="GET",status="200"}`
    is totaled. Dies if the endpoint never responds.
    """
    status, body = fetch(url, timeout)
    if status != 200:
        die(f"GET {url} returned status {status}, expected 200")
    total = 0.0
    text = body.decode("utf-8", "replace")
    for line in text.splitlines():
        line = line.strip()
        if not line or line.startswith("#"):
            continue
        # "<name>{labels} <value>" or "<name> <value>"
        head, _, value = line.rpartition(" ")
        metric = head.split("{", 1)[0]
        if metric == name:
            try:
                total += float(value)
            except ValueError:
                continue
    return total


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

    nodes: list[_Node] = []
    fab_a, fab_b = free_port(), free_port()
    fe_a, fe_b = free_port(), free_port()
    met_a, met_b = free_port(), free_port()
    log(f"Ports: fabric=({fab_a},{fab_b}) frontends=({fe_a},{fe_b}) metrics=({met_a},{met_b})")

    log("Writing node configs")
    cfg1 = TMPDIR / f"{kind}-node1.toml"
    cfg2 = TMPDIR / f"{kind}-node2.toml"
    write_config(
        cfg1,
        kind=kind,
        local_id=1,
        peer_id=2,
        peer_addr=f"127.0.0.1:{fab_b}",
        disk_path=TMPDIR / f"{kind}-node1.disk",
        origin_addr=origin_addr,
        frontend_addr=f"127.0.0.1:{fe_a}",
        fabric_addr=f"127.0.0.1:{fab_a}",
        metrics_addr=f"127.0.0.1:{met_a}",
    )
    write_config(
        cfg2,
        kind=kind,
        local_id=2,
        peer_id=1,
        peer_addr=f"127.0.0.1:{fab_a}",
        disk_path=TMPDIR / f"{kind}-node2.disk",
        origin_addr=origin_addr,
        frontend_addr=f"127.0.0.1:{fe_b}",
        fabric_addr=f"127.0.0.1:{fab_b}",
        metrics_addr=f"127.0.0.1:{met_b}",
    )

    try:
        log("Bringing up two unbounded-storage nodes")
        nodes.append(start_node(kind, 1, cfg1, TMPDIR / f"{kind}-node1.log"))
        nodes.append(start_node(kind, 2, cfg2, TMPDIR / f"{kind}-node2.log"))

        wait_port("127.0.0.1", fe_a)
        wait_port("127.0.0.1", fe_b)
        # Give the fabric peers a moment to dial each other before routing.
        log("  Letting fabric peers establish...")
        time.sleep(3)

        # Scope the origin GET assertion to this scenario's requests.
        origin.requests = []

        corrupt: dict[str, set[int]] = {}
        for label, fe_port in (("A", fe_a), ("B", fe_b)):
            log(f"Fetching object through frontend {label}")
            status, body = fetch(f"http://127.0.0.1:{fe_port}{object_path}")
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
        gets = [r for r in origin.requests if r[0] == "GET" and r[1] == object_path]
        if not gets:
            die(
                f"stub origin received no GET for {object_path}; "
                f"the {kind} backend was not exercised"
            )
        log(f"  stub origin served {len(gets)} backend GET(s)")

        # Scrape each node's Prometheus exporter and assert the frontend
        # request counter advanced. Each frontend served exactly one direct
        # GET above, so its node's exporter must report at least one
        # frontend_requests_total sample with a non-zero total. This proves
        # the metrics subsystem (registry, instrumentation, and the
        # dedicated std::net exporter thread) is wired end to end in the
        # real binary.
        metric = "unbounded_storage_frontend_requests_total"
        for label, met_port in (("A", met_a), ("B", met_b)):
            url = f"http://127.0.0.1:{met_port}/metrics"
            log(f"Scraping metrics from node {label} ({url})")
            count = scrape_metric_sum(url, metric)
            if count < 1:
                die(
                    f"node {label} reported {metric}={count}, expected >= 1; "
                    "metrics exporter not wired correctly"
                )
            log(f"  node {label} reports {metric}={count}")

        log(f"  {kind} scenario PASSED")
    finally:
        log(f"  Tearing down {kind} ring")
        terminate(nodes)


# ============================================================================
# MAIN
# ============================================================================


def main() -> None:
    signal.signal(signal.SIGINT, _sigint_handler)
    atexit.register(cleanup)

    if USE_SYSTEMD:
        # systemd mode installs from the prebuilt release tarball; the binary
        # comes from inside it, not from BINARY.
        if not STORAGE_TARBALL:
            die("SMOKE_STORAGE_SYSTEMD=1 requires SMOKE_STORAGE_TARBALL to be set")
        if not Path(STORAGE_TARBALL).is_file():
            die(f"SMOKE_STORAGE_TARBALL {STORAGE_TARBALL} not found")
    elif not BINARY.exists():
        die(f"{BINARY} not found; build it first with `make unbounded-storage-build`")

    # io_uring registers fixed buffers; raise the memlock limit so the
    # storage processes (which inherit our limits) can pin their pages.
    try:
        resource.setrlimit(
            resource.RLIMIT_MEMLOCK, (resource.RLIM_INFINITY, resource.RLIM_INFINITY)
        )
    except (ValueError, OSError) as e:
        log(f"  (could not raise RLIMIT_MEMLOCK: {e}; continuing)")

    # The daemon backs its shards with 2 MiB hugepages; reserve them on the
    # host before any storage process starts so the hugetlb mmap succeeds.
    reserve_hugepages()

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
