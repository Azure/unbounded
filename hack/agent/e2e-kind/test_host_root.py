#!/usr/bin/env python3
# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0

"""The harness side of the host root and of moving to it.

A host installed by this build keeps the agent under /opt/unbounded. A host
installed by a release before that keeps it under /usr/local, and the current
agent links /opt/unbounded there until no older agent is left to roll back to,
then moves the files. These tests cover what the harness asserts about each,
and the fixtures that stand in for agents it cannot build.
"""
import hashlib
import io
import os
import subprocess
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

import e2e


def _run_script(script: str, *args: str) -> subprocess.CompletedProcess[str]:
    with tempfile.TemporaryDirectory() as tmp:
        path = Path(tmp) / "unbounded-agent"
        path.write_text(script)
        path.chmod(0o755)
        return subprocess.run([str(path), *args], capture_output=True, text=True, check=False)


def _fixture(builder) -> str:
    captured = {}
    with patch.object(e2e, "_build_script_agent_tarball",
                      side_effect=lambda _tarball, _name, script: captured.setdefault("script", script)):
        builder(Path("unused.tar.gz"))
    return captured["script"]


class TestFixtures(unittest.TestCase):
    def test_daemon_failing_agent_gets_past_verification(self):
        """AgentUpgrade verifies a candidate by running its version command.
        The rollback scenario needs the failure to come from the daemon, so the
        fixture has to answer that."""
        script = _fixture(e2e._build_daemon_failing_agent_tarball)

        self.assertEqual(_run_script(script, "version").returncode, 0)
        self.assertEqual(_run_script(script, "daemon").returncode, 42)


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
        self.assertIn(f'[ -L "{e2e.HOST_ROOT}" ]', checks)
        self.assertIn(f'[ -e "{e2e.HOST_ROOT_STAGING}" ]', checks, "an interrupted move's copy goes too")
        for root in (e2e.HOST_ROOT, e2e.LEGACY_HOST_ROOT):
            self.assertIn(f"{root}/bin/unbounded-agent-current", checks)
            self.assertIn(f"{root}/libexec/unbounded-localdns-network", checks)

    def test_a_leftover_fails_the_step(self):
        with patch.object(e2e, "ssh_capture", return_value=e2e.HOST_ROOT + "\n"), \
                patch.object(e2e, "host_image") as image:
            image.return_value.provisioning = "cloud-init"
            with self.assertRaises(SystemExit):
                e2e.validate_reset_cleanup()


class TestHostRootStates(unittest.TestCase):
    """Each validation accepts only the layout it names."""

    LEGACY_BIN = f"{e2e.LEGACY_HOST_ROOT}/bin"

    # For each validation: the state it accepts, and what the host reports
    # otherwise, chosen so that the state is the only thing that can fail it.
    CASES = {
        "validate_host_root": ("dir", e2e.DAEMON_BIN_DIR, e2e.DAEMON_BIN_DIR),
        "validate_host_root_legacy": ("absent", LEGACY_BIN, LEGACY_BIN),
        "validate_host_root_linked": (f"link:{e2e.LEGACY_HOST_ROOT}", LEGACY_BIN, LEGACY_BIN),
    }

    def _check(self, name, state, bin_dir, unit_dir):
        unit = f"ExecStart={unit_dir}/unbounded-agent-current daemon\n"
        with patch.object(e2e, "host_root_state", return_value=state), \
                patch.object(e2e, "resolve_on_host",
                             side_effect=lambda path: bin_dir if path.endswith("/bin") else f"{bin_dir}/unbounded-agent-blue"), \
                patch.object(e2e, "read_daemon_current_target", return_value=f"{bin_dir}/unbounded-agent-blue"), \
                patch.object(e2e, "_legacy_layout_present", return_value=[]), \
                patch.object(e2e, "ssh_capture", return_value=unit), \
                patch.object(e2e, "ssh_capture_quiet") as quiet:
            quiet.return_value.returncode = 0
            quiet.return_value.stdout = ""
            quiet.return_value.stderr = ""
            getattr(e2e, name)()

    def test_the_named_state_passes(self):
        for name, (accepted, bin_dir, unit_dir) in self.CASES.items():
            with self.subTest(validation=name):
                self._check(name, accepted, bin_dir, unit_dir)

    def test_every_other_state_is_refused(self):
        states = ["absent", "dir", f"link:{e2e.LEGACY_HOST_ROOT}", "link:/elsewhere", "other"]
        for name, (accepted, bin_dir, unit_dir) in self.CASES.items():
            for state in states:
                if state == accepted:
                    continue
                with self.subTest(validation=name, state=state):
                    with self.assertRaises(SystemExit):
                        self._check(name, state, bin_dir, unit_dir)

    def test_the_daemon_unit_must_name_the_expected_root(self):
        """A fresh host's unit runs the agent under the host root. A legacy or
        linked host's unit keeps naming the legacy path an older agent wrote,
        which rolling back to that agent depends on."""
        for name, (accepted, bin_dir, unit_dir) in self.CASES.items():
            other = self.LEGACY_BIN if unit_dir == e2e.DAEMON_BIN_DIR else e2e.DAEMON_BIN_DIR
            with self.subTest(validation=name):
                with self.assertRaises(SystemExit):
                    self._check(name, accepted, bin_dir, other)


