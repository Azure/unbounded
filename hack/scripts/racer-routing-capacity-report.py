# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0
"""Summarize model results and baseline-calibrated demand sensitivity."""
import json
import sys
from pathlib import Path

p = json.loads(Path(sys.argv[1]).read_text())
m = json.loads(Path(sys.argv[2]).read_text())
if len(sys.argv) == 4:
    compact = {k: v for k, v in m.items() if k != "scenarios"}
    compact["scenarios"] = {name: {k: v for k, v in s.items() if k != "rows"}
                            for name, s in m["scenarios"].items()}
    compact["baseline_cohort"] = [dict(node=p["names"][i], uid=p["ids"][i], **p["baseline"][i]) for i in p["bad"]]
    Path(sys.argv[3]).write_text(json.dumps(compact, indent=2) + "\n")
    print("saved compact model", sys.argv[3])
    sys.exit(0)
for name, s in m["scenarios"].items():
    if name.startswith("uniform/"):
        continue
    print(name, "cohort", {k: round(v, 3) for k, v in s["cohort_sums"].items()},
          "healthy", s["healthy"], "links", s["mean_links"])
base = m["scenarios"]["baseline/uniform"]["rows"]
print("\nAll eleven exact expected transit and TX/RX Gbit/s (baseline, routing-only, both)")
for i in sorted(p["bad"], key=lambda i: p["names"][i]):
    rows = [m["scenarios"]["baseline/" + label]["rows"][i] for label in ("uniform", "routing_only", "both")]
    print(p["names"][i], [[round(r[k], 3) for k in ("transit", "tx", "rx")] for r in rows])
for label in ("routing_only", "both", "tie_limit"):
    print("\n", label, "all eleven: baseline calibrated TX/RX fixed-demand and recovered-demand")
    fixed = m["scenarios"]["baseline/" + label]["rows"]
    recovery = m["scenarios"]["recovered/" + label]["rows"]
    feedback = []
    for i in p["bad"]:
        b = p["baseline"][i]
        # A ratio calibration preserves positivity and exposes the large mismatch
        # between idealized primary-hit traffic and actual delivered NIC traffic.
        values = [b[k + "_Gbps"] * r[i][k] / base[i][k]
                  for r in (fixed, recovery) for k in ("tx", "rx")]
        print(p["names"][i], "transit relief%", round(100 * (1 - fixed[i]["transit"] / base[i]["transit"]), 2),
              "measured", round(b["tx_Gbps"], 3), round(b["rx_Gbps"], 3),
              "calibrated", [round(v, 3) for v in values])
        # Fit only relay traffic to measured RX minus useful local consumption.
        # This optimistic sensitivity assumes no overhead and unchanged caching.
        alpha = max(0, b["rx_Gbps"] - base[i]["local"]) / base[i]["transit"]
        beta = max(0, b["tx_Gbps"] - alpha * base[i]["transit"]) / base[i]["owner"]
        def fitted(r):
            return (alpha * r["transit"] + beta * r["owner"],
                    alpha * r["transit"] + r["local"])
        tx, rx = fitted(fixed[i])
        rtx, rrx = fitted(recovery[i])
        limit = min(1, (b["tx_Gbps"] - tx) / (rtx - tx), (b["rx_Gbps"] - rx) / (rrx - rx))
        feedback.append(limit)
        print("  residual-fit fixed/recovered", [round(v, 3) for v in (tx, rx, rtx, rrx)],
              "recovery fraction at baseline operating caps", round(limit, 4))
    fraction = max(0, min(feedback))
    baseline_goodput = sum(p["baseline"][i]["verified_GBs"] for i in p["bad"])
    healthy_mean = sum(r["verified_GBs"] for i, r in enumerate(p["baseline"]) if i not in p["bad"]) / 1489
    print("cohort common recovery fraction", fraction, "conditional cohort GB/s",
          baseline_goodput + fraction * (11 * healthy_mean - baseline_goodput))
    for key in ("tx", "rx"):
        ratios = [(fixed[i][key] / base[i][key], i) for i in range(1500) if i not in p["bad"]]
        ratio, i = max(ratios)
        print("max healthy relative", key, ratio, p["names"][i], "host_cpu", p["baseline"][i]["host_cpu"])
