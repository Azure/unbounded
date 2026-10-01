#!/usr/bin/env python3
# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0
"""Bounded one-host TSO experiment. No cluster access on import or --help.

Run through an external 300s TERM timeout. See designs/racer-tso-canary-helper.md.
Only canary mode writes a host setting; collect never restores or writes it.
"""

import argparse
import concurrent.futures
import datetime
import json
import os
from pathlib import Path
import re
import shlex
import signal
import subprocess
import sys
import threading
import time
import urllib.request


CONTEXT = "joolshev-scale-test"
NAMESPACE = "unbounded-system"
PREFIX = "aks-ddsv6-84072342-vmss"
HOSTS = {
    "bad": (PREFIX + "0000bl", "unbounded-net-node-57j4m", "10.224.3.206"),
    "control": (PREFIX + "00002b", "unbounded-net-node-2kssd", "10.224.2.121"),
}
SYSCTL = "net.ipv4.tcp_min_tso_segs"
SYSCTL_PATH = Path("/proc/sys/net/ipv4/tcp_min_tso_segs")
AUTHORIZATION = "0000bl:tcp_min_tso_segs:2->32->2"
OUTPUT_LOCK = threading.Lock()
CHECKPOINT = None


def utc():
    return datetime.datetime.now(datetime.timezone.utc).isoformat()


def emit(event, **fields):
    with OUTPUT_LOCK:
        row = dict(utc=utc(), event=event, **fields)
        if CHECKPOINT is not None and (event != "remote" or fields.get("record", {}).get("event") != "snapshot"):
            try:
                with CHECKPOINT.open("a") as checkpoint:
                    checkpoint.write("\n- " + json.dumps(row) + "\n")
                    checkpoint.flush()
                    os.fsync(checkpoint.fileno())
            except OSError:
                # Journal failure must never prevent restoration. Stdout still records it.
                row["checkpoint_error"] = "checkpoint append failed; parent must verify recovery"
        try:
            print(json.dumps(row), flush=True)
        except BrokenPipeError:
            # Never let a disconnected observer prevent remote/parent restoration.
            sys.stdout = open(os.devnull, "w")


def command(argv, seconds=5, **kwargs):
    """External timeout bounds commands and their process groups, including SSH/exec."""
    return subprocess.run(
        ["timeout", "--signal=TERM", "--kill-after=10s", str(seconds) + "s", *argv],
        check=True, text=True, capture_output=True, **kwargs,
    ).stdout


def kubectl(*args):
    return ["kubectl", "--context", CONTEXT, "--request-timeout=10s", *args]


def host_exec(role, *args):
    return ["kubectl", "--context", CONTEXT, "--request-timeout=220s",
            "-n", NAMESPACE, "exec", "-i", HOSTS[role][1], "-c", "node",
            "--", "nsenter", "-t", "1", "-m", "-n", "--", *args]


def http(ip, port, endpoint):
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
    with opener.open(f"http://{ip}:{port}/{endpoint}", timeout=2) as response:
        return response.read(4 * 1024 * 1024).decode()


def metrics(text):
    result = {}
    for line in text.splitlines():
        if not line or line.startswith("#"):
            continue
        key, value = line.rsplit(None, 1)
        if key.startswith(("racer_loadgen_", "racer_peer_page_", "racer_crypto_decrypt_",
                           "racer_integrity_", "racer_fill_", "racer_ready")):
            result[key] = float(value)
    return result


