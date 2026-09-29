"""Single-threaded root UDS startup gate plus bounded periodic reconciliation."""

import argparse
import json
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


def pinned(policy, cm, node, own):
    c.require(cm["metadata"]["uid"] == policy["sourceUID"], "source CM recreated")
    c.require(set(policy["denyNodes"]) == c.DENY11, "DENY11 drift")
    row = policy["nodes"][node]
    c.require(row["ip"] == own and row["uid"], "bootstrap node identity mismatch")
    return row["uid"]


def reconcile(api, host, policy, node):
    cm = api.request(w.CM)  # Direct read for EVERY startup, never a proof-file ack.
    uid = pinned(policy, cm, node, host.own)
    return host.tick(cm, node, uid)


def respond(connection, api, host, policy, node):
    pid, uid, gid = struct.unpack("3i", connection.getsockopt(socket.SOL_SOCKET, socket.SO_PEERCRED, 12))
    c.require(uid == 0 and node not in c.DENY11, "root host DP only; DENY11")
    connection.settimeout(2)
    data = b""
    while b"\n" not in data:
        piece = connection.recv(1025 - len(data))
        c.require(piece and len(data) + len(piece) <= 1024, "invalid startup request")
        data += piece
    request = json.loads(data)
    c.require(set(request) == {"nonce", "node"} and request["node"] == node
              and re.fullmatch("[0-9a-f]{64}", request["nonce"]), "invalid nonce/node")
    proof = reconcile(api, host, policy, node)
    c.require(proof["valid_until"] > int(time.time()) + 2, "proof expired before reply")
    proof.pop("ip")
    proof["nonce"] = request["nonce"]
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
        while end - time.monotonic() >= 25:
            try:
                signal.alarm(25)
                if not initialized:
                    bootstrap.install(host, api.request(w.CM), policy, node, bootstrap.listeners())
                    initialized = True
                if time.monotonic() >= next_poll:
                    reconcile(api, host, policy, node)
                    next_poll = time.monotonic() + 5
                try:
                    connection, _ = listener.accept()
                except socket.timeout:
                    continue
                with connection:
                    respond(connection, api, host, policy, node)
            except Exception:
                # Kernel timer enforces expiry even if cleanup or process fails.
                signal.alarm(10)
                try:
                    with host.lock():
                        host.close_admission()
                except Exception:
                    pass
                print('{"result":"guard-denied"}', flush=True)
                time.sleep(1)
            finally:
                signal.alarm(0)
    finally:
        listener.close()
        path.unlink(missing_ok=True)
        os.close(fd)


if __name__ == "__main__":
    main()
