"""Install only on empty host guard state with no bound DP listeners.

Recovers only marked, unreferenced CLOSED creation prefixes; no live chain repair.
Separate parent authorization required before any deployment execution.
"""

import argparse
import json
import os
from pathlib import Path
import stat
import tempfile

import contract as c
import local
import server
import watcher as w


MARKER = "bootstrap.json"
SETS = {"R47_PEERS", "R47_FRESH", "R47_DYNAMIC_TMP"}


def sync_directory(directory):
    fd = os.open(directory, os.O_RDONLY | os.O_DIRECTORY)
    try:
        os.fsync(fd)
    finally:
        os.close(fd)


def read_intent(host):
    try:
        fd = os.open(host.directory / MARKER, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
    except FileNotFoundError:
        return None
    with os.fdopen(fd) as stream:
        info = os.fstat(stream.fileno())
        c.require(stat.S_ISREG(info.st_mode) and info.st_uid == 0
                  and stat.S_IMODE(info.st_mode) == 0o600 and info.st_nlink == 1,
                  "unsafe bootstrap marker")
        return json.load(stream)


def write_intent(host, intent):
    # Unique O_EXCL file: a crash before rename leaves no ownership claim. Never
    # follow or truncate a stale temporary symlink. The lock covers publication.
    fd, name = tempfile.mkstemp(prefix=".bootstrap-", dir=host.directory)
    try:
        with os.fdopen(fd, "w") as stream:
            c.require(os.fstat(stream.fileno()).st_uid == 0, "root bootstrap marker required")
            stream.write(c.canonical(intent))
            stream.flush()
            os.fsync(stream.fileno())
        os.replace(name, host.directory / MARKER)
        sync_directory(host.directory)
    finally:
        Path(name).unlink(missing_ok=True)


def recover_sets(host, names, sources):
    c.require("R47_DYNAMIC_TMP" not in names, "foreign staging set")
    c.require("R47_PEERS" not in names or "R47_FRESH" in names, "invalid creation order")
    for name in ("R47_FRESH", "R47_PEERS"):
        if name not in names:
            continue
        values = local.members(host.run(["ipset", "save", name]), name, name == "R47_FRESH")
        # ipset restore is not atomic. Only its exact ordered source prefix can
        # have been written by this intent, including an empty newly created set.
        c.require(values == ([] if name == "R47_FRESH" else sources[:len(values)]),
                  "bootstrap creation prefix drift")
        # Kernel references include every table and list:set, not just filter -S.
        refs = [line for line in host.run(["ipset", "list", name]).splitlines()
                if line.startswith("References:")]
        c.require(refs == ["References: 0"], "bootstrap set referenced")
    c.require(not listeners(), "listener appeared during bootstrap")
    # Validate every set before removing any. Reverse creation order means a
    # crash during cleanup leaves another recognizable creation prefix.
    for name in ("R47_PEERS", "R47_FRESH"):
        if name in names:
            host.run(["ipset", "destroy", name])


def install(host, cm, policy, node, bound_listeners):
    uid = server.pinned(policy, cm, node, host.own)
    a, sources = w.authority(cm, int(host.clock()))
    c.require(dict(name=node, uid=uid, ip=host.own) in a["generation"]["body"]["nodes"], "inventory identity mismatch")
    with host.lock():
        intent = dict(schema=1, state="CLOSED", bootID=Path("/proc/sys/kernel/random/boot_id").read_text().strip(),
                      node=node, nodeUID=uid, ip=host.own, sourceUID=policy["sourceUID"],
                      sequence=a["sequence"], contentDigest=a["contentDigest"],
                      sources=sources, policy=policy, rules=host.policy)
        previous = read_intent(host)
        if previous is not None:
            c.require(previous == intent, "bootstrap ownership/generation mismatch")
            c.require(not bound_listeners and not listeners(), "listener predates bootstrap")
            w.authority(cm, int(host.clock()))
        all_rules = host.run(["iptables", "-w", "2", "-S"])
        if "RACER_STAGE47" in all_rules or "racer-stage47-fresh" in all_rules:
            if previous is not None:
                # A crash after COMMIT may leave the intent. Only the complete,
                # still CLOSED policy is a valid completion of that transaction.
                host.verify_policy()
                c.require(local.members(host.run(["ipset", "save", "R47_FRESH"]), "R47_FRESH", True) == [],
                          "bootstrap freshness not CLOSED")
                c.require(local.members(host.run(["ipset", "save", "R47_PEERS"]), "R47_PEERS") == sources,
                          "bootstrap membership mismatch")
                c.require("R47_DYNAMIC_TMP" not in host.run(["ipset", "list", "-name"]).splitlines(),
                          "foreign staging set")
                (host.directory / MARKER).unlink()
                sync_directory(host.directory)
                return
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
        c.require(not bound_listeners and not listeners(), "listener predates bootstrap")
        w.authority(cm, int(host.clock()))
        names = SETS & set(host.run(["ipset", "list", "-name"]).splitlines())
        if previous is None:
            c.require(not names, "partial ipset state without ownership")
            write_intent(host, intent)
        else:
            recover_sets(host, names, sources)
        host.run(["ipset", "restore"], "create R47_FRESH hash:ip family inet maxelem 1 timeout 60\n")
        host.run(["ipset", "restore"], "create R47_PEERS hash:ip family inet hashsize 2048 maxelem 1511\n")
        host.run(["ipset", "restore"], "".join(f"add R47_PEERS {ip}\n" for ip in sources))
        c.require(local.members(host.run(["ipset", "save", "R47_PEERS"]), "R47_PEERS") == sources,
                  "bootstrap membership mismatch")
        p = host.policy
        lines = ["*filter", ":RACER_STAGE47 - [0:0]"]
        # These are fixed tokens and validated IPv4 addresses, not shell input.
        # iptables-restore does not parse shell single quotes around '!'.
        lines += [" ".join(["-A", p["chain"], *r]) for r in p["ordered_rules"]]
        # One filter-table transaction. NEW-deny at position1, old jump at2.
        lines += [" ".join(["-I", "INPUT", "1", *p["input_jump"]]),
                  " ".join(["-I", "INPUT", "1", *local.admission_rule(host.own)[2:]]), "COMMIT"]
        c.require(local.members(host.run(["ipset", "save", "R47_FRESH"]), "R47_FRESH", True) == [],
                  "bootstrap freshness not CLOSED")
        c.require(not listeners(), "listener appeared during bootstrap")
        w.authority(cm, int(host.clock()))
        host.run(["iptables-restore", "--wait", "2", "--noflush"], "\n".join(lines) + "\n")
        host.verify_policy()
        (host.directory / MARKER).unlink()
        sync_directory(host.directory)
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
