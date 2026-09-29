# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0

"""Offline stage41 forecast. Reads retained evidence, never contacts Kubernetes."""
import argparse
import collections
import hashlib
import heapq
import json
from fractions import Fraction
from pathlib import Path


SUFFIXES = "00000h 00002i 00003z 00005f 000066 00006d 000078 0000b1 0000bl 0000cp 0000d9".split()
PAGE = 16 * 1024 * 1024


def load(root, name):
    return json.loads((root / name).read_text())


def framed(value):
    return len(value).to_bytes(4, "big") + value


def slot(cache, key, page):
    data = b"racer/slot/v1\0" + framed(cache.encode()) + key + page.to_bytes(8, "big")
    return int.from_bytes(hashlib.sha256(data).digest()[:4], "big") >> 12


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


def check_vectors():
    for page, expected_slot, order in [(0, 887651, [1, 3, 2]), (1, 348931, [3, 2, 1]), ((1 << 64) - 1, 665200, [2, 1, 3])]:
        key = slot("cache-a", bytes([0x42]) * 32, page)
        assert key == expected_slot
        scores = []
        for i, weight in enumerate([1, 3, 6, 4]):
            data = b"racer/hrw/v1\0" + key.to_bytes(4, "big") + framed(f"node-{i:06}".encode())
            sample = int.from_bytes(hashlib.sha256(data).digest()[:8], "big")
            scores.append((Fraction(cost(sample), weight), i))
        assert [i for _, i in sorted(scores)[:3]] == order
    for sample, expected in [(0xe41095812e885f6f, 715971622), (0xc4251196ce419070, 1650232626), (0x9322018f0806e768, 3431784333), (0xb379deaba20d903a, 2200536977)]:
        assert cost(sample) == expected


def summarize(result):
    rows = result["cohort"]
    before, after = result["cohort_primary_bytes"]
    return {
        "owner_reduction_percent": 100 * (1 - after / before),
        "changed_catalog_percent": 100 * result["changed_primary_bytes"] / result["catalog_layer_bytes"],
        "cohort_owner_Gbps_at_586_before_after": [sum(r[s]["owner_Gbps_at_586_GBs"] for r in rows) for s in ("before", "after")],
        "cohort_transit_Gbps_at_586_before_after": [sum(r[s]["transit_Gbps_at_586_GBs"] for r in rows) for s in ("before", "after")],
        "uniform_demand_cohort_capacity_bound_GBs": {
            str(capacity): {s: min(586 * capacity / max(
                r[s]["owner_Gbps_at_586_GBs"] + r[s]["transit_Gbps_at_586_GBs"],
                r[s]["local_remote_Gbps_at_586_GBs"] + r[s]["transit_Gbps_at_586_GBs"])
                for r in rows) for s in ("before", "after")}
            for capacity in (2, 4, 12.5)
        },
    }


