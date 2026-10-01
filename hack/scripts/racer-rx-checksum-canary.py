#!/usr/bin/env python3
# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0
"""Receiver-only RX checksum A/B/A. No cluster access on import or --help."""

import argparse
import contextlib
import datetime
import ipaddress
import json
import math
import os
from pathlib import Path
import re
import selectors
import signal
import subprocess
import sys
import time
import urllib.request

CONTEXT = "joolshev-scale-test"
NS = "unbounded-system"
NODE = "aks-adsv5-13731677-vmss00000w"
UID = "7de2d936-08ba-4676-9218-30eff4c83380"
SENDER = "8816d91d-e896-49bf-ba8a-da97ede93818"
IP = "10.224.4.69"
POD = "unbounded-net-node-z6z2d"
VF = "enP60223s1"
AUTH = UID + ":rx:on->off->on"
PROFILE = "historical-ddv5-eg-to-adsv5-w"
# Reviewed identities only. No arbitrary node, interface, or action flags.
PROFILES = {
    PROFILE: (NODE, UID, SENDER, IP, POD, VF),
    "ddv5-6o-to-adsv5-7n-20261001": (
        "aks-adsv5-13731677-vmss00007n",
        "4c4c2810-8c4d-49aa-8c2c-389f9e1728a4",
        "024467ab-e873-46d1-9d92-d15119f3ea28",
        "10.224.5.52", "unbounded-net-node-km9tz", "enP1216s1"),
}
CHECKPOINT = None
SIGNALS = (signal.SIGTERM, signal.SIGINT, signal.SIGHUP, signal.SIGALRM)


def select_profile(name):
    global PROFILE, NODE, UID, SENDER, IP, POD, VF, AUTH
    if name not in PROFILES:
        raise RuntimeError("unknown reviewed profile")
    NODE, UID, SENDER, IP, POD, VF = PROFILES[name]
    PROFILE = name
    AUTH = UID + ":rx:on->off->on"


def host_identity():
    """Read-only guard before any baseline or changed-stage settings work."""
    if command(["hostname"]).strip().lower() != NODE:
        raise RuntimeError("receiver hostname mismatch")
    addresses = json.loads(command(["ip", "-j", "-4", "address", "show", "dev", "eth0"]))
    if IP not in [a.get("local") for link in addresses for a in link.get("addr_info", [])]:
        raise RuntimeError("receiver host IP mismatch")
    for dev, expected in (("eth0", "hv_netvsc"), (VF, "mlx5_core")):
        info = command(["ethtool", "-i", dev])
        if f"driver: {expected}" not in info.splitlines():
            raise RuntimeError("receiver interface driver mismatch")
    # The reviewed VF must be the actual eth0 lower device, not another NIC.
    if not Path(f"/sys/class/net/eth0/lower_{VF}").is_dir():
        raise RuntimeError("receiver VF association mismatch")


@contextlib.contextmanager
def cleanup_signals():
    """Repeated TERM must not interrupt bounded restoration, even inside finally."""
    handlers = {sig: signal.signal(sig, signal.SIG_IGN) for sig in SIGNALS}
    try:
        yield
    finally:
        for sig, handler in handlers.items():
            signal.signal(sig, handler)


def emit(event, **fields):
    row = dict(utc=datetime.datetime.now(datetime.timezone.utc).isoformat(), event=event, **fields)
    text = json.dumps(row)
    if CHECKPOINT:
        with open(CHECKPOINT, "a") as out:
            out.write(text + "\n")
            out.flush()
            os.fsync(out.fileno())
    try:
        print(text, flush=True)
    except BrokenPipeError:
        sys.stdout = open(os.devnull, "w")


def command(argv, seconds=3, **kwargs):
    return subprocess.run(["timeout", "--signal=TERM", "--kill-after=10s",
                           f"{seconds}s", *argv], check=True, capture_output=True,
                          text=True, **kwargs).stdout


def kube(*args):
    return ["kubectl", "--context", CONTEXT, "--request-timeout=260s", *args]


