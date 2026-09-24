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
import unittest
from unittest.mock import patch

import e2e


def _data_url(text: str) -> str:
    return "data:;base64," + base64.b64encode(text.encode()).decode()


class TestRewriteAPIServer(unittest.TestCase):
    """The agent config travels base64 inside a data URL."""

    def test_rewrites_inside_an_encoded_file(self):
        """A plain-text substitution over the rendered document finds nothing
        here, and silently leaves the VM pointed at a loopback address it
        cannot reach."""
        config = json.dumps({"Kubelet": {"ApiServer": "https://127.0.0.1:6443"}})
        doc = {"storage": {"files": [{
            "path": "/etc/unbounded/agent/config.json",
            "contents": {"source": _data_url(config), "verification": {"hash": "sha256-old"}},
        }]}}

        e2e.rewrite_ignition_api_server(doc, "https://127.0.0.1:6443", "https://10.0.0.2:6443")

        source = doc["storage"]["files"][0]["contents"]["source"]
        rewritten = json.loads(base64.b64decode(source.partition(",")[2]))
        self.assertEqual(rewritten["Kubelet"]["ApiServer"], "https://10.0.0.2:6443")

    def test_drops_the_digest_it_invalidates(self):
        """Ignition verifies before writing, so a rewritten body with its
        original digest fails the whole config rather than the one file."""
        doc = {"storage": {"files": [{
            "path": "/etc/unbounded/agent/config.json",
            "contents": {"source": _data_url("server=old"), "verification": {"hash": "sha256-old"}},
        }]}}

        e2e.rewrite_ignition_api_server(doc, "old", "new")

        self.assertNotIn("verification", doc["storage"]["files"][0]["contents"])

    def test_leaves_untouched_files_verified(self):
        """The agent binary is fetched by URL and its digest is the only thing
        proving the host got the build under test."""
        doc = {"storage": {"files": [{
            "path": "/opt/unbounded/bin/unbounded-agent",
            "contents": {"source": "https://example.test/agent", "verification": {"hash": "sha256-abc"}},
        }]}}

        e2e.rewrite_ignition_api_server(doc, "old", "new")

        self.assertEqual(
            doc["storage"]["files"][0]["contents"]["verification"]["hash"], "sha256-abc")

    def test_rewrites_unit_contents(self):
        doc = {"systemd": {"units": [{"name": "u.service", "contents": "ExecStart=x --server old"}]}}
        e2e.rewrite_ignition_api_server(doc, "old", "new")
        self.assertIn("new", doc["systemd"]["units"][0]["contents"])

    def test_no_change_is_a_no_op(self):
        doc = {"storage": {"files": [{
            "path": "/f", "contents": {"source": _data_url("same"), "verification": {"hash": "h"}},
        }]}}

        e2e.rewrite_ignition_api_server(doc, "same", "same")

        self.assertIn("verification", doc["storage"]["files"][0]["contents"],
                      "an unchanged body keeps a digest that is still correct")


class TestHarnessAccess(unittest.TestCase):
    """What the harness adds so it can drive the host."""

    def test_a_new_user_is_specified_in_full(self):
        """A bare name leaves usermod nothing to apply.

        The image ships this account with /sbin/nologin. Adding only a name and
        a key produces a host that accepts the key and then refuses the session
        with "This account is currently not available", which looks like an SSH
        problem rather than a config one.
        """
        doc = e2e.add_ignition_harness_access({}, "ssh-ed25519 AAAA", "52:54:00:12:34:56")

        user = doc["passwd"]["users"][0]
        self.assertEqual(user["name"], e2e.VM_SSH_USER)
        self.assertEqual(user["shell"], "/bin/bash")
        self.assertIn("sudo", user["groups"])
        self.assertIn("ssh-ed25519 AAAA", user["sshAuthorizedKeys"])

    def test_an_existing_user_keeps_its_record(self):
        doc = {"passwd": {"users": [{"name": e2e.VM_SSH_USER, "shell": "/bin/zsh"}]}}

        e2e.add_ignition_harness_access(doc, "ssh-ed25519 KEY", "52:54:00:12:34:56")

        user = doc["passwd"]["users"][0]
        self.assertEqual(user["shell"], "/bin/zsh", "an explicit record must not be overwritten")
        self.assertIn("ssh-ed25519 KEY", user["sshAuthorizedKeys"])

    def test_hostname_is_set(self):
        """The image masks the metadata hostname service and has no cloud-init,
        so it stays "localhost" and the node registers under the wrong name."""
        doc = e2e.add_ignition_harness_access({}, "key", "52:54:00:12:34:56")

        names = {f["path"] for f in doc["storage"]["files"]}
        self.assertIn("/etc/hostname", names)

    def test_network_unit_matches_on_mac_only(self):
        """Every condition in [Match] has to hold. The interface name depends on
        the machine type, so naming it as well makes the unit silently not
        apply and leaves the VM on DHCP."""
        doc = e2e.add_ignition_harness_access({}, "key", "52:54:00:12:34:56")

        unit = next(f for f in doc["storage"]["files"]
                    if f["path"].endswith(e2e.IGNITION_NETWORK_UNIT))
        body = base64.b64decode(unit["contents"]["source"].partition(",")[2]).decode()

        self.assertIn("MACAddress=52:54:00:12:34:56", body)
        self.assertNotIn("Name=", body)

    def test_waagent_is_masked_once(self):
        """The image's own config masks it, but a user config displaces that
        section entirely, so the harness has to restate it."""
        doc = e2e.add_ignition_harness_access({}, "key", "52:54:00:12:34:56")
        doc = e2e.add_ignition_harness_access(doc, "key", "52:54:00:12:34:56")

        masked = [u for u in doc["systemd"]["units"] if u["name"] == "waagent.service"]
        self.assertEqual(len(masked), 1)
        self.assertTrue(masked[0]["mask"])