def safety(previous, current, concurrency):
    """Fail closed on resets, missing load gauges, failures, and integrity increments."""
    for role in HOSTS:
        app = current[role]
        if app["health"] != "ok" or app["ready"] != "ready":
            raise RuntimeError(role + " health/readiness failed")
        m = app["load"]
        if m.get("racer_loadgen_applied_concurrency") != concurrency:
            raise RuntimeError(role + " applied concurrency changed/missing")
        for name in ("racer_loadgen_verified_bytes_total", "racer_loadgen_in_flight"):
            if name not in m:
                raise RuntimeError(role + " required load metric missing: " + name)
        for name in ("racer_crypto_decrypt_aead_rejected_total", "racer_crypto_decrypt_crc_rejected_total",
                     "racer_peer_page_body_nanoseconds_count", "racer_peer_page_body_nanoseconds_sum"):
            if name not in app["dataplane"]:
                raise RuntimeError(role + " required diagnostic metric missing: " + name)
        if previous is None:
            continue
        for group in ("load", "dataplane"):
            before, after = previous[role][group], app[group]
            for key, value in before.items():
                if key not in after:
                    raise RuntimeError(role + " metric disappeared: " + key)
                if ("_total" in key or "_nanoseconds_" in key) and after[key] < value:
                    raise RuntimeError(role + " counter reset: " + key)
            for key, value in after.items():
                failure = ("reject" in key or "corrupt" in key or "integrity" in key
                           or "digest_mismatch" in key
                           or (key.startswith("racer_loadgen_pulls_total")
                               and 'result="success"' not in key))
                if failure and value > before.get(key, 0):
                    raise RuntimeError(role + " new failure/integrity event: " + key)


