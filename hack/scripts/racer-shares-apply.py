#!/usr/bin/env python3
# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0
"""Explicitly authorized, C0-only, resumable shares phases. Never resumes load."""
import argparse
import concurrent.futures as futures
from datetime import datetime, timezone
import hashlib
import json
import math
import os
from pathlib import Path
import re
import signal
import subprocess
import sys
import threading
import time
from urllib.parse import urlencode

CONTEXT = "joolshev-scale-test"
SHA = "65ac1f3576631495585aebb42374fef3bedeea6ba82179f31ed0b57b9dc9ba47"
BASELINE_SHA = "2d2d84a5552e20972e8b52b32a2be180e3c9bcabc7c5fed35b4f29bce26aa722"
KEY = "racer.unbounded-cloud.io/shares"
ENROLLED = "racer.unbounded-cloud.io/enrolled-shares"
EXCLUDE = "racer.unbounded-cloud.io/exclude"
PTR = "/metadata/annotations/racer.unbounded-cloud.io~1shares"
GAUGES = (
    ("racer_loadgen_applied_concurrency", "racer-loadgen"),
    ("racer_loadgen_in_flight", "racer-loadgen"),
    ("racer_active_requests", "racer-dataplane"),
    ("racer_active_fills", "racer-dataplane"),
    ("racer_peer_exchanges_active", "racer-dataplane"),
    ("racer_pending_disk_writes", "racer-dataplane"),
    ("racer_active_deliveries", "racer-dataplane"),
    ("racer_worker_relay_used", "racer-dataplane"),
)


def require(ok, message):
    if not ok:
        raise ValueError(message)


def unique(pairs):
    result = {}
    for key, value in pairs:
        require(key not in result, "duplicate JSON key")
        result[key] = value
    return result


def decode(text):
    return json.loads(text, object_pairs_hook=unique)


def plan_rows(plan, authorize, start, count):
    require(authorize == SHA == plan["map_sha256"], "authorization/map identity mismatch")
    require(plan["schema"] == 1 and plan["applied"] is False and plan["annotation"] == KEY, "invalid plan")
    rows = plan["nodes"]
    require(len(rows) == 1500 and len({r["node"] for r in rows}) == 1500, "plan node count")
    require([r["uid"] for r in rows] == sorted({r["uid"] for r in rows}), "UID ordering/duplicates")
    pairs = []
    for row in rows:
        require(re.fullmatch(r"[a-z0-9][a-z0-9.-]*", row["node"]), "node name")
        value = row["candidate_annotation"]
        require(isinstance(value, str) and re.fullmatch(r"[1-9][0-9]*", value)
                and int(value) < 2**32, "candidate weight")
        require(row["original_annotation"] == (None if row["shares"] == 4 else "1")
                and row["shares"] in (1, 4), "original annotation")
        require(row["rollback"] == {"operation": "remove" if row["shares"] == 4 else "set",
                                    "value": row["original_annotation"]}, "rollback mismatch")
        pairs.append((row["uid"], int(value)))
    require(hashlib.sha256(json.dumps(pairs, separators=(",", ":")).encode()).hexdigest() == SHA,
            "candidate hash mismatch")
    original_pairs = [(r["uid"], r["shares"]) for r in rows]
    require(hashlib.sha256(json.dumps(original_pairs, separators=(",", ":")).encode()).hexdigest()
            == BASELINE_SHA, "original shares hash mismatch")
    require(0 <= start < 1500 and 1 <= count <= 500 and start + count <= 1500, "phase bounds")
    return rows, rows[start:start + count]


