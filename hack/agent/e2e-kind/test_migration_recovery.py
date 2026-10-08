# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0

"""Contracts for interrupting host-root migration before SELinux relabeling.

These test the harness and its guest fixture, not SELinux itself. The real
policy and the agent's recovery path are exercised by the AlmaLinux CI guest.
"""
import os
import subprocess
import tempfile
import time
import unittest
from pathlib import Path
from unittest.mock import patch

import e2e


FIXTURE = Path(__file__).with_name("host-root-recovery.sh")


class TestMigrationRecoverySequence(unittest.TestCase):
    def run_recovery(self, events, fail_at="", checkpoint_reached=True):
        def probe(action):
            events.append(action)
            if action == fail_at:
                raise RuntimeError(f"failed {action}")

        def wait(check, _timeout):
            problem = check()
            if problem:
                raise RuntimeError(problem)

        with patch.object(e2e, "validate_host_root_linked", side_effect=lambda: events.append("linked")), \
                patch.object(e2e, "_host_root_recovery_probe", side_effect=probe), \
                patch.object(e2e, "validate_agent_upgrade_operation", side_effect=lambda: events.append("upgrade")), \
                patch.object(e2e, "bounded_ssh", return_value=subprocess.CompletedProcess([], 0 if checkpoint_reached else 255, "", "")), \
                patch.object(e2e, "_wait_until", side_effect=wait), \
                patch.object(e2e, "host_root_state", return_value="dir"), \
                patch.object(e2e, "validate_host_root_moved", side_effect=lambda: events.append("moved")):
            e2e.validate_host_root_migration_recovery()

    def test_interrupts_before_removing_hook_and_resumes_without_it(self):
        events = []
        self.run_recovery(events)
        self.assertEqual(events, ["linked", "arm", "upgrade", "checkpoint", "interrupt",
                                  "disarm", "resume", "verify", "moved"])

    def test_failure_does_not_resume_or_claim_recovery(self):
        for action in ("arm", "checkpoint", "interrupt", "disarm", "resume", "verify"):
            with self.subTest(action=action):
                events = []
                with self.assertRaisesRegex(RuntimeError, f"failed {action}"):
                    self.run_recovery(events, fail_at=action)
                self.assertIn("disarm", events)
                self.assertNotIn("moved", events)
                if action in ("arm", "checkpoint", "interrupt", "disarm"):
                    self.assertNotIn("resume", events)
                if action == "arm":
                    self.assertNotIn("upgrade", events)

    def test_missing_checkpoint_does_not_pass_as_an_interruption(self):
        events = []
        with self.assertRaisesRegex(RuntimeError, "did not reach the pre-relabel checkpoint"):
            self.run_recovery(events, checkpoint_reached=False)
        self.assertEqual(events, ["linked", "arm", "upgrade", "disarm"])

    def test_failed_guest_check_keeps_diagnostics(self):
        with tempfile.TemporaryDirectory() as tmp, \
                patch.object(e2e, "LOGS_DIR", Path(tmp)), \
                patch.object(e2e, "bounded_ssh", return_value=subprocess.CompletedProcess([], 1, "wrong labels\n", "denied\n")):
            with self.assertRaises(SystemExit):
                e2e._host_root_recovery_probe("verify")
            self.assertEqual((Path(tmp) / "host-root-recovery-verify.log").read_text(), "wrong labels\ndenied\n")

    def test_suite_reaches_linked_host_before_fault_and_reboots_after_recovery(self):
        e2e.validate_suites()
        steps = e2e.SUITES["migration-recovery"]
        fault = steps.index("validate-host-root-migration-recovery")
        self.assertEqual(steps[:fault], ["run-legacy-agent", "wait-for-node",
                                       "validate-agent-upgrade-operation", "validate-host-root-linked"])
        self.assertEqual(steps[fault + 1:], ["validate-host-reboot", "validate-host-root", "reset-agent"])


