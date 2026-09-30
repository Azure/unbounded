"""Single-threaded root UDS startup gate plus bounded periodic reconciliation."""

import argparse
import hashlib
import json
import math
import os
from pathlib import Path
import re
import signal
import socket
import stat
import struct
import time

import contract as c
import local
import watcher as w

POLL_SECONDS = 10
PROOF_MAX_AGE = 15


class Diagnostics:
    """Bounded, payload-free failure context; never stringify an exception."""

    stages = frozenset(
        (
            "cycle",
            "poll",
            "bootstrap-get",
            "bootstrap-install",
            "direct-get",
            "pin",
            "tick",
            "accept",
            "peer",
            "receive",
            "decode",
            "request",
            "status",
            "reply-check",
            "send",
            "cleanup",
        )
    )
    messages = frozenset(
        (
            "API observation too old",
            "source CM recreated",
            "DENY11 drift",
            "bootstrap node identity mismatch",
            "not host netns",
            "cached proof identity/content drift",
            "node drift",
            "peer set drift",
            "admission expired",
            "root guard client only",
            "invalid startup request",
            "host DP startup denied; DENY11",
            "invalid nonce/node",
            "proof expired before reply",
            "bootstrap incomplete",
            "bounded operation deadline",
            "API response exceeds limit",
        )
    )
    classes = frozenset(
        (
            "ValueError",
            "KeyError",
            "TypeError",
            "OSError",
            "TimeoutError",
            "ConnectionError",
            "ConnectionResetError",
            "BrokenPipeError",
            "PermissionError",
            "FileNotFoundError",
            "JSONDecodeError",
            "Deadline",
            "SSLError",
            "SSLCertVerificationError",
            "HTTPException",
            "RemoteDisconnected",
            "IncompleteRead",
            "CalledProcessError",
            "TimeoutExpired",
            "RuntimeError",
        )
    )

    def __init__(self, cache=None):
        self.started = time.monotonic()
        self.stage, self.stage_started = "cycle", self.started
        self.observed = (cache or {}).get("observed")
        self.observed_wall = (cache or {}).get("observed_wall")

    def mark(self, stage):
        self.stage = stage if stage in self.stages else "cycle"
        self.stage_started = time.monotonic()

    def failure(self, error):
        # Only exact constant messages and allowlisted class names may leave the
        # process. In particular, JSON/HTTP/subprocess errors can contain secrets.
        event = dict(result="guard-denied", stage=self.stage, exception="Exception", message="redacted")
        try:
            name = type(error).__name__
            if name in self.classes:
                event["exception"] = name
            if type(error) in (ValueError, w.Deadline) and len(error.args) == 1:
                message = error.args[0]
                if type(message) is str and message in self.messages:
                    event["message"] = message
            now = time.monotonic()
            for key, end, start in (
                ("duration_seconds", now, self.started),
                ("stage_duration_seconds", now, self.stage_started),
                ("observation_age_seconds", now, self.observed),
                ("observation_wall_age_seconds", time.time(), self.observed_wall),
            ):
                if type(start) in (int, float) and math.isfinite(start):
                    value = end - start
                    if math.isfinite(value):
                        event[key] = round(value, 6)
        except Exception:
            pass  # Diagnostic collection must not prevent fail-closed cleanup.
        return event


