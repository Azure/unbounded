#!/usr/bin/env python3
# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0
"""Metadata capture and deterministic offline shares planning; never applies changes.

Capture reads Kubernetes metadata and existing origin manifests, not payloads.
Plan uses only the captured JSON. Output files are exclusive-create, never replaced.
"""
import argparse
import collections
import concurrent.futures
import hashlib
import importlib.util
import json
import math
from pathlib import Path
import signal
import statistics
import sys
from datetime import datetime, timezone


ROOT = Path(__file__).resolve().parent
sys.path.insert(0, str(ROOT / "racer-rollout"))
import measure  # noqa: E402

spec = importlib.util.spec_from_file_location("forecast", ROOT / "racer-capacity-forecast.py")
forecast = importlib.util.module_from_spec(spec)
spec.loader.exec_module(forecast)

ANNOTATION = "racer.unbounded-cloud.io/shares"
MEMBERS_HASH = "2d2d84a5552e20972e8b52b32a2be180e3c9bcabc7c5fed35b4f29bce26aa722"
CATALOG_HASH = "6ebf2b0537e7fc723ee251c5c041ba1624c1ef67a9390d96d8537fe68abb957b"
MAP_HASH = "65ac1f3576631495585aebb42374fef3bedeea6ba82179f31ed0b57b9dc9ba47"
STAMP = "2026-10-01T11:29:07.871078Z"
CACHE = "79f749cf-6c6c-4e26-928f-2057ca9b4279"


def require(condition, message):
    if not condition:
        raise ValueError(message)


def digest(value):
    return hashlib.sha256(json.dumps(value, separators=(",", ":")).encode()).hexdigest()


def save(path, value):
    with path.open("x") as stream:
        json.dump(value, stream, indent=2, allow_nan=False)
        stream.write("\n")


def checkpoint(message):
    print(datetime.now(timezone.utc).isoformat(), message, flush=True)


def capture(args):
    require(args.output.parent.is_dir() and not args.output.exists(), "new output in existing parent required")
    report = json.loads(args.baseline.read_text())["report"]
    require(report["timestamp"] == STAMP, "baseline timestamp differs")
    by_name = {row["node"]: row for row in report["per_node"]}

    def kube(argv):
        return json.loads(measure.bounded_command([
            "kubectl", "--context=joolshev-scale-test", "--request-timeout=7s", *argv], 7))

    nodes = kube(["get", "nodes", "-o", "json"])["items"]
    members = []
    for node in nodes:
        meta = node["metadata"]
        if meta["name"] not in by_name:
            continue
        annotations = meta.get("annotations", {})
        original = annotations.get(ANNOTATION)
        row = by_name[meta["name"]]
        shares = int(original if original is not None else annotations.get(
            "racer.unbounded-cloud.io/enrolled-shares", "4"))
        require(meta["creationTimestamp"] < STAMP, "node newer than baseline")
        require(original in (None, "1"), "unexpected original annotation")
        require(shares == (1 if row["applied_concurrency"] == 1 else 4), "shares/caps mismatch")
        admitted = json.loads(annotations.get("racer.unbounded-cloud.io/last-admitted-member", "{}"))
        require(admitted.get("node") == meta["uid"] and admitted.get("shares") == shares,
                "controller admitted shares differ")
        members.append({"uid": meta["uid"], "node": meta["name"],
                        "resource_version": meta["resourceVersion"], "shares": shares,
                        "original_annotation": original, "demand_gbps": row["verified_goodput_gbps"],
                        "observed_tx_gbps": row["eth0_egress_gbps"]})
    members.sort(key=lambda m: m["uid"])
    cache = kube(["get", "clustercache", "gantry", "-o", "json"])
    require(cache["metadata"]["uid"] == CACHE, "cache identity differs")
    require(cache["metadata"]["creationTimestamp"] < STAMP, "cache newer than baseline")
    require(digest([(m["uid"], m["shares"]) for m in members]) == MEMBERS_HASH, "membership differs")
    # Choose a retained loadgen Pod, not a Racer/Gantry path that could populate caches.
    pods = kube(["-n", "unbounded-system", "get", "pods", "-l",
                 "app.kubernetes.io/name=racer-loadgen", "-o", "json"])["items"]
    pods = sorted((p for p in pods if p["metadata"].get("deletionTimestamp") is None
                   and p["metadata"]["creationTimestamp"] < STAMP
                   and any(c.get("type") == "Ready" and c.get("status") == "True"
                           for c in p.get("status", {}).get("conditions", []))),
                  key=lambda p: p["metadata"]["name"])
    require(bool(pods), "no retained ready origin")
    pod = pods[0]
    origin = pod["metadata"]["name"]
    manifests = []

    def fetch(index):
        return kube(["get", "--raw", f"/api/v1/namespaces/unbounded-system/pods/{origin}:8080/"
                     f"proxy/v2/benchmark/image/manifests/image-{index:06d}"])

    with concurrent.futures.ThreadPoolExecutor(max_workers=8) as executor:
        for index, manifest in enumerate(executor.map(fetch, range(512))):
            manifests.append(manifest)
            if index % 128 == 127:
                checkpoint(f"capture manifests {index + 1}/512")
    result = {"schema": 1, "captured_at": datetime.now(timezone.utc).isoformat(),
              "baseline_timestamp": STAMP,
              "baseline_file_sha256": hashlib.sha256(args.baseline.read_bytes()).hexdigest(),
              "cache": CACHE, "members": members, "manifests": manifests,
              "origin": {"pod": origin, "uid": pod["metadata"]["uid"]}}
    validate(result)
    save(args.output, result)
    checkpoint(f"capture complete {args.output}")