def inspect(row, live, direction):
    meta = live["metadata"]
    require(meta["name"] == row["node"] and meta["uid"] == row["uid"], "node identity changed")
    require(meta.get("resourceVersion") and not meta.get("deletionTimestamp")
            and EXCLUDE not in meta.get("labels", {}), "deleting/excluded/versionless node")
    ann = meta.get("annotations")
    require(isinstance(ann, dict) and ann.get(ENROLLED) == "4", "annotation/enrollment drift")
    current = (KEY in ann, ann.get(KEY))
    original = (row["original_annotation"] is not None, row["original_annotation"])
    candidate = (True, row["candidate_annotation"])
    source, target = (original, candidate) if direction == "apply" else (candidate, original)
    require(current in (source, target), "unexpected shares state")
    return current == target, meta["resourceVersion"], current


def patch_for(row, live, direction):
    done, rv, current = inspect(row, live, direction)
    if done:
        return None
    patch = [{"op": "test", "path": "/metadata/uid", "value": row["uid"]},
             {"op": "test", "path": "/metadata/resourceVersion", "value": rv}]
    if current[0]:
        patch.append({"op": "test", "path": PTR, "value": current[1]})
    if direction == "apply":
        patch.append({"op": "replace" if current[0] else "add", "path": PTR,
                      "value": row["candidate_annotation"]})
    elif row["original_annotation"] is None:
        patch.append({"op": "remove", "path": PTR})
    else:
        patch.append({"op": "replace", "path": PTR, "value": row["original_annotation"]})
    return patch


def control_identity(cm, rows):
    meta, data = cm["metadata"], cm["data"]
    require(meta.get("uid") and not meta.get("deletionTimestamp"), "control identity")
    require(data.get("concurrency", "").strip() == "0", "global concurrency is not C0")
    caps = decode(data["node-caps.json"])
    expected = {r["node"]: 1 for r in rows if r["shares"] == 1}
    require(set(caps) == {"version", "caps"} and type(caps["version"]) is int
            and caps["version"] == 1 and len(expected) == 11 and caps["caps"] == expected
            and all(type(v) is int for v in caps["caps"].values()), "caps changed")
    return meta["uid"], data


def drain_query(metric, app, zero=True):
    selector = f'{metric}{{job="kubernetes-pods",namespace="unbounded-system",app_kubernetes_io_name="{app}",node!=""}}'
    # 60s scrapes, with 15s scheduling margin: early sample age is [180,255)s
    # (range selectors exclude their left boundary). All samples back through
    # that bucket must be zero, not just samples after the three-minute boundary.
    # Four samples and current age <=75s accommodate normal scrape phase/jitter.
    # No `or 0`: absence is a failed coverage gate. This is sampled evidence,
    # never proof of unobserved values between scrapes.
    value = (f"max by(node)(max_over_time({selector}[255s])) == 0" if zero else
             f"min by(node)(min_over_time({selector}[255s])) == 1")
    return (f"({value}) and on(node) (min by(node)(count_over_time({selector}[255s])) >= 4) "
            f"and on(node) (min by(node)(timestamp({selector})) >= time()-75) "
            f"and on(node) (count by(node)(count_over_time({selector}[75s] offset 180s)) "
            f"== on(node) count by(node)({selector}))")


def coverage(payload, names, expected):
    require(payload.get("status") == "success" and not payload.get("warnings")
            and payload["data"]["resultType"] == "vector", "Prometheus result invalid")
    result = {}
    for item in payload["data"]["result"]:
        node = item["metric"].get("node")
        value = float(item["value"][1])
        require(node and node not in result and math.isfinite(value) and value == expected,
                "nonzero/duplicate/invalid drain result")
        result[node] = value
    require(set(result) == names, "drain coverage incomplete or unexpected")


class CommandError(RuntimeError):
    pass


