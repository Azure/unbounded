"""Host adapter; requires separately reviewed, preinstalled admission gate.

No bootstrap, chain modification or deployment. Call tick under a 25s process
deadline; all writers must use the same root-owned host lock directory.
Kernel timeout admission persists across agent death, but not host reboot.
"""

import contextlib
import argparse
import fcntl
import ipaddress
import json
import os
from pathlib import Path
import shlex
import signal
import stat
import subprocess
import time

import contract as c
import watcher


def host_root():
    os.chroot("/host")
    os.chdir("/")


def command(args, data=None):
    process = subprocess.Popen(["timeout", "--signal=TERM", "--kill-after=10s", "5s", *args],
                               stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                               text=True, preexec_fn=host_root, start_new_session=True)
    try:
        output, error = process.communicate(data, timeout=16)
        if process.returncode:
            raise subprocess.CalledProcessError(process.returncode, args)
        return output
    finally:
        if process.poll() is None:
            # An outer alarm must not leave a host child running after lock release.
            os.killpg(process.pid, signal.SIGTERM)
            try:
                process.communicate(timeout=10)
            except subprocess.TimeoutExpired:
                os.killpg(process.pid, signal.SIGKILL)
                process.communicate(timeout=1)


def normalized(tokens):
    out = []
    i = 0
    while i < len(tokens):
        if tokens[i:i + 2] == ["-m", "tcp"]:
            i += 2
            continue
        token = tokens[i]
        if out and out[-1] in ("-s", "-d"):
            token = str(ipaddress.IPv4Network(token, strict=True))
        out.append(token)
        i += 1
    return out


def parse_rules(text):
    return [normalized(shlex.split(line)) for line in text.splitlines() if line.startswith("-A ")]


def admission_rule(own):
    return ["-A", "INPUT", "-d", c.ipv4(own) + "/32", "-p", "tcp", "-m", "multiport",
            "--dports", "18082,19090", "-m", "conntrack", "--ctstate", "NEW",
            "-m", "set", "!", "--match-set", "R47_FRESH", "dst", "-m", "comment",
            "--comment", "racer-stage47-fresh", "-j", "REJECT", "--reject-with", "tcp-reset"]


def members(text, name, timed=False):
    rows = [shlex.split(line) for line in text.splitlines() if line]
    c.require(rows and rows[0][:3] == ["create", name, "hash:ip"], "set type drift")
    header = rows[0][3:]
    c.require(len(header) % 2 == 0, "set option drift")
    options = dict(zip(header[::2], header[1::2]))
    allowed = {"family", "hashsize", "maxelem", "bucketsize", "initval"}
    if timed:
        allowed.add("timeout")
    c.require(len(options) * 2 == len(header) and set(options) <= allowed
              and options.get("family") == "inet"
              and options.get("maxelem") == ("1" if timed else "1511")
              and (not timed or options.get("timeout") == "60"), "set options drift")
    values = []
    for row in rows[1:]:
        c.require(row[:2] == ["add", name] and len(row) == (5 if timed else 3), "set entry drift")
        if timed:
            c.require(row[3] == "timeout" and 0 < int(row[4]) <= 60, "invalid freshness timeout")
        values.append(c.ipv4(row[2]))
    c.require(len(values) == len(set(values)), "duplicate set member")
    return sorted(values)


