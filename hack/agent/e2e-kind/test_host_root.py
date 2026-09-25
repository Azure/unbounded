#!/usr/bin/env python3
# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0

"""The harness side of the host root and of migrating to it.

A host installed by this build keeps the agent under /opt/unbounded. A host
installed by a release before that keeps it under /usr/local, and the current
agent links /opt/unbounded there. These tests cover what the harness asserts
about each, and the fixtures that stand in for agents it cannot build.
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
        """AgentUpgrade verifies a candidate by asking for its version and its
        host root. The rollback scenario needs the failure to come from the
        daemon, so the fixture has to answer both the way a current agent
        does."""
        script = _fixture(e2e._build_daemon_failing_agent_tarball)

        self.assertEqual(_run_script(script, "version").returncode, 0)
        host_root = _run_script(script, "host-root")
        self.assertEqual(host_root.returncode, 0)
        self.assertEqual(host_root.stdout.strip(), os.path.realpath(e2e.HOST_ROOT))
        self.assertEqual(_run_script(script, "daemon").returncode, 42)

    def test_legacy_agent_has_no_host_root(self):
        """An agent released before the host root answers version and nothing
        else, which is all verification can tell it apart by."""
        script = _fixture(e2e._build_legacy_agent_tarball)

        self.assertEqual(_run_script(script, "version").returncode, 0)
        self.assertNotEqual(_run_script(script, "host-root").returncode, 0)


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
        "validate_host_root_migrated": (f"link:{e2e.LEGACY_HOST_ROOT}", LEGACY_BIN, LEGACY_BIN),
    }

    def _check(self, name, state, bin_dir, unit_dir):
        unit = f"ExecStart={unit_dir}/unbounded-agent-current daemon\n"
        with patch.object(e2e, "host_root_state", return_value=state), \
                patch.object(e2e, "resolve_on_host",
                             side_effect=lambda path: bin_dir if path.endswith("/bin") else f"{bin_dir}/unbounded-agent-blue"), \
                patch.object(e2e, "read_daemon_current_target", return_value=f"{bin_dir}/unbounded-agent-blue"), \
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
        migrated host's unit keeps naming the legacy path an older agent wrote,
        which rolling back to that agent depends on."""
        for name, (accepted, bin_dir, unit_dir) in self.CASES.items():
            other = self.LEGACY_BIN if unit_dir == e2e.DAEMON_BIN_DIR else e2e.DAEMON_BIN_DIR
            with self.subTest(validation=name):
                with self.assertRaises(SystemExit):
                    self._check(name, accepted, bin_dir, other)


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
    def test_migration_starts_legacy_and_ends_with_the_current_agent_resetting(self):
        """Reset is performed by the daemon. An older one leaves the link, so
        the suite returns to this build before resetting."""
        steps = e2e.SUITES["migration"]

        self.assertEqual(steps[0], "run-legacy-agent")
        self.assertEqual(steps[-1], "reset-agent")
        downgrade = steps.index("validate-agent-downgrade-to-legacy")
        self.assertIn("validate-agent-upgrade-operation", steps[downgrade:])
        self.assertLess(steps.index("validate-host-root-legacy"), steps.index("validate-host-root-migrated"))
        e2e.validate_suites()

    def test_lifecycle_checks_the_host_root_after_each_install(self):
        steps = e2e.SUITES["lifecycle"]

        installs = [i for i, step in enumerate(steps) if step in ("run-agent", "reinstall-agent")]
        self.assertEqual(len(installs), 2)
        for index in installs:
            self.assertIn("validate-host-root", steps[index:index + 3])


if __name__ == "__main__":
    unittest.main()
