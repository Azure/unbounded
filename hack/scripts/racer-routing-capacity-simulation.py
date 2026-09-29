# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0
"""Finite-flow hash simulation of expected maxima, using actual checkpoint UIDs.

Eight reproducible seeds, 64 payload samples per consumer per seed. Each sample
represents 1/64 of that consumer's baseline demand, with catalog-byte-weighted
owner sampling. This estimates variability, not the live number of independent
flows or queue dynamics. No endpoint is omitted.
"""
import bisect
import hashlib
import importlib.util
import itertools
import json
import random
import sys
from pathlib import Path

spec = importlib.util.spec_from_file_location("model", Path(__file__).with_name("racer-routing-capacity-model.py"))
model = importlib.util.module_from_spec(spec)
spec.loader.exec_module(model)
p = model.read(sys.argv[1])
n = len(p["ids"])
bad = set(p["bad"])
adjacency = model.graph(n)
eligible = []
for dest in range(n):
    distance = [-1] * n
    distance[dest] = 0
    queue = [dest]
    for node in queue:
        for other in adjacency[node]:
            if distance[other] == -1:
                distance[other] = distance[node] + 1
                queue.append(other)
    eligible.append([[j for j in adjacency[i] if distance[j] == distance[i] - 1] for i in range(n)])

prefixes = [model.frame(uid.encode()) for uid in p["ids"]]
cumulative = [list(itertools.accumulate(owner)) for owner in p["owner"]]
out = {}
for label, owner, version in (("uniform", 0, 3), ("routing_only", 0, 4), ("both", 1, 4)):
    peaks = []
    transit_sum = [0.] * n
    for seed in range(8):
        rng = random.Random(seed)
        tx = [0.] * n
        rx = [0.] * n
        transit = [0.] * n
        for source in range(n):
            amount = p["baseline"][source]["verified_GBs"] * 8 / 64
            for sample in range(64):
                dest = bisect.bisect_right(cumulative[owner], rng.randrange(p["total"]))
                request = ((seed * n + source) * 64 + sample).to_bytes(16, "big")
                current = source
                links = 0
                while current != dest:
                    nexts = eligible[dest][current]
                    if len(nexts) == 1:
                        next_node = nexts[0]
                    else:
                        data = (f"racer/next-hop/v{version}\0".encode() + request + bytes([2]) * 16
                                + prefixes[current] + prefixes[dest])
                        if version == 3:
                            draw = int.from_bytes(hashlib.sha256(data).digest()[:8], "big")
                            next_node = nexts[draw % len(nexts)]
                        else:
                            weights = [1 if j in bad else 4 for j in nexts]
                            total = sum(weights)
                            for counter in range(64):
                                draw = int.from_bytes(hashlib.sha256(data + counter.to_bytes(4, "big")).digest()[:8], "big")
                                if draw >= (1 << 64) % total:
                                    break
                            else:
                                raise AssertionError("rejection cap")
                            ticket = draw % total
                            next_node = nexts[bisect.bisect_right(list(itertools.accumulate(weights)), ticket)]
                    rx[current] += amount
                    tx[next_node] += amount
                    if next_node != dest:
                        transit[next_node] += amount
                    current = next_node
                    links += 1
                assert links <= 4
        assert abs(sum(tx) - sum(rx)) < 1e-7
        peaks.append(dict(tx=max(tx), rx=max(rx)))
        transit_sum = [a + b / 8 for a, b in zip(transit_sum, transit)]
    out[label] = dict(mean_sample_max_Gbps={k: sum(r[k] for r in peaks) / 8 for k in ("tx", "rx")},
                      largest_sample_max_Gbps={k: max(r[k] for r in peaks) for k in ("tx", "rx")},
                      cohort_transit_mean_Gbps=sum(transit_sum[i] for i in bad), seed_maxima=peaks)
    print(label, out[label], flush=True)
Path(sys.argv[2]).write_text(json.dumps(dict(seeds=8, samples_per_source=64, nodes=n, scenarios=out), indent=2) + "\n")