class TestLegacyLayoutWait(unittest.TestCase):
    """The install script seeds the legacy root for older agents on every host
    that allows it, and the daemon removes the seed once it runs."""

    def test_waits_for_the_seed_to_go(self):
        seed = f"{e2e.LEGACY_HOST_ROOT}/bin/unbounded-agent"
        with patch.object(e2e, "_legacy_layout_present", side_effect=[[seed], [seed], []]) as present, \
                patch.object(e2e.time, "sleep"):
            e2e._wait_for_legacy_layout_gone()
        self.assertEqual(present.call_count, 3)

    def test_a_file_that_stays_fails(self):
        seed = f"{e2e.LEGACY_HOST_ROOT}/bin/unbounded-agent"
        with patch.object(e2e, "_legacy_layout_present", return_value=[seed]), \
                patch.object(e2e.time, "sleep"):
            with self.assertRaises(SystemExit):
                e2e._wait_for_legacy_layout_gone(timeout_secs=0)


class TestHostRootMoved(unittest.TestCase):
    """What a host moved from the legacy root has to show."""

    BIN = e2e.DAEMON_BIN_DIR
    HELPER = f"{e2e.DAEMON_BIN_DIR}/unbounded-agent-nspawn-lifecycle"

    def _host(self, *, states=("dir",), moving=(False,), executable=None, hooks=None, agents=False):
        """Return a stand-in for ssh_capture_quiet, and the state sequence."""
        moving = list(moving)
        hooks = [f"ExecStartPre={self.HELPER} nspawn-lifecycle pre-start kube1"] if hooks is None else hooks
        executable = f"{self.BIN}/unbounded-agent-blue" if executable is None else executable

        def quiet(command):
            result = subprocess.CompletedProcess(command, 0, "", "")
            if e2e.HOST_ROOT_MOVING_MARKER in command:
                result.returncode = 0 if (moving.pop(0) if len(moving) > 1 else moving[0]) else 1
            elif e2e.HOST_ROOT_AGENTS in command:
                result.returncode = 0 if agents else 1
            elif "MainPID" in command:
                result.stdout = executable + "\n"
            elif "nspawn-lifecycle" in command:
                result.stdout = "\n".join(hooks) + "\n"
            return result

        return quiet, list(states)

    def _run(self, **host):
        quiet, states = self._host(**host)
        with patch.object(e2e, "ssh_capture_quiet", side_effect=quiet), \
                patch.object(e2e, "host_root_state", side_effect=lambda: states.pop(0) if len(states) > 1 else states[0]), \
                patch.object(e2e, "resolve_on_host", return_value=self.BIN), \
                patch.object(e2e, "read_daemon_last_good_target", return_value=f"{self.BIN}/unbounded-agent-green"), \
                patch.object(e2e, "validate_host_root") as validate, \
                patch.object(e2e, "wait_for_node_ready"), \
                patch.object(e2e.time, "sleep"):
            e2e.validate_host_root_moved()
        return validate

    def test_a_moved_host_passes(self):
        validate = self._run()
        validate.assert_called_once()

    def test_waits_for_the_link_to_be_replaced_and_the_move_to_finish(self):
        validate = self._run(states=(f"link:{e2e.LEGACY_HOST_ROOT}", "dir", "dir"), moving=(True, True, False))
        validate.assert_called_once()

    def test_hooks_still_naming_the_legacy_helper_fail(self):
        stale = [f"ExecStartPre={e2e.LEGACY_HOST_ROOT}/bin/unbounded-agent-nspawn-lifecycle nspawn-lifecycle pre-start kube1"]
        with self.assertRaises(SystemExit):
            self._run(hooks=stale)

    def test_the_agents_record_must_be_gone(self):
        with self.assertRaises(SystemExit):
            self._run(agents=True)


