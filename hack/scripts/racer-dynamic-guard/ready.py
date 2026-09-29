"""Active readiness and offline fleet-proof verification. Never trusts saved ack."""

import argparse
import json
import os
from pathlib import Path
import signal
import time

import contract as c
import local
import server
import watcher as w


def active(api, host, policy, node):
    c.require(os.stat("/proc/self/ns/net").st_ino == 0xF0000000, "not host netns")
    cm = api.request(w.CM)
    uid = server.pinned(policy, cm, node, host.own)
    a, sources = w.authority(cm, int(host.clock()))
    with host.lock():
        host.verify_policy()
        c.require(dict(name=node, uid=uid, ip=host.own) in a["generation"]["body"]["nodes"], "node drift")
        c.require(local.members(host.run(["ipset", "save", "R47_PEERS"]), "R47_PEERS") == sources, "peer set drift")
        c.require(local.members(host.run(["ipset", "save", "R47_FRESH"]), "R47_FRESH", True) == [host.own], "admission expired")
        w.authority(cm, int(host.clock()))
        return dict(node=node, nodeUID=uid, ip=host.own,
                    bootID=Path("/proc/sys/kernel/random/boot_id").read_text().strip(),
                    sequence=a["sequence"], contentDigest=a["contentDigest"],
                    valid_until=a["valid_until"], verified=int(host.clock()))


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
                  and now < p["valid_until"] and 0 <= now - p["verified"] <= 30,
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
        print(json.dumps(active(w.API(source_uid=policy["sourceUID"]), host, policy, os.environ["NODE_NAME"])))