def validate(data):
    require(data["schema"] == 1 and data["baseline_timestamp"] == STAMP and data["cache"] == CACHE,
            "unsupported baseline")
    members = data["members"]
    require(len(members) == 1500 and len({m["node"] for m in members}) == 1500,
            "expected 1500 unique node names")
    require([m["uid"] for m in members] == sorted({m["uid"] for m in members}), "UID ordering/duplicates")
    require(digest([(m["uid"], m["shares"]) for m in members]) == MEMBERS_HASH, "membership hash")
    for member in members:
        require(math.isfinite(member["demand_gbps"]) and member["demand_gbps"] > 0, "invalid demand")
        require(member["original_annotation"] == ("1" if member["shares"] == 1 else None),
                "rollback annotation mismatch")
    require(len(data["manifests"]) == 512, "catalog length")
    h = hashlib.sha256()
    for manifest in data["manifests"]:
        h.update(json.dumps(manifest, sort_keys=True).encode())
    require(h.hexdigest() == CATALOG_HASH, "catalog hash")


def costs(data, bad):
    slots = collections.Counter()
    pages = 0
    for manifest in data["manifests"]:
        for blob in manifest["layers"]:
            for page, offset in enumerate(range(0, blob["size"], 16 * 1024 * 1024)):
                slot = forecast.slot(data["cache"], bytes.fromhex(blob["digest"].split(":")[1]), page)
                slots[slot] += min(16 * 1024 * 1024, blob["size"] - offset)
                pages += 1
    require(sum(slots.values()) == 274439398400 and pages == 18417 and len(slots) == 18249,
            "catalog page accounting")
    suffixes = [forecast.framed(m["uid"].encode()) for m in data["members"]]
    candidates, sizes = [], []
    for k, (slot, size) in enumerate(sorted(slots.items())):
        prefix = hashlib.sha256(b"racer/hrw/v1\0" + slot.to_bytes(4, "big"))
        samples, special = [], []
        for i, suffix in enumerate(suffixes):
            h = prefix.copy()
            h.update(suffix)
            sample = int.from_bytes(h.digest()[:8], "big")
            if i in bad:
                special.append((forecast.cost(sample), i))
            else:
                samples.append((sample, i))
        samples.sort(reverse=True)
        cutoff = 4 * forecast.cost(samples[0][0])
        # Exact for healthy [200,800]; retain every impaired candidate at [1,100].
        for sample, i in samples:
            cost = forecast.cost(sample)
            if cost > cutoff:
                break
            special.append((cost, i))
        candidates.append(special)
        sizes.append(size)
        if k % 6000 == 5999:
            checkpoint(f"placement costs {k + 1}/{len(slots)}")
    return candidates, sizes


def place(weights, candidates, sizes):
    owners, winners = [0] * len(weights), []
    for choices, size in zip(candidates, sizes):
        best_cost, best = choices[0]
        for cost, i in choices[1:]:
            left, right = cost * weights[best], best_cost * weights[i]
            if left < right or (left == right and i < best):
                best_cost, best = cost, i
        winners.append(best)
        owners[best] += size
    return owners, winners