def host_exec(*args):
    return kube("-n", NS, "exec", "-i", POD, "-c", "node", "--",
                "nsenter", "-t", "1", "-m", "-n", "--", *args)


def features(text):
    result = {}
    for line in text.splitlines()[1:]:
        match = re.fullmatch(r"\s*([\w-]+): (on|off)( \[fixed\])?", line)
        if not match:
            raise RuntimeError("unrecognized feature output")
        result[match[1]] = (match[2], bool(match[3]))
    if "rx-checksumming" not in result:
        raise RuntimeError("missing RX feature")
    return result


def check_features(original, current, rx):
    for dev in ("eth0", VF):
        expected = dict(original[dev])
        expected["rx-checksumming"] = (rx, original[dev]["rx-checksumming"][1])
        # JSON round trips turn tuples into lists.
        if json.dumps(current[dev], sort_keys=True) != json.dumps(expected, sort_keys=True):
            raise RuntimeError("RX readback/dependent feature change on " + dev)


def metrics(text):
    out = {}
    for line in text.splitlines():
        if line.startswith(("racer_loadgen_verified_bytes_total", "racer_loadgen_pull_failures_total",
                            "racer_loadgen_pulls_total", "racer_loadgen_applied_concurrency",
                            "racer_crypto_decrypt_", "process_cpu_seconds_total")):
            key, value = line.rsplit(None, 1)
            out[key] = float(value)
    return out


def ring(text):
    lines = text.splitlines()
    header = dict(re.findall(r"(\w+)=(\d+)", lines[0]))
    total = int(header["total"])
    records = []
    for line in lines[1:]:
        fields = dict(x.split("=", 1) for x in line.split())
        # Never export arbitrary response text or payloads.
        allowed = {k: v for k, v in fields.items() if k in
                   ("seq", "ms", "acquisition", "attempt", "remote", "page", "crc")}
        if len(allowed) != 7:
            raise RuntimeError("invalid AEAD record")
        records.append(allowed)
    return dict(total=total, records=records)


def http(ip, port, path):
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
    with opener.open(f"http://{ip}:{port}/{path}", timeout=1) as response:
        return response.read(1024 * 1024).decode()


class Host:
    def __init__(self, config):
        self.config = config

    def features(self):
        return {dev: features(command(["ethtool", "-k", dev])) for dev in ("eth0", VF)}

    def write(self, value):
        command(["ethtool", "-K", "eth0", "rx", value])

    def snapshot(self):
        start = time.monotonic()
        stats = command(["ethtool", "-S", VF])
        nic = {k: int(v) for k, v in re.findall(r"^\s*(rx_(?:packets|bytes|csum_\w+)): (\d+)\s*$", stats, re.M)}
        snmp = Path("/proc/net/snmp").read_text().splitlines()
        tcp = {}
        for a, b in zip(snmp, snmp[1:]):
            if a.startswith("Tcp:") and b.startswith("Tcp:"):
                tcp = dict(zip(a.split()[1:], map(int, b.split()[1:])))
                break
        row = dict(mono=start, ms=int(time.time() * 1000), features=self.features(), nic=nic,
                   tcp_csum=tcp["InCsumErrors"], cpu=Path("/proc/stat").read_text().splitlines()[0],
                   health=http(IP, 19090, "healthz").strip(), ready=http(IP, 19090, "readyz").strip(),
                   load=metrics(http(self.config["load_ip"], 9090, "metrics")),
                   dataplane=metrics(http(IP, 19090, "metrics")), aead=ring(http(IP, 19090, "debug/aead")))
        if time.monotonic() - start > 15:
            raise RuntimeError("snapshot exceeded checkpoint budget")
        return row


