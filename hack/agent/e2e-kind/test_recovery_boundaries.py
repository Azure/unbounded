# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0
"""Regression checks for reboot verification and scenario cleanup."""
from pathlib import Path
from subprocess import CompletedProcess, TimeoutExpired
import tempfile
import unittest
from unittest.mock import patch

import e2e


class RecoveryBoundaryTests(unittest.TestCase):
    def test_reboot_disconnect_requires_new_boot(self):
        results = [CompletedProcess([], 0, "old\n", ""), CompletedProcess([], 255, "", "disconnected"),
                   CompletedProcess([], 255, "", "offline"),
                   CompletedProcess([], 0, "old\n", ""),
                   CompletedProcess([], 0, "new\n", "")]
        with patch.object(e2e, "host_boot_id", return_value="old"), \
                patch.object(e2e.subprocess, "run", side_effect=results), \
                patch.object(e2e.time, "sleep"):
            self.assertEqual(e2e.reboot_host_and_wait(), "new")

    def test_disconnect_without_reboot_fails(self):
        with patch.object(e2e, "bounded_ssh", side_effect=[CompletedProcess([], 0, "old", ""), CompletedProcess([], 255, "", "")]), \
                patch.object(e2e.time, "monotonic", side_effect=[0, 301]), \
                self.assertRaises(SystemExit):
            e2e.reboot_host_and_wait()

    def test_reboot_permission_failure_is_not_accepted(self):
        with patch.object(e2e, "bounded_ssh", side_effect=[CompletedProcess([], 0, "old", ""), CompletedProcess([], 1, "", "denied")]), \
                self.assertRaises(SystemExit):
            e2e.reboot_host_and_wait()

    def test_stalled_session_uses_remaining_budget(self):
        with patch.object(e2e.time, "monotonic", return_value=98), \
                patch.object(e2e.subprocess, "run", side_effect=TimeoutExpired("ssh", 2)) as run:
            result = e2e.bounded_ssh("true", 100)
            self.assertEqual(result.returncode, 255)
            self.assertEqual(run.call_args.kwargs["timeout"], 2)
            with self.assertRaises(TimeoutError):
                e2e.bounded_ssh("mutate", 100, check=True)

    def test_injection_waits_for_ssh(self):
        with patch.object(e2e, "bounded_ssh", side_effect=[CompletedProcess([], 255), CompletedProcess([], 0)]) as ssh, \
                patch.object(e2e.time, "sleep"), patch.object(e2e.time, "monotonic", return_value=0):
            e2e.wait_for_injection_ssh(100)
            self.assertEqual(ssh.call_count, 2)

    def test_ignition_offline_env_rejected_before_bootstrap(self):
        with patch.object(e2e, "OFFLINE_BOOTSTRAP", True), patch.object(e2e, "host_image") as image, \
                patch.object(e2e, "prepare_agent_artifacts") as prepare, self.assertRaises(SystemExit):
            image.return_value.provisioning = "ignition"
            e2e.run_agent(e2e.NodeConfig(name="test", node_labels={}, register_with_taints=[]))
        prepare.assert_not_called()

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
