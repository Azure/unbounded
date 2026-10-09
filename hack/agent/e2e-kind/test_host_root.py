#!/usr/bin/env python3
# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0

"""The harness side of the host root and of moving to it.

A host installed by this build keeps the agent under /opt/unbounded/agent. A
host installed by a release before that keeps it under /usr/local, and the
current agent links /opt/unbounded/agent there until no older agent is left to
roll back to, then moves the files. Anything else under /opt/unbounded is the
host's. These cover what the harness asserts about each.
"""
import hashlib
import io
import os
import subprocess
import tarfile
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

import e2e


class TestResetCleanup(unittest.TestCase):
    def test_both_roots_and_the_root_itself_are_checked(self):
        """Reset does not depend on the host root having been migrated, so it
        sweeps both roots, and it removes the host root last. A check that only
        looked where this run installed would not see the other left behind."""
        with patch.object(e2e, "ssh_capture", return_value="") as capture, \
                patch.object(e2e, "host_image") as image:
            image.return_value.provisioning = "cloud-init"
            e2e.validate_reset_cleanup()

        checks = capture.call_args.args[0]
        self.assertIn("/var/lib/unbounded/agent/install-state.json", checks, "ownership goes too")
        self.assertIn(f'[ -L "{e2e.HOST_ROOT}" ]', checks)
        self.assertIn(f'[ -e "{e2e.HOST_ROOT_STAGING}" ]', checks, "an interrupted move's copy goes too")
        for root in (e2e.HOST_ROOT, e2e.LEGACY_HOST_ROOT):
            self.assertIn(f"{root}/bin/unbounded-agent-current", checks)
            self.assertIn(f"{root}/libexec/unbounded-localdns-network", checks)


class TestIgnitionResetCleanup(unittest.TestCase):
    def test_the_bootstrap_unit_and_its_enablement_are_checked(self):
        """Either left behind bootstraps the reset host again on the next boot."""
        with patch.object(e2e, "ssh_capture", return_value="") as capture, \
                patch.object(e2e, "host_image") as image:
            image.return_value.provisioning = "ignition"
            e2e.validate_reset_cleanup()

        checks = capture.call_args.args[0]
        self.assertIn(f"/etc/systemd/system/{e2e.IGNITION_BOOTSTRAP_UNIT}", checks)
        self.assertIn(f"/etc/systemd/system/multi-user.target.wants/{e2e.IGNITION_BOOTSTRAP_UNIT}", checks)


class TestAgentUpgradeSlots(unittest.TestCase):
    """The upgrade check compares slots by name only when the host root moved
    while it ran; otherwise a last-good under the wrong root would pass."""

    def _validate(self, states, before, after, last_good):
        targets = iter([before, after])
        with patch.object(e2e, "host_root_state", side_effect=states), \
                patch.object(e2e, "read_daemon_current_target", side_effect=lambda: next(targets)), \
                patch.object(e2e, "read_daemon_last_good_target", return_value=last_good), \
                patch.object(e2e, "_build_agent_upgrade_tarball"), \
                patch.object(e2e, "_serve_agent_upgrade_tarball",
                             return_value={"status": {"message": "AgentUpgrade completed"}}), \
                patch.object(e2e, "wait_for_daemon_active"), \
                patch.object(e2e, "kubectl"):
            e2e.validate_agent_upgrade_operation()

    def test_paths_must_match_while_the_root_stays(self):
        blue, green = f"{e2e.DAEMON_BIN_DIR}/unbounded-agent-blue", f"{e2e.DAEMON_BIN_DIR}/unbounded-agent-green"
        self._validate(["dir", "dir"], blue, green, blue)
        with self.assertRaises(SystemExit):
            self._validate(["dir", "dir"], blue, green, f"{e2e.LEGACY_HOST_ROOT}/bin/unbounded-agent-blue")

    def test_slots_are_compared_by_name_once_the_root_moves(self):
        legacy_blue = f"{e2e.LEGACY_HOST_ROOT}/bin/unbounded-agent-blue"
        self._validate([f"link:{e2e.LEGACY_HOST_ROOT}", "moving"], legacy_blue,
                       f"{e2e.DAEMON_BIN_DIR}/unbounded-agent-green", f"{e2e.DAEMON_BIN_DIR}/unbounded-agent-blue")