def safety(previous, row, concurrency):
    if row["health"] != "ok" or row["ready"] != "ready":
        raise RuntimeError("health/readiness loss")
    if row["load"].get("racer_loadgen_applied_concurrency") != concurrency:
        raise RuntimeError("load changed")
    if "racer_loadgen_verified_bytes_total" not in row["load"]:
        raise RuntimeError("missing verified bytes")
    for key in ("racer_crypto_decrypt_aead_rejected_total", "racer_crypto_decrypt_crc_rejected_total"):
        if key not in row["dataplane"]:
            raise RuntimeError("missing integrity counter " + key)
    for group in ("load", "dataplane", "nic"):
        if any(not math.isfinite(value) for value in row[group].values()):
            raise RuntimeError("nonfinite metric")
    for key in ("rx_packets", "rx_bytes", "rx_csum_none", "rx_csum_complete", "rx_csum_unnecessary"):
        if key not in row["nic"]:
            raise RuntimeError("missing NIC counter " + key)
    if previous:
        for group in ("load", "dataplane", "nic"):
            for key, value in previous[group].items():
                if key not in row[group] or row[group][key] < value:
                    raise RuntimeError("counter reset/missing " + key)
            for key, value in row[group].items():
                if "digest_mismatch" in key and value > previous[group].get(key, 0):
                    raise RuntimeError("digest mismatch")
        if row["aead"]["total"] < previous["aead"]["total"]:
            raise RuntimeError("AEAD ring reset")
        if row["tcp_csum"] < previous["tcp_csum"]:
            raise RuntimeError("TCP checksum counter reset")
    # AEAD rejects and ordinary pull errors are measurements, NOT abort triggers.


def stage(host, name, seconds, original, rx, concurrency, report=emit,
          clock=time.monotonic, sleep=time.sleep, state=None):
    start = clock()
    state = state if state is not None else {}
    first = None
    previous = state.get("previous")
    seen = {tuple(x) for x in state.get("seen", [])}
    fresh = 0
    overwritten = False
    while True:
        row = host.snapshot()
        check_features(original, row["features"], rx)
        safety(previous, row, concurrency)
        if first is None:
            first = row
        if previous and row["aead"]["total"] - previous["aead"]["total"] > 64:
            overwritten = True
        new = []
        for record in row["aead"]["records"]:
            identity = (record["acquisition"], record["attempt"])
            if (record["remote"] == SENDER and record["acquisition"] != "none"
                    and int(record["seq"]) > first["aead"]["total"]
                    and int(record["ms"]) >= first["ms"] and identity not in seen):
                fresh += 1
                new.append(record)
            seen.add(identity)
        report("checkpoint", stage=name, data=row, fresh_pair_records=new)
        previous = row
        state.update(previous=row, seen=list(seen))
        if clock() >= start + seconds:
            break
        sleep(min(max(0, start + seconds - clock()), max(0, 20 - (clock() - row["mono"]))))
    summary = dict(stage=name, seconds=row["mono"] - first["mono"], fresh_pair_rejects=fresh,
                   ring_gap=overwritten, nic_delta={k: v - first["nic"][k] for k, v in row["nic"].items()},
                   tcp_csum_delta=row["tcp_csum"] - first["tcp_csum"],
                   verified_bytes=row["load"]["racer_loadgen_verified_bytes_total"] - first["load"]["racer_loadgen_verified_bytes_total"])
    report("summary", **summary, state=state)
    return summary


def changed(host, config, report=emit, stage_fn=stage):
    original = config["original"]
    check_features(original, host.features(), "on")
    state = config.get("state", {})
    # Check safety again before mutation, including changes since baseline ended.
    if state:
        current = host.snapshot()
        check_features(original, current["features"], "on")
        safety(state["previous"], current, config["concurrency"])
        state["previous"] = current
    try:
        report("before_mutation", command="ethtool -K eth0 rx off", next="verify both interfaces")
        if time.time() > config["not_after"]:
            raise RuntimeError("mutation launch expired")
        host.write("off")
        check_features(original, host.features(), "off")
        report("after_mutation", error=None, next="off checkpoints")
        stage_fn(host, "off", 90, original, "off", config["concurrency"], report=report, state=state)
    finally:
        # Cleanup must not depend on a functioning output stream/journal.
        with cleanup_signals():
            host.write("on")
            check_features(original, host.features(), "on")
    report("restored", error=None, next="independent parent readback", state=state)


def stop(signum, frame):
    raise RuntimeError("signal " + str(signum))