def next_poll_at(node, now):
    # Stable per-node phase spreads fleet polls without extending the interval.
    phase = int.from_bytes(hashlib.sha256(node.encode()).digest()[:8], "big") / 2**64 * POLL_SECONDS
    return (int((now - phase) // POLL_SECONDS) + 1) * POLL_SECONDS + phase


def pinned(policy, cm, node, own):
    c.require(cm["metadata"]["uid"] == policy["sourceUID"], "source CM recreated")
    c.require(set(policy["denyNodes"]) == c.DENY11, "DENY11 drift")
    row = policy["nodes"][node]
    c.require(row["ip"] == own and row["uid"], "bootstrap node identity mismatch")
    return row["uid"]


def reconcile(api, host, policy, node, cache=None, diagnostic=None):
    diagnostic = diagnostic or Diagnostics(cache)
    if cache is not None:
        cache.clear()
    observed, observed_wall = time.monotonic(), host.clock()
    diagnostic.mark("direct-get")
    cm = api.request(w.CM)  # Direct read for EVERY startup, never a proof-file ack.
    diagnostic.mark("pin")
    uid = pinned(policy, cm, node, host.own)
    diagnostic.mark("tick")
    proof = host.tick(cm, node, uid)
    if cache is not None:
        cache.update(cm=cm, proof=dict(proof), observed=observed, observed_wall=observed_wall)
        diagnostic.observed, diagnostic.observed_wall = observed, observed_wall
    return proof


def status(host, policy, node, cache):
    """Read-only kernel proof; only successful direct reads populate this cache."""
    def authority():
        c.require(cache and 0 <= time.monotonic() - cache["observed"] <= PROOF_MAX_AGE
                  and 0 <= host.clock() - cache["observed_wall"] <= PROOF_MAX_AGE,
                  "API observation too old")
        return w.authority(cache["cm"], int(host.clock()))

    c.require(os.stat("/proc/self/ns/net").st_ino == 0xF0000000, "not host netns")
    a, sources = authority()
    uid = pinned(policy, cache["cm"], node, host.own)
    proof = dict(cache["proof"])
    c.require(proof == dict(node=node, nodeUID=uid, ip=host.own,
                  bootID=Path("/proc/sys/kernel/random/boot_id").read_text().strip(),
                  sequence=a["sequence"], contentDigest=a["contentDigest"], valid_until=a["valid_until"]),
              "cached proof identity/content drift")
    c.require(dict(name=node, uid=uid, ip=host.own) in a["generation"]["body"]["nodes"], "node drift")
    with host.lock():
        host.verify_policy()
        c.require(local.members(host.run(["ipset", "save", "R47_PEERS"]), "R47_PEERS") == sources, "peer set drift")
        c.require(local.members(host.run(["ipset", "save", "R47_FRESH"]), "R47_FRESH", True) == [host.own], "admission expired")
        authority()  # Check age and original expiry again after kernel commands.
        proof.update(verified=int(host.clock()), sourceVerified=cache["observed_wall"])
        return proof


def respond(connection, api, host, policy, node, cache=None, diagnostic=None):
    diagnostic = diagnostic or Diagnostics(cache)
    diagnostic.mark("peer")
    pid, uid, gid = struct.unpack("3i", connection.getsockopt(socket.SOL_SOCKET, socket.SO_PEERCRED, 12))
    c.require(uid == 0, "root guard client only")
    connection.settimeout(2)
    diagnostic.mark("receive")
    data = b""
    while b"\n" not in data:
        piece = connection.recv(1025 - len(data))
        c.require(piece and len(data) + len(piece) <= 1024, "invalid startup request")
        data += piece
    diagnostic.mark("decode")
    request = json.loads(data)
    if request == {"ready": node}:
        diagnostic.mark("status")
        proof = status(host, policy, node, cache)
        diagnostic.mark("send")
        connection.sendall((c.canonical(proof) + "\n").encode())
        return
    # All 1500 guards need readiness; DENY11 excludes only host DP startup.
    diagnostic.mark("request")
    c.require(node not in c.DENY11, "host DP startup denied; DENY11")
    c.require(set(request) == {"nonce", "node"} and request["node"] == node
              and re.fullmatch("[0-9a-f]{64}", request["nonce"]), "invalid nonce/node")
    proof = reconcile(api, host, policy, node, cache, diagnostic)
    diagnostic.mark("reply-check")
    c.require(proof["valid_until"] > int(time.time()) + 2, "proof expired before reply")
    proof.pop("ip")
    proof["nonce"] = request["nonce"]
    diagnostic.mark("send")
    connection.sendall((c.canonical(proof) + "\n").encode())


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--policy", required=True)
    parser.add_argument("--seconds", type=int, default=0, help="0: continuous bounded cycles")
    args = parser.parse_args()
    c.require(0 <= args.seconds <= 240 and os.geteuid() == 0, "bounded-cycle root guard required")
    policy = json.loads(Path(args.policy).read_text())
    host = local.Host(os.environ["NODE_IP"], policy["monitors"])
    node, api = os.environ["NODE_NAME"], w.API()
    directory = host.directory
    info = directory.lstat()
    c.require(stat.S_ISDIR(info.st_mode) and info.st_uid == 0 and not info.st_mode & 0o022,
              "private root directory required")
    os.chmod(directory, 0o700)
    path = directory / "start.sock"
    # Lifetime lock prevents a second server unlinking a live socket.
    import fcntl
    fd = os.open(directory / "server.lock", os.O_CREAT | os.O_RDWR | os.O_NOFOLLOW, 0o600)
    fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
    if path.exists():
        info = path.lstat()
        c.require(stat.S_ISSOCK(info.st_mode) and info.st_uid == 0, "unsafe stale socket")
        path.unlink()
    signal.signal(signal.SIGALRM, w.expired)
    signal.signal(signal.SIGTERM, w.terminate)
    # Every guard process start, including restart after a host reboot. Never
    # assume kubelet reran the pod init container.
    import bootstrap
    c.require(os.stat("/proc/self/ns/net").st_ino == 0xF0000000, "not host netns")
    listener = socket.socket(socket.AF_UNIX)
    try:
        os.umask(0o077)
        listener.bind(str(path))
        os.chmod(path, 0o600)
        listener.listen(16)
        listener.settimeout(1)
        end = time.monotonic() + args.seconds if args.seconds else float("inf")
        next_poll, initialized = 0, False
        cache = {}  # Never restore readiness authority from durable proof.json.
        while end - time.monotonic() >= 25:
            diagnostic = Diagnostics(cache)
            try:
                signal.alarm(25)
                diagnostic.mark("poll")
                if time.monotonic() >= next_poll:
                    next_poll = next_poll_at(node, time.monotonic())
                    if not initialized:
                        diagnostic.mark("bootstrap-get")
                        cm = api.request(w.CM)
                        diagnostic.mark("bootstrap-install")
                        bootstrap.install(host, cm, policy, node, bootstrap.listeners())
                        initialized = True
                    reconcile(api, host, policy, node, cache, diagnostic)
                    next_poll = next_poll_at(node, time.monotonic())
                try:
                    diagnostic.mark("accept")
                    connection, _ = listener.accept()
                except socket.timeout:
                    continue
                with connection:
                    c.require(initialized, "bootstrap incomplete")
                    respond(connection, api, host, policy, node, cache, diagnostic)
            except Exception as error:
                cache.clear()
                # Kernel timer enforces expiry even if cleanup or process fails.
                signal.alarm(10)
                event = diagnostic.failure(error)
                diagnostic.mark("cleanup")
                try:
                    with host.lock():
                        host.close_admission()
                except Exception as cleanup_error:
                    event["cleanup"] = diagnostic.failure(cleanup_error)
                print(json.dumps(event), flush=True)
                time.sleep(1)
            finally:
                signal.alarm(0)
    finally:
        listener.close()
        path.unlink(missing_ok=True)
        os.close(fd)


if __name__ == "__main__":
    main()
