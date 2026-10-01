#!/usr/bin/env python3
# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0
"""Read-only, stdlib Prometheus snapshot through kubectl service proxy."""
import argparse
from collections import Counter
from datetime import datetime, timezone
import json
import math
import os
from pathlib import Path
import re
import signal
import subprocess
import sys
import time
from urllib.parse import urlencode


def queries(namespace, node_regex, resource_job="kubelet-resource"):
    scope = f'namespace={json.dumps(namespace)},node!="",node=~{json.dumps(node_regex)}'
    load = f'job="kubernetes-pods",app_kubernetes_io_name="racer-loadgen",{scope}'
    host = f'job="node-exporter",node!="",node=~{json.dumps(node_regex)}'
    verified = f'rate(racer_loadgen_verified_bytes_total{{{load}}}[5m])'
    # Result labels are created lazily. An observed verified counter with a
    # rate establishes the zero baseline; absent scrape history does not.
    zero = f'0 * sum by (node) ({verified})'
    result = {
        "loadgen_up": f'min by (node) (up{{{load}}})',
        "dataplane_up": f'min by (node) (up{{job="kubernetes-pods",app_kubernetes_io_name="racer-dataplane",{scope}}})',
        "verified_goodput_gbps": f'sum by (node) ({verified}) * 8 / 1e9',
        "eth0_egress_gbps": f'max by (node) (rate(node_network_transmit_bytes_total{{{host},device="eth0"}}[5m])) * 8 / 1e9',
        "eth0_ingress_gbps": f'max by (node) (rate(node_network_receive_bytes_total{{{host},device="eth0"}}[5m])) * 8 / 1e9',
        "applied_concurrency": f'sum by (node) (racer_loadgen_applied_concurrency{{{load}}})',
        "inflight": f'sum by (node) (racer_loadgen_in_flight{{{load}}})',
        "racer_ready": f'min by (node) (racer_ready{{job="kubernetes-pods",{scope}}})',
        "node_cpu_nonidle_fraction": f'1 - avg by (node) (rate(node_cpu_seconds_total{{{host},mode="idle"}}[5m]))',
        "resource_cpu_cores": f'sum by (node) (rate(container_cpu_usage_seconds_total{{job={json.dumps(resource_job)},{scope},container!="",container!="POD"}}[5m]))',
        "process_cpu_cores_by_app": f'sum by (node,app_kubernetes_io_name) (rate(process_cpu_seconds_total{{job="kubernetes-pods",{scope},app_kubernetes_io_name=~"racer-.*|gantry"}}[5m]))',
    }
    for name, matcher in (("success_pulls_per_second", 'result="success"'),
                          ("error_pulls_per_second", 'result!="success"')):
        result[name] = f'sum by (node) (rate(racer_loadgen_pulls_total{{{load},{matcher}}}[5m])) or on (node) ({zero})'
    return result


def bounded_command(argv, seconds):
    """TERM the entire process group on timeout, cancellation, or interruption."""
    command = ["timeout", "--signal=TERM", "--kill-after=10s", f"{seconds}s", *argv]
    process = subprocess.Popen(command, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                               text=True, start_new_session=True)
    try:
        stdout, _ = process.communicate(timeout=seconds + 12)
    except BaseException:
        os.killpg(process.pid, signal.SIGTERM)
        try:
            process.communicate(timeout=10)
        except subprocess.TimeoutExpired:
            os.killpg(process.pid, signal.SIGKILL)
            process.communicate(timeout=2)
        raise
    if process.returncode:
        # Do not reflect credential-plugin stderr or kubectl authentication data.
        raise RuntimeError(f"kubectl read failed (exit {process.returncode}); stderr suppressed")
    return stdout


def query(args, expression, timestamp, seconds):
    params = urlencode({"query": expression, "time": timestamp, "timeout": "15s"})
    path = (f"/api/v1/namespaces/{args.prometheus_namespace}/services/"
            f"{args.prometheus_service}/proxy/api/v1/query?{params}")
    text = bounded_command(["kubectl", f"--context={args.context}",
                            f"--request-timeout={seconds}s", "get", "--raw", path], seconds)
    payload = json.loads(text)
    if payload.get("status") != "success" or payload.get("data", {}).get("resultType") != "vector":
        raise ValueError("Prometheus did not return a successful instant vector")
    return payload


def vector(payload, labels=("node",)):
    result = {}
    for item in payload["data"]["result"]:
        key = tuple(item["metric"].get(label, "") for label in labels)
        if not all(key) or key in result:
            raise ValueError(f"missing or duplicate labels: {labels} {key}")
        value = float(item["value"][1])
        if not math.isfinite(value):
            raise ValueError(f"non-finite measurement for {key}")
        result[key] = value
    return result


def statistics(values):
    values = sorted(values)
    if not values:
        return {"count": 0, "sum": None, "p05": None, "median": None,
                "p95": None, "min": None, "max": None}

    def percentile(q):
        position = (len(values) - 1) * q
        low, high = math.floor(position), math.ceil(position)
        return values[low] + (values[high] - values[low]) * (position - low)

    return {"count": len(values), "sum": sum(values), "p05": percentile(.05),
            "median": percentile(.5), "p95": percentile(.95),
            "min": values[0], "max": values[-1]}