class TestHostRootStates(unittest.TestCase):
    """Each validation accepts only the layout it names."""

    LEGACY_BIN = f"{e2e.LEGACY_HOST_ROOT}/bin"

    # For each validation: the state it accepts and the directory its units
    # name, chosen so that the state is the only thing that can fail it.
    CASES = {
        "validate_host_root": ("dir", e2e.DAEMON_BIN_DIR),
        "validate_host_root_linked": (f"link:{e2e.LEGACY_HOST_ROOT}", LEGACY_BIN),
    }

    def _check(self, name, state, bin_dir, unit_dir=None):
        units = unit_dir or bin_dir
        unit = (f"ExecStart={units}/unbounded-agent-current daemon\n"
                f"ExecStart={units}/unbounded-agent-daemon-recovery.sh\n"
                f"ExecStartPost={units}/unbounded-agent-nspawn-lifecycle nspawn-lifecycle post-start kube1\n")
        with patch.object(e2e, "host_root_state", return_value=state), \
                patch.object(e2e, "read_daemon_current_target", return_value=f"{bin_dir}/unbounded-agent-blue"), \
                patch.object(e2e, "present_on_host", return_value=[]), \
                patch.object(e2e, "ssh_capture", return_value=unit), \
                patch.object(e2e, "ssh_capture_quiet") as quiet:
            quiet.return_value = subprocess.CompletedProcess([], 0, "", "")
            getattr(e2e, name)()

    def test_the_named_state_passes(self):
        for name, (accepted, bin_dir) in self.CASES.items():
            with self.subTest(validation=name):
                self._check(name, accepted, bin_dir)

    def test_an_unfinished_move_is_not_installed(self):
        with self.assertRaises(SystemExit):
            self._check("validate_host_root", "moving", e2e.DAEMON_BIN_DIR)

    def test_every_unit_must_name_the_expected_root(self):
        """The recovery unit and the machine's hooks follow the daemon unit;
        one left naming the other root would run a file the move removed."""
        bin_dir = e2e.DAEMON_BIN_DIR
        complete = (f"ExecStart={bin_dir}/unbounded-agent-current daemon\n"
                    f"ExecStart={bin_dir}/unbounded-agent-daemon-recovery.sh\n"
                    f"ExecStartPost={bin_dir}/unbounded-agent-nspawn-lifecycle nspawn-lifecycle post-start kube1\n")
        for missing in complete.splitlines():
            units = complete.replace(missing + "\n", "")
            with self.subTest(missing=missing), patch.object(e2e, "ssh_capture", return_value=units):
                with self.assertRaises(SystemExit):
                    e2e._daemon_unit_runs(bin_dir)

    def test_the_daemon_unit_must_name_the_expected_root(self):
        """A fresh host's unit runs the agent under the host root. A linked
        host's unit keeps naming the legacy path an older agent wrote, which
        rolling back to that agent depends on."""
        for name, (accepted, bin_dir) in self.CASES.items():
            other = self.LEGACY_BIN if bin_dir == e2e.DAEMON_BIN_DIR else e2e.DAEMON_BIN_DIR
            with self.subTest(validation=name), self.assertRaises(SystemExit):
                self._check(name, accepted, bin_dir, other)


