"""Disposable private-netns bootstrap crash and real TCP admission tests only."""

import concurrent.futures
import os
import socket
import subprocess
import tempfile
import time

import bootstrap
import contract as c
import local
import watcher as w
from test_adapters import FakeAPI


def run(args, data=None):
    result = subprocess.run(["timeout", "--signal=TERM", "--kill-after=10s", "5s", *args],
                            input=data, text=True, capture_output=True)
    if result.returncode:
        print(result.stderr, flush=True)
        result.check_returncode()
    return result.stdout


def reset():
    # Only called after the explicit namespace guard, never on the host.
    run(["iptables", "-F"])
    run(["iptables", "-X"])
    run(["ipset", "destroy"])


def echo(listener):
    connection, _ = listener.accept()
    with connection:
        connection.settimeout(5)
        while data := connection.recv(16):
            connection.sendall(data)


def connect(source, own, port):
    connection = socket.socket()
    connection.settimeout(2)
    connection.bind((source, 0))
    try:
        connection.connect((own, port))
        connection.sendall(b"probe")
        c.require(connection.recv(5) == b"probe", "TCP response mismatch")
    except BaseException:
        connection.close()
        raise
    return connection


def rejected(source, own, port):
    try:
        connection = connect(source, own, port)
    except ConnectionRefusedError:
        return
    connection.close()
    raise AssertionError(f"unauthorized TCP flow passed: {source}:{port}")