def summarize(raw):
    cpu = "process_cpu_cores_by_app"
    series = {name: vector(payload) for name, payload in raw.items() if name != cpu}
    # Scrape targets remain in up even while failing. Include dataplane nodes
    # so missing loadgen telemetry cannot silently shrink the population.
    nodes = sorted(set(series["loadgen_up"]) | set(series["dataplane_up"]) | set(series["racer_ready"]))
    if not nodes:
        raise ValueError("no loadgen targets or Racer readiness nodes in scope")
    rows = [{"node": node[0], **{name: values.get(node) for name, values in series.items()}}
            for node in nodes]
    summary = {name: statistics(row[name] for row in rows if row[name] is not None)
               for name in series}
    missing = {name: [row["node"] for row in rows if row[name] is None] for name in series}
    success = summary["success_pulls_per_second"]["sum"]
    errors = summary["error_pulls_per_second"]["sum"]
    total = None if success is None or errors is None else success + errors
    complete = not missing["success_pulls_per_second"] and not missing["error_pulls_per_second"]
    return {
        "node_count": len(nodes), "per_node": rows, "summary": summary,
        "missing_nodes_by_metric": missing,
        "zero_progress_nodes": [r["node"] for r in rows if r["verified_goodput_gbps"] == 0],
        "active_zero_progress_nodes": [r["node"] for r in rows
                                       if r["verified_goodput_gbps"] == 0 and (r["applied_concurrency"] or 0) > 0],
        "applied_concurrency_distribution": dict(sorted(Counter(
            str(r["applied_concurrency"]) for r in rows if r["applied_concurrency"] is not None).items())),
        "readiness": {"ready_nodes": sum(r["racer_ready"] == 1 and r["dataplane_up"] == 1 for r in rows),
                      "not_ready_nodes": [r["node"] for r in rows if r["racer_ready"] == 0],
                      "scrape_down_nodes": [r["node"] for r in rows if r["dataplane_up"] == 0],
                      "missing_nodes": sorted(set(missing["racer_ready"]) | set(missing["dataplane_up"]))},
        "completion_fractions": {"success": success / total if complete and total else None,
                                 "error": errors / total if complete and total else None},
        cpu: [{"node": node, "app": app, "cores": value}
              for (node, app), value in sorted(vector(raw[cpu], ("node", "app_kubernetes_io_name")).items())
              if (node,) in nodes],
    }


def parse_args(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--context", default="joolshev-scale-test")
    parser.add_argument("--namespace", default="unbounded-system", help="workload namespace")
    parser.add_argument("--prometheus-namespace", default="monitoring")
    parser.add_argument("--prometheus-service", default="prometheus:9090", help="service:port")
    parser.add_argument("--node-regex", default=".+")
    parser.add_argument("--resource-job", default="kubelet-resource", help="job scraping kubelet /metrics/resource")
    parser.add_argument("--timestamp", help="RFC3339 evaluation time; defaults to current UTC")
    parser.add_argument("--raw-output", type=Path, help="new JSON file in an existing directory; never overwrite")
    args = parser.parse_args(argv)
    for field in ("namespace", "prometheus_namespace"):
        if not re.fullmatch(r"[a-z0-9][a-z0-9.-]*", getattr(args, field)):
            parser.error(f"invalid {field}")
    if not re.fullmatch(r"[a-z0-9][a-z0-9.-]*:[a-z0-9-]+", args.prometheus_service):
        parser.error("Prometheus service must be service:port")
    if not args.context or not args.node_regex:
        parser.error("context and node regex must be nonempty")
    if args.raw_output and (not args.raw_output.parent.is_dir() or args.raw_output.exists()):
        parser.error("raw output requires a new filename in an existing directory")
    if args.timestamp:
        try:
            stamp = datetime.fromisoformat(args.timestamp.replace("Z", "+00:00"))
            if stamp.tzinfo is None:
                raise ValueError("timezone required")
        except ValueError:
            parser.error("timestamp must be RFC3339 with timezone")
    else:
        stamp = datetime.now(timezone.utc)
    args.timestamp = stamp.astimezone(timezone.utc).isoformat().replace("+00:00", "Z")
    return args


def main(argv=None):
    args = parse_args(argv)
    expressions = queries(args.namespace, args.node_regex, args.resource_job)
    deadline = time.monotonic() + 240
    raw = {}
    for name, expression in expressions.items():
        remaining = int(deadline - time.monotonic())
        if remaining < 1:
            raise TimeoutError("240-second measurement deadline exceeded")
        raw[name] = query(args, expression, args.timestamp, min(20, remaining))
    report = {"timestamp": args.timestamp, "window": "5m", "context": args.context,
              "namespace": args.namespace, "node_regex": args.node_regex,
              "prometheus": f"{args.prometheus_namespace}/{args.prometheus_service}",
              "queries": expressions, "warnings": {name: payload["warnings"] for name, payload in raw.items()
                                                   if payload.get("warnings")}, **summarize(raw)}
    if args.raw_output:
        with args.raw_output.open("x") as stream:
            json.dump({"report": report, "responses": raw}, stream, indent=2, allow_nan=False)
            stream.write("\n")
    print(json.dumps(report, indent=2, allow_nan=False))
    return 0


def interrupted(signum, frame):
    raise KeyboardInterrupt


if __name__ == "__main__":
    signal.signal(signal.SIGTERM, interrupted)
    try:
        sys.exit(main())
    except (OSError, ValueError, KeyError, TypeError, RuntimeError, subprocess.TimeoutExpired) as error:
        print(f"measurement failed: {error}", file=sys.stderr)
        sys.exit(1)
    except KeyboardInterrupt:
        print("measurement interrupted", file=sys.stderr)
        sys.exit(130)