class TestInitramfsNetworking(unittest.TestCase):
    def test_the_interface_is_named(self):
        """With the device field empty, bootengine's parse-ip-for-networkd
        writes Name=*, which matches loopback first and assigns the address to
        lo. Every fetch then fails with connection refused without a packet
        reaching the wire.
        """
        karg = e2e.initramfs_ip_karg()

        self.assertTrue(karg.startswith("ip="))
        fields = karg[len("ip="):].split(":")
        self.assertEqual(fields[5], e2e.IGNITION_INITRAMFS_INTERFACE)
        self.assertNotEqual(fields[5], "", "an empty device field assigns the address to lo")

    def test_it_carries_address_gateway_and_dns(self):
        fields = e2e.initramfs_ip_karg()[len("ip="):].split(":")

        self.assertEqual(fields[0], e2e.VM_IP)
        self.assertEqual(fields[2], e2e.VM_GATEWAY)
        self.assertTrue(fields[7], "Ignition fetches by URL and needs a resolver")


class TestDataURLs(unittest.TestCase):
    def test_round_trip(self):
        self.assertEqual(e2e._decode_ignition_source(e2e.ignition_data_url("hello")), "hello")

    def test_percent_encoded_is_understood(self):
        """Not every producer base64s, and a config written by hand commonly
        does not."""
        self.assertEqual(e2e._decode_ignition_source("data:,line%0A"), "line\n")

    def test_a_remote_source_is_not_inline(self):
        self.assertIsNone(e2e._decode_ignition_source("https://example.test/f"))



class TestIgnitionHostBoundaries(unittest.TestCase):
    """Paths that assume a host the harness can prepare before it boots."""

    @staticmethod
    def _ignition_image():
        return e2e.HostImage(url="file:///x", file_name="x.qcow2", backing_format="qcow2",
                             sudo_group="sudo", packages=[], ssh_user="core",
                             provisioning="ignition", host_prefix="/opt/unbounded")

    def test_blocked_network_preparation_installs_nothing(self):
        """There is no package manager and /usr is read-only, so the apt/dnf
        path would fail on a host whose prerequisites are in the image by
        design. Reaching it at all means the premise of the host entry is
        wrong, so it returns before any SSH."""
        with patch.object(e2e, "host_image", return_value=self._ignition_image()), \
                patch.object(e2e, "wait_for_cloud_init") as waited, \
                patch.object(e2e, "ssh_cmd") as ssh:
            e2e.prepare_blocked_network_vm()

        waited.assert_not_called()
        ssh.assert_not_called()

    def test_offline_bootstrap_is_refused_before_anything_is_built(self):
        """The offline path delivers a bundle over SSH before the agent runs.
        An Ignition host has no such window, and finding out later costs an
        agent build and a VM boot first."""
        with patch.object(e2e, "host_image", return_value=self._ignition_image()), \
                patch.object(e2e, "OFFLINE_BOOTSTRAP", True), \
                patch.object(e2e, "prepare_agent_artifacts") as prepared:
            with self.assertRaises(SystemExit):
                e2e.run_agent(e2e.NodeConfig(name="n", node_labels={}, register_with_taints=[]))

        prepared.assert_not_called()

    def test_an_outside_agent_is_refused_before_anything_is_built(self):
        """The configuration scenarios pass their own AGENT_URL. The Ignition
        path only serves the binary it staged, so such a run would die later,
        after minting a bootstrap token."""
        with patch.object(e2e, "host_image", return_value=self._ignition_image()), \
                patch.dict(e2e.os.environ, {"AGENT_URL": "http://runner/unbounded-agent.tar.gz"}), \
                patch.object(e2e, "prepare_agent_artifacts") as prepared, \
                patch.object(e2e, "_run_agent_inner") as ran:
            with self.assertRaises(SystemExit):
                e2e.run_agent(e2e.NodeConfig(name="n", node_labels={}, register_with_taints=[]))

        prepared.assert_not_called()
        ran.assert_not_called()

    def test_the_configuration_suite_is_refused_up_front(self):
        with patch.object(e2e, "host_image", return_value=self._ignition_image()), \
                patch.object(e2e, "patch_kind_control_plane_node_ip") as patched, \
                patch.object(e2e, "discover_node_configs") as discovered:
            with self.assertRaises(SystemExit):
                e2e.validate_node_config_scenarios()

        patched.assert_not_called()
        discovered.assert_not_called()

    def test_reset_failed_is_only_optional_on_an_ignition_host(self):
        """A refused reset-failed is expected on Azure Container Linux. Elsewhere
        it means something is wrong, and scenarios would share a start-limit
        budget without anyone noticing."""
        import subprocess

        refused = subprocess.CompletedProcess(["ssh"], 1, "", "Access denied")
        for provisioning, should_die in (("ignition", False), ("cloud-init", True)):
            with self.subTest(provisioning=provisioning):
                image = self._ignition_image()
                image = e2e.replace(image, provisioning=provisioning)
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
