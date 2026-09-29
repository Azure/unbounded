"""Install only on empty host guard state with no bound DP listeners.

Refuses legacy migration and partial states; no live chain flush or repair.
Separate parent authorization required before any deployment execution.
"""

import argparse
import json
import os
from pathlib import Path
import shlex

import contract as c
import local
import server
import watcher as w


def install(host, cm, policy, node, listeners):
    uid = server.pinned(policy, cm, node, host.own)
    a, sources = w.authority(cm, int(host.clock()))
    c.require(dict(name=node, uid=uid, ip=host.own) in a["generation"]["body"]["nodes"], "inventory identity mismatch")
    with host.lock():
        all_rules = host.run(["iptables", "-w", "2", "-S"])
        if "RACER_STAGE47" in all_rules or "racer-stage47-fresh" in all_rules:
            if "racer-stage47-fresh" not in all_rules:
                c.require(policy.get("allowLegacyTransition") is True, "legacy transition not authorized")
                host.verify_policy(legacy=True)
                current = local.members(host.run(["ipset", "save", "R47_PEERS"]), "R47_PEERS")
                # Current exact authority required; never retain bridge/old IPs.
                c.require(current == sources, "legacy1511 differs from fresh authority")
                names = host.run(["ipset", "list", "-name"]).splitlines()
                if "R47_FRESH" in names:
                    c.require(local.members(host.run(["ipset", "save", "R47_FRESH"]), "R47_FRESH", True) == [],
                              "legacy partial freshness not empty")
                else:
                    host.run(["ipset", "restore"], "create R47_FRESH hash:ip family inet maxelem 1 timeout 60\n")
                w.authority(cm, int(host.clock()))
                host.run(["iptables", "-w", "2", "-I", "INPUT", "1", *local.admission_rule(host.own)[2:]])
            host.verify_policy()  # Idempotent only for the complete exact policy.
            return
        c.require(not listeners, "listener predates bootstrap")
        names = host.run(["ipset", "list", "-name"]).splitlines()
        c.require(not {"R47_PEERS", "R47_FRESH", "R47_DYNAMIC_TMP"} & set(names), "partial ipset state")
        host.run(["ipset", "restore"], "create R47_FRESH hash:ip family inet maxelem 1 timeout 60\n"
                 + "create R47_PEERS hash:ip family inet hashsize 2048 maxelem 1511\n"
                 + "".join(f"add R47_PEERS {ip}\n" for ip in sources))
        c.require(local.members(host.run(["ipset", "save", "R47_PEERS"]), "R47_PEERS") == sources,
                  "bootstrap membership mismatch")
        p = host.policy
        lines = ["*filter", ":RACER_STAGE47 - [0:0]"]
        lines += [shlex.join(["-A", p["chain"], *r]) for r in p["ordered_rules"]]
        # One filter-table transaction. NEW-deny at position1, old jump at2.
        lines += [shlex.join(["-I", "INPUT", "1", *p["input_jump"]]),
                  shlex.join(["-I", "INPUT", "1", *local.admission_rule(host.own)[2:]]), "COMMIT"]
        host.run(["iptables-restore", "--wait", "2", "--noflush"], "\n".join(lines) + "\n")
        host.verify_policy()
        # Admission remains CLOSED. Only a later fresh tick can open it.


def listeners():
    bound = []
    for family in ("tcp", "tcp6"):
        for line in Path("/proc/net/" + family).read_text().splitlines()[1:]:
            fields = line.split()
            if fields[3] == "0A" and int(fields[1].split(":")[1], 16) in (18082, 19090):
                bound.append(family)
    return bound


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--policy", required=True)
    args = parser.parse_args()
    c.require(os.environ.get("GUARD_BOOTSTRAP_AUTHORIZED") == "true", "bootstrap not authorized")
    c.require(os.stat("/proc/self/ns/net").st_ino == 0xF0000000, "not host netns")
    policy = json.loads(Path(args.policy).read_text())
    host = local.Host(os.environ["NODE_IP"], policy["monitors"])
    install(host, w.API().request(w.CM), policy, os.environ["NODE_NAME"], listeners())


if __name__ == "__main__":
    main()
