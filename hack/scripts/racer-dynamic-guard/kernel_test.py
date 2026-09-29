"""Run ONLY in disposable private Docker netns with NET_ADMIN, never hostNetwork."""

import contextlib
import os
import subprocess
import time

import contract as c
import local


def run(args, data=None):
    return subprocess.run(["timeout", "--signal=TERM", "--kill-after=10s", "5s", *args],
                          input=data, text=True, capture_output=True, check=True).stdout


c.require(os.environ.get("PRIVATE_KERNEL_TEST") == "true", "explicit isolated test required")
c.require(os.stat("/proc/self/ns/net").st_ino != 0xF0000000, "refuse initial host netns")
own = "10.224.0.5"
host = local.Host(own, ["10.3.0.1"], run=run)
p = host.policy
run(["ipset", "restore"], "create R47_PEERS hash:ip family inet maxelem 1511\nadd R47_PEERS 10.1.0.1\n"
    "create R47_FRESH hash:ip family inet maxelem 1 timeout 60\n")
run(["iptables", "-N", p["chain"]])
for rule in p["ordered_rules"]:
    run(["iptables", "-A", p["chain"], *rule])
run(["iptables", "-I", "INPUT", "1", *p["input_jump"]])
host.verify_policy(legacy=True)
run(["iptables", "-I", "INPUT", "1", *local.admission_rule(own)[2:]])
host.verify_policy()
host.replace(["10.1.0.2"])
c.require(local.members(run(["ipset", "save", "R47_PEERS"]), "R47_PEERS") == ["10.1.0.2"], "old IP retained")
run(["ipset", "add", "R47_FRESH", own, "timeout", "1"])
c.require(local.members(run(["ipset", "save", "R47_FRESH"]), "R47_FRESH", True) == [own], "freshness missing")
time.sleep(2)
c.require(local.members(run(["ipset", "save", "R47_FRESH"]), "R47_FRESH", True) == [], "kernel timeout did not expire")
host.verify_policy()
print("PASS isolated legacy-prefix, exact-chain, atomic-swap-old-removal, kernel-timeout", flush=True)
