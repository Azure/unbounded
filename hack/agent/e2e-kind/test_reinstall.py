#!/usr/bin/env python3
# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0

"""Reinstalling on an Ignition host must reuse the disk it already has.

Reinstall exists to prove a reset host can be provisioned again from what is
already on it. Replacing the disk would answer a different and easier question,
and would quietly turn the same-disk assertion in reinstall_agent into a
fresh-install one, since a new disk boots with a new boot id.
"""
import hashlib
import json
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

import e2e


class TestReinstallUsesTheSameDisk(unittest.TestCase):
    def test_reinstall_asks_for_the_same_disk(self):
        """The flag is the whole mechanism.

        Without it the Ignition path destroys the disk and boots a fresh VM,
        which is exactly what the caller then fails on.
        """
        config = e2e.NodeConfig(name="default", node_labels={}, register_with_taints=[])

        with patch.object(e2e, "run_agent") as run, \
                patch.object(e2e, "destroy_vm") as destroy, \
                patch.object(e2e, "bounded_ssh") as ssh:
            ssh.return_value.stdout = "boot-a"
            ssh.return_value.returncode = 0
            e2e.reinstall_agent(config)

        run.assert_called_once_with(config, reinstall=True)
        destroy.assert_not_called()


class TestReinstallPayload(unittest.TestCase):
    """What a reinstall is allowed to touch."""

    @staticmethod
    def _doc(prefix: str, agent_config: str) -> dict:
        return {
            "storage": {"files": [
                {"path": prefix + "/bin/unbounded-agent", "mode": 0o755,
                 "contents": {"source": "http://runner/unbounded-agent", "verification": {
                     "hash": "sha256-" + hashlib.sha256(b"test-binary").hexdigest()}}},
                {"path": "/etc/unbounded/agent/config.json", "mode": 0o600,
                 "contents": {"source": e2e.ignition_data_url(agent_config)}},
                {"path": "/etc/hostname", "contents": {"source": "ignored"}},
            ]},
            "systemd": {"units": [
                {"name": "unbounded-agent-bootstrap.service", "contents": "[Service]\n"},
                {"name": "waagent.service", "mask": True},
            ]},
        }

    def test_delivers_only_the_agent_payloads(self):
        """Identity, networking and boot state have to survive a reset.

        Rewriting /etc/hostname or remasking waagent would recreate host state
        that reset is supposed to have left alone, hiding exactly the cleanup
        defects this step exists to find.
        """
        prefix = "/opt/unbounded"
        agent_config = json.dumps({"HostPrefix": prefix})

        with tempfile.TemporaryDirectory() as tmp, \
                patch.object(e2e, "VM_DIR", Path(tmp)), \
                patch.object(e2e, "host_image") as image, \
                patch.object(e2e, "scp_cmd") as scp, \
                patch.object(e2e, "ssh_cmd") as ssh:
            image.return_value.host_prefix = prefix
            (Path(tmp) / "unbounded-agent").write_bytes(b"test-binary")

            e2e._reinstall_ignition_payload(self._doc(prefix, agent_config))

            self.assertEqual(scp.call_count, 3, "binary, agent config, bootstrap unit")

            commands = "\n".join(str(call) for call in ssh.call_args_list)
            self.assertNotIn("/etc/hostname", commands)
            self.assertNotIn("waagent", commands)
            self.assertIn("enable --now --no-block", commands)

    def test_the_binary_is_checked_against_the_rendered_digest(self):
        """The binary is fetched by URL in the Ignition path, so its digest is
        the only thing tying what gets installed to the build under test. A
        reinstall that delivers a different binary would pass every later
        assertion while testing the wrong artifact."""
        prefix = "/opt/unbounded"
        doc = self._doc(prefix, json.dumps({"HostPrefix": prefix}))

        with tempfile.TemporaryDirectory() as tmp, \
                patch.object(e2e, "VM_DIR", Path(tmp)), \
                patch.object(e2e, "host_image") as image, \
                patch.object(e2e, "scp_cmd"), patch.object(e2e, "ssh_cmd"):
            image.return_value.host_prefix = prefix
            (Path(tmp) / "unbounded-agent").write_bytes(b"a different binary")

            with self.assertRaises(SystemExit):
                e2e._reinstall_ignition_payload(doc)

    def test_an_unexpected_payload_is_refused(self):
        """The payload set is asserted rather than filtered. A file appearing
        here that the test does not know about is a change in what bootstrap
        installs, and it should stop the run rather than be skipped silently."""
        prefix = "/opt/unbounded"
        doc = self._doc(prefix, json.dumps({"HostPrefix": prefix}))
        doc["storage"]["files"][0]["path"] = "/somewhere/else/unbounded-agent"

        with tempfile.TemporaryDirectory() as tmp, \
                patch.object(e2e, "VM_DIR", Path(tmp)), \
                patch.object(e2e, "host_image") as image, \
                patch.object(e2e, "scp_cmd"), patch.object(e2e, "ssh_cmd"):
            image.return_value.host_prefix = prefix
            (Path(tmp) / "unbounded-agent").write_bytes(b"test-binary")

            with self.assertRaises(SystemExit):
                e2e._reinstall_ignition_payload(doc)


