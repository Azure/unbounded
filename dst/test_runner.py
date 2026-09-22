# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0
import unittest

from run import classify, deletions, execute, failure_identity, gate


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


if __name__ == "__main__":
    unittest.main()
