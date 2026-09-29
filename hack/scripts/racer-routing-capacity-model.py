# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0
"""Offline shortest-hop capacity model. No cluster access; explicit evidence inputs.

Two bounded phases: placement writes a reusable checkpoint; routing consumes it.
Payload responses follow request paths in reverse. Transit contributes equally
to TX/RX; an owner contributes TX and a remote local consumer contributes RX.
This is an expectation, not a replay of retries, caches, or admission.
"""
import argparse
import collections
import csv
import hashlib
import heapq
import json
import math
from pathlib import Path


def read(path):
    return json.loads(Path(path).read_text())


def frame(value):
    return len(value).to_bytes(4, "big") + value


def cost(sample):
    value = sample + 1
    exponent = value.bit_length() - 1
    if exponent == 64:
        return 1
    normalized = value << (63 - exponent)
    fraction = 0
    for bit in range(31, -1, -1):
        normalized = normalized * normalized >> 63
        if normalized >= 1 << 64:
            normalized >>= 1
            fraction |= 1 << bit
    return ((64 - exponent) << 32) - fraction


def placement(args):
    publication = read(args.membership)
    members = publication["members"]
    ids = [m["node"] for m in members]
    forecast = read(args.cohort)
    bad = {ids.index(r["uid"]) for r in forecast["cohort"]}
    assert len(ids) == len(set(ids)) == 1500 and ids == sorted(ids)
    assert len(bad) == 11 and all(m["shares"] == 4 for m in members)
    nodes = read(args.inventory)["nodes"]
    names = {r["metadata"]["uid"]: r["metadata"]["name"] for r in nodes}
    baseline = {r["node"]: r for r in csv.DictReader(Path(args.baseline).open())}
    assert set(baseline) == {names[uid] for uid in ids}
    records = read(args.catalog)["records"][:512]
    assert [r["index"] for r in records] == list(range(512))
    slots = collections.Counter()
    pages = 0
    for record in records:
        for blob in record["layers"]:
            for page, offset in enumerate(range(0, blob["size"], 16 * 1024 * 1024)):
                data = (b"racer/slot/v1\0" + frame(forecast["cache"].encode())
                        + bytes.fromhex(blob["digest"].split(":")[1]) + page.to_bytes(8, "big"))
                slot = int.from_bytes(hashlib.sha256(data).digest()[:4], "big") >> 12
                slots[slot] += min(16 * 1024 * 1024, blob["size"] - offset)
                pages += 1
    owner = [[0] * len(ids) for _ in range(2)]
    suffixes = [frame(uid.encode()) for uid in ids]
    for slot, size in slots.items():
        prefix = hashlib.sha256(b"racer/hrw/v1\0" + slot.to_bytes(4, "big"))
        # Keep all low-share candidates plus healthy maximum and exact Q32 ties.
        healthy = []
        changed = []
        maximum = -1
        for i, suffix in enumerate(suffixes):
            digest = prefix.copy()
            digest.update(suffix)
            sample = int.from_bytes(digest.digest()[:8], "big")
            if i in bad:
                changed.append((cost(sample), i))
            else:
                healthy.append((sample, i))
                maximum = max(maximum, sample)
        cutoff = cost(maximum)
        top = heapq.nlargest(2, healthy)
        candidates = ([(cost(s), i) for s, i in healthy if cost(s) == cutoff]
                      if cost(top[1][0]) == cutoff else [(cutoff, top[0][1])])
        candidates += changed
        for scenario in range(2):
            _, index = min((c * (4 if scenario and i in bad else 1), i) for c, i in candidates)
            owner[scenario][index] += size
    total = sum(slots.values())
    assert total == forecast["catalog_layer_bytes"] and pages == forecast["pages"]
    for r in forecast["cohort"]:
        i = ids.index(r["uid"])
        assert owner[0][i] == r["before"]["primary_bytes"]
        assert owner[1][i] == r["after"]["primary_bytes"]
    assert all(sum(o) == total for o in owner)
    return dict(ids=ids, names=[names[u] for u in ids], bad=sorted(bad), owner=owner,
                total=total, pages=pages, slots=len(slots), membership=publication["version"],
                baseline=[{k: float(baseline[names[u]][k]) for k in
                           ("verified_GBs", "tx_Gbps", "rx_Gbps", "host_cpu")} for u in ids],
                input_sha256={str(p): hashlib.sha256(Path(p).read_bytes()).hexdigest() for p in
                              (args.membership, args.cohort, args.inventory, args.baseline, args.catalog)})


