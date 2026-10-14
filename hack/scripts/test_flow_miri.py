# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0
"""Check that the flow Miri gate cannot pass with missing or ignored tests."""

from pathlib import Path
import runpy
import unittest

FLOW = runpy.run_path(str(Path(__file__).with_name("flow-miri.py")))


class AllowlistTests(unittest.TestCase):
    def output(self, names=None, passed=None, ignored=0):
        names = FLOW["TESTS"] if names is None else names
        passed = len(names) if passed is None else passed
        return "\n".join([
            *(f"test {name} ... ok" for name in names),
            f"test result: ok. {passed} passed; 0 failed; {ignored} ignored;",
        ])

    def test_complete_run(self):
        self.assertTrue(FLOW["complete"](self.output()))

    def test_empty_or_partial_run(self):
        for names in [[], FLOW["TESTS"][1:]]:
            with self.subTest(names=names):
                self.assertFalse(FLOW["complete"](self.output(names)))

    def test_renamed_test_with_same_count(self):
        names = ["unrelated::test", *FLOW["TESTS"][1:]]
        self.assertFalse(FLOW["complete"](self.output(names)))

    def test_ignored_or_failed_test(self):
        self.assertFalse(FLOW["complete"](self.output(ignored=1)))
        self.assertFalse(FLOW["complete"](self.output().replace(" ... ok", " ... FAILED", 1)))


if __name__ == "__main__":
    unittest.main()