class Runner:
    def __init__(self, state, context):
        require(context == CONTEXT, "wrong context")
        self.context = context
        self.state = Path(state)
        root = Path(__file__).resolve().parents[2]
        # Parent operational tmp is also allowed when running in its worktree.
        repository = next((p for p in root.parents if (p / ".git").is_dir()), root)
        resolved = self.state.resolve()
        require(resolved.is_relative_to(repository) and resolved != repository
                and resolved.parent.is_dir() and not resolved.exists(), "state must be fresh inside repository")
        resolved.mkdir(mode=0o700)
        self.state = resolved
        self.deadline = time.monotonic() + 220
        self.stop = threading.Event()
        self.lock = threading.RLock()
        self.processes = set()

    def note(self, event, **fields):
        with self.lock:
            record = {"at": datetime.now(timezone.utc).isoformat(), "event": event, **fields}
            line = json.dumps(record, allow_nan=False)
            with (self.state / "events.jsonl").open("a") as stream:
                stream.write(line + "\n")
                stream.flush()
            print(line, flush=True)

    def cancel(self):
        self.stop.set()
        with self.lock:
            for proc in self.processes:
                try:
                    os.killpg(proc.pid, signal.SIGTERM)
                except ProcessLookupError:
                    pass

    def command(self, args):
        with self.lock:
            require(not self.stop.is_set() and time.monotonic() < self.deadline, "phase stopped/deadline")
            seconds = max(1, min(10, int(self.deadline - time.monotonic())))
            argv = ["timeout", "--signal=TERM", "--kill-after=10s", f"{seconds}s", "kubectl",
                    "--context", self.context, "--request-timeout=8s", "-n", "unbounded-system", *args]
            proc = subprocess.Popen(argv, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                                    text=True, start_new_session=True)
            self.processes.add(proc)
            # A main-thread signal may arrive while Popen is registering a child.
            if self.stop.is_set():
                try:
                    os.killpg(proc.pid, signal.SIGTERM)
                except ProcessLookupError:
                    pass
        try:
            out, _ = proc.communicate(timeout=seconds + 11)
            if proc.returncode:
                raise CommandError("kubectl failed; response suppressed")
            return decode(out)
        finally:
            # Kill the entire group even if its leader exited leaving descendants.
            try:
                os.killpg(proc.pid, signal.SIGKILL)
            except ProcessLookupError:
                pass
            proc.communicate(timeout=2)
            with self.lock:
                self.processes.discard(proc)

    def get_node(self, row):
        return self.command(["get", "node", row["node"], "-o", "json"])

    def control(self):
        return self.command(["get", "cm", "racer-loadgen-control", "-o", "json"])

    def query(self, expression, timestamp):
        path = "/api/v1/namespaces/monitoring/services/prometheus:9090/proxy/api/v1/query?"
        return self.command(["get", "--raw", path + urlencode({
            "query": expression, "time": timestamp, "timeout": "7s"})])

    def patch(self, row, patch):
        return self.command(["patch", "node", row["node"], "--type=json",
                             "-p", json.dumps(patch, separators=(",", ":")), "-o", "json"])


def gate(runner, rows):
    before = control_identity(runner.control(), rows)
    names = {r["node"] for r in rows}
    timestamp = datetime.now(timezone.utc).isoformat()
    checks = [(m, a, True) for m, a in GAUGES] + [("up", a, False) for a in ("racer-loadgen", "racer-dataplane")]
    for metric, app, zero in checks:
        coverage(runner.query(drain_query(metric, app, zero), timestamp), names, 0 if zero else 1)
        runner.note("drain_gate", metric=metric, app=app, nodes=len(names), timestamp=timestamp)
    require(control_identity(runner.control(), rows) == before, "control changed during drain gate")
    return before


def apply_node(runner, row, direction):
    live = runner.get_node(row)
    for attempt in range(1, 4):
        done, rv, current = inspect(row, live, direction)
        if done:
            runner.note("already_desired", node=row["node"], uid=row["uid"], direction=direction)
            return
        patch = patch_for(row, live, direction)
        runner.note("before_patch", node=row["node"], uid=row["uid"], attempt=attempt,
                    resource_version=rv, present=current[0], value=current[1], direction=direction)
        try:
            result = runner.patch(row, patch)
        except CommandError:
            # Ambiguous writes are reconciled by live state, never replayed blindly.
            live = runner.get_node(row)
            done, fresh_rv, _ = inspect(row, live, direction)
            if done:
                runner.note("desired_after_ambiguous", node=row["node"], uid=row["uid"], attempt=attempt)
                return
            require(fresh_rv != rv and attempt < 3, "unchanged RV or retry budget exhausted")
            runner.note("fresh_snapshot_retry", node=row["node"], uid=row["uid"], attempt=attempt)
            continue
        require(inspect(row, result, direction)[0], "patch response not desired")
        runner.note("applied", node=row["node"], uid=row["uid"], attempt=attempt,
                    resource_version=result["metadata"]["resourceVersion"], direction=direction)
        return


