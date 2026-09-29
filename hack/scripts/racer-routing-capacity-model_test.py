# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0
"""Independent small-graph and retained 1500-node expectation assertions."""
import importlib.util
import json
import os
from pathlib import Path
import unittest

spec = importlib.util.spec_from_file_location("model", Path(__file__).with_name("racer-routing-capacity-model.py"))
model = importlib.util.module_from_spec(spec)
spec.loader.exec_module(model)


class ModelTests(unittest.TestCase):
    def test_graph_inverse(self):
        for n in (1, 18, 37, 401):
            graph = model.graph(n)
            for i, neighbors in enumerate(graph):
                expected = [j for j in range(n) if j != i and any(
                    (18 * i + k) % n == j or (18 * j + k) % n == i for k in range(18))]
                self.assertEqual(neighbors, expected)
                self.assertLessEqual(len(neighbors), 36)

    def test_cost_vectors(self):
        self.assertEqual(model.cost(0), 64 << 32)
        self.assertEqual(model.cost((1 << 64) - 1), 1)
        for sample, expected in ((0xe41095812e885f6f, 715971622), (0xc4251196ce419070, 1650232626),
                                 (0x9322018f0806e768, 3431784333), (0xb379deaba20d903a, 2200536977)):
            self.assertEqual(model.cost(sample), expected)

    @unittest.skipUnless(os.environ.get("RACER_MODEL_CHECKPOINT"), "requires explicit placement checkpoints")
    def test_exact_cutoff_recheck(self):
        path = Path(os.environ["RACER_MODEL_CHECKPOINT"])
        original = json.loads(path.read_text())
        recheck = json.loads(path.with_name(path.stem + "-recheck.json").read_text())
        self.assertEqual(original, recheck)

    @unittest.skipUnless(os.environ.get("RACER_MODEL_RESULT"), "requires explicit retained actual-UID result")
    def test_actual_1500_expectation(self):
        result = json.loads(Path(os.environ["RACER_MODEL_RESULT"]).read_text())
        self.assertEqual(result["nodes"], 1500)
        self.assertEqual(sum(result["histogram"].values()), 1500 ** 2)
        self.assertLessEqual(max(map(int, result["histogram"])), 4)
        scenarios = result["scenarios"]
        for demand in ("baseline", "uniform", "recovered"):
            base = scenarios[demand + "/uniform"]
            for label in ("routing_only", "both"):
                changed = scenarios[demand + "/" + label]
                self.assertEqual(len(changed["rows"]), 1500)
                self.assertEqual(len(changed["cohort"]), 11)
                self.assertTrue(all(r["local"] > 0 for r in changed["rows"]))
                for before, after in zip(base["cohort"], changed["cohort"]):
                    self.assertEqual(before["node"], after["node"])
                    self.assertLess(after["transit"], .72 * before["transit"])
                self.assertAlmostEqual(sum(r["tx"] for r in changed["rows"]),
                                       sum(r["rx"] for r in changed["rows"]), places=7)
            self.assertEqual(base["mean_links"], scenarios[demand + "/routing_only"]["mean_links"])
            self.assertEqual(scenarios[demand + "/owner_only"]["mean_links"], scenarios[demand + "/both"]["mean_links"])


if __name__ == "__main__":
    unittest.main()