def main():
    c.require(os.environ.get("PRIVATE_KERNEL_TEST") == "true", "explicit isolated test required")
    c.require(os.stat("/proc/self/ns/net").st_ino != 0xF0000000, "refuse host netns")
    c.require(os.geteuid() == 0, "private root required")
    api = FakeAPI()
    w.cycle(api, "a", lambda: 100)
    row = next(n for n in api.nodes if n["metadata"]["name"] not in c.DENY11)
    node, uid = row["metadata"]["name"], row["metadata"]["uid"]
    own = row["status"]["addresses"][0]["address"]
    _, sources = w.authority(api.cm, 110)
    peer = next(ip for ip in sources if ip != own)
    monitor, foreign = "192.0.2.10", "192.0.2.11"
    policy = dict(sourceUID="cm", denyNodes=sorted(c.DENY11), nodes={node: dict(uid=uid, ip=own)})
    run(["ip", "link", "set", "lo", "up"])
    for ip in (own, peer, monitor, foreign):
        run(["ip", "address", "replace", ip + "/32", "dev", "lo"])
    with tempfile.TemporaryDirectory(dir="/run") as directory:
        host = local.Host(own, [monitor], directory=directory, run=run, clock=lambda: 110)
        for point in ("R47_FRESH", "R47_PEERS", "transaction"):
            reset()
            def fail(args, data=None):
                if point == "transaction" and args[0] == "iptables-restore":
                    # Real restore parse failure: no filter-table COMMIT.
                    return run(args, data.replace("COMMIT", "INVALID\nCOMMIT"))
                result = run(args, data)
                if data and data.startswith(f"create {point} "):
                    raise RuntimeError("injected crash after creation")
                return result
            host.run = fail
            try:
                bootstrap.install(host, api.cm, policy, node, [])
                raise AssertionError("injected failure not reached")
            except (RuntimeError, subprocess.CalledProcessError):
                pass
            c.require(not bootstrap.listeners(), "listener during interrupted bootstrap")
            c.require("RACER_STAGE47" not in run(["iptables", "-S"]), "partial transaction committed")
            marker = host.directory / bootstrap.MARKER
            c.require(marker.stat().st_uid == 0 and marker.stat().st_mode & 0o777 == 0o600,
                      "unsafe marker permissions")
            host.run = run
            if point == "R47_FRESH":
                # A real reference in a different table must block reclamation.
                run(["iptables", "-t", "raw", "-A", "PREROUTING", "-m", "set",
                     "--match-set", "R47_FRESH", "src", "-j", "ACCEPT"])
                try:
                    bootstrap.install(host, api.cm, policy, node, [])
                    raise AssertionError("cross-table referenced set accepted")
                except ValueError:
                    pass
                c.require("R47_FRESH" in run(["ipset", "list", "-name"]), "referenced set removed")
                run(["iptables", "-t", "raw", "-F"])
            # Even a marked, otherwise valid prefix cannot bypass a real listener.
            with socket.socket() as listener:
                listener.bind((own, 18082))
                listener.listen()
                try:
                    bootstrap.install(host, api.cm, policy, node, [])
                    raise AssertionError("active listener accepted")
                except ValueError:
                    pass
            bootstrap.install(host, api.cm, policy, node, [])
            host.verify_policy()
            c.require(local.members(run(["ipset", "save", "R47_FRESH"]), "R47_FRESH", True) == [], "opened on retry")
            c.require(local.members(run(["ipset", "save", "R47_PEERS"]), "R47_PEERS") == sources, "source drift")
            print(f"PASS kernel crash/retry {point}, real-listener refusal, exact CLOSED policy", flush=True)

        # Listeners exist only AFTER successful installation. Loopback TCP still
        # traverses real INPUT/conntrack with non-loopback source/destination IPs.
        with socket.socket() as peer_listener, socket.socket() as diag_listener:
            peer_listener.bind((own, 18082))
            diag_listener.bind((own, 19090))
            peer_listener.listen(8)
            diag_listener.listen(8)
            peer_listener.settimeout(5)
            diag_listener.settimeout(5)
            with concurrent.futures.ThreadPoolExecutor(max_workers=2) as pool:
                workers = [pool.submit(echo, peer_listener), pool.submit(echo, diag_listener)]
                rejected(peer, own, 18082)
                rejected(monitor, own, 19090)
                run(["ipset", "add", "R47_FRESH", own, "timeout", "3"])
                with connect(peer, own, 18082) as peer_flow, connect(monitor, own, 19090) as diag_flow:
                    rejected(foreign, own, 18082)
                    rejected(foreign, own, 19090)
                    time.sleep(4)
                    c.require(local.members(run(["ipset", "save", "R47_FRESH"]), "R47_FRESH", True) == [], "not expired")
                    run(["iptables", "-Z", "INPUT"])
                    run(["iptables", "-Z", "RACER_STAGE47"])
                    rejected(peer, own, 18082)
                    rejected(monitor, own, 19090)
                    counters = run(["iptables-save", "-c"])
                    gate = next(line for line in counters.splitlines() if "racer-stage47-fresh" in line)
                    jump = next(line for line in counters.splitlines() if "-j RACER_STAGE47" in line)
                    c.require(int(gate.split(":")[0][1:]) == 2 and jump.startswith("[0:0]"),
                              "expired NEW reached old jump")
                    # Explicitly not total revocation: these established flows
                    # continue through the unchanged source policy after expiry.
                    for connection in (peer_flow, diag_flow):
                        connection.sendall(b"alive")
                        c.require(connection.recv(5) == b"alive", "established flow changed")
                for worker in workers:
                    worker.result(timeout=5)
        print("PASS real TCP peer+diagnostic: fresh authorized passes, unauthorized rejects; "
              "expired NEW rejects before old jump; established flows continue (NOT revoked)", flush=True)
        reset()
        run(["ipset", "restore"], "create R47_FRESH hash:ip family inet maxelem 1 timeout 60\n")
        try:
            bootstrap.install(host, api.cm, policy, node, [])
            raise AssertionError("foreign partial accepted")
        except ValueError:
            pass
        c.require("RACER_STAGE47" not in run(["iptables", "-S"]), "foreign partial opened rules")
        print("PASS kernel foreign partial refusal", flush=True)
        reset()


if __name__ == "__main__":
    main()
