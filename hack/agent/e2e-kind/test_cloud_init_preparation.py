# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0

import json
import os
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest.mock import patch

import e2e


class CloudInitPreparationTests(unittest.TestCase):
    def test_rendered_preparation_fails_before_marker(self):
        for failure in ("dnf", "nft_compat", "xt_conntrack", "xt_comment", ""):
            with self.subTest(failure=failure), tempfile.TemporaryDirectory() as directory:
                root = Path(directory)
                with patch.object(e2e, "HOST_BASE_OS", "almalinux10"):
                    rendered = e2e._cloud_init_user_data(e2e.host_image(), "ssh-test")
                script = "\n".join(line[4:] for line in rendered.split("runcmd:\n  - |\n", 1)[1].splitlines())
                script = script.replace("/etc/agent", str(root / "agent"))
                for command in ("dnf", "modprobe", "uname"):
                    path = root / command
                    path.write_text("#!/bin/sh\n"
                                    "if [ \"$(basename \"$0\")\" = uname ]; then echo test-kernel; exit 0; fi\n"
                                    "echo \"$(basename \"$0\") $*\" >> \"$CALL_LOG\"\n"
                                    "if [ \"$FAIL\" = dnf ] && [ \"$(basename \"$0\")\" = dnf ]; then exit 23; fi\n"
                                    "if [ -n \"$FAIL\" ] && [ \"$FAIL\" = \"$1\" ]; then exit 24; fi\n")
                    path.chmod(0o755)
                result = subprocess.run(["sh", "-c", script], capture_output=True, text=True,
                                        env={**os.environ, "PATH": str(root) + ":" + os.environ["PATH"],
                                             "FAIL": failure, "CALL_LOG": str(root / "calls")})
                self.assertEqual((root / "agent/provisioned").exists(), not bool(failure))
                self.assertEqual(result.returncode == 0, not bool(failure))
                self.assertIn("kernel-modules-extra-test-kernel", (root / "calls").read_text())

    def test_cloud_init_failure_prevents_bootstrap(self):
        for code in (1, 2):
            with self.subTest(code=code), patch.object(e2e, "bounded_ssh", side_effect=[
                subprocess.CompletedProcess([], code, json.dumps({"status": "error"}), ""),
                subprocess.CompletedProcess([], 0, "module installation failed", ""),
            ]) as ssh, self.assertRaises(SystemExit):
                e2e.wait_for_cloud_init()
            self.assertEqual(ssh.call_count, 2)

    def test_cloud_init_success_requires_marker(self):
        with patch.object(e2e, "bounded_ssh", side_effect=[
            subprocess.CompletedProcess([], 0, json.dumps({"status": "done"}), ""),
            subprocess.CompletedProcess([], 0, "", ""),
        ]) as ssh:
            e2e.wait_for_cloud_init()
        self.assertIn("/etc/agent/provisioned", ssh.call_args.args[0])

    def test_cloud_init_timeout_is_fatal(self):
        with patch.object(e2e.time, "monotonic", side_effect=[0, 601, 601]), \
                patch.object(e2e, "bounded_ssh", return_value=subprocess.CompletedProcess([], 0, "still running", "")), \
                self.assertRaises(SystemExit):
            e2e.wait_for_cloud_init()

    def test_cloud_init_done_without_marker_is_fatal(self):
        with patch.object(e2e, "bounded_ssh", side_effect=[
            subprocess.CompletedProcess([], 0, json.dumps({"status": "done"}), ""),
            subprocess.CompletedProcess([], 1, "", ""),
            subprocess.CompletedProcess([], 0, "marker absent", ""),
        ]), self.assertRaises(SystemExit):
            e2e.wait_for_cloud_init()

    def test_recovered_fedora_hostname_requires_completion_and_postconditions(self):
        warning = f"Failed to set the hostname to {e2e.VM_NAME} ({e2e.VM_NAME})"
        for postcondition in ("valid", "wrong-hostname", "missing-marker", "unknown-warning", "fatal-error"):
            with self.subTest(postcondition=postcondition):
                running = {"status": "running", "errors": [], "recoverable_errors": {"WARNING": [warning]}}
                done = {**running, "status": "done"}
                if postcondition == "unknown-warning":
                    done["recoverable_errors"] = {"WARNING": [warning, "package installation failed"]}
                if postcondition == "fatal-error":
                    done["errors"] = ["package installation failed"]
                responses = [subprocess.CompletedProcess([], 2, json.dumps(running), ""),
                             subprocess.CompletedProcess([], 2, json.dumps(done), "")]
                if postcondition not in ("unknown-warning", "fatal-error"):
                    hostname = "wrong" if postcondition == "wrong-hostname" else e2e.VM_NAME
                    responses.extend([subprocess.CompletedProcess([], 0, hostname, ""),
                                      subprocess.CompletedProcess([], 0, e2e.VM_NAME, "")])
                    if postcondition != "wrong-hostname":
                        responses.append(subprocess.CompletedProcess([], int(postcondition == "missing-marker"), "", ""))
                if postcondition != "valid":
                    responses.append(subprocess.CompletedProcess([], 0, "diagnostics", ""))
                with patch.object(e2e, "HOST_BASE_OS", "fedora"), patch.object(e2e.time, "sleep") as sleep, \
                        patch.object(e2e, "bounded_ssh", side_effect=responses):
                    if postcondition == "valid":
                        e2e.wait_for_cloud_init()
                    else:
                        with self.assertRaises(SystemExit):
                            e2e.wait_for_cloud_init()
                sleep.assert_called_once_with(2)

    def test_hostname_warning_exception_is_fedora_only(self):
        warning = f"Failed to set the hostname to {e2e.VM_NAME} ({e2e.VM_NAME})"
        with patch.object(e2e, "HOST_BASE_OS", "ubuntu2404"):
            self.assertFalse(e2e.recovered_hostname_warning({"recoverable_errors": {"WARNING": [warning]}}))
