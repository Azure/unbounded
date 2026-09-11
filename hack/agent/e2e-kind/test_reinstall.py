#!/usr/bin/env python3
# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0
"""Reinstallation must use the existing disk and only replace agent payloads."""
import json
import hashlib
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

import e2e


class ReinstallTests(unittest.TestCase):
    def test_suites_preserve_same_disk_contract(self):
        steps = e2e.SUITES["lifecycle"]
        self.assertEqual(steps.count("run-agent"), 1)
        self.assertEqual(steps.count("reinstall-agent"), 1)
        self.assertLess(steps.index("validate-reset-cleanup"), steps.index("validate-reset-reboot"))
        self.assertLess(steps.index("validate-reset-reboot"), steps.index("reinstall-agent"))
        self.assertIn("configure-kind-kube-proxy", e2e.SUITES["setup"])
        e2e.validate_suites()

    def test_reinstall_does_not_boot_or_destroy_vm(self):
        config = e2e.NodeConfig(name="default", node_labels={}, register_with_taints=[])
        with patch.object(e2e, "host_boot_id", side_effect=["boot-a", "boot-a"]), \
                patch.object(e2e, "run_agent") as run, \
                patch.object(e2e, "destroy_vm") as destroy:
            e2e.reinstall_agent(config)
        run.assert_called_once_with(config, reinstall=True)
        destroy.assert_not_called()

    def test_reinstall_rejects_changed_boot_identity(self):
        with patch.object(e2e, "host_boot_id", side_effect=["boot-a", "boot-b"]), \
                patch.object(e2e, "run_agent"), self.assertRaises(SystemExit):
            e2e.reinstall_agent(e2e.NodeConfig(name="default", node_labels={}, register_with_taints=[]))

    def test_delivers_only_agent_payloads(self):
        prefix = "/opt/unbounded"
        cfg = json.dumps({"HostPrefix": prefix})
        doc = {"storage": {"files": [
            {"path": prefix + "/bin/unbounded-agent", "mode": 0o755,
             "contents": {"source": "http://runner/unbounded-agent", "verification": {
                 "hash": "sha256-" + hashlib.sha256(b"test-binary").hexdigest()}}},
            {"path": "/etc/unbounded/agent/config.json", "mode": 0o600,
             "contents": {"source": e2e.ignition_data_url(cfg)}},
            {"path": "/etc/hostname", "contents": {"source": "ignored"}},
        ]}, "systemd": {"units": [
            {"name": "unbounded-agent-bootstrap.service", "contents": "[Service]\n"},
            {"name": "waagent.service", "mask": True},
        ]}}
        with tempfile.TemporaryDirectory() as tmp, \
                patch.object(e2e, "VM_DIR", Path(tmp)), \
                patch.object(e2e, "host_image") as image, \
                patch.object(e2e, "scp_cmd") as scp, \
                patch.object(e2e, "ssh_cmd") as ssh:
            image.return_value.host_prefix = prefix
            (Path(tmp) / "unbounded-agent").write_bytes(b"test-binary")
            e2e._reinstall_ignition_payload(doc)
            self.assertEqual(scp.call_count, 3)
            self.assertEqual((Path(tmp) / "reinstall-1").read_text(), cfg)
            commands = "\n".join(str(call) for call in ssh.call_args_list)
            self.assertNotIn("/etc/hostname", commands)
            self.assertNotIn("waagent", commands)
            self.assertIn("enable --now --no-block", commands)


if __name__ == "__main__":
    unittest.main()
