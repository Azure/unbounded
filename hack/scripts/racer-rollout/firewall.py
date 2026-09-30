#!/usr/bin/env python3
"""One-shot removal of the retired stage47 firewall objects, not other rules."""
import re
import shlex
import signal
import subprocess

CHAIN = "RACER_STAGE47"


def run(*args):
    return subprocess.run(
        ["timeout", "--signal=TERM", "--kill-after=2s", "15s", "chroot", "/host", *args],
        check=True, text=True, capture_output=True, timeout=18,
    ).stdout


def value(rule, key):
    return rule[rule.index(key) + 1] if key in rule else None


def removable(rule):
    if rule[:2] != ["-A", "INPUT"]:
        return False
    tag, target = value(rule, "--comment"), value(rule, "-j")
    return (tag == "racer-stage47-owned" and target == CHAIN) or (
        tag == "racer-stage47-fresh" and target == "REJECT"
        and value(rule, "--match-set") == "R47_FRESH"
    )


def cleanup():
    # Read and validate the whole filter snapshot before deleting anything.
    rules = [shlex.split(line) for line in run("iptables", "-w", "5", "-t", "filter", "-S").splitlines()]
    owned = [r for r in rules if r[:2] == ["-A", CHAIN]]
    if any(value(r, "--comment") != "racer-stage47-owned" for r in owned):
        raise RuntimeError("unexpected untagged rule in retired chain; inspect manually")
    if any(value(r, "-j") == CHAIN and not removable(r) for r in rules):
        raise RuntimeError("unexpected reference to retired chain; inspect manually")
    for rule in [r for r in rules if removable(r)] + owned:
        run("iptables", "-w", "5", "-t", "filter", "-D", *rule[1:])
    if ["-N", CHAIN] in rules:
        run("iptables", "-w", "5", "-t", "filter", "-X", CHAIN)
    for name in run("ipset", "list", "-name").splitlines():
        if re.fullmatch(r"R47_(PEERS|FRESH|DYNAMIC_TMP|TMP_[0-9a-f]{16})", name):
            run("ipset", "destroy", name)
    print("retired firewall objects removed", flush=True)


if __name__ == "__main__":
    signal.alarm(240)
    cleanup()
