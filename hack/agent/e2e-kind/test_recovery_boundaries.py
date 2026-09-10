# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0
"""Regression checks for reboot verification and scenario cleanup."""
from pathlib import Path
from subprocess import CompletedProcess
import tempfile
import unittest
from unittest.mock import patch

import e2e


class RecoveryBoundaryTests(unittest.TestCase):
    def test_reboot_disconnect_requires_new_boot(self):
        results = [CompletedProcess([], 255, "", "disconnected"),
                   CompletedProcess([], 255, "", "offline"),
                   CompletedProcess([], 0, "old\n", ""),
                   CompletedProcess([], 0, "new\n", "")]
        with patch.object(e2e, "host_boot_id", return_value="old"), \
                patch.object(e2e.subprocess, "run", side_effect=results), \
                patch.object(e2e.time, "sleep"):
            self.assertEqual(e2e.reboot_host_and_wait(), "new")

    def test_disconnect_without_reboot_fails(self):
        with patch.object(e2e, "host_boot_id", return_value="old"), \
                patch.object(e2e.subprocess, "run", return_value=CompletedProcess([], 255, "", "")), \
                patch.object(e2e.time, "monotonic", side_effect=[0, 301]), \
                self.assertRaises(SystemExit):
            e2e.reboot_host_and_wait()

    def test_reboot_permission_failure_is_not_accepted(self):
        with patch.object(e2e, "host_boot_id", return_value="old"), \
                patch.object(e2e.subprocess, "run", return_value=CompletedProcess([], 1, "", "denied")), \
                self.assertRaises(SystemExit):
            e2e.reboot_host_and_wait()

    def test_default_local_name_cleans_scenario_firewalls(self):
        with tempfile.TemporaryDirectory() as directory, \
                patch.object(e2e, "VM_DIR", Path(directory)), \
                patch.object(e2e, "VM_NAME", "agent-e2e"), \
                patch.object(e2e, "discover_node_configs", return_value=[object(), object()]), \
                patch.object(e2e, "unblock_external_network") as unblock:
            (Path(directory) / "node-config-scenarios").touch()
            e2e.unblock_all_external_network_rules()
            self.assertEqual(unblock.call_count, 3)
            unblock.assert_any_call(f"{e2e.VM_SUBNET}.11")


if __name__ == "__main__":
    unittest.main()