class TestWaitUntil(unittest.TestCase):
    def test_waits_for_the_check_to_pass(self):
        """The install script seeds the legacy root for older agents, and the
        daemon removes the seed once it runs."""
        answers = ["seed left", "seed left", ""]
        with patch.object(e2e.time, "sleep") as slept:
            e2e._wait_until(lambda: answers.pop(0), 120)
        self.assertEqual(slept.call_count, 2)

    def test_a_check_that_keeps_failing_fails(self):
        with patch.object(e2e.time, "sleep"), patch.object(e2e, "die", side_effect=SystemExit) as died:
            with self.assertRaises(SystemExit):
                e2e._wait_until(lambda: "seed left", 0)
        died.assert_called_once_with("seed left")


class TestHostRootMoved(unittest.TestCase):
    def test_waits_for_the_link_to_be_replaced_and_the_move_to_finish(self):
        states = [f"link:{e2e.LEGACY_HOST_ROOT}", "moving", "dir"]
        running = subprocess.CompletedProcess([], 0, f"{e2e.DAEMON_BIN_DIR}/unbounded-agent-blue\n", "")
        with patch.object(e2e, "host_root_state", side_effect=states), \
                patch.object(e2e, "ssh_capture_quiet", return_value=running), \
                patch.object(e2e, "validate_host_root") as validate, \
                patch.object(e2e.time, "sleep") as slept:
            e2e.validate_host_root_moved()

        self.assertEqual(slept.call_count, 2)
        validate.assert_called_once()

    def test_waits_for_the_daemon_to_start_from_the_moved_root(self):
        executables = ["", f"{e2e.LEGACY_HOST_ROOT}/bin/unbounded-agent-blue",
                       f"{e2e.DAEMON_BIN_DIR}/unbounded-agent-blue"]
        with patch.object(e2e, "host_root_state", return_value="dir"), \
                patch.object(e2e, "_daemon_executable", side_effect=executables), \
                patch.object(e2e, "validate_host_root") as validate, \
                patch.object(e2e.time, "sleep") as slept:
            e2e.validate_host_root_moved()

        self.assertEqual(slept.call_count, 2)
        validate.assert_called_once()

    def test_a_moved_directory_does_not_hide_a_failed_daemon_start(self):
        for executable in ("", f"{e2e.LEGACY_HOST_ROOT}/bin/unbounded-agent-blue"):
            with self.subTest(executable=executable), \
                    patch.object(e2e, "host_root_state", return_value="dir"), \
                    patch.object(e2e, "_daemon_executable", return_value=executable), \
                    patch.object(e2e, "validate_host_root") as validate, \
                    patch.object(e2e.time, "monotonic", side_effect=[0, 0, 181]), \
                    patch.object(e2e, "die", side_effect=SystemExit) as died:
                with self.assertRaises(SystemExit):
                    e2e.validate_host_root_moved()
                died.assert_called_once_with(f"daemon runs {executable!r}, not a binary under {e2e.DAEMON_BIN_DIR}")
                validate.assert_not_called()