def remote_shell(mutate):
    script = hostname_guard()
    if mutate:
        # This shell's timer does not depend on Python polling or HTTP progress.
        script += '''cleanup() {
  trap '' TERM INT HUP
  test -z "$watchdog" || kill -TERM "$watchdog" 2>/dev/null || :
  test -z "$watchdog" || wait "$watchdog" 2>/dev/null || :
  timeout --signal=TERM --kill-after=10s 3s ethtool -K eth0 rx on >&2
  if test -n "$child"; then
    kill -TERM "$child" 2>/dev/null || :
    wait "$child" 2>/dev/null || :
  fi
  timeout --signal=TERM --kill-after=10s 3s ethtool -K eth0 rx on >&2
}
trap cleanup EXIT
trap 'exit 143' TERM INT HUP
python3 -u -B -c "$@" &
child=$!
python3 -B -c 'import time,subprocess; time.sleep(110); subprocess.run(["timeout","--signal=TERM","--kill-after=10s","3s","ethtool","-K","eth0","rx","on"],check=True)' </dev/null >/dev/null 2>&1 &
watchdog=$!
wait "$child"
rc=$?
child=
kill "$watchdog" 2>/dev/null || :
wait "$watchdog" 2>/dev/null || :
exit "$rc"
'''
    else:
        script += 'python3 -u -B -c "$@"\n'
    return script


def hostname_guard():
    # Azure hostnames can preserve an uppercase VMSS suffix; Kubernetes node
    # names are lowercase. Compare DNS names case-insensitively in both paths.
    return (f'test "$(hostname | LC_ALL=C tr A-Z a-z)" = {NODE} || '
            '{ printf "rx-canary: hostname mismatch\\n" >&2; exit 41; }\n')


def safe_stderr(line):
    """Retain exact allowlisted diagnostics, never arbitrary remote output."""
    text = line.decode("utf-8", errors="replace").strip()
    if re.fullmatch(r"command terminated with exit code \d{1,3}", text):
        return text
    if text == "rx-canary: hostname mismatch":
        return text
    if re.fullmatch(r"(?:sh: \d+: )?(?:python3|timeout|ethtool|nsenter): (?:not found|Permission denied)", text):
        return text
    return None


def preflight(concurrency):
    node = json.loads(command(kube("get", "node", NODE, "-o", "json"), 5))
    if (node["metadata"]["name"] != NODE or node["metadata"]["uid"] != UID
            or node["metadata"].get("deletionTimestamp")
            or IP not in [a["address"] for a in node["status"]["addresses"] if a["type"] == "InternalIP"]):
        raise RuntimeError("receiver identity mismatch")
    pods = json.loads(command(kube("-n", NS, "get", "pods", "--field-selector", "spec.nodeName=" + NODE,
                                   "-o", "json"), 5))["items"]
    access = [p for p in pods if p["metadata"]["name"] == POD and p["status"]["phase"] == "Running"
              and not p["metadata"].get("deletionTimestamp")]
    loads = [p for p in pods if p["metadata"].get("labels", {}).get("app.kubernetes.io/name") == "racer-loadgen"
              and p["status"]["phase"] == "Running" and not p["metadata"].get("deletionTimestamp")]
    planes = [p for p in pods if p["metadata"].get("labels", {}).get("app.kubernetes.io/name") == "racer-dataplane"
              and p["status"]["phase"] == "Running" and not p["metadata"].get("deletionTimestamp")]
    if len(access) != 1 or len(loads) != 1 or len(planes) != 1:
        raise RuntimeError("access/load/dataplane pod identity mismatch")
    for pod in (access[0], loads[0], planes[0]):
        if pod["spec"]["nodeName"] != NODE:
            raise RuntimeError("pod node mismatch")
        if not any(c["type"] == "Ready" and c["status"] == "True"
                    for c in pod["status"].get("conditions", [])):
            raise RuntimeError("access/load/dataplane pod not ready")
    if (access[0]["status"]["podIP"] != IP or planes[0]["status"]["podIP"] != IP
            or not access[0]["spec"].get("hostNetwork")
            or not any(c["name"] == "node" for c in access[0]["spec"]["containers"])):
        raise RuntimeError("access/dataplane endpoint mismatch")
    ip = str(ipaddress.ip_address(loads[0]["status"]["podIP"]))
    return dict(load_ip=ip, concurrency=concurrency, profile=PROFILE)