def tcp_summary(text):
    rtts = sorted(float(v) for v in re.findall(r"\brtt:([0-9.]+)/", text))
    memory = re.findall(r"skmem:\(([^)]+)\)", text)
    charges = [dict((k, int(v)) for k, v in re.findall(r"([a-z]+)(\d+)", x))
               for x in memory]
    return {
        "rtt_samples": len(rtts),
        "rtt_p50_ms": rtts[len(rtts) // 2] if rtts else None,
        "rtt_p95_ms": rtts[min(len(rtts) - 1, int(len(rtts) * .95))] if rtts else None,
        "memory": {k: sum(x.get(k, 0) for x in charges) for k in ("t", "tb", "w")},
        "notsent": sum(int(x) for x in re.findall(r"\bnotsent:(\d+)", text)),
        "retrans_open_socket_lifetime": sum(int(x) for x in re.findall(r"\bretrans:\d+/(\d+)", text)),
    }


def summarize(first, last, rows):
    seconds = last["mono"] - first["mono"]
    if seconds <= 0:
        raise RuntimeError("nonpositive sample interval")
    delta = {k: v - first["nic"].get(k, v) for k, v in last["nic"].items()}
    if any(v < 0 for v in delta.values()):
        raise RuntimeError("NIC counters reset")
    tso_count = sum(v for k, v in delta.items() if re.fullmatch(r"tx_\d+_tso_packets", k))
    tso_bytes = sum(v for k, v in delta.items() if re.fullmatch(r"tx_\d+_tso_bytes", k))
    cq = sum(last["queues"][k]["cq_head"] - v["cq_head"] for k, v in first["queues"].items())
    if cq < 0:
        raise RuntimeError("CQ counter reset or wrap; refuse interval")
    tx = delta.get("hc_tx_bytes", 0)
    apps = {}
    for role in first["apps"]:
        a, b = first["apps"][role], last["apps"][role]
        count = b["dataplane"].get("racer_peer_page_body_nanoseconds_count", 0) - a["dataplane"].get("racer_peer_page_body_nanoseconds_count", 0)
        ns = b["dataplane"].get("racer_peer_page_body_nanoseconds_sum", 0) - a["dataplane"].get("racer_peer_page_body_nanoseconds_sum", 0)
        verified = b["load"]["racer_loadgen_verified_bytes_total"] - a["load"]["racer_loadgen_verified_bytes_total"]
        apps[role] = dict(body_samples=count, body_mean_ms=ns / count / 1e6 if count else None,
                          verified_bytes=verified, verified_gbps=verified * 8 / seconds / 1e9)
    return dict(seconds=seconds, nic_delta=delta, cq_delta=cq,
                cq_per_GB=cq * 1e9 / tx if tx else None,
                tso_mean_bytes=tso_bytes / tso_count if tso_count else None,
                pending_mean=sum(sum(q["sq_pend_skb_qlen"] for q in r["queues"].values()) for r in rows) / len(rows),
                apps=apps)


class Host:
    def __init__(self, role, load_ips):
        self.role, self.load_ips = role, load_ips

    def read(self):
        return int(SYSCTL_PATH.read_text().strip())

    def write(self, value):
        command(["sysctl", "-w", f"{SYSCTL}={value}"])
        if self.read() != value:
            raise RuntimeError("sysctl readback failed")

    def snapshot(self):
        start = time.monotonic()
        nic = command(["ethtool", "-S", "ens1"])
        queues = {}
        for path in sorted(Path("/sys/kernel/debug/mana").glob("*/vport0/TX-*")):
            queues[path.name] = {key: int((path / key).read_text()) for key in
                                 ("sq_head", "sq_tail", "sq_pend_skb_qlen", "cq_head")}
        if len(queues) != 8:
            raise RuntimeError("expected eight MANA TX queues")
        # Only the canary host checks both applications. Control still records its host.
        apps = {}
        if self.role == "bad":
            for role, (_, _, ip) in HOSTS.items():
                apps[role] = dict(
                    health=http(ip, 19090, "healthz").strip(),
                    ready=http(ip, 19090, "readyz").strip(),
                    dataplane=metrics(http(ip, 19090, "metrics")),
                    load=metrics(http(self.load_ips[role], 9090, "metrics")),
                )
        return dict(
            mono=start, utc=utc(), setting=self.read(), queues=queues, apps=apps,
            nic={k: int(v) for k, v in re.findall(r"^\s*(\w+):\s*(\d+)\s*$", nic, re.M)},
            qdisc=json.loads(command(["tc", "-j", "-s", "qdisc", "show", "dev", "ens1"])),
            tcp=tcp_summary(command(["ss", "-tinmH", "state", "established", "(",
                                     "sport", "=", ":18082", "or", "dport", "=", ":18082", ")"])),
            snmp=Path("/proc/net/snmp").read_text(),
            netstat=Path("/proc/net/netstat").read_text(),
            softnet=Path("/proc/net/softnet_stat").read_text(),
            cpu=Path("/proc/stat").read_text(),
        )


def experiment(host, mode, concurrency, report=emit, clock=time.monotonic, sleep=time.sleep):
    """Fixed stages. Restore is attempted even if mutation acknowledgement is lost."""
    if host.read() != 2:
        raise RuntimeError("refuse experiment: original setting is not 2")
    armed = False
    previous = None

    def stage(name, seconds, expected):
        nonlocal previous
        deadline = clock() + seconds
        rows = []
        while True:
            row = host.snapshot()
            rows.append(row)
            report("snapshot", role=host.role, stage=name, data=row)
            if row["setting"] != expected:
                raise RuntimeError("concurrent setting change")
            if host.role == "bad":
                safety(previous, row["apps"], concurrency)
                previous = row["apps"]
            if clock() >= deadline:
                report("stage_summary", role=host.role, stage=name,
                       data=summarize(rows[0], rows[-1], rows))
                return
            sleep(min(5, max(0, deadline - clock())))

    try:
        stage("baseline", 30, 2)
        if mode == "canary" and host.role == "bad":
            report("before_write", command=["sysctl", "-w", SYSCTL + "=32"], next="changed")
            armed = True
            host.write(32)
            report("after_write", value=32, error=None)
        stage("changed" if mode == "canary" else "observe", 60,
              32 if armed else 2)
    finally:
        if armed:
            # Cleanup cannot depend on a working stdout pipe.
            try:
                report("before_restore", command=["sysctl", "-w", SYSCTL + "=2"], next="readback")
            finally:
                host.write(2)
            report("restored", value=2, error=None)
    stage("after", 30, 2)
    report("complete", role=host.role, verdict="measurement_only_not_automatic_success")


def remote_shell(role, mode):
    """Backup EXIT/TERM restoration lives outside the Python collector."""
    check = f'test "$(cat /proc/sys/kernel/hostname | tr A-Z a-z)" = {shlex.quote(HOSTS[role][0])} || exit 41\n'
    check += 'test "$(cat /proc/sys/net/ipv4/tcp_min_tso_segs)" = 2 || exit 42\n'
    if role == "bad" and mode == "canary":
        check += """cleanup() {
  trap '' TERM INT HUP
  timeout --signal=TERM --kill-after=10s 5s sysctl -w net.ipv4.tcp_min_tso_segs=2 >&2
  test "$(cat /proc/sys/net/ipv4/tcp_min_tso_segs)" = 2 || exit 43
}
trap cleanup EXIT
trap 'exit 143' TERM INT HUP
"""
    return check + 'python3 -u - "$@"\n'


def remote_main(config):
    def terminated(signum, frame):
        raise RuntimeError("remote signal " + str(signum))
    for sig in (signal.SIGTERM, signal.SIGINT, signal.SIGHUP):
        signal.signal(sig, terminated)
    experiment(Host(config["role"], config["load_ips"]), config["mode"], config["concurrency"])


def preflight(concurrency):
    load_ips = {}
    for role, (node, pod, _) in HOSTS.items():
        data = json.loads(command(kubectl("-n", NAMESPACE, "get", "pod", pod, "-o", "json"), 15))
        if data["spec"]["nodeName"] != node or data["status"]["phase"] != "Running":
            raise RuntimeError("host-access pod identity/status mismatch")
        pods = json.loads(command(kubectl("-n", NAMESPACE, "get", "pods", "--field-selector",
                                         "spec.nodeName=" + node, "-o", "json"), 15))["items"]
        loads = [p for p in pods if p["metadata"]["labels"].get("app.kubernetes.io/name") == "racer-loadgen"
                 and p["status"]["phase"] == "Running" and not p["metadata"].get("deletionTimestamp")]
        if len(loads) != 1:
            raise RuntimeError("expected one live loadgen on " + node)
        load_ips[role] = loads[0]["status"]["podIP"]
        original = command(host_exec(role, "timeout", "--signal=TERM", "--kill-after=10s", "5s",
                                     "sysctl", "-n", SYSCTL), 15).strip()
        if original != "2":
            raise RuntimeError("original sysctl differs from 2 on " + role)
    query = f'count(min by(node)(racer_loadgen_applied_concurrency{{job="kubernetes-pods",namespace="{NAMESPACE}",node!=""}})=={concurrency})'
    import urllib.parse
    url = "/api/v1/namespaces/monitoring/services/prometheus:9090/proxy/api/v1/query?" + urllib.parse.urlencode({"query": query})
    result = json.loads(command(kubectl("get", "--raw", url), 15))["data"]["result"]
    if len(result) != 1 or float(result[0]["value"][1]) != 1500:
        raise RuntimeError("fleet applied-concurrency gate is not 1500")
    emit("preflight", concurrency=concurrency, load_ips=load_ips, error=None)
    return load_ips


def independent_restore():
    script = ('test "$(cat /proc/sys/kernel/hostname | tr A-Z a-z)" = ' + HOSTS["bad"][0] +
              ' || exit 41; v=$(cat /proc/sys/net/ipv4/tcp_min_tso_segs); '
              'case "$v" in 2) ;; 32) sysctl -w net.ipv4.tcp_min_tso_segs=2 >&2;; *) exit 44;; esac; '
              'test "$(cat /proc/sys/net/ipv4/tcp_min_tso_segs)" = 2')
    argv = host_exec("bad", "timeout", "--signal=TERM", "--kill-after=10s", "10s", "sh", "-c", script)
    emit("before_independent_restore", command=argv, next="readback")
    command(argv, 15)
    value = command(host_exec("bad", "timeout", "--signal=TERM", "--kill-after=10s", "5s",
                              "sysctl", "-n", SYSCTL), 8).strip()
    if value != "2":
        raise RuntimeError("independent restoration verification failed")
    emit("independent_restored", value=2, error=None)


def run(args):
    def preflight_timeout(signum, frame):
        raise RuntimeError("preflight exceeded 30 seconds; no remote experiment launched")
    old_handler = signal.signal(signal.SIGALRM, preflight_timeout)
    signal.alarm(30)
    try:
        load_ips = preflight(args.concurrency)
    finally:
        signal.alarm(0)
        signal.signal(signal.SIGALRM, old_handler)
    source = Path(__file__).read_text()
    futures = []
    started = time.monotonic()
    failure = None

    def worker(role):
        config = dict(role=role, mode=args.mode, load_ips=load_ips, concurrency=args.concurrency)
        argv = host_exec(role, "timeout", "--signal=TERM", "--kill-after=10s", "180s",
                         "sh", "-c", remote_shell(role, args.mode), "racer-tso", "--remote", json.dumps(config))
        emit("launch", role=role, command=argv, remote_deadline_seconds=180)
        # Stream checkpoints; timeout kills the local process group on cancellation.
        with subprocess.Popen(["timeout", "--signal=TERM", "--kill-after=10s", "200s", *argv],
                              stdin=subprocess.PIPE, stdout=subprocess.PIPE,
                              stderr=subprocess.STDOUT, text=True) as proc:
            proc.stdin.write(source)
            proc.stdin.close()
            for line in proc.stdout:
                try:
                    record = json.loads(line)
                except ValueError:
                    record = {"text": line.rstrip()}
                emit("remote", role=role, record=record)
            if proc.wait() != 0:
                raise RuntimeError(role + " remote failed")

    # Remote timeout + shell trap remain active if local exec is disconnected.
    try:
        with concurrent.futures.ThreadPoolExecutor(max_workers=2) as pool:
            futures = [pool.submit(worker, role) for role in HOSTS]
            while not all(f.done() for f in futures):
                emit("heartbeat", command="remote fixed-stage capture", elapsed=time.monotonic() - started)
                time.sleep(5)
            for future in futures:
                future.result()
    except BaseException as error:
        failure = error
    finally:
        if args.mode == "canary":
            # On an ambiguous disconnect, do not race a remote still in baseline that
            # might subsequently write 32. Wait out its bounded lifetime first.
            if failure:
                while time.monotonic() - started < 210:
                    emit("rollback_wait", next="independent_restore", elapsed=time.monotonic() - started)
                    time.sleep(5)
            independent_restore()
    if failure:
        raise failure


def main():
    global CHECKPOINT
    if len(sys.argv) == 3 and sys.argv[1] == "--remote":
        remote_main(json.loads(sys.argv[2]))
        return
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("mode", choices=("collect", "canary"))
    parser.add_argument("--concurrency", type=int, required=True)
    parser.add_argument("--authorize", default="")
    parser.add_argument("--checkpoint", default="tmp/racer-tso-canary-checkpoint.md")
    args = parser.parse_args()
    if args.concurrency < 1 or args.concurrency > 64:
        parser.error("concurrency must be 1..64")
    if args.mode == "canary" and args.authorize != AUTHORIZATION:
        parser.error("canary requires --authorize " + AUTHORIZATION)
    root = Path(__file__).resolve().parents[2]
    CHECKPOINT = (root / args.checkpoint).resolve()
    if not CHECKPOINT.is_relative_to(root / "tmp") or not CHECKPOINT.parent.is_dir():
        parser.error("checkpoint must be under this worktree existing tmp directory")
    # Refuse before any cluster access if the journal cannot be opened.
    with CHECKPOINT.open("a"):
        pass
    def stop(signum, frame):
        raise RuntimeError("parent signal " + str(signum))
    for sig in (signal.SIGTERM, signal.SIGINT, signal.SIGHUP):
        signal.signal(sig, stop)
    emit("start", mode=args.mode, command=sys.argv, next="preflight")
    try:
        run(args)
    except BaseException as error:
        emit("failed", error=str(error), next="parent_verify_recovery")
        raise SystemExit(1) from error
    emit("finished", mode=args.mode, error=None, next="parent_review_not_auto_promote")


if __name__ == "__main__":
    main()
