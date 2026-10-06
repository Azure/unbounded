#!/usr/bin/env python3
# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0
"""Read-only racer-loadgen sampler. OUTPUT is a new directory of raw files and summary.json.

Rates sum per-pod counter deltas divided by each pod's monotonic scrape-midpoint
interval. Scrape durations expose timing uncertainty; these are not wire rates.
Histogram estimates use pooled interval bucket deltas (not lifetime buckets).
Errors leave evidence and a partial summary, and cause exit status 1.
"""

import argparse
from concurrent.futures import ThreadPoolExecutor
from datetime import datetime, timezone
import json
import math
import os
from pathlib import Path
import re
import signal
import statistics
import subprocess
import sys
import time
from urllib.parse import quote


PREFIX = "racer_loadgen_"
SAMPLE = re.compile(r'([a-zA-Z_:][a-zA-Z0-9_:]*)(?:\{(.*)\})?\s+(\S+)(?:\s+\S+)?')
LABEL = re.compile(r'([a-zA-Z_][a-zA-Z0-9_]*)="((?:[^"\\]|\\[\\"n])*)"(?:,|$)')


def parse_metrics(text):
    metrics = {}
    for line in text.splitlines():
        if not line.startswith((PREFIX, "process_start_time_seconds")):
            continue
        match = SAMPLE.fullmatch(line)
        if not match:
            raise ValueError(f"malformed metric: {line}")
        name, raw_labels, raw_value = match.groups()
        labels = {}
        position = 0
        while raw_labels and position < len(raw_labels):
            label = LABEL.match(raw_labels, position)
            if not label or label[1] in labels:
                raise ValueError(f"malformed or duplicate labels: {line}")
            labels[label[1]] = re.sub(r'\\([\\"n])', lambda m: '\n' if m[1] == 'n' else m[1], label[2])
            position = label.end()
        value = float(raw_value)
        if not math.isfinite(value) or value < 0:
            raise ValueError(f"invalid metric value: {line}")
        key = (name, tuple(sorted(labels.items())))
        if key in metrics:
            raise ValueError(f"duplicate metric: {line}")
        metrics[key] = value
    for name in ("verified_bytes_total", "received_bytes_total", "in_flight", "applied_concurrency"):
        if (PREFIX + name, ()) not in metrics:
            raise ValueError(f"missing required metric {PREFIX + name}")
    return metrics


def counter_deltas(before, after):
    deltas = {}
    for key in before.keys() | after.keys():
        name = key[0]
        if name == "process_start_time_seconds":
            if before.get(key) != after.get(key):
                raise ValueError("process start time changed or disappeared")
        elif name.endswith(("_total", "_bucket", "_count", "_sum")):
            if key not in after or after[key] < before.get(key, 0):
                raise ValueError(f"counter reset or series disappeared: {key}")
            # CounterVec series are created lazily, so new labels start at zero.
            deltas[key] = after[key] - before.get(key, 0)
    return deltas


def quantiles(buckets):
    """Prometheus-style linear interpolation; +Inf ranks use last finite bound."""
    ordered = sorted(buckets.items())
    if not ordered or ordered[-1][0] != math.inf:
        raise ValueError("histogram missing +Inf bucket")
    if any(a[1] > b[1] for a, b in zip(ordered, ordered[1:])):
        raise ValueError("non-monotonic histogram deltas")
    total = ordered[-1][1]
    result = {"observations": total}
    for name, q in (("p50", 0.5), ("p95", 0.95), ("p99", 0.99)):
        value = None
        low, count = 0.0, 0.0
        if total:
            for high, cumulative in ordered:
                if cumulative >= total * q:
                    value = low if math.isinf(high) else low + (high - low) * (total * q - count) / (cumulative - count)
                    break
                low, count = high, cumulative
        result[name] = value
    return result


def identity(pod):
    statuses = {s["name"]: s for s in pod.get("status", {}).get("containerStatuses", [])}
    return {
        "uid": pod["metadata"]["uid"],
        "node": pod.get("spec", {}).get("nodeName"),
        "pod_ip": pod.get("status", {}).get("podIP"),
        "containers": [{
            "name": c["name"], "image": c["image"],
            "image_id": statuses.get(c["name"], {}).get("imageID"),
            "container_id": statuses.get(c["name"], {}).get("containerID"),
            "restarts": statuses.get(c["name"], {}).get("restartCount"),
            "ready": statuses.get(c["name"], {}).get("ready"),
        } for c in pod.get("spec", {}).get("containers", [])],
    }