class TestLegacyAgent(unittest.TestCase):
    def test_version_is_checked_before_any_download(self):
        with patch.object(e2e, "LEGACY_AGENT_VERSION", "../v0.8.0"), \
                patch.object(e2e, "http_get") as get:
            with self.assertRaises(SystemExit):
                e2e._download_legacy_agent_tarball()
        get.assert_not_called()

    def test_version_must_be_a_release_tag(self):
        """It is interpolated into a URL and a shell command line."""
        config = e2e.NodeConfig(name="default", node_labels={}, register_with_taints=[])
        with patch.object(e2e, "LEGACY_AGENT_VERSION", "v0.8.0; true"), \
                patch.object(e2e, "prepare_agent_artifacts") as prepare:
            with self.assertRaises(SystemExit):
                e2e.run_legacy_agent(config)
        prepare.assert_not_called()

    def test_installs_the_release_and_restores_the_environment(self):
        config = e2e.NodeConfig(name="default", node_labels={}, register_with_taints=[])
        seen = {}

        def run_agent(_config):
            seen["url"] = os.environ.get("AGENT_URL")

        with patch.dict(os.environ, {"AGENT_URL": "http://previous"}), \
                patch.object(e2e, "prepare_agent_artifacts"), \
                patch.object(e2e, "run_agent", side_effect=run_agent):
            e2e.run_legacy_agent(config)
            after = os.environ.get("AGENT_URL")

        self.assertEqual(seen["url"], f"{e2e.LEGACY_AGENT_RELEASE_URL}/{e2e.LEGACY_AGENT_TARBALL}")
        self.assertEqual(after, "http://previous")

    def test_downloaded_release_is_checked_and_repackaged_alone(self):
        """AgentUpgrade takes an archive holding the agent alone, and the release
        also ships its license files."""
        def release(content: bytes) -> bytes:
            buffer = io.BytesIO()
            with tarfile.open(fileobj=buffer, mode="w:gz") as archive:
                for name, data in (("LICENSE", b"license"), ("unbounded-agent", content)):
                    info = tarfile.TarInfo(name)
                    info.size = len(data)
                    archive.addfile(info, io.BytesIO(data))
            return buffer.getvalue()

        good = release(b"the release")
        checksums = f"{hashlib.sha256(good).hexdigest()}  {e2e.LEGACY_AGENT_TARBALL}\n"

        for tarball, ok in ((good, True), (release(b"something else"), False)):
            with self.subTest(ok=ok), tempfile.TemporaryDirectory() as tmp, \
                    patch.object(e2e, "VM_DIR", Path(tmp)), \
                    patch.object(e2e, "http_get", return_value=checksums), \
                    patch.object(e2e, "download_file", side_effect=lambda _url, dest, data=tarball: dest.write_bytes(data)):
                if ok:
                    with tarfile.open(e2e._download_legacy_agent_tarball()) as archive:
                        self.assertEqual(archive.getnames(), ["unbounded-agent"])
                        self.assertEqual(archive.extractfile("unbounded-agent").read(), b"the release")
                else:
                    with self.assertRaises(SystemExit):
                        e2e._download_legacy_agent_tarball()


class TestDaemonLinks(unittest.TestCase):
    def test_links_are_found_under_the_legacy_root_before_migration(self):
        """The first upgrade in the migration suite starts on a host the
        current agent has not run on yet, where the host root does not exist."""
        with tempfile.TemporaryDirectory() as tmp:
            root, legacy = Path(tmp, "opt", "unbounded", "agent"), Path(tmp, "usr", "local")
            (legacy / "bin").mkdir(parents=True)
            (legacy / "bin" / "unbounded-agent-blue").write_text("")
            (legacy / "bin" / "unbounded-agent-current").symlink_to(legacy / "bin" / "unbounded-agent-blue")

            def resolve():
                with patch.object(e2e, "HOST_ROOT", str(root)), \
                        patch.object(e2e, "LEGACY_HOST_ROOT", str(legacy)):
                    command = e2e._resolve_daemon_link(f"{root}/bin/unbounded-agent-current")
                return subprocess.run(command.removeprefix("sudo "), shell=True,
                                      capture_output=True, text=True, check=True).stdout.strip()

            self.assertEqual(resolve(), str(legacy / "bin" / "unbounded-agent-blue"))

            root.parent.mkdir(parents=True)
            root.symlink_to(legacy)
            self.assertEqual(resolve(), str(legacy / "bin" / "unbounded-agent-blue"),
                             "through the link once migrated")