class Host:
    def __init__(self, own, monitors, directory="/run/racer-guard", run=command, clock=time.time):
        self.own, self.policy = c.ipv4(own), c.rules(own, monitors)
        self.directory, self.run, self.clock = Path(directory), run, clock

    @contextlib.contextmanager
    def lock(self):
        info = self.directory.lstat()
        c.require(stat.S_ISDIR(info.st_mode) and info.st_uid == 0
                  and not info.st_mode & 0o022, "guard directory must be root-owned/private")
        fd = os.open(self.directory / "lock", os.O_CREAT | os.O_RDWR | os.O_NOFOLLOW, 0o600)
        try:
            info = os.fstat(fd)
            c.require(stat.S_ISREG(info.st_mode) and info.st_uid == 0
                      and not info.st_mode & 0o022, "unsafe lock")
            deadline = time.monotonic() + 2
            while True:
                try:
                    fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
                    break
                except BlockingIOError:
                    c.require(time.monotonic() < deadline, "guard lock timeout")
                    time.sleep(0.05)
            yield
        finally:
            os.close(fd)

    def verify_policy(self, legacy=False):
        p = self.policy
        actual = parse_rules(self.run(["iptables", "-w", "2", "-S"]))
        chain = [r for r in actual if r[1] == p["chain"]]
        c.require(chain == [normalized(["-A", p["chain"], *r]) for r in p["ordered_rules"]], "chain drift")
        jump = normalized(["-A", "INPUT", *p["input_jump"]])
        gate = normalized(admission_rule(self.own))
        inputs = [r for r in actual if r[1] == "INPUT"]
        if legacy:
            c.require(inputs[:1] == [jump] and not any("racer-stage47-fresh" in r for r in actual), "legacy top jump drift")
        else:
            c.require(inputs[:2] == [gate, jump] and inputs.count(gate) == 1, "admission/top jump drift")
        refs = [r for r in actual if any(r[i:i + 2] in (["-j", p["chain"]], ["-g", p["chain"]])
                                        for i in range(len(r) - 1))]
        c.require(refs == [jump], "unexpected chain references")
        if not legacy:
            fresh = members(self.run(["ipset", "save", "R47_FRESH"]), "R47_FRESH", True)
            c.require(fresh in ([], [self.own]), "foreign freshness member")

    def replace(self, sources):
        # Fixed staging name is safe only under the host-wide lock. Flush affects
        # staging only and makes interrupted restore/swap cleanup resumable.
        name = "R47_DYNAMIC_TMP"
        # Never flush a staging object referenced by any packet-filter rule.
        rules = self.run(["iptables", "-w", "2", "-S"])
        c.require(name not in rules, "staging set has firewall references")
        data = f"create {name} hash:ip family inet hashsize 2048 maxelem 1511 -exist\nflush {name}\n"
        data += "".join(f"add {name} {c.ipv4(ip)}\n" for ip in sources)
        self.run(["ipset", "restore"], data)
        c.require(members(self.run(["ipset", "save", name]), name) == sources, "staging mismatch")
        self.run(["ipset", "swap", name, "R47_PEERS"])
        c.require(members(self.run(["ipset", "save", "R47_PEERS"]), "R47_PEERS") == sources, "swap mismatch")
        self.run(["ipset", "destroy", name])

    def close_admission(self):
        # Empty timeout set rejects NEW flows through preinstalled gate. Existing
        # flows still meet the unchanged original chain; no blanket bypass added.
        self.run(["ipset", "flush", "R47_FRESH"])

    def tick(self, cm, node, uid):
        with self.lock():
            try:
                c.require(os.stat("/proc/self/ns/net").st_ino == 0xF0000000, "not host netns")
                self.verify_policy()
                a, sources = watcher.authority(cm, int(self.clock()))
                c.require(dict(name=node, uid=uid, ip=self.own) in a["generation"]["body"]["nodes"], "node identity drift")
                addresses = json.loads(self.run(["ip", "-j", "-4", "address", "show"]))
                c.require(self.own in {x["local"] for interface in addresses for x in interface["addr_info"]}, "IP not assigned")
                proof_path = self.directory / "proof.json"
                if proof_path.exists():
                    previous = json.loads(proof_path.read_text())
                    c.require(a["sequence"] >= previous["sequence"] and
                              (a["sequence"] != previous["sequence"] or
                               a["contentDigest"] == previous["contentDigest"]), "authority rollback")
                current = members(self.run(["ipset", "save", "R47_PEERS"]), "R47_PEERS")
                if current != sources:
                    self.close_admission()
                    self.replace(sources)
                self.verify_policy()
                watcher.authority(cm, int(self.clock()))
                # Account for maximum successful command duration plus rounding.
                remaining = a["valid_until"] - int(self.clock()) - 6
                c.require(0 < remaining <= 60, "authority expired during reconcile")
                # -exist refreshes this entry's timeout without touching peer set.
                self.run(["ipset", "add", "R47_FRESH", self.own, "timeout", str(remaining), "-exist"])
                proof = dict(sequence=a["sequence"], contentDigest=a["contentDigest"],
                             bootID=Path("/proc/sys/kernel/random/boot_id").read_text().strip(),
                             node=node, nodeUID=uid, ip=self.own, valid_until=a["valid_until"])
                temporary = self.directory / "proof.next"
                with temporary.open("w") as stream:
                    json.dump(proof, stream)
                    stream.flush()
                    os.fsync(stream.fileno())
                os.replace(temporary, proof_path)
                directory_fd = os.open(self.directory, os.O_RDONLY | os.O_DIRECTORY)
                try:
                    os.fsync(directory_fd)
                finally:
                    os.close(directory_fd)
                return proof
            except Exception:
                self.close_admission()
                raise


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--policy", required=True)
    parser.add_argument("--seconds", type=int, default=240)
    args = parser.parse_args()
    c.require(1 <= args.seconds <= 240, "bounded runtime required")
    policy = json.loads(Path(args.policy).read_text())
    c.require(set(policy["denyNodes"]) == c.DENY11, "fixed DENY11 drift")
    host = Host(os.environ["NODE_IP"], policy["monitors"])
    api = watcher.API()
    signal.signal(signal.SIGALRM, watcher.expired)
    end = time.monotonic() + args.seconds
    while end - time.monotonic() >= 25:
        signal.alarm(25)
        try:
            cm = api.request(watcher.CM)
            proof = host.tick(cm, os.environ["NODE_NAME"], os.environ["NODE_UID"])
            print(json.dumps(proof), flush=True)
        except (ValueError, KeyError, OSError, watcher.Deadline,
                watcher.http.client.HTTPException, subprocess.SubprocessError):
            # The kernel timeout remains the backstop if lock/command/API fails.
            signal.alarm(10)
            try:
                with host.lock():
                    host.close_admission()
            except (ValueError, OSError, watcher.Deadline, subprocess.SubprocessError):
                pass
            print('{"result":"closed-or-kernel-expiry-pending"}', flush=True)
        finally:
            signal.alarm(0)
        time.sleep(min(5, max(0, end - time.monotonic())))


if __name__ == "__main__":
    main()