def pod_result(first, last, before_identity, after_identity):
    if before_identity != after_identity:
        raise ValueError("pod UID, container identity, readiness, or restart count changed")
    for container in before_identity["containers"]:
        if not container["image_id"] or container["restarts"] is None:
            raise ValueError("missing container image identity or restart count")
    if "error" in first or "error" in last:
        raise ValueError(f"scrape failed: {first.get('error', '')} {last.get('error', '')}")
    elapsed = last["midpoint"] - first["midpoint"]
    if elapsed <= 0:
        raise ValueError("nonpositive monotonic sample interval")
    before, after = parse_metrics(first["text"]), parse_metrics(last["text"])
    deltas = counter_deltas(before, after)
    result = {"interval_seconds": elapsed, "scrape_seconds": [first["duration"], last["duration"]]}
    for kind in ("verified", "received"):
        value = deltas[(PREFIX + kind + "_bytes_total", ())]
        result[kind + "_bytes_delta"] = value
        result[kind + "_gib_per_second"] = value / elapsed / 2**30
    successes = deltas.get((PREFIX + "pulls_total", (("result", "success"),)), 0)
    result["successful_operations_delta"] = successes
    result["successful_operations_per_second"] = successes / elapsed
    result["failure_deltas_by_reason"] = {
        dict(labels)["reason"]: value for (name, labels), value in deltas.items()
        if name == PREFIX + "pull_failures_total"
    }
    for gauge in ("applied_concurrency", "in_flight"):
        result[gauge] = {"before": before[(PREFIX + gauge, ())], "after": after[(PREFIX + gauge, ())]}
    buckets = {}
    for (name, labels), value in deltas.items():
        if name == PREFIX + "pull_duration_seconds_bucket":
            bound = float(dict(labels)["le"])
            if math.isnan(bound) or bound < 0:
                raise ValueError("invalid latency histogram boundary")
            buckets[bound] = buckets.get(bound, 0) + value
    result["operation_latency_seconds"] = quantiles(buckets) if buckets else None
    return result, buckets


