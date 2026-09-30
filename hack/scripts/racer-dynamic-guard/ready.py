"""Active readiness and offline fleet-proof verification. Never trusts saved ack."""

import argparse
import json
import os
from pathlib import Path
import signal
import socket
import stat
import struct
import time

import contract as c
import local
import server
import watcher as w


def active(host, policy, node):
    c.require(os.stat("/proc/self/ns/net").st_ino == 0xF0000000, "not host netns")
    for path, kind in ((host.directory, stat.S_ISDIR), (host.directory / "start.sock", stat.S_ISSOCK)):
        info = path.lstat()
        c.require(kind(info.st_mode) and info.st_uid == 0 and not info.st_mode & 0o077, "unsafe guard socket")
    with socket.socket(socket.AF_UNIX) as connection:
        connection.settimeout(15)
        connection.connect(str(host.directory / "start.sock"))
        _, uid, _ = struct.unpack("3i", connection.getsockopt(socket.SOL_SOCKET, socket.SO_PEERCRED, 12))
        c.require(uid == 0, "nonroot guard")
        connection.sendall((c.canonical(dict(ready=node)) + "\n").encode())
        data = b""
        while b"\n" not in data:
            piece = connection.recv(4097 - len(data))
            c.require(piece and len(data) + len(piece) <= 4096, "invalid readiness response")
            data += piece
    proof = json.loads(data)
    now = host.clock()
    c.require(proof["node"] == node and proof["nodeUID"] == policy["nodes"][node]["uid"]
              and proof["ip"] == host.own == policy["nodes"][node]["ip"]
              and proof["bootID"] == Path("/proc/sys/kernel/random/boot_id").read_text().strip()
              and now < proof["valid_until"] and 0 <= now - proof["verified"] <= server.PROOF_MAX_AGE
              and 0 <= now - proof["sourceVerified"] <= server.PROOF_MAX_AGE, "stale/wrong readiness proof")
    return proof


def fleet(cm, proofs, nodes, now):
    a, _ = w.authority(cm, now)
    inventory = {n["metadata"]["name"]: n for n in nodes}
    c.require(len(proofs) == len(nodes) == len(inventory) == 1500, "exact fleet required")
    seen = set()
    for p in proofs:
        name = p["node"]
        c.require(name not in seen and name in inventory, "duplicate/foreign proof")
        seen.add(name)
        n = inventory[name]
        c.require(p["nodeUID"] == n["metadata"]["uid"] and p["bootID"] == n["status"]["nodeInfo"]["bootID"]
                  and p["sequence"] == a["sequence"] and p["contentDigest"] == a["contentDigest"]
                  and now < p["valid_until"] and 0 <= now - p["verified"] <= 30
                  and 0 <= now - p["sourceVerified"] <= server.PROOF_MAX_AGE,
                  "stale/wrong node boot or source generation proof")
        c.require(dict(name=name, uid=p["nodeUID"], ip=p["ip"]) in a["generation"]["body"]["nodes"], "authority node drift")
    return dict(verified=1500, sequence=a["sequence"], contentDigest=a["contentDigest"])


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--policy")
    parser.add_argument("--fleet", help="offline JSON: cm,proofs,nodes")
    args = parser.parse_args()
    signal.signal(signal.SIGALRM, w.expired)
    signal.alarm(20)
    if args.fleet:
        saved = json.loads(Path(args.fleet).read_text())
        print(json.dumps(fleet(saved["cm"], saved["proofs"], saved["nodes"], int(time.time()))))
    else:
        policy = json.loads(Path(args.policy).read_text())
        host = local.Host(os.environ["NODE_IP"], policy["monitors"])
        print(json.dumps(active(host, policy, os.environ["NODE_NAME"])))
