#!/usr/bin/env python3
# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0

"""Reinstalling on an Ignition host must reuse the disk it already has.

Reinstall exists to prove a reset host can be provisioned again from what is
already on it. Replacing the disk would answer a different and easier question,
and would quietly turn the same-disk assertion in reinstall_agent into a
fresh-install one, since a new disk boots with a new boot id.
"""
import json
import subprocess
import tempfile
import unittest
from contextlib import contextmanager
from pathlib import Path
from unittest.mock import patch

import e2e


class TestReinstallPayload(unittest.TestCase):
    """What a reinstall is allowed to touch."""

    @staticmethod
    def _doc() -> dict:
        return {
            "storage": {"files": [
                {"path": e2e.DAEMON_BINARY, "mode": 0o755,
                 "contents": {"source": "http://runner/unbounded-agent", "verification": {"hash": "sha256-x"}}},
                {"path": "/etc/unbounded/agent/config.json", "mode": 0o600,
                 "contents": {"source": e2e.ignition_data_url(json.dumps({"MachineName": "agent-e2e"}))}},
                {"path": "/etc/hostname", "contents": {"source": "ignored"}},
            ]},
            "systemd": {"units": [
                {"name": "unbounded-agent-bootstrap.service", "contents": "[Service]\n"},
                {"name": "waagent.service", "mask": True},
            ]},
        }

    @contextmanager
    def _host(self):
        with tempfile.TemporaryDirectory() as tmp, \
                patch.object(e2e, "VM_DIR", Path(tmp)), \
                patch.object(e2e, "scp_cmd") as scp, \
                patch.object(e2e, "ssh_cmd") as ssh, \
                patch.object(e2e, "bootstrap_unit_state", return_value={"InvocationID": "inv-before"}):
            (Path(tmp) / "unbounded-agent").write_bytes(b"test-binary")
            yield scp, ssh

    def test_delivers_only_the_agent_payloads(self):
        """Rewriting /etc/hostname or remasking waagent would recreate host
        state that reset is supposed to have left alone, hiding exactly the
        cleanup defects this step exists to find."""
        with self._host() as (scp, ssh):
            previous = e2e._reinstall_ignition_payload(self._doc())

        self.assertEqual(previous, "inv-before", "the wait needs the run from before the reinstall")
        self.assertEqual(scp.call_count, 3, "binary, agent config, bootstrap unit")

        commands = "\n".join(str(call) for call in ssh.call_args_list)
        self.assertNotIn("/etc/hostname", commands)
        self.assertNotIn("waagent", commands)
        self.assertIn("enable --now --no-block", commands)

    def test_an_unexpected_payload_is_refused(self):
        """The payload set is asserted rather than filtered. Anything else is a
        change in what bootstrap installs, and should stop the run."""
        def moved_binary(doc):
            doc["storage"]["files"][0]["path"] = "/somewhere/else/unbounded-agent"

        def extra_file(doc):
            doc["storage"]["files"].append({"path": "/etc/unbounded/agent/extra", "contents": {"source": "x"}})

        def extra_unit(doc):
            doc["systemd"]["units"].append({"name": "surprise.service", "contents": "[Service]\n"})

        for name, change in (("moved binary", moved_binary), ("extra file", extra_file), ("extra unit", extra_unit)):
            with self.subTest(name), self._host() as (scp, _ssh):
                doc = self._doc()
                change(doc)

                with self.assertRaises(SystemExit):
                    e2e._reinstall_ignition_payload(doc)
                scp.assert_not_called()


class TestBootstrapChoosesThePath(unittest.TestCase):
    """The flag and the payload delivery are each covered elsewhere; this is
    the branch that wires them together."""

    def test_reinstall_keeps_the_disk(self):
        config = e2e.NodeConfig(name="default", node_labels={}, register_with_taints=[])
        doc = {"storage": {"files": []}, "systemd": {"units": []}}

        for reinstall in (False, True):
            with self.subTest(reinstall=reinstall), \
                    patch.object(e2e, "_ensure_vm_ssh_key", return_value="ssh-ed25519 AAAA"), \
                    patch.object(e2e, "agent_binary_url_and_digest", return_value=("http://x/a", "d" * 64)), \
                    patch.object(e2e, "node_config_bootstrap_args", return_value=[]), \
                    patch.object(e2e, "log_active_node_config"), \
                    patch.object(e2e, "capture", return_value=json.dumps(doc)), \
                    patch.object(e2e, "qemu_mac_address", return_value="52:54:00:12:34:56"), \
                    patch.object(e2e, "add_ignition_harness_access", side_effect=lambda d, *a: d), \
                    patch.object(e2e, "_wait_for_ignition_bootstrap") as wait, \
                    patch.object(e2e, "_stop_qemu") as stop, \
                    patch.object(e2e, "launch_ignition_vm") as launch, \
                    patch.object(e2e, "_reinstall_ignition_payload", return_value="inv-before") as payload:
                e2e._bootstrap_via_ignition(config, "https://api:6443", "https://127.0.0.1:6443",
                                            reinstall=reinstall)

                self.assertEqual(payload.called, reinstall)
                self.assertEqual(stop.called, not reinstall,
                                 "stopping the VM would change the boot id that reinstall_agent checks")
                self.assertEqual(launch.called, not reinstall)
                # The invocation read before the reinstall has to reach the
                # wait, or the wait accepts the previous run as this one. A
                # fresh VM has no previous run.
                wait.assert_called_once_with("inv-before" if reinstall else "")


class TestBootstrapCompletionIsFresh(unittest.TestCase):
    """A run that already finished is not a run; see settled_bootstrap_unit."""

    @staticmethod
    def _ssh(invocations):
        def ssh(command, _deadline, **_kwargs):
            out = "journal"
            if "InvocationID" in command:
                out = f"ActiveState=active\nInvocationID={next(invocations)}\nResult=success\nNRestarts=0\n"
            return subprocess.CompletedProcess([], 0, out, "")

        return ssh

    def _wait(self, invocations, previous):
        with tempfile.TemporaryDirectory() as tmp, \
                patch.object(e2e, "bounded_ssh", side_effect=self._ssh(iter(invocations))), \
                patch.object(e2e.time, "sleep") as slept, \
                patch.object(e2e, "LOGS_DIR", Path(tmp)):
            e2e._wait_for_ignition_bootstrap(previous)
            self.assertTrue((Path(tmp) / "ignition-bootstrap.log").exists(), "the journal is kept for CI")
        return slept.call_count

    def test_a_stale_active_unit_is_not_accepted(self):
        """It waits while the unit reports the previous run, and stops only once
        systemd reports a new one."""
        self.assertEqual(self._wait(["inv-old", "inv-old", "inv-old", "inv-new"], "inv-old"), 3)

    def test_a_new_invocation_completes_immediately(self):
        for previous in ("inv-old", ""):
            with self.subTest(previous=previous):
                self.assertEqual(self._wait(["inv-new"], previous), 0)


if __name__ == "__main__":
    unittest.main()
