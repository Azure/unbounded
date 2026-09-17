# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0

import json
import os
from pathlib import Path
import subprocess
import tempfile
import threading
import time
import unittest
from unittest.mock import patch

import e2e
import monitor_suite


class ReliabilityTests(unittest.TestCase):
    def test_suites_preserve_main_lifecycle_and_exclude_future_features(self):
        e2e.validate_suites()
        steps = e2e.SUITES["lifecycle"]
        for name in ("validate-agent-upgrade-operation", "validate-agent-upgrade-rollback", "validate-host-agent-upgrade",
                     "validate-node-reboot-operation", "validate-node-repave-upgrade", "reset-agent", "reinstall-agent"):
            self.assertIn(name, steps)
        self.assertLess(steps.index("validate-host-reboot"), steps.index("reset-agent"))
        self.assertEqual(steps.count("run-agent"), 1)
        self.assertFalse(any("repave-recovery" in name or "ignition" in name for name in e2e.COMMANDS))
        self.assertEqual(e2e.SUITES["bootstrap-recovery"], [
            "run-agent-recovery", "wait-for-node", "validate-workload",
            "validate-node-repave-upgrade", "validate-bootstrap-repair",
        ])

    def test_reboot_disconnect_requires_new_identity(self):
        values = [(0, "old"), (255, ""), (255, ""), (0, "old"), (0, "new")]
        with patch.object(e2e, "bounded_ssh", side_effect=[subprocess.CompletedProcess([], code, out, "") for code, out in values]), patch.object(e2e.time, "sleep"):
            self.assertEqual(e2e.reboot_host_and_wait(), "new")

    def test_repair_script_is_valid_shell_and_embedded_python(self):
        with patch.object(e2e, "bounded_ssh") as ssh, patch.object(e2e, "validate_workload"):
            e2e.validate_bootstrap_repair()
        import shlex
        script = shlex.split(ssh.call_args.args[0])[-1]
        subprocess.run(["bash", "-n"], input=script, text=True, check=True)
        python = script.split("python3 - <<'PY'\n", 1)[1].split("\nPY\n", 1)[0]
        compile(python, "repair-fixture", "exec")

    def test_agent_config_patch_changes_only_the_embedded_config(self):
        script = (
            "#!/bin/bash\nset -eu\n"
            "cat > \"${UNBOUNDED_AGENT_CONFIG_FILE}\" <<'AGENT_CONFIG_EOF'\n"
            + json.dumps({"Kubelet": {"ApiServer": "https://api.test", "Labels": {"keep": "yes"}}}, indent=2)
            + "\nAGENT_CONFIG_EOF\n\"${AGENT_BIN}\" start\n"
        )

        def add_label(config):
            config["Kubelet"].setdefault("Labels", {})["e2e.unbounded.test/retry"] = "changed"

        patched = e2e.patch_agent_config(script, add_label)
        self.assertNotEqual(patched, script)
        self.assertTrue(patched.startswith("#!/bin/bash\nset -eu\n"))
        self.assertTrue(patched.endswith("\nAGENT_CONFIG_EOF\n\"${AGENT_BIN}\" start\n"))

        config = json.loads(patched.split("<<'AGENT_CONFIG_EOF'\n", 1)[1].split("\nAGENT_CONFIG_EOF", 1)[0])
        self.assertEqual(config["Kubelet"]["Labels"], {"keep": "yes", "e2e.unbounded.test/retry": "changed"})
        self.assertEqual(config["Kubelet"]["ApiServer"], "https://api.test")

    def test_agent_config_patch_fails_on_malformed_script(self):
        for script in ("#!/bin/bash\ntrue\n",
                       "cat > \"${UNBOUNDED_AGENT_CONFIG_FILE}\" <<'AGENT_CONFIG_EOF'\n{}",
                       "cat > \"${UNBOUNDED_AGENT_CONFIG_FILE}\" <<'AGENT_CONFIG_EOF'\nnot-json\nAGENT_CONFIG_EOF\n"):
            with self.assertRaises(SystemExit):
                e2e.patch_agent_config(script, lambda config: None)

    def test_recovery_mode_is_scoped_to_attempt(self):
        cfg = e2e.NodeConfig(name="test", node_labels={}, register_with_taints=[])
        with patch.dict(os.environ, {}, clear=True), patch.object(e2e, "run_agent", side_effect=RuntimeError("injected")):
            with self.assertRaises(RuntimeError):
                e2e.run_agent_recovery(cfg)
            self.assertNotIn("E2E_BOOTSTRAP_RECOVERY", os.environ)

    def test_reboot_permission_failure_fails(self):
        with patch.object(e2e, "bounded_ssh", side_effect=[subprocess.CompletedProcess([], 0, "old", ""), subprocess.CompletedProcess([], 1, "", "denied")]), self.assertRaises(SystemExit):
            e2e.reboot_host_and_wait()

    def test_reboot_without_new_identity_times_out(self):
        with patch.object(e2e, "bounded_ssh", side_effect=[subprocess.CompletedProcess([], 0, "old", ""), subprocess.CompletedProcess([], 255, "", "")]), patch.object(e2e.time, "monotonic", side_effect=[0, 301]), self.assertRaises(SystemExit):
            e2e.reboot_host_and_wait()

    def test_stalled_ssh_uses_remaining_budget(self):
        with patch.object(e2e.time, "monotonic", return_value=98), patch.object(e2e.subprocess, "run", side_effect=subprocess.TimeoutExpired("ssh", 2)) as run:
            self.assertEqual(e2e.bounded_ssh("true", 100).returncode, 255)
            self.assertEqual(run.call_args.kwargs["timeout"], 2)
            with self.assertRaises(TimeoutError):
                e2e.bounded_ssh("mutate", 100, check=True)

    def test_same_disk_reinstall_checks_host_identity(self):
        for after in ("same", "different"):
            with self.subTest(after=after), patch.object(e2e, "run_agent") as run, patch.object(e2e, "bounded_ssh", side_effect=[subprocess.CompletedProcess([], 0, value, "") for value in ("same", after)]):
                cfg = e2e.NodeConfig(name="test", node_labels={}, register_with_taints=[])
                if after == "different":
                    with self.assertRaises(SystemExit):
                        e2e.reinstall_agent(cfg)
                else:
                    e2e.reinstall_agent(cfg)
                run.assert_called_once_with(cfg)

    def test_recovered_hostname_needs_done_correct_host_and_marker(self):
        warning = f"Failed to set the hostname to {e2e.VM_NAME} ({e2e.VM_NAME})"
        for outcome in ("success", "wrong-host", "missing-marker", "unknown-warning", "fatal"):
            with self.subTest(outcome=outcome):
                state = {"status": "running", "errors": [], "recoverable_errors": {"WARNING": [warning]}}
                responses = [subprocess.CompletedProcess([], 2, json.dumps(state), "")]
                state["status"] = "done"
                if outcome == "unknown-warning":
                    state["recoverable_errors"]["WARNING"].append("other failure")
                if outcome == "fatal":
                    state["errors"] = ["failed"]
                responses.append(subprocess.CompletedProcess([], 2, json.dumps(state), ""))
                if outcome not in ("unknown-warning", "fatal"):
                    responses.extend([subprocess.CompletedProcess([], 0, "wrong" if outcome == "wrong-host" else e2e.VM_NAME, ""), subprocess.CompletedProcess([], 0, e2e.VM_NAME, "")])
                    if outcome != "wrong-host":
                        responses.append(subprocess.CompletedProcess([], int(outcome == "missing-marker"), "", ""))
                if outcome != "success":
                    responses.append(subprocess.CompletedProcess([], 0, "diagnostics", ""))
                with patch.object(e2e, "HOST_BASE_OS", "fedora"), patch.object(e2e, "bounded_ssh", side_effect=responses), patch.object(e2e.time, "sleep") as sleep:
                    if outcome == "success":
                        e2e.wait_for_cloud_init()
                    else:
                        with self.assertRaises(SystemExit):
                            e2e.wait_for_cloud_init()
                sleep.assert_called_once_with(2)

    def test_rendered_el10_preparation_fails_before_marker(self):
        for failure in ("dnf", "nft_compat", "xt_conntrack", "xt_comment", ""):
            with self.subTest(failure=failure), tempfile.TemporaryDirectory() as directory:
                root = Path(directory)
                with patch.object(e2e, "HOST_BASE_OS", "almalinux10"):
                    rendered = e2e._cloud_init_user_data(e2e.host_image(), "ssh-test")
                script = "\n".join(line[4:] for line in rendered.split("runcmd:\n  - |\n", 1)[1].splitlines()).replace("/etc/agent", str(root / "agent"))
                for command in ("dnf", "modprobe", "uname"):
                    path = root / command
                    path.write_text("#!/bin/sh\n"
                                    "if [ \"$(basename \"$0\")\" = uname ]; then echo test-kernel; exit 0; fi\n"
                                    "echo \"$(basename \"$0\") $*\" >> \"$CALL_LOG\"\n"
                                    "if [ \"$FAIL\" = dnf ] && [ \"$(basename \"$0\")\" = dnf ]; then exit 23; fi\n"
                                    "if [ -n \"$FAIL\" ] && [ \"$FAIL\" = \"$1\" ]; then exit 24; fi\n")
                    path.chmod(0o755)
                result = subprocess.run(["sh", "-c", script], capture_output=True, text=True, env={**os.environ, "PATH": str(root)+":"+os.environ["PATH"], "FAIL": failure, "CALL_LOG": str(root / "calls")})
                self.assertEqual(result.returncode == 0, not bool(failure))
                self.assertEqual((root / "agent/provisioned").exists(), not bool(failure))
                self.assertIn("kernel-modules-extra-test-kernel", (root / "calls").read_text())

    def test_scenario_limit_and_failed_guest_preservation(self):
        for fail in (False, True):
            with self.subTest(fail=fail), tempfile.TemporaryDirectory() as directory:
                configs = [e2e.NodeConfig(name=str(i), node_labels={}, register_with_taints=[], block_external_network=i==5) for i in range(6)]
                lock = threading.Lock()
                active, peak = 0, 0
                started = []
                def scenario(cfg, index, url):
                    nonlocal active, peak
                    with lock:
                        active += 1
                        peak = max(peak, active)
                        started.append(index)
                    time.sleep(.02)
                    with lock:
                        active -= 1
                    if fail and index == 0:
                        raise RuntimeError("failed guest")
                with patch.dict(e2e.os.environ, {"CONFIG_SCENARIO_WORKERS":"2"}), patch.object(e2e, "VM_DIR", Path(directory)), patch.object(e2e, "patch_kind_control_plane_node_ip"), patch.object(e2e, "discover_node_configs", return_value=configs), patch.object(e2e, "mirror_oci_refs_to_local_registry", side_effect=lambda x:x), patch.object(e2e, "prepare_agent_artifacts", return_value="url"), patch.object(e2e, "HTTPServer"), patch.object(e2e, "validate_kube_proxy"), patch.object(e2e, "_validate_node_config_scenario", side_effect=scenario):
                    if fail:
                        with self.assertRaises(SystemExit):
                            e2e.validate_node_config_scenarios()
                        self.assertEqual(sorted(started), [0,1])
                    else:
                        e2e.validate_node_config_scenarios()
                        self.assertEqual(sorted(started), list(range(6)))
                        self.assertEqual(started[-1], 5)
                self.assertLessEqual(peak, 2)

    def test_scenario_retirement_captures_before_stop(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / "guest.qcow2").touch()
            calls = []
            env = {"VM_DIR":str(root), "VM_NAME":"guest", "VM_IP":"192.0.2.10", "AGENT_MACHINE_NAME":"guest"}
            with patch.object(e2e, "REPO_ROOT", root), patch.object(e2e, "_collect_one_vm_logs", side_effect=lambda *a:calls.append("collect")), patch.object(e2e, "_stop_qemu_by_pid_file", side_effect=lambda *a:calls.append("stop")), patch.object(e2e, "kubectl", side_effect=lambda *a, **k:calls.append("delete-node")):
                e2e.retire_config_scenario(e2e.NodeConfig(name="test", node_labels={}, register_with_taints=[]), env)
            self.assertEqual(calls, ["collect", "stop", "delete-node"])
            self.assertTrue((root / "guest.qcow2").exists())

    def test_monitor_preserves_daemonized_vm(self):
        with patch.object(monitor_suite.subprocess, "run", return_value=subprocess.CompletedProcess([], 0, "10 1\n11 10\n12 11\n13 1\n", "")), patch.object(monitor_suite.os, "kill") as kill, patch.object(monitor_suite.time, "sleep"), patch.object(monitor_suite.subprocess, "Popen") as popen:
            process = popen.return_value
            process.pid = 10
            monitor_suite.stop_command_tree(process)
            self.assertEqual({c.args[0] for c in kill.call_args_list}, {10,11,12})

    def test_scenario_firewall_cleanup_uses_marker(self):
        with tempfile.TemporaryDirectory() as directory, patch.object(e2e, "VM_DIR", Path(directory)), patch.object(e2e, "discover_node_configs", return_value=[object(), object()]), patch.object(e2e, "unblock_external_network") as unblock:
            (Path(directory) / "node-config-scenarios").touch()
            e2e.unblock_all_external_network_rules()
            self.assertEqual(unblock.call_count, 3)
