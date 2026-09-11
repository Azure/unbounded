# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0

from pathlib import Path
import subprocess
import tempfile
import threading
import time
import unittest
from unittest.mock import patch

import e2e
import monitor_suite


class ScenarioResourceTests(unittest.TestCase):
    def test_scenario_batches_are_bounded_and_stop_after_failure(self):
        for fail in (False, True):
            with self.subTest(fail=fail), tempfile.TemporaryDirectory() as directory:
                configs = [e2e.NodeConfig(name=str(i), node_labels={}, register_with_taints=[],
                                         block_external_network=i == 5) for i in range(6)]
                lock = threading.Lock()
                active, peak = 0, 0
                started = []

                def scenario(cfg, index, url):
                    nonlocal active, peak
                    with lock:
                        started.append(index)
                        active += 1
                        peak = max(peak, active)
                    time.sleep(0.02)
                    with lock:
                        active -= 1
                    if fail and index == 0:
                        raise RuntimeError("preserve failed guest")

                with patch.dict(e2e.os.environ, {"CONFIG_SCENARIO_WORKERS": "2"}), \
                        patch.object(e2e, "VM_DIR", Path(directory)), \
                        patch.object(e2e, "patch_kind_control_plane_node_ip"), \
                        patch.object(e2e, "discover_node_configs", return_value=configs), \
                        patch.object(e2e, "mirror_oci_refs_to_local_registry", side_effect=lambda cfgs: cfgs), \
                        patch.object(e2e, "prepare_agent_artifacts", return_value="http://test"), \
                        patch.object(e2e, "HTTPServer"), \
                        patch.object(e2e, "validate_kube_proxy"), \
                        patch.object(e2e, "_validate_node_config_scenario", side_effect=scenario):
                    if fail:
                        with self.assertRaises(SystemExit):
                            e2e.validate_node_config_scenarios()
                        self.assertEqual(sorted(started), [0, 1])
                    else:
                        e2e.validate_node_config_scenarios()
                        self.assertEqual(sorted(started), list(range(6)))
                        self.assertEqual(started[-1], 5)
                self.assertLessEqual(peak, 2)

    def test_retirement_collects_before_stop_and_retains_disk(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            disk = root / "guest.qcow2"
            disk.touch()
            cfg = e2e.NodeConfig(name="example", node_labels={}, register_with_taints=[])
            calls = []
            env = {"VM_DIR": str(root), "VM_NAME": "guest", "VM_IP": "192.0.2.10", "AGENT_MACHINE_NAME": "guest"}
            with patch.object(e2e, "REPO_ROOT", root), \
                    patch.object(e2e, "_collect_one_vm_logs", side_effect=lambda *args: calls.append("collect")), \
                    patch.object(e2e, "_stop_qemu_by_pid_file", side_effect=lambda *args: calls.append("stop")), \
                    patch.object(e2e, "kubectl", side_effect=lambda *args, **kw: calls.append("delete-node")):
                e2e.retire_config_scenario(cfg, env)
            self.assertEqual(calls, ["collect", "stop", "delete-node"])
            self.assertTrue(disk.exists())
            self.assertTrue((root / "retired").exists())

    def test_scenario_timeout_terminates_command_group(self):
        cfg = e2e.NodeConfig(name="example", node_labels={}, register_with_taints=[])
        with patch.object(e2e.subprocess, "Popen") as popen, patch.object(e2e.os, "killpg") as kill:
            popen.return_value.pid = 123
            popen.return_value.wait.side_effect = [subprocess.TimeoutExpired("ssh", 1500), 0]
            with self.assertRaises(TimeoutError):
                e2e._run_scenario_command("run-agent", cfg, {})
            kill.assert_called_once_with(123, 15)
            self.assertTrue(popen.call_args.kwargs["start_new_session"])

    def test_monitor_stops_separate_session_children_but_not_daemonized_vm(self):
        with patch.object(monitor_suite.subprocess, "run", return_value=subprocess.CompletedProcess([], 0,
                "10 1\n11 10\n12 11\n13 1\n", "")), \
                patch.object(monitor_suite.os, "kill") as kill, patch.object(monitor_suite.time, "sleep"), \
                patch.object(monitor_suite.subprocess, "Popen") as popen:
            process = popen.return_value
            process.pid = 10
            monitor_suite.stop_command_tree(process)
            self.assertEqual({call.args[0] for call in kill.call_args_list}, {10, 11, 12})
            process.wait.assert_called_once_with(timeout=10)
