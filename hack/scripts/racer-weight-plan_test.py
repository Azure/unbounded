# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0
import importlib.util
import json
import os
from pathlib import Path
import tempfile
import unittest

spec = importlib.util.spec_from_file_location("planner", Path(__file__).with_name("racer-weight-plan.py"))
p = importlib.util.module_from_spec(spec)
spec.loader.exec_module(p)


class PlannerTests(unittest.TestCase):
    def test_vectors(self):
        p.forecast.check_vectors()

    def test_integer_weight_ties_and_scale(self):
        choices = [[(12, 1), (3, 0)], [(3, 0), (11, 1)]]
        a = p.place([1, 4], choices, [7, 9])
        self.assertEqual(a, ([7, 9], [0, 1]))
        self.assertEqual(a, p.place([100, 400], choices, [7, 9]))

    def test_graph_independent_inverse(self):
        for n in (1, 33, 81):
            for i, neighbors in enumerate(p.graph(n)):
                oracle = [j for j in range(n) if i != j and any(
                    (32 * i + k) % n == j or (32 * j + k) % n == i for k in range(32))]
                self.assertEqual(neighbors, oracle)

    def test_route_conservation_and_line_oracle(self):
        result = p.route([4, 1, 4], [0, 0, 10], [[3, 0, 0]], [[1], [0, 2], [1]])[0]
        self.assertEqual(result, ([0, 3, 3], [3, 3, 0]))
        self.assertEqual(sum(result[0]), sum(result[1]))
        self.assertEqual(p.route([1], [1], [[5]], [[]])[0], ([0], [0]))
        with self.assertRaisesRegex(ValueError, "disconnected"):
            p.route([1, 1], [1, 1], [[1, 1]], [[], []])

    def test_normalization_bounds_and_determinism(self):
        w = [400] * 10 + [70]
        loads = [1, 100, 10, 20, 30, 40, 50, 60, 70, 80, 2]
        out = p.normalize(w, loads, list(range(10)))
        self.assertEqual(sum(out[:10]), 4000)
        self.assertTrue(all(200 <= x <= 800 for x in out[:10]))
        self.assertEqual(out[-1], 70)
        self.assertEqual(out, p.normalize(w, loads, list(range(10))))

    def test_invalid_input_fails_closed(self):
        with self.assertRaises(ValueError):
            p.validate({"schema": 0, "baseline_timestamp": p.STAMP, "cache": p.CACHE})
        with self.assertRaises(ValueError):
            p.attest({"map_sha256": "bad"}, {})

    def test_output_never_overwrites(self):
        # Test scratch is confined to the worktree; removed by TemporaryDirectory.
        with tempfile.TemporaryDirectory(dir=Path(__file__).resolve().parents[2] / "tmp") as directory:
            path = Path(directory) / "result.json"
            p.save(path, {"ok": True})
            with self.assertRaises(FileExistsError):
                p.save(path, {})
            self.assertEqual(json.loads(path.read_text()), {"ok": True})

    @unittest.skipUnless(os.environ.get("RACER_WEIGHT_ARTIFACTS"), "explicit retained artifacts required")
    def test_retained_exact_plan_and_publication(self):
        root = Path(os.environ["RACER_WEIGHT_ARTIFACTS"])
        inputs = json.loads((root / "inputs.json").read_text())
        result = json.loads((root / "plan.json").read_text())
        p.validate(inputs)
        self.assertEqual(result["input_sha256"], p.digest(inputs))
        self.assertEqual(result["map_sha256"], p.MAP_HASH)
        self.assertEqual(result["selected"]["iteration"], 5)
        self.assertAlmostEqual(result["selected"]["peak"], 11.006339470400885)
        self.assertAlmostEqual(result["baseline_fit_r"], .9986836517935267)
        for row in result["nodes"]:
            self.assertEqual(row["rollback"]["value"], row["original_annotation"])
            if row["shares"] == 1:
                for scenario in ("fixed", "equal_healthy"):
                    for direction in ("tx", "rx"):
                        self.assertLessEqual(row[scenario][direction], row["baseline"][direction])
        publication = {"sequence": "9", "membership_version": "8", "caches": [{"id": p.CACHE}],
                       "members": [{"node": r["uid"], "shares": int(r["candidate_annotation"])}
                                   for r in result["nodes"]]}
        self.assertTrue(p.attest(result, publication)["controller_map_matches"])
        self.assertFalse(p.attest(result, publication)["all_dataplanes_attested"])
        publication["members"][0]["shares"] += 1
        with self.assertRaisesRegex(ValueError, "shares differ"):
            p.attest(result, publication)
        publication["members"].pop()
        with self.assertRaises(ValueError):
            p.attest(result, publication)


if __name__ == "__main__":
    unittest.main()