class Collector:
    def __init__(self, args, output):
        self.args = args
        self.output = output
        # Leave time below 240 seconds for process cleanup and writing the summary.
        self.deadline = time.monotonic() + 225

    def command(self, tag, arguments):
        start = time.monotonic()
        timeout = min(28, self.deadline - start - 2)
        command = ["kubectl", "--request-timeout=25s", *arguments]
        record = {"command": command, "started_monotonic": start}
        stdout, stderr = "", ""
        try:
            if timeout <= 0:
                raise TimeoutError("total collection deadline exhausted")
            process = subprocess.Popen(command, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                                       text=True, start_new_session=True)
            try:
                stdout, stderr = process.communicate(timeout=timeout)
            except subprocess.TimeoutExpired:
                os.killpg(process.pid, signal.SIGTERM)
                try:
                    stdout, stderr = process.communicate(timeout=1)
                except subprocess.TimeoutExpired:
                    os.killpg(process.pid, signal.SIGKILL)
                    stdout, stderr = process.communicate(timeout=1)
                raise TimeoutError(f"kubectl exceeded {timeout:.2f}s")
            record["returncode"] = process.returncode
            if process.returncode:
                raise RuntimeError(f"kubectl exit {process.returncode}: {stderr.strip()}")
        except (OSError, RuntimeError, TimeoutError, subprocess.TimeoutExpired) as exc:
            record["error"] = str(exc)
        end = time.monotonic()
        record.update(midpoint=(start + end) / 2, duration=end - start)
        (self.output / (tag + ".txt")).write_text(stdout)
        (self.output / (tag + ".stderr.txt")).write_text(stderr)
        (self.output / (tag + ".json")).write_text(json.dumps(record, indent=2) + "\n")
        record["text"] = stdout
        return record

    def pods(self, tag):
        record = self.command(tag, ["get", "pods", "-n", self.args.namespace,
                                    "-l", self.args.selector, "-o", "json"])
        if "error" in record:
            raise RuntimeError(record["error"])
        return {p["metadata"]["name"]: identity(p) for p in json.loads(record["text"])["items"]}

    def scrape(self, name, phase, due=0):
        delay = min(due, self.deadline) - time.monotonic()
        if delay > 0:
            time.sleep(delay)
        path = (f"/api/v1/namespaces/{quote(self.args.namespace, safe='')}/pods/"
                f"{quote(name, safe='')}:9090/proxy/metrics")
        return self.command(f"{phase}-{name}-metrics", ["get", "--raw", path])

    def collect(self, summary):
        before = self.pods("pods-before")
        summary["identities_before"] = before
        if not before:
            raise ValueError("selector matched no pods")
        names = sorted(before)
        with ThreadPoolExecutor(max_workers=self.args.parallel) as pool:
            first = dict(zip(names, pool.map(lambda n: self.scrape(n, "before"), names)))
            last = dict(zip(names, pool.map(
                lambda n: self.scrape(n, "after", first[n]["midpoint"] + self.args.seconds), names)))
        after = self.pods("pods-after")
        summary["identities_after"] = after
        if before.keys() != after.keys():
            summary["errors"].append("selected pod set changed during sampling")
        pooled = {}
        for name in names:
            try:
                result, buckets = pod_result(first[name], last[name], before[name], after.get(name))
                summary["pods"][name] = result
                for bound, count in buckets.items():
                    pooled[bound] = pooled.get(bound, 0) + count
            except (ValueError, KeyError) as exc:
                summary["errors"].append(f"{name}: {exc}")
        valid = list(summary["pods"].values())
        aggregate = {"selected_pods": len(names), "valid_pods": len(valid)}
        if valid:
            for key in ("verified_gib_per_second", "received_gib_per_second", "successful_operations_per_second"):
                values = [p[key] for p in valid]
                aggregate[key] = sum(values)
                aggregate["per_pod_" + key] = {"min": min(values), "median": statistics.median(values), "max": max(values)}
            for gauge in ("applied_concurrency", "in_flight"):
                aggregate[gauge] = {phase: sum(p[gauge][phase] for p in valid) for phase in ("before", "after")}
            failures = {}
            for pod in valid:
                for reason, delta in pod["failure_deltas_by_reason"].items():
                    failures[reason] = failures.get(reason, 0) + delta
            aggregate["failure_deltas_by_reason"] = failures
            aggregate["operation_latency_seconds"] = quantiles(pooled) if pooled else None
        summary["aggregate"] = aggregate
        if self.args.top:
            top = self.command("top-containers", ["top", "pods", "-n", self.args.namespace,
                                                 "-l", self.args.selector, "--containers"])
            summary["top_containers"] = top
            if "error" in top:
                summary["errors"].append("optional top: " + top["error"])


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--namespace", default="unbounded-system")
    parser.add_argument("--selector", default="app.kubernetes.io/name=racer-s3-loadgen")
    parser.add_argument("--seconds", type=float, default=60)
    parser.add_argument("--parallel", type=int, choices=range(8, 17), default=12)
    parser.add_argument("--output", type=Path, required=True, help="new output directory (must not exist)")
    parser.add_argument("--top", action="store_true", help="also save kubectl top pods --containers")
    args = parser.parse_args(argv)
    if not math.isfinite(args.seconds) or not 0 < args.seconds <= 180:
        parser.error("--seconds must be greater than zero and at most 180")
    try:
        args.output.mkdir(parents=True, exist_ok=False)
    except OSError as exc:
        parser.error(str(exc))
    summary = {
        "started_utc": datetime.now(timezone.utc).isoformat(),
        "namespace": args.namespace, "selector": args.selector,
        "requested_seconds": args.seconds, "parallel": args.parallel,
        "notes": [
            "Rates sum per-pod deltas / monotonic scrape-midpoint intervals; GiB = 2^30 bytes.",
            "Verified bytes credit successful SHA-256-verified batches only; received bytes include failures, not wire traffic.",
            "Success operations are pulls_total{result=success}; failure deltas include cancellations.",
            "Latency pools interval pull_duration_seconds buckets across results, including failures; linear interpolation, +Inf capped at last finite bound.",
            "Gauges are endpoint snapshots, not interval averages. Identities cover selected pods and their sidecars, not remote upstreams.",
            "Two samples cannot detect a counter reset that catches up unless process-start or pod/container identity changes.",
            "Partial aggregates exclude invalid pods; consult errors and valid_pods before comparing runs.",
        ],
        "pods": {}, "errors": [],
    }
    start = time.monotonic()
    try:
        Collector(args, args.output).collect(summary)
    except (OSError, ValueError, RuntimeError, KeyError) as exc:
        summary["errors"].append(str(exc))
    summary["elapsed_seconds"] = time.monotonic() - start
    summary["complete"] = not summary["errors"]
    encoded = json.dumps(summary, indent=2, sort_keys=True, allow_nan=False) + "\n"
    (args.output / "summary.json").write_text(encoded)
    print(encoded, end="")
    for error in summary["errors"]:
        print(error, file=sys.stderr)
    return 0 if summary["complete"] else 1


if __name__ == "__main__":
    sys.exit(main())
