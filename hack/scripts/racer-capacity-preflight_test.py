# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0

"""Focused preflight safety checks; no cluster access or files written."""
import copy
import importlib.util
import json
from pathlib import Path
import subprocess
import unittest
from unittest.mock import patch

SPEC = importlib.util.spec_from_file_location("preflight", Path(__file__).with_name("racer-capacity-preflight.py"))
MODULE = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(MODULE)


class PreflightTests(unittest.TestCase):
    def execute(self, mutate=None, reject=False):
        row = {"node": "worker", "uid": "expected-uid"}
        node = {"metadata": {"name": "worker", "uid": "expected-uid", "resourceVersion": "42", "annotations": {
            "racer.unbounded-cloud.io/enrolled-shares": "4",
            "racer.unbounded-cloud.io/last-admitted-member": json.dumps({"node": "expected-uid", "shares": 4})}}}
        if mutate:
            mutate(node["metadata"])
        calls = []

        def kube(*args):
            calls.append(args)
            if args[:2] == ("get", "nodes"):
                return {"items": [node]}
            if args[0] == "patch":
                self.assertIn("--dry-run=server", args)
                operations = json.loads(args[args.index("-p") + 1])
                self.assertEqual([o["op"] for o in operations], ["test", "test", "add"])
                self.assertEqual(operations[0]["value"], row["uid"])
                self.assertEqual(operations[1]["value"], "42")
                if reject:
                    raise subprocess.CalledProcessError(1, "kubectl")
                proposed = copy.deepcopy(node)
                proposed["metadata"]["annotations"]["racer.unbounded-cloud.io/shares"] = "1"
                return proposed
            return {"items": []}

        with patch.object(MODULE, "kube", side_effect=kube), \
                patch("sys.argv", ["preflight", "forecast.json", "output.json"]), \
                patch.object(Path, "read_text", return_value=json.dumps({"cohort": [row]})), \
                patch.object(Path, "write_text") as write:
            try:
                MODULE.main()
            except (AssertionError, subprocess.CalledProcessError):
                write.assert_not_called()
                raise
            output = json.loads(write.call_args.args[0])
            self.assertFalse(output["applied"])
            self.assertEqual(output["patches"][0]["previous_override"], None)
        return calls

    def test_dry_run_only_and_guarded_patch(self):
        self.assertEqual(len(self.execute()), 3)

    def test_rejects_identity_or_existing_policy_changes(self):
        for mutate in [
            lambda m: m.update(uid="replacement-uid"),
            lambda m: m.update(labels={"racer.unbounded-cloud.io/exclude": ""}),
            lambda m: m["annotations"].update({"racer.unbounded-cloud.io/shares": "2"}),
            lambda m: m["annotations"].update({"racer.unbounded-cloud.io/enrolled-shares": "1"}),
        ]:
            with self.subTest(mutate=mutate), self.assertRaises(AssertionError):
                self.execute(mutate)

    def test_admission_failure_does_not_emit_apply_plan(self):
        with self.assertRaises(subprocess.CalledProcessError):
            self.execute(reject=True)


if __name__ == "__main__":
    unittest.main()