def graph(count):
    return [sorted(({(18 * i + j) % count for j in range(18)}
                    | {(i + j * count) // 18 for j in range(18)}) - {i}) for i in range(count)]


def stats(values):
    mean = sum(values) / len(values)
    return dict(mean=mean, maximum=max(values), cv=math.sqrt(sum((v - mean) ** 2 for v in values)
                                                         / len(values)) / mean if mean else 0)


def routing(args):
    p = read(args.checkpoint)
    n = len(p["ids"])
    bad = set(p["bad"])
    healthy = [i for i in range(n) if i not in bad]
    adjacency = graph(n)
    actual = [r["verified_GBs"] * 8 for r in p["baseline"]]
    healthy_mean = sum(actual[i] for i in healthy) / len(healthy)
    demands = {"baseline": actual, "uniform": [sum(actual) / n] * n,
               "recovered": [healthy_mean if i in bad else actual[i] for i in range(n)]}
    # Near-zero is the pointwise limit of positive next-hop weights. If every
    # eligible hop is constrained, retain uniform choices, never drop endpoints.
    cases = [("uniform", 0, 4), ("owner_only", 1, 4),
             ("routing_only", 0, 1), ("both", 1, 1), ("tie_limit", 1, 0)]
    scenarios = [(d, label, owner, weight) for d in demands for label, owner, weight in cases]
    transit = [[0.] * n for _ in scenarios]
    hops = [0.] * len(scenarios)
    histogram = collections.Counter()
    for dest in range(n):
        distance = [-1] * n
        distance[dest] = 0
        queue = [dest]
        for node in queue:
            for other in adjacency[node]:
                if distance[other] < 0:
                    distance[other] = distance[node] + 1
                    queue.append(other)
        assert len(queue) == n and max(distance) <= 4
        histogram.update(distance)
        eligible = [[j for j in adjacency[i] if distance[j] == distance[i] - 1] for i in range(n)]
        for s, (d, _, owner, weight) in enumerate(scenarios):
            demand = demands[d]
            fraction = p["owner"][owner][dest] / p["total"]
            flow = demand.copy()
            for node in reversed(queue[1:]):
                nexts = eligible[node]
                weights = [weight if j in bad else 4 for j in nexts]
                denominator = sum(weights)
                if denominator == 0:
                    weights = [1] * len(nexts)
                    denominator = len(nexts)
                for j, w in zip(nexts, weights):
                    flow[j] += flow[node] * w / denominator
                transit[s][node] += (flow[node] - demand[node]) * fraction
            hops[s] += sum(distance[i] * demand[i] for i in range(n)) * fraction
        if dest % 250 == 0:
            print("routing destination", dest, flush=True)
    results = {}
    for s, (d, label, owner, _) in enumerate(scenarios):
        demand = demands[d]
        total_demand = sum(demand)
        rows = []
        for i in range(n):
            fraction = p["owner"][owner][i] / p["total"]
            served = (total_demand - demand[i]) * fraction
            received = demand[i] * (1 - fraction)
            rows.append(dict(node=p["names"][i], transit=transit[s][i], owner=served,
                             local=received, tx=served + transit[s][i], rx=received + transit[s][i]))
        assert abs(sum(r["tx"] for r in rows) - hops[s]) < 1e-7
        assert abs(sum(r["rx"] for r in rows) - hops[s]) < 1e-7
        results[d + "/" + label] = dict(
            demand_GBs=total_demand / 8, mean_links=hops[s] / total_demand,
            cohort=[rows[i] for i in p["bad"]],
            cohort_sums={k: sum(rows[i][k] for i in bad) for k in ("transit", "owner", "local", "tx", "rx")},
            healthy={k: stats([rows[i][k] for i in healthy]) for k in ("transit", "tx", "rx")},
            max_expected={k: max(rows, key=lambda r: r[k]) for k in ("tx", "rx")},
            rows=rows)
    return dict(model="Exact expected primary-hit payload Gbit/s, not actual flow telemetry; no retries/local caching/coalescing",
                histogram=dict(histogram), max_degree=max(map(len, adjacency)),
                nodes=n, input_sha256=p["input_sha256"], scenarios=results)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("phase", choices=("placement", "routing"))
    for name in ("membership", "cohort", "inventory", "baseline", "catalog", "checkpoint"):
        parser.add_argument("--" + name)
    parser.add_argument("--output", required=True)
    args = parser.parse_args()
    result = placement(args) if args.phase == "placement" else routing(args)
    Path(args.output).write_text(json.dumps(result, indent=2) + "\n")
    print("saved", args.output, flush=True)


if __name__ == "__main__":
    main()
