#!/usr/bin/env python3
"""Summarize per-node NIC samples collected by `NIC_SECS=<n> bench.sh run ...`.

Usage: nicstats.py <results.tsv> [step-seconds]

Each NIC line is `NIC <node> 0:<tx>:<rx> 1:<tx>:<rx> ...` with cumulative eth0
byte counters sampled once per second from the common start time. The report
shows, per time step, the fleet distribution of per-node transmit and receive
rates, and the distribution of each node's total transmitted bytes. Transmit
concentrated on few nodes means a few serving nodes bound the fleet.
"""

import sys


def pct(v, p):
    if not v:
        return 0.0
    v = sorted(v)
    return v[min(len(v) - 1, int(round(p / 100 * (len(v) - 1))))]


def main():
    path = sys.argv[1]
    step = int(sys.argv[2]) if len(sys.argv) > 2 else 5
    nodes = {}
    skipped = 0
    for line in open(path):
        f = line.split()
        if not f or f[0] != "NIC":
            continue
        samples = []
        try:
            for s in f[2:]:
                i, tx, rx = s.split(":")
                samples.append((int(i), int(tx), int(rx)))
        except ValueError:
            skipped += 1
            continue
        nodes[f[1]] = samples
    if not nodes:
        sys.exit("no NIC lines")
    if skipped:
        print(f"skipped {skipped} malformed NIC lines")
    n = min(len(s) for s in nodes.values())
    mb = 1e6
    print(f"nodes={len(nodes)} seconds={n - 1} (rates MB/s over {step}s windows)")
    print(f"{'t':>4} {'tx p50':>8} {'tx p90':>8} {'tx p99':>8} {'tx max':>8} {'rx p50':>8} {'rx p99':>8} {'fleet rx GB/s':>14}")
    for t in range(step, n, step):
        tx = [(s[t][1] - s[t - step][1]) / step / mb for s in nodes.values()]
        rx = [(s[t][2] - s[t - step][2]) / step / mb for s in nodes.values()]
        print(f"{t:>4} {pct(tx, 50):8.0f} {pct(tx, 90):8.0f} {pct(tx, 99):8.0f} {max(tx):8.0f} "
              f"{pct(rx, 50):8.0f} {pct(rx, 99):8.0f} {sum(rx) / 1000:14.1f}")
    tot = [(s[n - 1][1] - s[0][1]) / 1e9 for s in nodes.values()]
    peak = [max((s[t][1] - s[t - 1][1]) / mb for t in range(1, n)) for s in nodes.values()]
    print(f"per-node total tx GB: p10={pct(tot, 10):.2f} p50={pct(tot, 50):.2f} p90={pct(tot, 90):.2f} "
          f"p99={pct(tot, 99):.2f} max={max(tot):.2f} sum={sum(tot):.0f}")
    print(f"per-node peak 1s tx MB/s: p50={pct(peak, 50):.0f} p90={pct(peak, 90):.0f} p99={pct(peak, 99):.0f} max={max(peak):.0f}")


if __name__ == "__main__":
    main()
