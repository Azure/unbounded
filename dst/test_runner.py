# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0
import unittest

from run import campaign_summary, classify, deletions, execute, failure_identity, gate, reductions, witness_signature


class RunnerTests(unittest.TestCase):
    def test_zero_matches_and_ignored_tests_are_not_passes(self):
        self.assertEqual(classify(0, "test result: ok. 0 passed; 0 failed; 0 ignored;", False, 0), "unexercised")
        self.assertEqual(classify(0, "test result: ok. 1 passed; 0 failed; 1 ignored;", False, 2), "unexercised")
        self.assertEqual(classify(0, "test result: ok. 2 passed; 0 failed; 0 ignored;", False, 2), "pass")

    def test_failure_classes(self):
        self.assertEqual(classify(101, "test result: FAILED.", False, 1), "product_failure")
        self.assertEqual(classify(-9, "", False, 1), "infrastructure_failure")
        self.assertEqual(classify(0, "", True, 1), "infrastructure_failure")
        self.assertEqual(classify(101, "invalid scenario: input JSON", False, 1), "invalid_scenario")

    def test_reduction_requires_named_product_failure(self):
        self.assertIsNone(failure_identity({"status": "simulator_failure", "failure": "response.status"}))
        self.assertIsNone(failure_identity({"status": "product_failure", "failure": "arbitrary panic"}))
        self.assertEqual(failure_identity({"status": "product_failure", "failure": {"oracle": "response.status"}}), "response.status")
        proposals = list(deletions([1, 2, 3, 4]))
        self.assertIn([1, 3, 4], proposals)
        self.assertTrue(all(len(proposal) < 4 for proposal in proposals))
        self.assertEqual(list(deletions([1])), [[]])
        self.assertEqual(list(deletions([])), [])

    def test_deadline_kills_process_group(self):
        code, _, expired, seconds = execute(["python3", "-c", "import time; time.sleep(30)"], 0.1)
        self.assertTrue(expired)
        self.assertLess(code, 0)
        self.assertLess(seconds, 5)

    def test_reduction_dimensions_preserve_seeds_and_do_not_mutate_input(self):
        source = {"nodes": 8, "rdma": True, "zc_retirement": True,
                  "phase_policy": "Permuted", "peer_failure_delay": 17,
                  "seeds": {"scheduler": 19}, "mutant": "PrematureZcRetirement",
                  "actions": [{"Turn": 8}, {"WallOffset": [0, -7]},
                              {"CrashSectors": [0, [3, 9, 15]]}]}
        proposals = list(reductions(source))
        dimensions = {dimension for dimension, _ in proposals}
        self.assertEqual(dimensions, {"actions", "nodes", "rdma", "zc_retirement",
                                     "phase_policy", "peer_failure_delay", "actions.0.Turn",
                                     "actions.1.WallOffset", "actions.2.CrashSectors"})
        self.assertEqual([proposal["nodes"] for dimension, proposal in proposals
                          if dimension == "nodes"], [2, 4, 7])
        self.assertTrue(all(proposal["seeds"] == source["seeds"] and
                            proposal["mutant"] == source["mutant"] for _, proposal in proposals))
        self.assertIn(("actions.1.WallOffset", dict(source, actions=[
            {"Turn": 8}, {"WallOffset": [0, -3]}, {"CrashSectors": [0, [3, 9, 15]]}])), proposals)
        self.assertEqual(source["actions"][2], {"CrashSectors": [0, [3, 9, 15]]})
        self.assertEqual(list(reductions({"nodes": 2, "actions": []})), [])

    def test_reduction_preserves_fault_scope_and_mutant_activation(self):
        def history(kind, fields, tick=1):
            return {"kind": "history", "value": {"node": 2, "worker": 0,
                    "incarnation": 1, "tick": tick, "transition": {kind: fields}}}

        terminal = {"kind": "terminal"}
        source = [history("FaultArmed", {"fault": 3, "target": "/a"}),
                  history("FaultEffective", {"fault": 3}),
                  history("MutantActivated", {"mutant": "SuccessfulGetStatus"}),
                  history("Response", {"request": 7, "status": 201}), terminal]
        required = witness_signature(source)
        self.assertEqual(required, witness_signature([
            history("ActionExecuted", {"index": 12}), *source]))
        reindexed = [history("FaultArmed", {"fault": 0, "target": "/a"}, 20),
                     history("FaultEffective", {"fault": 0}, 21),
                     source[2], history("Response", {"request": 1, "status": 201}), terminal]
        self.assertEqual(required, witness_signature(reindexed))
        self.assertFalse(required.issubset(witness_signature(source[:2] + source[3:])))
        wrong_target = [history("FaultArmed", {"fault": 3, "target": "/b"}), *source[1:]]
        self.assertFalse(required.issubset(witness_signature(wrong_target)))
        with self.assertRaises(ValueError):
            witness_signature(source[:-1])
        with self.assertRaises(ValueError):
            witness_signature(source[1:])

    def test_required_cell_needs_witnesses_replay_and_control(self):
        cell = {"expected": "product_failure", "oracle": "response.status",
                "control": "control", "minimum_transitions": {"MutantActivated": 1}}
        semantic = {"status": "product_failure", "failure": {"oracle": "response.status"}}
        witnesses = {"terminal_record_present": True, "transitions": {"MutantActivated": 1}}
        self.assertEqual(gate(cell, "product_failure", semantic, witnesses, True, {"control": "pass"}), "pass")
        self.assertEqual(gate(cell, "product_failure", semantic, witnesses, True, {}), "unexercised")
        self.assertEqual(gate(cell, "product_failure", semantic, witnesses, False, {"control": "pass"}), "replay_divergence")
        self.assertEqual(gate(cell, "product_failure", semantic, dict(witnesses, transitions={}), True, {"control": "pass"}), "unexercised")
        self.assertEqual(gate(cell, "pass", {}, witnesses, True, {"control": "pass"}), "unexercised")
        self.assertEqual(gate(cell, "simulator_failure", semantic, witnesses, True, {"control": "pass"}), "simulator_failure")
        self.assertEqual(gate(cell, "product_failure", semantic, dict(witnesses, terminal_record_present=False), True, {"control": "pass"}), "unexercised")

    def test_campaign_denominator_preserves_unattempted_and_failed_cells(self):
        cells = [{"id": name, "scenario": "artifact", "minimum_transitions": {"Response": 2}}
                 for name in ("passed", "partial", "pending")]
        cells.append({"id": "unavailable", "scenario": "absent", "minimum_transitions": {}})
        runs = [{"cell": "passed", "outcome": "pass", "exact_replay": True,
                 "coverage": {"transitions": {"Response": 2}}},
                {"cell": "partial", "outcome": "unexercised", "exact_replay": True,
                 "coverage": {"transitions": {"Response": 1}}}]
        summary = campaign_summary(cells, runs)
        self.assertEqual([summary[key] for key in ("planned", "feasible", "attempted", "exercised")], [4, 3, 2, 1])
        self.assertEqual(summary["transitions"], {"Response": 3})
        self.assertEqual(summary["gaps"][0]["missing_transitions"], {"Response": 1})
        self.assertEqual(summary["gaps"][1]["outcome"], "not_attempted")
        self.assertFalse(summary["gaps"][2]["feasible"])


if __name__ == "__main__":
    unittest.main()