def forecast(root):
    check_vectors()
    membership = load(root, "racer-stage40-recovery-membership.json")
    members = membership["members"]
    ids = [m["node"] for m in members]
    assert len(ids) == 1500 and ids == sorted(ids) and len(set(ids)) == 1500
    assert all(m["shares"] == 4 for m in members)
    inv = load(root, "racer-stage40-inventory.json")
    names = {n["metadata"]["uid"]: n["metadata"]["name"] for n in inv["nodes"]}
    bad = {i for i, uid in enumerate(ids) if names[uid] in {"aks-ddsv6-84072342-vmss" + s for s in SUFFIXES}}
    assert len(bad) == 11
    cache = load(root, "racer-stage28-inventory.json")["cache"]["items"][0]["metadata"]["uid"]
    records = load(root, "racer-stage16-origin-verify.json")["records"][:512]
    assert [r["index"] for r in records] == list(range(512))
    slots = collections.Counter()
    pages = collections.Counter()
    for record in records:
        for blob in record["layers"]:
            for page, offset in enumerate(range(0, blob["size"], PAGE)):
                key = slot(cache, bytes.fromhex(blob["digest"].split(":")[1]), page)
                slots[key] += min(PAGE, blob["size"] - offset)
                pages[key] += 1
    # Golden vector from topology/hash.rs and fixtures.rs.
    assert cost(0) == 64 << 32 and cost((1 << 64) - 1) == 1
    owner = [[0] * len(ids) for _ in range(2)]
    counts = [[0] * len(ids) for _ in range(2)]
    top3 = [[0] * len(ids) for _ in range(2)]
    suffixes = [framed(uid.encode()) for uid in ids]
    changed = 0
    for key, size in slots.items():
        prefix = hashlib.sha256(b"racer/hrw/v1\0" + key.to_bytes(4, "big"))
        samples = []
        for i, suffix in enumerate(suffixes):
            h = prefix.copy()
            h.update(suffix)
            samples.append((int.from_bytes(h.digest()[:8], "big"), i))
        # Exponential cost is monotonic. Keep all low-share nodes and the top
        # three healthy samples; no other healthy node can enter either top3.
        # Q32.32 costs can tie. Retain all healthy samples tied at the cutoff
        # cost so final node-position tie breaking is identical to Rust.
        candidates = heapq.nlargest(4, (s for s in samples if s[1] not in bad))
        cutoff = cost(candidates[2][0])
        if cost(candidates[3][0]) == cutoff:
            candidates = [(s, i) for s, i in samples if i not in bad and cost(s) <= cutoff]
        else:
            candidates = candidates[:3]
        candidates += [s for s in samples if s[1] in bad]
        scores = [(cost(sample), i) for sample, i in candidates]
        ranks = [sorted(scores), sorted((c * (4 if i in bad else 1), i) for c, i in scores)]
        for scenario, rank in enumerate(ranks):
            i = rank[0][1]
            owner[scenario][i] += size
            counts[scenario][i] += pages[key]
            for _, j in rank[:3]:
                top3[scenario][j] += size
        if ranks[0][0][1] != ranks[1][0][1]:
            changed += size
    print("placement complete", flush=True)
    # Exact healthy V3 next-hop expectation: equal probability over neighbors
    # one hop closer to the destination, reselected independently at each hop.
    # This is NOT a trace replay: misses, retries and coalescing are not modeled.
    n = len(ids)
    graph = [sorted(({(18 * i + j) % n for j in range(18)} | {(i + j * n) // 18 for j in range(18)}) - {i}) for i in range(n)]
    transit = [[0.0] * n for _ in range(2)]
    hops = [0.0, 0.0]
    for dest in range(n):
        distance = [-1] * n
        distance[dest] = 0
        queue = [dest]
        for node in queue:
            for other in graph[node]:
                if distance[other] < 0:
                    distance[other] = distance[node] + 1
                    queue.append(other)
        assert len(queue) == n and max(distance) <= 4
        flow = [1.0] * n
        for node in reversed(queue[1:]):
            nexts = [j for j in graph[node] if distance[j] == distance[node] - 1]
            for j in nexts:
                flow[j] += flow[node] / len(nexts)
            for scenario in range(2):
                transit[scenario][node] += (flow[node] - 1) * owner[scenario][dest]
        for scenario in range(2):
            hops[scenario] += sum(distance) * owner[scenario][dest]
    total = sum(slots.values())
    assert total == 274439398400 and sum(pages.values()) == 18417
    assert all(sum(o) == total for o in owner)
    assert all(owner[1][i] <= owner[0][i] for i in bad)
    assert all(owner[1][i] >= owner[0][i] for i in range(n) if i not in bad)
    rows = []
    for i in sorted(bad, key=lambda i: names[ids[i]]):
        row = {"node": names[ids[i]], "uid": ids[i], "position": i, "neighbors": len(graph[i])}
        for scenario, label in enumerate(("before", "after")):
            o = owner[scenario][i]
            t = transit[scenario][i] / (n * total)
            row[label] = {"primary_bytes": o, "primary_pages": counts[scenario][i], "top3_bytes": top3[scenario][i],
                          "owner_Gbps_at_586_GBs": o * (n - 1) / (n * total) * 586 * 8,
                          "transit_Gbps_at_586_GBs": t * 586 * 8,
                          "local_remote_Gbps_at_586_GBs": (total - o) / (n * total) * 586 * 8}
        rows.append(row)
    result = {"model": "layer payload only; uniform full-catalog demand on all1500; primary hit; healthy V3 expected next hops; no retries/coalescing/local retention",
              "membership_version": membership["version"], "cache": cache, "catalog_layer_bytes": total,
              "pages": sum(pages.values()), "slots": len(slots), "changed_primary_bytes": changed,
              "mean_links": [h / (n * total) for h in hops], "cohort": rows,
              "all_nodes_modeled": n, "healthy_nodes": n - len(bad),
              "cohort_primary_bytes": [sum(o[i] for i in bad) for o in owner],
              "cohort_top3_bytes": [sum(o[i] for i in bad) for o in top3]}
    return result


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("evidence", type=Path)
    parser.add_argument("--inspect", action="store_true")
    parser.add_argument("--output", type=Path)
    parser.add_argument("--vectors-only", action="store_true")
    parser.add_argument("--summarize", type=Path)
    args = parser.parse_args()
    root = args.evidence
    if args.summarize:
        print(json.dumps(summarize(json.loads(args.summarize.read_text())), indent=2))
        return
    if args.vectors_only:
        check_vectors()
        print("Rust placement/hash golden vectors passed")
        return
    if args.inspect:
        inv = load(root, "racer-stage28-inventory.json")
        print("cache", inv["cache"])
        before = load(root, "racer-stage40-before.json")
        for ds in before["ds"]["items"]:
            print(ds["metadata"]["name"], [(c["name"], c.get("args"), c.get("env")) for c in ds["spec"]["template"]["spec"]["containers"]])
        return
    result = forecast(root)
    if args.output:
        args.output.write_text(json.dumps(result, indent=2) + "\n")
    print(json.dumps(result, indent=2))


if __name__ == "__main__":
    main()