class TestMigrationRecoveryFixture(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        base = Path(self.tmp.name)
        self.root = base / "host root"
        self.legacy = base / "legacy"
        self.probe = base / "probe"
        self.drop_in = base / "systemd" / "recovery.conf"
        self.tools = base / "tools"
        self.tools.mkdir()
        (self.legacy / "bin").mkdir(parents=True)
        binary = self.legacy / "bin/unbounded-agent-blue"
        binary.write_text("#!/bin/sh\nexit 0\n")
        binary.chmod(0o755)
        (self.legacy / "bin/unbounded-agent-current").symlink_to(binary)
        self.root.symlink_to(self.legacy)
        self.calls = base / "systemctl-calls"
        self.restore_calls = base / "restorecon-calls"
        self.env = {**os.environ, "PATH": f"{self.tools}:{os.environ['PATH']}",
                    "SELINUX_MODE": "Enforcing", "LABEL_DRIFT": "1", "TEST_LEGACY": str(self.legacy),
                    "SYSTEMCTL_CALLS": str(self.calls), "RESTORECON_CALLS": str(self.restore_calls)}
        self.tool("getenforce", 'printf "%s\\n" "$SELINUX_MODE"\n')
        self.tool("chcon", "exit 0\n")
        self.tool("ls", "echo 'fixture contexts (not a SELinux assertion)'\n")
        self.tool("systemctl", '''printf '%s\\n' "$*" >> "$SYSTEMCTL_CALLS"
case "$*" in
    *--property=MainPID*) echo "${DAEMON_PID:-0}" ;;
    *--property=ExecStart*) echo "$TEST_LEGACY/bin/unbounded-agent-current daemon" ;;
esac
''')
        self.tool("restorecon", '''printf '%s\\n' "$*" >> "$RESTORECON_CALLS"
if [ "$1" = -nvR ] && [ "$LABEL_DRIFT" = 1 ]; then
    echo "Would relabel $2/bin/unbounded-agent-blue from usr_t to bin_t"
fi
''')
        # A bounded stand-in for infinity, which macOS sleep does not accept.
        self.tool("sleep", "exec /bin/sleep 60\n")

    def tool(self, name, body):
        path = self.tools / name
        path.write_text("#!/bin/sh\nset -eu\n" + body)
        path.chmod(0o755)

    def action(self, action):
        return subprocess.run(["bash", str(FIXTURE), action, str(self.root), str(self.legacy),
                               str(self.probe), str(self.drop_in)],
                              env=self.env, text=True, capture_output=True, timeout=10)

    def require_action(self, action):
        result = self.action(action)
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        return result

    def swap(self):
        self.root.unlink()
        (self.root / "bin").mkdir(parents=True)
        (self.root / ".moving").touch()
        (self.root / "bin/unbounded-agent-blue").write_text("copied binary")

    def test_enforcement_is_required_before_installing_hook(self):
        for mode in ("Permissive", "Disabled"):
            with self.subTest(mode=mode):
                self.env["SELINUX_MODE"] = mode
                result = self.action("arm")
                self.assertNotEqual(result.returncode, 0)
                self.assertIn("requires SELinux enforcing", result.stderr)
                self.assertFalse(self.probe.exists())
                self.assertFalse(self.drop_in.exists())

    def test_wrapper_delegates_other_calls_but_blocks_before_relabel(self):
        self.require_action("arm")
        wrapper = self.probe / "bin/restorecon"
        subprocess.run([str(wrapper), "-R", str(self.root)], env=self.env, check=True, timeout=5)
        self.assertFalse((self.probe / "reached").exists(), "a linked host is not the checkpoint")
        before = self.restore_calls.read_text()
        self.swap()
        # Inspection still delegates, even while a move is in progress.
        subprocess.run([str(wrapper), "-nvR", str(self.root)], env=self.env, check=True,
                       capture_output=True, timeout=5)
        self.assertFalse((self.probe / "reached").exists())
        self.assertNotEqual(self.restore_calls.read_text(), before)
        before = self.restore_calls.read_text()

        process = subprocess.Popen([str(wrapper), "-R", str(self.root)], env=self.env)
        try:
            deadline = time.monotonic() + 5
            while not (self.probe / "reached").exists() and process.poll() is None and time.monotonic() < deadline:
                time.sleep(0.01)
            self.assertTrue((self.probe / "reached").exists())
            self.assertIsNone(process.poll(), "the wrapper must keep the daemon blocked")
            self.assertEqual(self.restore_calls.read_text(), before, "the real relabel must not run")
        finally:
            if process.poll() is None:
                process.kill()
            process.wait(timeout=5)

    def test_checkpoint_requires_binary_label_drift_and_retains_it(self):
        self.require_action("arm")
        self.swap()
        self.assertNotEqual(self.action("checkpoint").returncode, 0, "a rename alone is not proof of the checkpoint")
        (self.probe / "reached").touch()
        self.require_action("checkpoint")
        self.assertIn("unbounded-agent-blue", (self.probe / "labels-before.txt").read_text())
        self.env["LABEL_DRIFT"] = "0"
        result = self.action("checkpoint")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("did not expose copied daemon binaries", result.stderr)
        self.assertTrue(all(line.startswith("-nvR ") for line in self.restore_calls.read_text().splitlines()))

    def test_cleanup_kills_a_blocked_daemon_before_reloading_normal_units(self):
        self.require_action("arm")
        self.swap()
        self.env["DAEMON_PID"] = "123"
        self.calls.write_text("")
        self.require_action("disarm")
        calls = self.calls.read_text().splitlines()
        self.assertEqual(calls[1:], [
            "kill --signal=SIGKILL --kill-who=all unbounded-agent-daemon.service",
            "stop unbounded-agent-daemon.service", "daemon-reload",
        ])
        self.assertFalse(self.drop_in.exists())
        self.assertFalse(self.probe.exists())
        self.assertTrue((self.root / ".moving").exists())

    def test_resume_requires_removing_hook_and_verify_rejects_unrepaired_labels(self):
        self.require_action("arm")
        self.swap()
        (self.probe / "reached").touch()
        self.require_action("interrupt")
        self.assertIn("kill --signal=SIGKILL --kill-who=all", self.calls.read_text())
        self.assertNotEqual(self.action("resume").returncode, 0)
        self.require_action("disarm")
        self.assertFalse(self.probe.exists())
        self.assertFalse(self.drop_in.exists())
        self.assertTrue((self.root / ".moving").exists(), "cleanup must not complete the migration")
        self.require_action("resume")
        self.assertNotEqual(self.action("verify").returncode, 0, "unfinished migration must fail")
        (self.root / ".moving").unlink()
        result = self.action("verify")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("left labels that differ", result.stdout)
        self.env["LABEL_DRIFT"] = "0"
        self.require_action("verify")
        self.assertTrue(all(line.startswith("-nvR ") for line in self.restore_calls.read_text().splitlines()))


if __name__ == "__main__":
    unittest.main()