if __name__ == "__main__":
    unittest.main()


class TestBootstrapChoosesThePath(unittest.TestCase):
    """The branch that connects the flag to the delivery.

    The flag and the payload delivery are each covered above, but neither says
    the two are wired together. Patching only the expensive parts leaves that
    decision exercised.
    """

    def _run(self, *, reinstall: bool):
        config = e2e.NodeConfig(name="default", node_labels={}, register_with_taints=[])
        doc = json.dumps({"storage": {"files": []}, "systemd": {"units": []}})

        # host_image is patched because these run inside every matrix job with
        # that job's HOST_BASE_OS set. Under acl the real one resolves a
        # published manifest, which would consume the patched capture below and
        # fail on a machine that has nothing to do with this branch.
        image = e2e.HostImage(url="file:///x", file_name="x.qcow2", backing_format="qcow2",
                              sudo_group="sudo", packages=[], ssh_user="core",
                              provisioning="ignition", host_prefix="/opt/unbounded")

        with patch.object(e2e, "host_image", return_value=image), \
                patch.object(e2e, "_ensure_vm_ssh_key", return_value="ssh-ed25519 AAAA"), \
                patch.object(e2e, "agent_binary_url_and_digest", return_value=("http://x/a", "d" * 64)), \
                patch.object(e2e, "node_config_bootstrap_args", return_value=[]), \
                patch.object(e2e, "log_active_node_config"), \
                patch.object(e2e, "capture", return_value=doc), \
                patch.object(e2e, "qemu_mac_address", return_value="52:54:00:12:34:56"), \
                patch.object(e2e, "add_ignition_harness_access", side_effect=lambda d, *a: d), \
                patch.object(e2e, "_wait_for_ignition_bootstrap") as wait, \
                patch.object(e2e, "destroy_vm") as destroy, \
                patch.object(e2e, "launch_ignition_vm") as launch, \
                patch.object(e2e, "_reinstall_ignition_payload") as payload:
            e2e._bootstrap_via_ignition(config, "https://api:6443", "https://127.0.0.1:6443",
                                        reinstall=reinstall)

        return destroy, launch, payload, wait

    def test_fresh_provisioning_replaces_the_disk(self):
        destroy, launch, payload, wait = self._run(reinstall=False)

        destroy.assert_called_once()
        launch.assert_called_once()
        payload.assert_not_called()
        wait.assert_called_once()

    def test_reinstall_keeps_the_disk(self):
        """Destroying it here would change the boot id that reinstall_agent
        checks, so the same-disk assertion would pass against a fresh host."""
        destroy, launch, payload, wait = self._run(reinstall=True)

        payload.assert_called_once()
        destroy.assert_not_called()
        launch.assert_not_called()
        wait.assert_called_once()