class TestLegacyAgent(unittest.TestCase):
    def test_refused_on_an_immutable_host(self):
        config = e2e.NodeConfig(name="default", node_labels={}, register_with_taints=[])
        with patch.object(e2e, "host_image") as image, \
                patch.object(e2e, "prepare_agent_artifacts") as prepare:
            image.return_value.provisioning = "ignition"
            with self.assertRaises(SystemExit):
                e2e.run_legacy_agent(config)
        prepare.assert_not_called()

    def test_version_must_be_a_release_tag(self):
        """It is interpolated into a URL and a shell command line."""
        config = e2e.NodeConfig(name="default", node_labels={}, register_with_taints=[])
        with patch.object(e2e, "host_image") as image, \
                patch.object(e2e, "LEGACY_AGENT_VERSION", "v0.8.0; true"), \
                patch.object(e2e, "prepare_agent_artifacts") as prepare:
            image.return_value.provisioning = "cloud-init"
            with self.assertRaises(SystemExit):
                e2e.run_legacy_agent(config)
        prepare.assert_not_called()

    def test_installs_the_release_and_restores_the_environment(self):
        config = e2e.NodeConfig(name="default", node_labels={}, register_with_taints=[])
        seen = {}

        def run_agent(_config):
            seen["url"] = os.environ.get("AGENT_URL")

        with patch.dict(os.environ, {"AGENT_URL": "http://previous"}), \
                patch.object(e2e, "host_image") as image, \
                patch.object(e2e, "prepare_agent_artifacts"), \
                patch.object(e2e, "run_agent", side_effect=run_agent):
            image.return_value.provisioning = "cloud-init"
            e2e.run_legacy_agent(config)
            after = os.environ.get("AGENT_URL")

        self.assertEqual(seen["url"], f"{e2e.LEGACY_AGENT_RELEASE_URL}/{e2e.LEGACY_AGENT_TARBALL}")
        self.assertEqual(after, "http://previous")

    @staticmethod
    def _release(content: bytes) -> bytes:
        """A release tarball: the agent and the license files beside it."""
        import tarfile

        buffer = io.BytesIO()
        with tarfile.open(fileobj=buffer, mode="w:gz") as archive:
            for name, data in (("LICENSE", b"license"), ("NOTICE", b"notice"), ("unbounded-agent", content)):
                info = tarfile.TarInfo(name)
                info.size = len(data)
                archive.addfile(info, io.BytesIO(data))
        return buffer.getvalue()

    def test_downloaded_release_is_checked_against_its_checksums(self):
        good = self._release(b"the release")

        def urlopen(url, timeout):
            if url.endswith("/checksums.txt"):
                body = f"{hashlib.sha256(good).hexdigest()}  {e2e.LEGACY_AGENT_TARBALL}\n".encode()
            else:
                body = urlopen.tarball
            return io.BytesIO(body)

        for tarball, ok in ((good, True), (self._release(b"something else"), False)):
            urlopen.tarball = tarball
            with self.subTest(ok=ok), tempfile.TemporaryDirectory() as tmp, \
                    patch.object(e2e, "VM_DIR", Path(tmp)), \
                    patch.object(e2e.urllib.request, "urlopen", side_effect=urlopen):
                if ok:
                    import tarfile

                    # AgentUpgrade takes an archive holding the agent alone.
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
            root, legacy = Path(tmp, "opt", "unbounded"), Path(tmp, "usr", "local")
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

        self.assertEqual(steps[0], "run-legacy-agent")
        self.assertEqual(steps[-1], "reset-agent")
        self.assertLess(steps.index("validate-host-root-legacy"), steps.index("validate-host-root-linked"))

        downgrade = steps.index("validate-agent-downgrade-to-legacy")
        moved = steps.index("validate-host-root-moved")
        self.assertLess(downgrade, moved, "a return to the older release happens while the host is linked")
        self.assertNotIn("validate-agent-downgrade-to-legacy", steps[moved:], "it is not supported after the move")
        self.assertEqual(steps[downgrade:moved].count("validate-agent-upgrade-operation"), 2)
        self.assertIn("validate-host-root-linked", steps[downgrade:moved],
                      "one upgrade after the older release still leaves it in last-good")
        self.assertIn("validate-host-reboot", steps[moved:], "a moved host has to boot from its new units")
        e2e.validate_suites()

    def test_lifecycle_checks_the_host_root_after_each_install(self):
        steps = e2e.SUITES["lifecycle"]

        installs = [i for i, step in enumerate(steps) if step in ("run-agent", "reinstall-agent")]
        self.assertEqual(len(installs), 2)
        for index in installs:
            self.assertIn("validate-host-root", steps[index:index + 3])


if __name__ == "__main__":
    unittest.main()
