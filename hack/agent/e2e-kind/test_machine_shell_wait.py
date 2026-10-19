# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0

import subprocess
import unittest
from unittest.mock import patch

import e2e


def _result(code, out="", err=""):
    return subprocess.CompletedProcess([], code, out, err)


class WaitForMachineShellSuccessTests(unittest.TestCase):
    def test_retries_until_command_succeeds(self):
        results = [_result(1, err="Failed to get shell PTY"), _result(1), _result(0)]
        with patch.object(e2e, "machine_shell_quiet", side_effect=results) as shell, \
                patch.object(e2e.time, "sleep") as sleep:
            e2e.wait_for_machine_shell_success("kube1", "true", "ready", timeout_secs=60)
        self.assertEqual(shell.call_count, 3)
        self.assertEqual(sleep.call_count, 2)
        shell.assert_called_with("kube1", "true")

    def test_succeeds_on_first_attempt_without_sleeping(self):
        with patch.object(e2e, "machine_shell_quiet", return_value=_result(0)), \
                patch.object(e2e.time, "sleep") as sleep:
            e2e.wait_for_machine_shell_success("kube1", "true", "ready")
        sleep.assert_not_called()

    def test_times_out_with_last_error(self):
        clock = iter([0.0, 5.0, 11.0])
        with patch.object(e2e, "machine_shell_quiet", return_value=_result(1, err="boom")), \
                patch.object(e2e.time, "sleep"), \
                patch.object(e2e.time, "monotonic", side_effect=lambda: next(clock)), \
                patch.object(e2e, "die", side_effect=SystemExit) as die:
            with self.assertRaises(SystemExit):
                e2e.wait_for_machine_shell_success("kube1", "true", "ready", timeout_secs=10)
        message = die.call_args.args[0]
        self.assertIn("2 attempt(s)", message)
        self.assertIn("boom", message)
        self.assertIn("kube1", message)


if __name__ == "__main__":
    unittest.main()