class TestSuites(unittest.TestCase):
    def test_migration_moves_only_once_the_older_release_is_out_of_the_slots(self):
        """The host stays linked, and the older release can be returned to,
        until two upgrades after the last time it ran: the first puts it in
        last-good, the second pushes it out. Reset is performed by the daemon,
        so the suite ends on this build."""
        steps = e2e.SUITES["migration"]

        self.assertEqual(steps[:2], ["stage-host-files", "run-legacy-agent"])
        self.assertEqual(steps[-2:], ["reset-agent", "validate-host-staged-files"])

        downgrade = steps.index("validate-agent-downgrade-to-legacy")
        moved = steps.index("validate-host-root-moved")
        self.assertLess(downgrade, moved, "a return to the older release happens while the host is linked")
        self.assertNotIn("validate-agent-downgrade-to-legacy", steps[moved:], "it is not supported after the move")
        self.assertEqual(steps[downgrade:moved].count("validate-agent-upgrade-operation"), 2)
        self.assertIn("validate-host-root-linked", steps[downgrade:moved],
                      "one upgrade after the older release still leaves it in last-good")
        self.assertIn("validate-host-reboot", steps[moved:], "a moved host has to boot from its new units")
        e2e.validate_suites()

    def test_migration_checks_staged_files_after_each_change_to_the_root(self):
        """Hosts stage files under /opt/unbounded, beside the host root, and an
        older agent's host may have them before the root is linked there."""
        steps = e2e.SUITES["migration"]
        checks = [i for i, step in enumerate(steps) if step == "validate-host-staged-files"]

        linked = steps.index("validate-host-root-linked")
        moved = steps.index("validate-host-root-moved")
        reset = steps.index("reset-agent")
        for change in (linked, moved, reset):
            self.assertTrue(any(check > change for check in checks), f"no check after {steps[change]}")


class TestWarn(unittest.TestCase):
    """warn() carries remote output, such as a refused reset-failed's stderr."""

    def test_remote_output_cannot_start_a_workflow_command(self):
        """GitHub reads every log line for workflow commands, so no raw line of
        the message may reach the log there."""
        message = "refused\n::add-mask::x\r\n100%"
        with patch.dict(os.environ, {"GITHUB_ACTIONS": "true"}), patch("builtins.print") as printed:
            e2e.warn(message)

        lines = [call.args[0] for call in printed.call_args_list]
        self.assertEqual(lines, ["::warning::refused%0A::add-mask::x%0D%0A100%25"])

    def test_outside_actions_it_is_printed_as_is(self):
        with patch.dict(os.environ, {"GITHUB_ACTIONS": ""}), patch("builtins.print") as printed:
            e2e.warn("refused")

        printed.assert_called_once_with("[WARN]  refused", flush=True)


class TestStagedFiles(unittest.TestCase):
    def test_staged_files_must_be_unchanged(self):
        for stdout, returncode, ok in ((e2e.HOST_STAGED_CONTENT, 0, True), ("changed", 0, False), ("", 1, False)):
            with self.subTest(stdout=stdout, returncode=returncode), \
                    patch.object(e2e, "ssh_capture_quiet",
                                 return_value=subprocess.CompletedProcess([], returncode, stdout, "")):
                if ok:
                    e2e.validate_host_staged_files()
                else:
                    with self.assertRaises(SystemExit):
                        e2e.validate_host_staged_files()

    def test_files_are_staged_beside_the_host_root(self):
        self.assertEqual(os.path.dirname(e2e.HOST_ROOT), e2e.HOST_ROOT_PARENT)
        self.assertTrue(e2e.HOST_STAGED_FILE.startswith(f"{e2e.HOST_ROOT_PARENT}/"))
        self.assertFalse(e2e.HOST_STAGED_FILE.startswith(f"{e2e.HOST_ROOT}/"))
        self.assertFalse(e2e.OFFLINE_ARTIFACTS_DIR.startswith(f"{e2e.HOST_ROOT}/"))

    def test_an_ignition_host_is_refused(self):
        """It boots with the agent already in place, so nothing can be staged first."""
        with patch.object(e2e, "host_image") as image, patch.object(e2e, "ssh_cmd") as ssh:
            image.return_value.provisioning = "ignition"
            with self.assertRaises(SystemExit):
                e2e.stage_host_files()
        ssh.assert_not_called()


if __name__ == "__main__":
    unittest.main()