def remote(config, mode, seconds):
    config = dict(config, mode=mode, profile=PROFILE)
    if mode == "changed":
        if config.get("authorize") != AUTH:
            raise RuntimeError("selected receiver mutation not authorized")
    argv = host_exec("timeout", "--signal=TERM", "--kill-after=10s", f"{seconds}s", "sh", "-c",
                     remote_shell(mode == "changed"), "rx-canary", Path(__file__).read_text(),
                     "--remote", json.dumps(config))
    return stream(argv, seconds + 5)


def stream(argv, seconds):
    """No stdin writer/thread to deadlock; source is an argument, not a payload."""
    proc = subprocess.Popen(["timeout", "--signal=TERM", "--kill-after=10s", f"{seconds}s", *argv],
                            stdout=subprocess.PIPE, stderr=subprocess.STDOUT, start_new_session=True)
    rows, pending = [], b""
    suppressed = 0
    deadline = time.monotonic() + seconds + 10
    try:
        with selectors.DefaultSelector() as selector:
            selector.register(proc.stdout, selectors.EVENT_READ)
            while True:
                if time.monotonic() >= deadline:
                    raise RuntimeError("exec stream deadline")
                if not selector.select(1):
                    continue
                chunk = os.read(proc.stdout.fileno(), 65536)
                if not chunk:
                    break
                pending += chunk
                if len(pending) > 2 * 1024 * 1024:
                    raise RuntimeError("oversized diagnostic line")
                while b"\n" in pending:
                    line, pending = pending.split(b"\n", 1)
                    try:
                        row = json.loads(line)
                    except ValueError:
                        diagnostic = safe_stderr(line)
                        if diagnostic:
                            emit("remote_stderr", diagnostic=diagnostic)
                        else:
                            suppressed += 1
                        continue
                    rows.append(row)
                    emit("remote", record=row)
            status = proc.wait(timeout=1)
            if pending:
                diagnostic = safe_stderr(pending)
                if diagnostic:
                    emit("remote_stderr", diagnostic=diagnostic)
                else:
                    suppressed += 1
            if status != 0 or pending:
                emit("remote_exit", status=status, incomplete=bool(pending),
                     suppressed_stderr_lines=suppressed)
                raise RuntimeError(f"remote failed or incomplete output: exit={status}; "
                                   f"suppressed_stderr_lines={suppressed}")
        return rows
    finally:
        with cleanup_signals():
            if proc.poll() is None:
                os.killpg(proc.pid, signal.SIGTERM)
                try:
                    proc.wait(timeout=2)
                except subprocess.TimeoutExpired:
                    os.killpg(proc.pid, signal.SIGKILL)
                    proc.wait(timeout=2)
            proc.stdout.close()


def independent_restore(original):
    # A failed journal must not suppress the independent write.
    command(host_exec("timeout", "--signal=TERM", "--kill-after=10s", "5s", "sh", "-c",
                      hostname_guard() + 'ethtool -K eth0 rx on'), 7)
    current = {dev: features(command(host_exec("ethtool", "-k", dev), 5)) for dev in ("eth0", VF)}
    check_features(original, current, "on")
    emit("independent_restored", error=None)


