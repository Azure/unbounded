#!/usr/bin/env python3
# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0

"""Tests for the Ignition config the harness hands an immutable host.

An Ignition config is applied once, before anything is reachable. A mistake in
it does not produce an error at the point it is made: the guest boots, does the
wrong thing quietly, and the harness fails later against a host that is
configured differently from the one the test meant to describe. These cover the
manipulations where that has actually happened.
"""
import base64
import json
import subprocess
import unittest
from unittest.mock import patch

import e2e


class TestPatchAgentConfig(unittest.TestCase):
    def test_rewrites_inside_the_encoded_config_only(self):
        """The agent config travels base64 inside a data URL, so a text
        substitution over the rendered document finds nothing and leaves the VM
        pointed at a loopback address. The binary's digest is the only thing
        proving the host got the build under test, so it is left alone."""
        config = json.dumps({"Kubelet": {"ApiServer": "https://127.0.0.1:6443"}})
        binary = {"path": e2e.DAEMON_BINARY,
                  "contents": {"source": "https://example.test/agent", "verification": {"hash": "sha256-abc"}}}
        doc = {"storage": {"files": [
            {"path": "/etc/unbounded/agent/config.json", "contents": {"source": e2e.ignition_data_url(config)}},
            json.loads(json.dumps(binary)),
        ]}}

        e2e.patch_ignition_agent_config(doc, lambda cfg: cfg["Kubelet"].update(ApiServer="https://10.0.0.2:6443"))

        source = doc["storage"]["files"][0]["contents"]["source"]
        rewritten = json.loads(base64.b64decode(source.partition(",")[2]))
        self.assertEqual(rewritten["Kubelet"]["ApiServer"], "https://10.0.0.2:6443")
        self.assertEqual(doc["storage"]["files"][1], binary)


class TestHarnessAccess(unittest.TestCase):
    """What the harness adds so it can drive the host."""

    def test_access_is_added(self):
        doc = e2e.add_ignition_harness_access({}, "ssh-ed25519 AAAA", "52:54:00:12:34:56")

        # A bare name leaves usermod nothing to apply. The image ships this
        # account with /sbin/nologin, so the host accepts the key and then
        # refuses the session, which looks like an SSH problem.
        user = doc["passwd"]["users"][0]
        self.assertEqual(user["name"], e2e.VM_SSH_USER)
        self.assertEqual(user["shell"], "/bin/bash")
        self.assertIn("sudo", user["groups"])
        self.assertIn("ssh-ed25519 AAAA", user["sshAuthorizedKeys"])

        # The image's own config masks it, but a user config displaces that.
        self.assertIn({"name": "waagent.service", "enabled": False, "mask": True}, doc["systemd"]["units"])

        # The image masks the metadata hostname service and has no cloud-init,
        # so it stays "localhost" and the node registers under the wrong name.
        files = {f["path"]: f for f in doc["storage"]["files"]}
        self.assertIn("/etc/hostname", files)

        # Every condition in [Match] has to hold, and the interface name depends
        # on the machine type, so naming it makes the unit silently not apply.
        unit = files[f"/etc/systemd/network/{e2e.IGNITION_NETWORK_UNIT}"]
        body = base64.b64decode(unit["contents"]["source"].partition(",")[2]).decode()
        self.assertIn("MACAddress=52:54:00:12:34:56", body)
        self.assertNotIn("Name=", body)

    def test_the_initramfs_interface_is_named(self):
        """With the device field empty, the address goes to lo; see
        initramfs_ip_karg."""
        fields = e2e.initramfs_ip_karg().removeprefix("ip=").split(":")
        self.assertEqual(fields[5], e2e.IGNITION_INITRAMFS_INTERFACE)


class TestIgnitionHostBoundaries(unittest.TestCase):
    """Paths that assume a host the harness can prepare before it boots."""

    @staticmethod
    def _ignition_image():
        return e2e.HostImage(url="file:///x", file_name="x.qcow2", backing_format="qcow2",
                             sudo_group="sudo", packages=[], ssh_user="core",
                             provisioning="ignition")

    def test_outside_agents_are_refused_before_anything_is_built(self):
        """The configuration scenarios pass their own AGENT_URL, and the
        Ignition path only serves the binary it staged."""
        with patch.object(e2e, "host_image", return_value=self._ignition_image()), \
                patch.dict(e2e.os.environ, {"AGENT_URL": "http://runner/unbounded-agent.tar.gz"}), \
                patch.object(e2e, "prepare_agent_artifacts") as prepared, \
                patch.object(e2e, "_run_agent_inner") as ran, \
                patch.object(e2e, "patch_kind_control_plane_node_ip") as patched, \
                patch.object(e2e, "discover_node_configs") as discovered:
            for name, call in (("run-agent", lambda: e2e.run_agent(
                    e2e.NodeConfig(name="n", node_labels={}, register_with_taints=[]))),
                               ("configuration suite", e2e.validate_node_config_scenarios)):
                with self.subTest(name), self.assertRaises(SystemExit):
                    call()

        for mock in (prepared, ran, patched, discovered):
            mock.assert_not_called()

    def test_reset_failed_is_only_optional_on_an_ignition_host(self):
        """A refused reset-failed is expected on Azure Container Linux. Elsewhere
        it means something is wrong, and scenarios would share a start-limit
        budget without anyone noticing."""
        refused = subprocess.CompletedProcess(["ssh"], 1, "", "Access denied")
        for provisioning, should_die in (("ignition", False), ("cloud-init", True)):
            with self.subTest(provisioning=provisioning):
                image = e2e.replace(self._ignition_image(), provisioning=provisioning)
                with patch.object(e2e, "host_image", return_value=image), \
                        patch.object(e2e, "ssh_capture_quiet", return_value=refused), \
                        patch.object(e2e, "die", side_effect=SystemExit) as died:
                    try:
                        e2e.check_reset_failed()
                    except SystemExit:
                        pass
                self.assertEqual(died.called, should_die)


class TestIgnitionReboot(unittest.TestCase):
    """What counts as the first-boot unit repairing a healthy host on reboot."""

    def test_a_verify_only_run_passes(self):
        self.assertEqual(e2e.ignition_reboot_problems("active", "0", "verified\n", "1 2", "1 2"), [])

    def test_each_sign_of_a_repair_is_reported(self):
        cases = {
            "failed unit": (("failed", "0", "", "1 2", "1 2"), "ActiveState=failed"),
            "retried": (("active", "1", "", "1 2", "1 2"), "NRestarts=1"),
            "unknown restarts": (("active", "", "", "1 2", "1 2"), "NRestarts=unknown"),
            "daemon repaired": (("active", "0", 'msg="daemon unit started"', "1 2", "1 2"),
                                "start repaired the daemon"),
            "record rewritten": (("active", "0", "", "1 2", "3 4"), "the install record was rewritten"),
        }
        for name, (args, want) in cases.items():
            with self.subTest(name):
                self.assertEqual(e2e.ignition_reboot_problems(*args), [want])


if __name__ == "__main__":
    unittest.main()
