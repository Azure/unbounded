# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0

"""Static contracts for the hand-edited dashboard; no third-party dependencies."""

import json
from pathlib import Path
import re
import unittest


HERE = Path(__file__).resolve().parent
ROOT = HERE.parent.parent
DATASOURCE = {"type": "prometheus", "uid": "prometheus"}


def unique_object(pairs):
    result = {}
    for key, value in pairs:
        if key in result:
            raise ValueError(f"duplicate JSON key: {key}")
        result[key] = value
    return result


class DashboardContractTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.dashboard = json.loads(
            (HERE / "grafana-dashboard.json").read_text(),
            object_pairs_hook=unique_object,
        )
        cls.panels = [p for p in cls.dashboard["panels"] if p["type"] != "row"]

    def test_identity_and_layout(self):
        self.assertEqual(self.dashboard["uid"], "racer-performance")
        self.assertEqual(self.dashboard["refresh"], "1m")
        ready = next(p for p in self.panels if p["id"] == 3)
        # Preserve exact fleet counts: short/decimals=0 renders 1500 as 2 K.
        self.assertEqual(ready["fieldConfig"]["defaults"]["unit"], "none")
        self.assertEqual(ready["fieldConfig"]["defaults"]["decimals"], 0)
        self.assertGreaterEqual(len(self.panels), 24)
        self.assertLessEqual(len(self.panels), 30)
        ids = [p["id"] for p in self.dashboard["panels"]]
        self.assertEqual(len(ids), len(set(ids)))
        cells = set()
        for panel in self.dashboard["panels"]:
            pos = panel["gridPos"]
            self.assertGreater(pos["h"], 0)
            self.assertGreater(pos["w"], 0)
            self.assertGreaterEqual(pos["x"], 0)
            self.assertGreaterEqual(pos["y"], 0)
            self.assertLessEqual(pos["x"] + pos["w"], 24)
            for x in range(pos["x"], pos["x"] + pos["w"]):
                for y in range(pos["y"], pos["y"] + pos["h"]):
                    self.assertNotIn((x, y), cells, panel["title"])
                    cells.add((x, y))

    def test_variables_and_discovery(self):
        variables = {v["name"]: v for v in self.dashboard["templating"]["list"]}
        self.assertEqual(set(variables), {"namespace", "node"})
        self.assertEqual(variables["namespace"]["current"]["value"], ["unbounded-system"])
        for variable in variables.values():
            self.assertEqual(variable["datasource"], DATASOURCE)
            self.assertTrue(variable["multi"])
            self.assertTrue(variable["includeAll"])
            self.assertEqual(variable["allValue"], ".*")
            self.assertIn('job="kubernetes-pods"', variable["query"]["query"])
            self.assertIn("app_kubernetes_io_name", variable["query"]["query"])

    def test_queries_scoped_and_no_fabricated_health(self):
        for panel in self.panels:
            self.assertEqual(panel["datasource"], DATASOURCE)
            self.assertEqual(panel["fieldConfig"]["defaults"]["noValue"], "No data")
            self.assertTrue(panel["description"])
            refs = [t["refId"] for t in panel["targets"]]
            self.assertEqual(len(refs), len(set(refs)))
            for target in panel["targets"]:
                expr = target["expr"]
                with self.subTest(panel=panel["id"], target=target["refId"]):
                    self.assertIn('node=~"${node:regex}"', expr)
                    if "node_cpu_" not in expr and "node_memory_" not in expr and "node_network_" not in expr:
                        self.assertIn('namespace=~"${namespace:regex}"', expr)
                        self.assertIn('job="kubernetes-pods"', expr)
                    else:
                        self.assertIn('node!=""', expr)
                    self.assertNotIn("or vector", expr)
                    self.assertNotIn("clamp_min", expr)
                    # rate drops metric names: a multi-family selector can collide.
                    self.assertNotIn("__name__=~", expr)
                    self.assertNotIn("container_", expr)
                    self.assertNotIn('job="racer-dataplane"', expr)
                    self.assertNotIn('job="racer-gantry"', expr)
                    self.assertEqual(expr.count("("), expr.count(")"))
                    self.assertEqual(expr.count("{"), expr.count("}"))
                    for window in re.findall(r"\[([^]]+)\]", expr):
                        # NIC device regex is not a range selector.
                        if window != "0-9":
                            self.assertEqual(window, "5m")

    def test_outliers_are_instant_top_ten(self):
        for panel in self.panels:
            for target in panel["targets"]:
                if "{{node}}" in target.get("legendFormat", ""):
                    self.assertTrue(target.get("instant"), panel["title"])
                    self.assertTrue(target["expr"].startswith("topk(10,"))

    def test_histograms_and_lookup_ratios(self):
        for panel in self.panels:
            for target in panel["targets"]:
                expr = target["expr"]
                if "histogram_quantile(" in expr:
                    self.assertIn("_bucket{", expr)
                    self.assertRegex(expr, r"sum by \([^)]*\ble\b")
                if "lookup hit ratios" in panel["title"]:
                    self.assertIn("_lookup_hits_total", expr)
                    self.assertIn("_lookup_misses_total", expr)
                    self.assertNotIn("_lookup_errors_total", expr)
                    self.assertNotIn("racer_requests_total", expr)
                    self.assertNotIn("racer_memory_hits_total", expr)

    def test_metric_names_exist_in_implementation(self):
        rust = (ROOT / "cmd/racer-dataplane/src/telemetry/metrics.rs").read_text()
        rust += (ROOT / "cmd/racer-dataplane/src/telemetry/server.rs").read_text()
        gantry = (ROOT / "cmd/gantry/agent_racer_metrics.go").read_text()
        loadgen = (ROOT / "cmd/racer-loadgen/metrics.go").read_text()
        known = set(re.findall(r'"(racer_[a-z_]+)"', rust))
        known.update({"racer_ready", "racer_live"})
        known.update(re.findall(r'Name: "(gantry_racer_[a-z_]+)"', gantry))
        known.update("gantry_racer_sdk_" + name for name in re.findall(r'add\("([a-z_]+)"', gantry))
        known.update("racer_loadgen_" + name for name in re.findall(r'Name: "([a-z_]+)"', loadgen))
        for panel in self.panels:
            for target in panel["targets"]:
                for name in re.findall(r"\b((?:gantry_racer|racer)_[a-z_]+)\{", target["expr"]):
                    base = name.removesuffix("_bucket")
                    self.assertIn(base, known)

    def test_duplicate_keys_rejected(self):
        with self.assertRaisesRegex(ValueError, "duplicate JSON key"):
            json.loads('{"uid": "one", "uid": "two"}', object_pairs_hook=unique_object)


if __name__ == "__main__":
    unittest.main()