def run(args):
    started = time.monotonic()
    config = preflight(args.concurrency)
    rows = remote(config, "baseline", 60)
    original = next(r["original"] for r in rows if r["event"] == "original")
    summary = next(r for r in rows if r["event"] == "summary")
    if (not summary["fresh_pair_rejects"] or summary["ring_gap"] or summary["verified_bytes"] <= 0
            or summary["nic_delta"]["rx_bytes"] <= 0):
        emit("inconclusive", reason="baseline recurrence/exposure absent or ring gap", mutation=False)
        return
    if not summary.get("state"):
        raise RuntimeError("missing baseline safety state")
    # Fail before entering any restoration path if the selected identity changed.
    if preflight(args.concurrency) != config:
        raise RuntimeError("preflight identity changed after baseline")
    config.update(original=original, state=summary["state"],
                  not_after=time.time() + 20, authorize=AUTH)
    launched = time.monotonic()
    completed = False
    try:
        changed_rows = remote(config, "changed", 115)
        completed = True
        config["state"] = next(r["state"] for r in changed_rows if r["event"] == "restored")
    finally:
        with cleanup_signals():
            # On ambiguous disconnect, wait out the remote timeout + kill grace.
            # No late watchdog or still-running collector can race this restore.
            if not completed:
                deadline = launched + 130
                while time.monotonic() < deadline:
                    try:
                        emit("recovery_wait", next="independent restore", remaining=deadline-time.monotonic())
                    except OSError:
                        pass
                    time.sleep(min(20, max(0, deadline-time.monotonic())))
            independent_restore(original)
    if time.monotonic() - started > 145:
        emit("inconclusive", reason="restored observation budget exhausted; RX restored")
        return
    remote(config, "restored", 60)
    emit("complete", verdict="measurement_only_not_a_fix; require_pair_exposure_and_return_in_A")


def main():
    global CHECKPOINT
    for sig in SIGNALS:
        signal.signal(sig, stop)
    if len(sys.argv) == 3 and sys.argv[1] == "--remote":
        config = json.loads(sys.argv[2])
        try:
            select_profile(config["profile"])
            if config["mode"] == "changed" and config.get("authorize") != AUTH:
                raise RuntimeError("remote mutation not authorized")
            host_identity()
            host = Host(config)
            if config["mode"] == "baseline":
                original = host.features()
                if any(original[d]["rx-checksumming"] != ("on", False) for d in original):
                    raise RuntimeError("original RX must be on and mutable")
                emit("original", original=original)
                stage(host, "baseline", 45, original, "on", config["concurrency"])
            elif config["mode"] == "restored":
                stage(host, "restored", 45, config["original"], "on", config["concurrency"], state=config["state"])
            elif config["mode"] == "changed":
                if config.get("authorize") != AUTH:
                    raise RuntimeError("remote mutation not authorized")
                changed(host, config)
            else:
                raise RuntimeError("invalid remote mode")
        except Exception as error:
            # Never stringify CalledProcessError: it embeds argv (source/config).
            fields = dict(kind=type(error).__name__)
            if isinstance(error, RuntimeError):
                fields["diagnostic"] = str(error)[:256]
            if isinstance(error, subprocess.CalledProcessError):
                fields["exit"] = error.returncode
                safe = [safe_stderr(line.encode()) for line in (error.stderr or "").splitlines()]
                fields["stderr"] = [line for line in safe if line]
            if isinstance(error, OSError):
                fields["errno"] = error.errno
            emit("remote_error", **fields)
            raise SystemExit(1) from None
        return
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--profile", choices=tuple(PROFILES), default=PROFILE)
    parser.add_argument("--authorize", required=True)
    parser.add_argument("--concurrency", required=True, type=int)
    parser.add_argument("--checkpoint", required=True)
    args = parser.parse_args()
    select_profile(args.profile)
    if args.authorize != AUTH or not 1 <= args.concurrency <= 64:
        parser.error("requires fixed receiver authorization and concurrency 1..64")
    root = Path(__file__).resolve().parents[2]
    CHECKPOINT = (root / args.checkpoint).resolve()
    if not CHECKPOINT.is_relative_to(root / "tmp") or not CHECKPOINT.parent.is_dir():
        parser.error("checkpoint must be in existing worktree tmp directory")
    emit("start", profile=PROFILE, receiver_uid=UID, next="read-only baseline", error=None)
    # Reserve recovery time before the required external 300s TERM timeout.
    signal.alarm(210)
    try:
        run(args)
    finally:
        signal.alarm(0)


if __name__ == "__main__":
    main()