def dispatch(runner, phase, direction, workers=16):
    iterator = iter(phase)
    completed = 0
    with futures.ThreadPoolExecutor(max_workers=workers) as pool:
        pending = set()

        def work(row):
            try:
                apply_node(runner, row, direction)
            except BaseException:
                runner.cancel()
                runner.note("node_failed", node=row["node"], uid=row["uid"], direction=direction,
                            error="inspect live state before resuming")
                raise

        while True:
            while len(pending) < workers and not runner.stop.is_set() and time.monotonic() < runner.deadline:
                row = next(iterator, None)
                if row is None:
                    break
                pending.add(pool.submit(work, row))
            if not pending:
                break
            ready, pending = futures.wait(pending, timeout=1, return_when=futures.FIRST_COMPLETED)
            for future in ready:
                try:
                    future.result()
                    completed += 1
                except BaseException:
                    runner.cancel()
            if time.monotonic() >= runner.deadline:
                runner.cancel()
            if not ready:
                runner.note("heartbeat", completed=completed, inflight=len(pending), stopped=runner.stop.is_set())
    require(not runner.stop.is_set() and completed == len(phase), "partial phase; inspect live state")
    return completed


def execute(runner, rows, phase, direction):
    authority = gate(runner, rows)
    # Whole-phase preflight is read-only and precedes every write. One LIST is
    # sufficient here; every actual mutation still starts with a fresh node GET.
    inventory = runner.command(["get", "nodes", "-o", "json"])["items"]
    by_name = {n["metadata"]["name"]: n for n in inventory}
    for row in phase:
        inspect(row, by_name[row["node"]], direction)
    require(control_identity(runner.control(), rows) == authority, "control changed before writes")
    runner.note("preflight_complete", count=len(phase), direction=direction)
    count = dispatch(runner, phase, direction)
    require(control_identity(runner.control(), rows) == authority, "control changed during writes")
    runner.note("phase_complete", count=count, direction=direction, membership_attested=False,
                next="parent final membership attestation; do not resume automatically")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--plan", type=Path, required=True)
    parser.add_argument("--context", required=True, choices=[CONTEXT])
    parser.add_argument("--direction", required=True, choices=["apply", "rollback"])
    parser.add_argument("--start", required=True, type=int)
    parser.add_argument("--count", required=True, type=int)
    parser.add_argument("--state", required=True, type=Path)
    parser.add_argument("--authorize", required=True)
    args = parser.parse_args()
    raw = args.plan.read_bytes()
    rows, phase = plan_rows(decode(raw), args.authorize, args.start, args.count)
    runner = Runner(args.state, args.context)

    def interrupted(signum, frame):
        runner.cancel()

    signal.signal(signal.SIGTERM, interrupted)
    signal.signal(signal.SIGINT, interrupted)
    signal.signal(signal.SIGALRM, interrupted)
    signal.alarm(220)
    runner.note("phase_start", direction=args.direction, start=args.start, count=args.count,
                map_sha256=SHA, plan_file_sha256=hashlib.sha256(raw).hexdigest(),
                deadline_seconds=220, concurrency=16)
    try:
        execute(runner, rows, phase, args.direction)
    except BaseException:
        runner.cancel()
        runner.note("phase_failed", error="inspect per-node checkpoints and live state; parent retains C0")
        return 1
    finally:
        signal.alarm(0)
        runner.cancel()
    return 0


if __name__ == "__main__":
    sys.exit(main())