def graph(n):
    return [sorted(({(32 * i + j) % n for j in range(32)}
                    | {(i + j * n) // 32 for j in range(32)}) - {i}) for i in range(n)]


def route(weights, owners, demands, adjacency):
    n, total = len(weights), sum(owners)
    transits = [[0.0] * n for _ in demands]
    for dest in range(n):
        distance = [-1] * n
        distance[dest] = 0
        queue = [dest]
        for i in queue:
            for j in adjacency[i]:
                if distance[j] < 0:
                    distance[j] = distance[i] + 1
                    queue.append(j)
        require(len(queue) == n, "disconnected graph")
        flows = [d.copy() for d in demands]
        fraction = owners[dest] / total
        for i in reversed(queue[1:]):
            nexts = [j for j in adjacency[i] if distance[j] == distance[i] - 1]
            denominator = sum(weights[j] for j in nexts)
            for j in nexts:
                probability = weights[j] / denominator
                for flow in flows:
                    flow[j] += flow[i] * probability
            for transit, flow, demand in zip(transits, flows, demands):
                transit[i] += (flow[i] - demand[i]) * fraction
    result = []
    for transit, demand in zip(transits, demands):
        served = [(sum(demand) - demand[i]) * owners[i] / total for i in range(n)]
        result.append(([served[i] + transit[i] for i in range(n)],
                       [demand[i] * (1 - owners[i] / total) + transit[i] for i in range(n)]))
    return result


def normalize(weights, loads, healthy):
    target = statistics.mean(loads[i] for i in healthy)
    raw = [weights[i] * (target / loads[i]) ** 0.5 for i in healthy]
    lo, hi = 0.0, 10.0
    total = 400 * len(healthy)
    for _ in range(60):
        scale = (lo + hi) / 2
        if sum(min(800, max(200, x * scale)) for x in raw) < total:
            lo = scale
        else:
            hi = scale
    exact = [min(800, max(200, x * ((lo + hi) / 2))) for x in raw]
    rounded = [math.floor(x) for x in exact]
    for j in sorted(range(len(healthy)), key=lambda j: (-(exact[j] - rounded[j]), healthy[j]))[:total - sum(rounded)]:
        rounded[j] += 1
    result = weights.copy()
    for i, value in zip(healthy, rounded):
        result[i] = value
    return result


def attest(plan_data, publication):
    """Compare an authenticated final controller publication, never a version CM alone."""
    require(plan_data["map_sha256"] == MAP_HASH, "unexpected plan identity")
    expected = {m["uid"]: int(m["candidate_annotation"]) for m in plan_data["nodes"]}
    require(len(expected) == 1500, "plan membership incomplete")
    actual = publication["members"]
    require(len(actual) == len(expected) and len({m["node"] for m in actual}) == len(expected),
            "publication membership count/duplicates")
    require({m["node"]: m["shares"] for m in actual} == expected, "publication shares differ")
    require(digest(sorted(expected.items())) == MAP_HASH, "plan map hash differs")
    require(any(c["id"] == CACHE for c in publication["caches"]), "cache absent")
    require(int(publication["sequence"]) > 0 and int(publication["membership_version"]) > 0,
            "invalid publication counters")
    return {"controller_map_matches": True, "all_dataplanes_attested": False,
            "sequence": publication["sequence"], "membership_version": publication["membership_version"],
            "publication_json_sha256": digest(publication), "map_sha256": MAP_HASH}


def plan(data):
    validate(data)
    forecast.check_vectors()
    members = data["members"]
    bad = {i for i, m in enumerate(members) if m["shares"] == 1}
    healthy = [i for i in range(1500) if i not in bad]
    demand = [m["demand_gbps"] for m in members]
    uniform = demand.copy()
    for i in healthy:
        uniform[i] = statistics.mean(demand[j] for j in healthy)
    candidates, sizes = costs(data, bad)
    adjacency = graph(1500)
    base_owner, base_winners = place([m["shares"] * 100 for m in members], candidates, sizes)
    baseline = route([m["shares"] * 100 for m in members], base_owner, [demand], adjacency)[0]
    weights = [70 if i in bad else 400 for i in range(1500)]
    reports, best = [], None
    for iteration in range(6):
        owner, winners = place(weights, candidates, sizes)
        scenarios = route(weights, owner, [demand, uniform], adjacency)
        loads = [max(s[d][i] for s in scenarios for d in (0, 1)) for i in range(1500)]
        ratios = {i: max(s[d][i] / baseline[d][i] for s in scenarios for d in (0, 1)) for i in sorted(bad)}
        feasible = max(ratios.values()) <= 1 + 1e-12
        moved = sum(size for size, a, b in zip(sizes, base_winners, winners) if a != b)
        report = {"iteration": iteration, "feasible": feasible, "peak": max(loads),
                  "capacity_multiplier": 12.5 / max(loads), "impaired_worst_ratio": max(ratios.values()),
                  "moved_bytes": moved, "moved_percent": 100 * moved / sum(sizes),
                  "scenarios": [{"max_tx": max(s[0]), "max_rx": max(s[1]),
                                 "mean_links": sum(s[0]) / sum(demand)} for s in scenarios]}
        reports.append(report)
        checkpoint(json.dumps(report))
        key = (max(loads), moved, iteration)
        if feasible and (best is None or key < best[0]):
            best = (key, weights.copy(), owner, scenarios, report)
        nxt = normalize(weights, loads, healthy)
        for i, ratio in ratios.items():
            if ratio > 0.98:
                nxt[i] = max(1, min(weights[i] - 1, math.floor(weights[i] * 0.95 / ratio)))
        weights = nxt
    require(best is not None, "no feasible candidate")
    key, weights, owner, scenarios, selected = best
    map_hash = digest([(m["uid"], w) for m, w in zip(members, weights)])
    require(map_hash == MAP_HASH and selected["iteration"] == 5, "candidate reproduction differs")
    bad_only = [demand[i] if i in bad else 0.0 for i in range(1500)]
    contribution = route(weights, owner, [bad_only], adjacency)[0]
    limits = []
    for s in scenarios:
        for d in (0, 1):
            for i in range(1500):
                slope = s[d][i] - contribution[d][i]
                ceiling = baseline[d][i] if i in bad else 12.5
                if slope > 1e-12:
                    limits.append((ceiling - contribution[d][i]) / slope)
    rows = []
    for i, (member, weight) in enumerate(zip(members, weights)):
        rows.append({**member, "candidate_annotation": str(weight),
                     "rollback": {"operation": "remove" if member["original_annotation"] is None else "set",
                                  "value": member["original_annotation"]},
                     "baseline": {"tx": baseline[0][i], "rx": baseline[1][i]},
                     "fixed": {"tx": scenarios[0][0][i], "rx": scenarios[0][1][i]},
                     "equal_healthy": {"tx": scenarios[1][0][i], "rx": scenarios[1][1][i]}})
    return {"schema": 1, "applied": False, "annotation": ANNOTATION, "map_sha256": map_hash,
            "input_sha256": digest(data), "baseline_file_sha256": data["baseline_file_sha256"],
            "members_sha256": MEMBERS_HASH, "catalog_sha256": CATALOG_HASH, "baseline_timestamp": STAMP,
            "baseline_fit_r": statistics.correlation(baseline[0], [m["observed_tx_gbps"] for m in members]),
            "selected": selected, "iterations": reports,
            "all_demand_scale_with_impaired_limits": min(12.5 / key[0], min(
                baseline[d][i] / s[d][i] for i in bad for s in scenarios for d in (0, 1))),
            "healthy_only_scale_with_impaired_limits": min(limits), "nodes": rows,
            "warning": "Offline primary-hit expectation, not an apply script or throughput guarantee. "
                       "Resource versions are capture-time evidence; refresh guarded operations before execution."}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    sub = parser.add_subparsers(dest="command", required=True)
    cap = sub.add_parser("capture")
    cap.add_argument("--baseline", type=Path, required=True)
    cap.add_argument("--output", type=Path, required=True)
    run = sub.add_parser("plan")
    run.add_argument("--input", type=Path, required=True)
    run.add_argument("--output", type=Path, required=True)
    check = sub.add_parser("attest")
    check.add_argument("--plan", type=Path, required=True)
    check.add_argument("--publication", type=Path, required=True)
    check.add_argument("--output", type=Path, required=True)
    args = parser.parse_args()
    require(args.output.parent.is_dir() and not args.output.exists(), "new output in existing parent required")
    if args.command == "capture":
        capture(args)
    elif args.command == "plan":
        result = plan(json.loads(args.input.read_text()))
        save(args.output, result)
        checkpoint(f"plan complete {result['map_sha256']}")
    else:
        result = attest(json.loads(args.plan.read_text()), json.loads(args.publication.read_text()))
        save(args.output, result)
        checkpoint(json.dumps(result))


if __name__ == "__main__":
    signal.signal(signal.SIGTERM, measure.interrupted)
    main()
