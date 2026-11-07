# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0

import json
import subprocess
import unittest
import unittest.mock
from unittest.mock import patch

import e2e


class IsolationModeTests(unittest.TestCase):
    def test_default_is_nspawn_and_passes_no_flag(self):
        with patch.object(e2e, "ISOLATION_MODE", ""):
            self.assertEqual(e2e.isolation_mode(), "nspawn")
            args = e2e.node_config_bootstrap_args(e2e.load_node_config(""))
        self.assertNotIn("--isolation", args)

    def test_explicit_mode_adds_flag(self):
        with patch.object(e2e, "ISOLATION_MODE", "none"):
            args = e2e.node_config_bootstrap_args(e2e.load_node_config(""))
        self.assertEqual(args[args.index("--isolation") + 1], "none")

    def test_applied_config_without_isolation_is_nspawn(self):
        self.assertEqual(e2e.parse_applied_isolation(json.dumps({"MachineName": "kube1"})), "nspawn")
        self.assertEqual(e2e.parse_applied_isolation(json.dumps({"Isolation": ""})), "nspawn")
        self.assertEqual(e2e.parse_applied_isolation(json.dumps({"Isolation": "none"})), "none")

    def test_only_the_modes_of_this_harness_are_known(self):
        self.assertEqual(e2e.ISOLATION_MODES, ("nspawn", "none"))

    def test_the_strategy_follows_from_the_pair(self):
        """none has no slot, so every change between nspawn and none is a
        reprovision (pkg/agent/isolation/migrate.Plan). The harness does not
        choose it."""
        for pair in (("nspawn", "none"), ("none", "nspawn")):
            self.assertEqual(e2e.expected_change_strategy(*pair), "reprovision", pair)

    def test_an_in_place_pair_is_expected_in_place(self):
        """IN_PLACE_PAIRS is where a mode with slots adds its pairs."""
        with patch.object(e2e, "IN_PLACE_PAIRS", frozenset({("nspawn", "slotted")})):
            self.assertEqual(e2e.expected_change_strategy("nspawn", "slotted"), "inplace")
            self.assertEqual(e2e.expected_change_strategy("slotted", "nspawn"), "reprovision")

    def test_every_mode_has_a_change_step(self):
        for mode in e2e.ISOLATION_MODES:
            self.assertIn(f"change-to-{mode}", e2e.COMMANDS)
        self.assertNotIn("change-to-container", e2e.COMMANDS)


class SuiteTests(unittest.TestCase):
    def test_all_suite_steps_exist(self):
        e2e.validate_suites()

    def test_default_lifecycle_has_no_isolation_steps(self):
        self.assertFalse([s for s in e2e.SUITES["lifecycle"] if "isolation" in s or s.startswith("migrate")])

    def test_the_checks_of_the_removed_mode_change_suite_live_in_the_surviving_ones(self):
        """isolation-mode-change is gone. Its changes run in the matrix, the
        refused change in drift and rollback, and the same-mode declaration in
        rollback."""
        self.assertNotIn("isolation-mode-change", e2e.SUITES)
        for step in ("change-to-nspawn", "change-to-none"):
            self.assertIn(step, e2e.SUITES["isolation-matrix"])
        self.assertIn("validate-reprovision-refused", e2e.SUITES["isolation-drift"])
        self.assertIn("validate-reprovision-refused", e2e.SUITES["isolation-rollback"])
        self.assertIn("validate-same-mode-declaration", e2e.SUITES["isolation-rollback"])

    def test_the_drift_suite_makes_no_successful_change(self):
        """isolation-matrix asserts AppliedModeMatches after every change, so the
        drift suite only checks the declaration states and a refused change."""
        steps = e2e.SUITES["isolation-drift"]
        self.assertFalse([s for s in steps if s.startswith("change-to-")])
        for step in ("declare-isolation-mismatch", "reconcile-machine-isolation-spec",
                     "validate-reprovision-refused"):
            self.assertIn(step, steps)

    def test_the_uninstall_suite_is_replaced_by_reset_on_the_host(self):
        """The uninstall script is gone; the reset suites run `unbounded-agent
        reset` on the host and check nothing is left, also after a reprovision."""
        self.assertNotIn("isolation-uninstall", e2e.SUITES)
        self.assertNotIn("validate-isolation-uninstall", e2e.COMMANDS)
        self.assertEqual(e2e.SUITES["isolation-reset"][-1], "reset-agent-on-host")
        after = e2e.SUITES["isolation-reset-after-reprovision"]
        self.assertLess(after.index("change-to-none"), after.index("reset-agent-on-host"))

    def test_the_matrix_checks_no_handoff_is_left_after_every_change(self):
        steps = e2e.SUITES["isolation-matrix"]
        changes = [i for i, s in enumerate(steps) if s.startswith("change-to-")]
        for start, end in zip(changes, changes[1:] + [len(steps)]):
            self.assertIn("validate-no-reprovision-handoff", steps[start:end], steps[start])

    def test_no_suite_refers_to_a_mode_this_harness_does_not_know(self):
        for name, steps in e2e.SUITES.items():
            self.assertFalse([s for s in steps if "container" in s], name)

    def test_the_operation_driven_names_are_gone(self):
        for suite in ("isolation-migration", "isolation-container-reprovision"):
            self.assertNotIn(suite, e2e.SUITES)
        for step in e2e.COMMANDS:
            self.assertFalse(step.startswith("migrate-to-"), step)
            self.assertNotIn("unsupported-pair", step)
        for steps in e2e.SUITES.values():
            self.assertFalse([step for step in steps if step.startswith("migrate-to-")])


class PlanStepTests(unittest.TestCase):
    def test_drift_expectation_follows_the_controller(self):
        # An empty spec declares nothing, whatever runs.
        self.assertEqual(e2e.isolation_drift_expectation("", "nspawn"), ("False", "NoModeDeclared"))
        self.assertEqual(e2e.isolation_drift_expectation("", "none"), ("False", "NoModeDeclared"))
        self.assertEqual(e2e.isolation_drift_expectation("nspawn", "nspawn"), ("False", "AppliedModeMatches"))
        self.assertEqual(e2e.isolation_drift_expectation("none", "nspawn"), ("True", "AppliedModeDiffers"))

    def test_drift_expectation_reports_the_change_for_the_declared_mode(self):
        failed = {"targetMode": "none", "phase": "Failed"}
        running = {"targetMode": "none", "phase": "InProgress"}
        done = {"targetMode": "none", "phase": "Succeeded"}
        self.assertEqual(e2e.isolation_drift_expectation("none", "nspawn", failed), ("True", "ChangeFailed"))
        self.assertEqual(e2e.isolation_drift_expectation("none", "nspawn", running), ("True", "ChangeInProgress"))
        # A change for another mode, or one that succeeded, does not describe this drift.
        self.assertEqual(e2e.isolation_drift_expectation("nspawn", "none", failed), ("True", "AppliedModeDiffers"))
        self.assertEqual(e2e.isolation_drift_expectation("none", "nspawn", done), ("True", "AppliedModeDiffers"))
        # Once the modes agree the old failure no longer matters.
        self.assertEqual(e2e.isolation_drift_expectation("none", "none", failed), ("False", "AppliedModeMatches"))

    def test_a_refused_change_is_reported_as_change_refused(self):
        for reason in ("ReprovisionNotAllowed", "UnsupportedMode"):
            refused = {"targetMode": "none", "phase": "Failed", "reason": reason}
            self.assertEqual(e2e.isolation_drift_expectation("none", "nspawn", refused), ("True", "ChangeRefused"))
        other = {"targetMode": "none", "phase": "Failed", "reason": "SomethingElse"}
        self.assertEqual(e2e.isolation_drift_expectation("none", "nspawn", other), ("True", "ChangeFailed"))

    def test_an_in_progress_change_turns_stale_after_two_hours(self):
        running = {"targetMode": "none", "phase": "InProgress", "lastTransitionTime": "2026-01-01T00:00:00Z"}
        start = 1767225600.0  # 2026-01-01T00:00:00Z
        self.assertEqual(e2e.CHANGE_STALE_AFTER_SECS, 2 * 3600)
        self.assertEqual(e2e.isolation_drift_expectation("none", "nspawn", running, now=start + 7199),
                         ("True", "ChangeInProgress"))
        self.assertEqual(e2e.isolation_drift_expectation("none", "nspawn", running, now=start + 7200),
                         ("True", "ChangeStale"))
        # Without a usable timestamp the change cannot be called stale.
        undated = dict(running, lastTransitionTime="")
        self.assertEqual(e2e.isolation_drift_expectation("none", "nspawn", undated, now=start + 99999),
                         ("True", "ChangeInProgress"))

    def test_preflight_report_skips_text_before_the_json(self):
        report = e2e.parse_preflight_report('warning: x\n{"status": "ok", "checks": [{"name": "none-backend"}]}')
        self.assertEqual(report["checks"][0]["name"], "none-backend")
        with self.assertRaises(SystemExit):
            e2e.parse_preflight_report("no json here")

    def test_preflight_report_check_accepts_the_modes_own_check(self):
        report = {"status": "ok", "checks": [{"name": "none-backend", "severity": "ok"}]}
        e2e.check_isolation_preflight_report("none", report)

    def test_preflight_report_check_rejects_a_missing_or_foreign_check(self):
        with self.assertRaises(SystemExit):
            e2e.check_isolation_preflight_report("none", {"checks": [{"name": "nspawn-runtime"}]})
        with self.assertRaises(SystemExit):
            e2e.check_isolation_preflight_report(
                "none", {"checks": [{"name": "none-backend"}, {"name": "nspawn-runtime"}]})
        with self.assertRaises(SystemExit):
            e2e.check_isolation_preflight_report(
                "none", {"checks": [{"name": "none-backend", "severity": "error", "message": "x"}]})

    def test_bootstrap_report_is_only_asserted_for_the_bootstrap_mode(self):
        import pathlib
        import tempfile

        (e2e.REPO_ROOT / "tmp").mkdir(exist_ok=True)

        with tempfile.TemporaryDirectory(dir=str(e2e.REPO_ROOT / "tmp")) as tmp:
            saved = pathlib.Path(tmp) / "unbounded-agent-preflight.json"
            saved.write_text(json.dumps({"checks": [{"name": "nspawn-runtime", "severity": "ok"}]}))
            with patch.object(e2e, "VM_DIR", pathlib.Path(tmp)):
                with patch.dict("os.environ", {"ISOLATION_MODE": ""}):
                    # An nspawn bootstrap says nothing about a node now in none mode.
                    with patch.object(e2e, "warn") as warned:
                        skipped = e2e.assert_bootstrap_preflight_mode_check("none")
                    self.assertIn("bootstrap ran in nspawn", skipped)
                    self.assertIn("SKIPPED", warned.call_args.args[0])
                    # The bootstrap mode itself is asserted, so nothing is skipped.
                    self.assertEqual(e2e.assert_bootstrap_preflight_mode_check("nspawn"), "")
                with patch.dict("os.environ", {"ISOLATION_MODE": "none"}):
                    with self.assertRaises(SystemExit):
                        e2e.assert_bootstrap_preflight_mode_check("none")

    def test_a_missing_bootstrap_report_is_reported_as_skipped(self):
        import pathlib
        import tempfile

        (e2e.REPO_ROOT / "tmp").mkdir(exist_ok=True)

        with tempfile.TemporaryDirectory(dir=str(e2e.REPO_ROOT / "tmp")) as tmp:
            with patch.object(e2e, "VM_DIR", pathlib.Path(tmp)), patch.object(e2e, "warn") as warned:
                reason = e2e.assert_bootstrap_preflight_mode_check("none")
        self.assertEqual(reason, "no bootstrap preflight report was saved")
        self.assertIn("SKIPPED", warned.call_args.args[0])

    def test_daemon_unit_check_has_no_machine_override_outside_nspawn(self):
        seen = []

        def fake_ssh(command):
            seen.append(command)
            return ("ExecStart=/opt/b/unbounded-agent-current daemon\n"
                    "ExecStart=/opt/b/unbounded-agent-daemon-recovery.sh\n")

        with patch.object(e2e, "ISOLATION_MODE", "none"), patch.object(e2e, "ssh_capture", fake_ssh):
            e2e._daemon_unit_runs("/opt/b")
        self.assertNotIn("override.conf", seen[0])

        with patch.object(e2e, "ISOLATION_MODE", ""), patch.object(e2e, "ssh_capture", fake_ssh):
            with self.assertRaises(SystemExit):
                e2e._daemon_unit_runs("/opt/b")
        self.assertIn("override.conf", seen[1])

    def test_every_mode_has_a_preflight_check(self):
        self.assertEqual(set(e2e.PREFLIGHT_MODE_CHECKS), set(e2e.ISOLATION_MODES))

    def test_new_steps_and_suites_exist(self):
        for step in ("validate-isolation-status", "reconcile-machine-isolation-spec", "validate-isolation-preflight",
                     "change-to-none", "change-to-nspawn", "declare-isolation-mismatch",
                     "validate-same-mode-declaration", "validate-reprovision-refused",
                     "validate-no-reprovision-handoff", "reset-agent-on-host", "collect-isolation-evidence"):
            self.assertIn(step, e2e.COMMANDS)
        for suite in ("isolation-operations", "isolation-reset", "isolation-reset-after-reprovision",
                      "isolation-matrix", "isolation-drift", "isolation-rollback"):
            self.assertIn(suite, e2e.SUITES)

    def test_existing_suites_are_unchanged(self):
        self.assertEqual(e2e.SUITES["lifecycle"][0], "run-agent")
        self.assertNotIn("validate-isolation-status", e2e.SUITES["lifecycle"])

    def test_nspawn_only_checks_stay_out_of_the_mode_neutral_suites(self):
        for suite in ("isolation-operations", "isolation-reset", "isolation-reset-after-reprovision",
                      "isolation-matrix", "isolation-drift", "isolation-rollback"):
            self.assertNotIn("validate-host-nspawn-distro", e2e.SUITES[suite])
            self.assertNotIn("validate-node-reboot-operation", e2e.SUITES[suite])

    def test_the_matrix_keeps_the_checks_of_the_removed_round_trip_suite(self):
        """isolation-roundtrip is gone: its post-change checks (soft reboot, host
        reboot, workload, and the none preflight) run in the matrix at the stop
        reached by reprovision to none and at the final nspawn stop."""
        self.assertNotIn("isolation-roundtrip", e2e.SUITES)
        steps = e2e.SUITES["isolation-matrix"]
        last_none = max(i for i, s in enumerate(steps) if s == "change-to-none")
        last_nspawn = max(i for i, s in enumerate(steps) if s == "change-to-nspawn")
        after_none = steps[last_none + 1:last_nspawn]
        after_nspawn = steps[last_nspawn + 1:]
        for step in ("validate-isolation-status", "validate-isolation-exec-logs", "validate-isolation-soft-reboot",
                     "validate-isolation-preflight", "validate-host-reboot", "validate-workload"):
            self.assertIn(step, after_none, step)
        for step in ("validate-isolation-status", "validate-isolation-exec-logs", "validate-isolation-soft-reboot",
                     "validate-host-reboot", "validate-workload"):
            self.assertIn(step, after_nspawn, step)

    def test_suites_that_change_the_mode_start_in_the_default_mode(self):
        for suite in ("isolation-matrix", "isolation-drift", "isolation-rollback",
                      "isolation-reset-after-reprovision"):
            self.assertEqual(e2e.SUITES[suite][0], "run-agent")


class NodeStackDiagnosticsTests(unittest.TestCase):
    def _run(self, mode, ssh):
        with patch.object(e2e, "ISOLATION_MODE", mode), patch.object(e2e, "ssh_capture_quiet", side_effect=ssh), \
                patch.object(e2e.subprocess, "run"):
            e2e.dump_node_stack_diagnostics()

    def test_the_reprovision_handoff_is_shown(self):
        commands = []

        def ssh(command):
            commands.append(command)
            return unittest.mock.Mock(stdout="out", stderr="", returncode=0)

        self._run("none", ssh)
        joined = "\n".join(commands)
        self.assertIn(f"{e2e.REPROVISION_DIR}/journal.json", joined)
        self.assertIn(f"systemctl status {e2e.REPROVISION_UNIT}", joined)

    def test_no_mode_queries_runc(self):
        for mode in e2e.ISOLATION_MODES:
            commands = []

            def ssh(command):
                commands.append(command)
                return unittest.mock.Mock(stdout="", stderr="", returncode=1)

            self._run(mode, ssh)
            self.assertTrue(commands)
            self.assertNotIn("runc", "\n".join(commands), mode)

    def test_a_failing_query_never_raises(self):
        def ssh(_command):
            raise OSError("ssh is gone")

        self._run("none", ssh)


class MachineShellTests(unittest.TestCase):
    def test_runs_through_systemd_run_so_status_and_output_come_back(self):
        cmd = e2e.machine_shell_command("kube1", "test -d /x && echo 'ok'")
        self.assertTrue(cmd.startswith("sudo systemd-run -M kube1 --wait --pipe --quiet /bin/sh -lc "))
        self.assertNotIn("machinectl shell", cmd)
        self.assertIn("'test -d /x && echo '\"'\"'ok'\"'\"''", cmd)

    def test_quiet_variant_uses_the_same_command(self):
        with patch.object(e2e, "ssh_capture_quiet") as capture:
            e2e.machine_shell_quiet("kube2", "true")
        capture.assert_called_once_with(e2e.machine_shell_command("kube2", "true"))


class BPFFSSentinelTests(unittest.TestCase):
    def test_script_needs_no_bpftool_and_stops_on_error(self):
        script = e2e.bpffs_sentinel_script()
        self.assertNotIn("bpftool", script)
        self.assertNotIn("\n", script)
        self.assertEqual(script.count(" && "), 3)
        self.assertIn(f"mkdir /sys/fs/bpf/{e2e.BPFFS_SENTINEL}", script)

    def test_nspawn_sentinel_is_checked_on_the_host(self):
        ok = unittest.mock.Mock(returncode=0)
        with patch.object(e2e, "machine_shell") as shell, \
                patch.object(e2e, "ssh_capture_quiet", return_value=ok) as host:
            e2e.create_bpffs_sentinel("kube1")
        shell.assert_called_once()
        host.assert_called_once_with(f"sudo test -d /run/bpffs/kube1/{e2e.BPFFS_SENTINEL}")

    def test_a_sentinel_missing_on_the_host_fails(self):
        missing = unittest.mock.Mock(returncode=1)
        with patch.object(e2e, "machine_shell"), \
                patch.object(e2e, "ssh_capture_quiet", return_value=missing), \
                self.assertRaises(SystemExit):
            e2e.create_bpffs_sentinel("kube1")

    def test_a_sentinel_left_in_the_new_machine_fails(self):
        visible = unittest.mock.Mock(returncode=1)
        with patch.object(e2e, "ssh_cmd"), \
                patch.object(e2e, "ssh_capture_quiet", return_value=visible), \
                self.assertRaises(SystemExit):
            e2e.assert_bpffs_sentinel_absent("kube2")


class SoftRebootTests(unittest.TestCase):
    def _run(self, mode, markers):
        """Run the soft reboot check with the kubelet start marker answering from
        markers, one value per read."""
        answers = iter(markers)
        quiet = unittest.mock.Mock(side_effect=lambda command: unittest.mock.Mock(
            returncode=0, stdout=next(answers), stderr=""))
        clock = iter(range(0, 100000, 40))
        with patch.object(e2e, "ISOLATION_MODE", mode), patch.object(e2e, "ssh_capture_quiet", quiet), \
                patch.object(e2e, "ssh_capture", return_value="boot-1\n"), patch.object(e2e, "ssh_cmd"), \
                patch.object(e2e, "run_quiet"), patch.object(e2e, "create_machine_operation"), \
                patch.object(e2e, "wait_for_machine_operation_complete"), patch.object(e2e, "wait_for_node_ready"), \
                patch.object(e2e, "applied_isolation", return_value=mode), patch.object(e2e.time, "sleep"), \
                patch.object(e2e.time, "monotonic", side_effect=lambda: next(clock)):
            e2e.validate_isolation_soft_reboot()

    def test_a_restarted_kubelet_passes_in_every_mode(self):
        for mode in e2e.ISOLATION_MODES:
            with self.subTest(mode=mode):
                self._run(mode, ["100\n", "250\n"])

    def test_a_node_reboot_that_did_nothing_fails_even_though_the_node_is_ready(self):
        with self.assertRaises(SystemExit):
            self._run("nspawn", ["100\n"] * 8)

    def test_an_unreadable_marker_before_the_reboot_fails(self):
        with self.assertRaises(SystemExit):
            self._run("nspawn", [""])

    def test_the_marker_reads_the_kubelet_unit_of_the_mode(self):
        for mode, unit in (("none", "unbounded-kubelet.service"), ("nspawn", "kubelet.service")):
            seen = []

            def ssh(command, seen=seen):
                seen.append(command)
                return unittest.mock.Mock(returncode=0, stdout="7\n", stderr="")

            with self.subTest(mode=mode), patch.object(e2e, "ISOLATION_MODE", mode), \
                    patch.object(e2e, "ssh_capture_quiet", ssh):
                self.assertEqual(e2e.node_stack_start_marker(), "7")
            self.assertIn(f"exec -- systemctl show -p ActiveEnterTimestampMonotonic --value {unit}", seen[0])


class DiagnosticsRedactionTests(unittest.TestCase):
    def test_tokens_and_kubeconfig_credentials_are_removed(self):
        text = ("kubelet: token abcdef.0123456789abcdef\n"
                "users:\n- user:\n    token: s3cr3t\n    client-key-data: QUJD\n"
                '{"BootstrapToken": "zzzzzz.yyyyyyyyyyyyyyyy"}\nplain line\n')
        out = e2e.redact_diagnostics(text)
        for secret in ("abcdef.0123456789abcdef", "s3cr3t", "QUJD", "zzzzzz.yyyyyyyyyyyyyyyy"):
            self.assertNotIn(secret, out)
        self.assertIn("plain line", out)

    def test_the_dump_prints_redacted_output(self):
        import contextlib
        import io

        commands = []

        def ssh(command):
            commands.append(command)
            return unittest.mock.Mock(stdout="token abcdef.0123456789abcdef", stderr="", returncode=0)

        out = io.StringIO()
        with patch.object(e2e, "ISOLATION_MODE", "none"), patch.object(e2e, "ssh_capture_quiet", ssh), \
                patch.object(e2e.subprocess, "run"), contextlib.redirect_stdout(out):
            e2e.dump_node_stack_diagnostics()
        self.assertNotIn("abcdef.0123456789abcdef", out.getvalue())
        self.assertIn("<redacted-token>", out.getvalue())
        self.assertTrue(commands)


def view(spec="", observed="nspawn", drift=None, change=None):
    return {"spec": spec, "observed": observed, "drift": drift, "change": change or {}}


def drift(status, reason, message=""):
    return {"status": status, "reason": reason, "message": message}


class WaitForIsolationChangeTests(unittest.TestCase):
    def _wait(self, views, target="none", phase="Succeeded", previous=None, timeout_secs=None):
        seen = iter(views)
        clock = iter(range(0, 100000, 100))
        with patch.object(e2e, "isolation_change_view", side_effect=lambda: next(seen)), \
                patch.object(e2e.time, "sleep"), patch.object(e2e.time, "monotonic", side_effect=lambda: next(clock)), \
                patch.object(e2e, "dump_node_stack_diagnostics") as dumped:
            try:
                return e2e.wait_for_isolation_change(target, phase, previous or {}, timeout_secs), dumped
            except SystemExit:
                raise

    def test_a_new_terminal_change_for_the_target_is_returned(self):
        done = {"targetMode": "none", "phase": "Succeeded", "lastTransitionTime": "t2"}
        got, _ = self._wait([{}, {"targetMode": "none", "phase": "InProgress", "lastTransitionTime": "t1"}, done])
        self.assertEqual(got, done)

    def test_a_stale_result_of_the_same_target_is_not_taken_for_the_new_one(self):
        old = {"targetMode": "none", "phase": "Succeeded", "lastTransitionTime": "t1"}
        new = {"targetMode": "none", "phase": "Succeeded", "lastTransitionTime": "t2"}
        got, _ = self._wait([old, old, new], previous=old)
        self.assertEqual(got["lastTransitionTime"], "t2")

    def test_a_change_for_another_mode_is_ignored(self):
        other = {"targetMode": "nspawn", "phase": "Succeeded", "lastTransitionTime": "t9"}
        mine = {"targetMode": "none", "phase": "Succeeded", "lastTransitionTime": "t2"}
        got, _ = self._wait([other, mine])
        self.assertEqual(got, mine)

    def test_the_wrong_terminal_phase_fails(self):
        failed = {"targetMode": "none", "phase": "Failed", "lastTransitionTime": "t2", "message": "boom"}
        with self.assertRaises(SystemExit):
            self._wait([failed], phase="Succeeded")
        succeeded = {"targetMode": "none", "phase": "Succeeded", "lastTransitionTime": "t2"}
        with self.assertRaises(SystemExit):
            self._wait([succeeded], phase="Failed")

    def test_no_result_in_time_fails_with_diagnostics(self):
        seen = iter([{}] * 50)
        clock = iter(range(0, 100000, 400))
        with patch.object(e2e, "isolation_change_view", side_effect=lambda: next(seen)), patch.object(e2e.time, "sleep"), \
                patch.object(e2e.time, "monotonic", side_effect=lambda: next(clock)), \
                patch.object(e2e, "dump_node_stack_diagnostics") as dumped:
            with self.assertRaises(SystemExit):
                e2e.wait_for_isolation_change("none", "Succeeded", {}, timeout_secs=600)
        dumped.assert_called_once()


class ChangePendingTests(unittest.TestCase):
    def _pending(self, views, marker_now="kube1", applied="nspawn", previous=None, target="none"):
        seen = iter(views)
        clock = iter(range(0, 100000, 100))
        with patch.object(e2e, "machine_isolation_view", side_effect=lambda: next(seen)), \
                patch.object(e2e, "isolation_node_marker", return_value=marker_now), \
                patch.object(e2e, "applied_isolation", return_value=applied), \
                patch.object(e2e.time, "sleep"), patch.object(e2e.time, "monotonic", side_effect=lambda: next(clock)):
            e2e.validate_change_pending("nspawn", target, "kube1", previous or {})

    def test_a_declared_mode_only_shows_a_drift(self):
        self._pending([view("", "nspawn", drift("False", "NoModeDeclared")),
                       view("none", "nspawn", drift("True", "AppliedModeDiffers"))])

    def test_an_old_failure_for_the_same_target_may_still_show_as_change_failed(self):
        previous = {"targetMode": "none", "phase": "Failed"}
        self._pending([view("none", "nspawn", drift("True", "ChangeFailed"))], previous=previous)

    def test_an_old_refusal_for_the_same_target_may_still_show_as_change_refused(self):
        previous = {"targetMode": "none", "phase": "Failed", "reason": "ReprovisionNotAllowed"}
        self._pending([view("none", "nspawn", drift("True", "ChangeRefused"))], previous=previous)
        with self.assertRaises(SystemExit):
            self._pending([view("none", "nspawn", drift("True", "ChangeFailed"))] * 5, previous=previous)

    def test_change_failed_is_not_accepted_without_an_old_failure(self):
        with self.assertRaises(SystemExit):
            self._pending([view("none", "nspawn", drift("True", "ChangeFailed"))] * 5)

    def test_a_node_that_moved_on_declaration_fails(self):
        with self.assertRaises(SystemExit):
            self._pending([view("none", "nspawn", drift("True", "AppliedModeDiffers"))], marker_now="kube2")
        with self.assertRaises(SystemExit):
            self._pending([view("none", "nspawn", drift("True", "AppliedModeDiffers"))], applied="none")

    def test_a_drift_that_never_appears_fails(self):
        with self.assertRaises(SystemExit):
            self._pending([view("none", "nspawn", drift("False", "AppliedModeMatches"))] * 5)


class ChangeIsolationModeTests(unittest.TestCase):
    def _run(self, source="nspawn", target="none", strategy="Reprovision", after=None, change_phase="Succeeded"):
        calls = []
        done = {"targetMode": target, "phase": change_phase, "strategy": strategy, "lastTransitionTime": "t2"}
        final = after or view(target, target, drift("False", "AppliedModeMatches"))

        def record(name, result=None):
            def run(*args, **kwargs):
                calls.append(name)
                return result
            return run

        patches = [
            patch.object(e2e, "applied_isolation", return_value=source),
            patch.object(e2e, "isolation_change_view", return_value={}),
            patch.object(e2e, "isolation_node_marker", return_value="m"),
            patch.object(e2e, "declare_isolation_spec", side_effect=record("declare")),
            patch.object(e2e, "validate_change_pending", side_effect=record("pending")),
            patch.object(e2e, "set_allow_reprovision",
                         side_effect=lambda allowed: calls.append("allow" if allowed else "disallow")),
            patch.object(e2e, "kubectl", side_effect=record("delete-node")),
            patch.object(e2e, "wait_for_isolation_change", side_effect=lambda *a, **k: (calls.append("wait"), done)[1]),
            patch.object(e2e, "set_effective_isolation", side_effect=record("effective")),
            patch.object(e2e, "wait_for_node", side_effect=record("node")),
            patch.object(e2e, "validate_isolation_mode", side_effect=record("mode")),
            patch.object(e2e, "validate_isolation_status", side_effect=record("status")),
            patch.object(e2e, "machine_isolation_view", return_value=final),
            patch.object(e2e, "kubectl_capture", return_value="{}"),
            patch.object(e2e, "_assert_expected_node_config", side_effect=record("config")),
            patch.object(e2e, "validate_workload", side_effect=record("workload")),
        ]
        for item in patches:
            item.start()
        try:
            e2e.change_isolation_mode(None, target)
        finally:
            for item in reversed(patches):
                item.stop()
        return calls

    def test_declare_then_check_then_allow_then_delete_then_wait_in_that_order(self):
        """A reprovision needs the allow-reprovision opt-in before the repave
        the Node deletion triggers; it is withdrawn once the change is done."""
        calls = self._run()
        self.assertEqual(calls[:7], ["declare", "pending", "allow", "delete-node", "wait", "disallow", "effective"])
        self.assertEqual(calls[7:], ["node", "mode", "status", "config", "workload"])

    def test_both_directions_are_reprovisions(self):
        self._run(source="none", target="nspawn", after=view("nspawn", "nspawn", drift("False", "AppliedModeMatches")))

    def test_a_reprovision_pair_must_record_the_reprovision_strategy(self):
        with self.assertRaises(SystemExit):
            self._run(source="nspawn", target="none", strategy="InPlace")

    def test_an_in_place_pair_needs_no_opt_in_and_must_not_record_a_reprovision(self):
        slotted = view("slotted", "slotted", drift("False", "AppliedModeMatches"))
        with patch.object(e2e, "IN_PLACE_PAIRS", frozenset({("nspawn", "slotted")})):
            calls = self._run(target="slotted", strategy="InPlace", after=slotted)
            self.assertNotIn("allow", calls)
            with self.assertRaises(SystemExit):
                self._run(target="slotted", strategy="Reprovision", after=slotted)

    def test_a_drift_that_survives_the_change_fails(self):
        with self.assertRaises(SystemExit):
            self._run(after=view("none", "none", drift("True", "AppliedModeDiffers")))
        with self.assertRaises(SystemExit):
            self._run(after=view("none", "nspawn", drift("False", "AppliedModeMatches")))


class SameModeDeclarationTests(unittest.TestCase):
    def _run(self, marker_after="kube1", applied_after="nspawn", change_after=None):
        markers = iter(["kube1", marker_after])
        applied = iter(["nspawn", applied_after])
        changes = iter([{}, change_after if change_after is not None else {}, change_after if change_after is not None else {}])
        with patch.object(e2e, "applied_isolation", side_effect=lambda: next(applied)), \
                patch.object(e2e, "isolation_node_marker", side_effect=lambda: next(markers)), \
                patch.object(e2e, "isolation_change_view", side_effect=lambda: next(changes)), \
                patch.object(e2e, "declare_isolation_spec") as declared, patch.object(e2e, "kubectl") as kubectl, \
                patch.object(e2e, "validate_isolation_status"), patch.object(e2e, "wait_for_node"), \
                patch.object(e2e, "restore_deleted_node") as restored, patch.object(e2e.time, "sleep"), \
                patch.object(e2e, "validate_isolation_mode"):
            e2e.validate_same_mode_declaration()
        self.restored = restored
        return declared, kubectl

    def test_declaring_the_applied_mode_moves_nothing_and_records_nothing(self):
        declared, kubectl = self._run()
        declared.assert_called_once_with("nspawn")
        self.assertIn("node", kubectl.call_args.args[0])
        self.restored.assert_called_once()

    def test_a_moved_stack_fails(self):
        with self.assertRaises(SystemExit):
            self._run(marker_after="kube2")
        with self.assertRaises(SystemExit):
            self._run(applied_after="none")

    def test_a_recorded_change_fails(self):
        with self.assertRaises(SystemExit):
            self._run(change_after={"targetMode": "nspawn", "phase": "Succeeded"})


class NoAutomaticRetryTests(unittest.TestCase):
    CHANGE = {"targetMode": "nspawn", "phase": "Failed", "lastTransitionTime": "t1"}

    def _soak(self, views, soak_secs=30):
        seen = iter(views)
        clock = iter(range(0, 100000, 10))
        with patch.object(e2e, "isolation_change_view", side_effect=lambda: next(seen)), patch.object(e2e.time, "sleep"), \
                patch.object(e2e.time, "monotonic", side_effect=lambda: next(clock)):
            e2e.assert_no_automatic_retry(self.CHANGE, soak_secs=soak_secs)

    def test_an_untouched_failure_passes(self):
        self._soak([self.CHANGE] * 10)

    def test_a_new_attempt_fails(self):
        retried = dict(self.CHANGE, lastTransitionTime="t2", phase="InProgress")
        with self.assertRaises(SystemExit):
            self._soak([self.CHANGE, retried])

    def test_a_failure_that_turned_into_something_else_fails(self):
        with self.assertRaises(SystemExit):
            self._soak([dict(self.CHANGE, phase="Succeeded")])


class ChangeFailedConditionTests(unittest.TestCase):
    CHANGE = {"targetMode": "nspawn", "phase": "Failed", "message": "bootstrap nspawn: x (attempt 5 of 5)"}
    REFUSED = {"targetMode": "nspawn", "phase": "Failed", "reason": "ReprovisionNotAllowed",
               "message": "the change from none to nspawn needs a reprovision"}

    def _check(self, views, change=None):
        seen = iter(views)
        clock = iter(range(0, 100000, 100))
        with patch.object(e2e, "machine_isolation_view", side_effect=lambda: next(seen)), patch.object(e2e.time, "sleep"), \
                patch.object(e2e.time, "monotonic", side_effect=lambda: next(clock)):
            e2e.validate_change_failed_condition("nspawn", change or self.CHANGE)

    def test_the_condition_carrying_the_failure_passes(self):
        self._check([view("nspawn", "none", drift("True", "AppliedModeDiffers")),
                     view("nspawn", "none", drift("True", "ChangeFailed", "bootstrap nspawn: x"))])

    def test_a_condition_that_never_reports_the_failure_fails(self):
        with self.assertRaises(SystemExit):
            self._check([view("nspawn", "none", drift("True", "AppliedModeDiffers"))] * 5)

    def test_a_condition_without_the_failure_text_fails(self):
        with self.assertRaises(SystemExit):
            self._check([view("nspawn", "none", drift("True", "ChangeFailed", "something else"))])

    def test_a_refusal_must_show_as_change_refused(self):
        message = "the change to isolation \"nspawn\" needs a reprovision: " + self.REFUSED["message"]
        self._check([view("nspawn", "none", drift("True", "ChangeRefused", message))], change=self.REFUSED)
        with self.assertRaises(SystemExit):
            self._check([view("nspawn", "none", drift("True", "ChangeFailed", message))] * 5, change=self.REFUSED)


class DeclareHelperTests(unittest.TestCase):
    def test_the_spec_is_patched_with_only_the_isolation_field(self):
        with patch.object(e2e, "kubectl") as kubectl:
            e2e.declare_isolation_spec("none")
        args = kubectl.call_args.args[0]
        self.assertEqual(args[:2], ["patch", "machine"])
        self.assertEqual(json.loads(args[-1]), {"spec": {"isolation": "none"}})

    def test_the_change_view_is_empty_before_any_change(self):
        with patch.object(e2e, "kubectl_capture", return_value=json.dumps({"status": {"isolation": "nspawn"}})):
            self.assertEqual(e2e.isolation_change_view(), {})
        change = {"targetMode": "none", "phase": "Succeeded"}
        with patch.object(e2e, "kubectl_capture", return_value=json.dumps({"status": {"isolationChange": change}})):
            self.assertEqual(e2e.isolation_change_view(), change)

    def test_the_machine_view_carries_the_change(self):
        machine = {"spec": {"isolation": "none"},
                   "status": {"isolation": "nspawn", "isolationChange": {"phase": "Failed"},
                              "conditions": [{"type": "IsolationDrift", "status": "True", "reason": "ChangeFailed"}]}}
        with patch.object(e2e, "kubectl_capture", return_value=json.dumps(machine)):
            got = e2e.machine_isolation_view()
        self.assertEqual((got["spec"], got["observed"], got["change"]["phase"], got["drift"]["reason"]),
                         ("none", "nspawn", "Failed", "ChangeFailed"))


class DeclareMismatchTests(unittest.TestCase):
    def test_the_drift_suite_withdraws_every_mismatch_it_declares(self):
        steps = e2e.SUITES["isolation-drift"]
        self.assertIn("declare-isolation-mismatch", e2e.COMMANDS)

        for index, step in enumerate(steps):
            if step == "declare-isolation-mismatch":
                self.assertEqual(steps[index + 1], "reconcile-machine-isolation-spec")

    def test_the_drift_suite_covers_a_refused_change(self):
        self.assertIn("validate-reprovision-refused", e2e.SUITES["isolation-drift"])

    def test_the_mismatch_is_a_mode_other_than_the_applied_one(self):
        for applied, other in (("none", "nspawn"), ("nspawn", "none")):
            declared = []
            seen = view(other, applied, drift("True", "AppliedModeDiffers"))
            with patch.object(e2e, "ISOLATION_MODE", applied), \
                    patch.object(e2e, "declare_isolation_spec", side_effect=declared.append), \
                    patch.object(e2e, "isolation_node_marker", return_value="m"), \
                    patch.object(e2e, "validate_isolation_status"), patch.object(e2e, "machine_isolation_view", return_value=seen):
                e2e.declare_isolation_mismatch()
            self.assertEqual(declared, [other], applied)

    def test_a_drift_that_does_not_appear_fails(self):
        seen = view("nspawn", "none", drift("False", "AppliedModeMatches"))
        with patch.object(e2e, "ISOLATION_MODE", "none"), patch.object(e2e, "declare_isolation_spec"), \
                patch.object(e2e, "isolation_node_marker", return_value="m"), patch.object(e2e, "validate_isolation_status"), \
                patch.object(e2e, "machine_isolation_view", return_value=seen):
            with self.assertRaises(SystemExit):
                e2e.declare_isolation_mismatch()

    def test_a_mismatch_that_moved_the_node_fails(self):
        seen = view("nspawn", "none", drift("True", "AppliedModeDiffers"))
        markers = iter(["a", "b"])
        with patch.object(e2e, "ISOLATION_MODE", "none"), patch.object(e2e, "declare_isolation_spec"), \
                patch.object(e2e, "isolation_node_marker", side_effect=lambda: next(markers)), \
                patch.object(e2e, "validate_isolation_status"), patch.object(e2e, "machine_isolation_view", return_value=seen):
            with self.assertRaises(SystemExit):
                e2e.declare_isolation_mismatch()

    def test_reconcile_declares_the_applied_mode_and_requires_the_drift_to_clear(self):
        seen = view("none", "none", drift("False", "AppliedModeMatches"))
        with patch.object(e2e, "ISOLATION_MODE", "none"), patch.object(e2e, "declare_isolation_spec") as declared, \
                patch.object(e2e, "validate_isolation_status"), patch.object(e2e, "machine_isolation_view", return_value=seen):
            e2e.reconcile_machine_isolation_spec()
        declared.assert_called_once_with("none")
        stuck = view("none", "nspawn", drift("True", "AppliedModeDiffers"))
        with patch.object(e2e, "ISOLATION_MODE", "none"), patch.object(e2e, "declare_isolation_spec"), \
                patch.object(e2e, "validate_isolation_status"), patch.object(e2e, "machine_isolation_view", return_value=stuck):
            with self.assertRaises(SystemExit):
                e2e.reconcile_machine_isolation_spec()


class AllowReprovisionTests(unittest.TestCase):
    def test_the_opt_in_is_set_and_removed_with_the_annotation(self):
        with patch.object(e2e, "kubectl") as kubectl:
            e2e.set_allow_reprovision(True)
            e2e.set_allow_reprovision(False)
        set_args, unset_args = (c.args[0] for c in kubectl.call_args_list)
        self.assertEqual(set_args[:3], ["annotate", "machine", e2e.AGENT_MACHINE_NAME])
        self.assertIn("unbounded-cloud.io/allow-reprovision=true", set_args)
        self.assertIn("unbounded-cloud.io/allow-reprovision-", unset_args)


class ReprovisionRefusedTests(unittest.TestCase):
    def _refuse(self, source="nspawn", reason="ReprovisionNotAllowed", strategy="Reprovision", applied_after=None):
        change = {"targetMode": e2e.other_isolation_mode(source), "phase": "Failed", "reason": reason,
                  "strategy": strategy, "message": "needs a reprovision", "lastTransitionTime": "t2"}
        applied = iter([source, applied_after or source, applied_after or source])
        calls = []
        patches = [
            patch.object(e2e, "applied_isolation", side_effect=lambda: next(applied)),
            patch.object(e2e, "set_allow_reprovision", side_effect=lambda allowed: calls.append(("allow", allowed))),
            patch.object(e2e, "isolation_change_view", return_value={}),
            patch.object(e2e, "isolation_node_marker", return_value="m"),
            patch.object(e2e, "declare_isolation_spec", side_effect=lambda mode: calls.append(("declare", mode))),
            patch.object(e2e, "validate_change_pending"),
            patch.object(e2e, "kubectl", side_effect=lambda args: calls.append(("kubectl", args[0], args[1]))),
            patch.object(e2e, "wait_for_isolation_change", return_value=change),
            patch.object(e2e, "wait_for_node", side_effect=lambda: calls.append(("node",))),
            patch.object(e2e, "validate_isolation_mode"),
            patch.object(e2e, "validate_change_failed_condition",
                         side_effect=lambda target, ch: calls.append(("condition", target))),
            patch.object(e2e, "assert_no_automatic_retry", side_effect=lambda ch: calls.append(("soak",))),
            patch.object(e2e, "validate_no_reprovision_handoff", side_effect=lambda: calls.append(("handoff",))),
            patch.object(e2e, "validate_isolation_status"),
            patch.object(e2e, "validate_workload"),
        ]
        for item in patches:
            item.start()
        try:
            e2e.validate_reprovision_refused()
        finally:
            for item in reversed(patches):
                item.stop()
        return calls

    def test_the_change_is_refused_without_the_opt_in_and_the_node_comes_back(self):
        calls = self._refuse()
        self.assertEqual(calls[0], ("allow", False), "the opt-in must be absent before the change is declared")
        self.assertEqual([c[1] for c in calls if c[0] == "declare"], ["none", "nspawn"])
        self.assertIn(("kubectl", "delete", "node"), calls)
        for step in (("node",), ("condition", "none"), ("soak",), ("handoff",)):
            self.assertIn(step, calls)

    def test_it_runs_from_either_mode(self):
        calls = self._refuse(source="none")
        self.assertEqual([c[1] for c in calls if c[0] == "declare"], ["nspawn", "none"])

    def test_a_failure_for_another_reason_fails(self):
        with self.assertRaises(SystemExit):
            self._refuse(reason="")
        with self.assertRaises(SystemExit):
            self._refuse(reason="UnsupportedMode")

    def test_a_refusal_recorded_as_in_place_fails(self):
        with self.assertRaises(SystemExit):
            self._refuse(strategy="InPlace")

    def test_a_refusal_that_changed_the_mode_fails(self):
        with self.assertRaises(SystemExit):
            self._refuse(applied_after="none")


class NoReprovisionHandoffTests(unittest.TestCase):
    def _check(self, present, state="inactive"):
        clock = iter(range(0, 100000, 100))
        with patch.object(e2e, "present_on_host", return_value=present) as asked, \
                patch.object(e2e, "ssh_capture_quiet", return_value=unittest.mock.Mock(stdout=state + "\n")), \
                patch.object(e2e.time, "sleep"), patch.object(e2e.time, "monotonic", side_effect=lambda: next(clock)):
            e2e.validate_no_reprovision_handoff(timeout_secs=300)
        return asked.call_args.args[0]

    def test_a_clean_host_passes_and_the_staged_token_is_looked_for(self):
        asked = self._check([])
        self.assertIn(f"/etc/systemd/system/{e2e.REPROVISION_UNIT}", asked)
        self.assertIn(f"{e2e.REPROVISION_DIR}/config.json", asked)
        self.assertIn(f"{e2e.REPROVISION_DIR}/unbounded-agent", asked)

    def test_a_left_unit_or_staged_config_fails(self):
        for leftover in ([f"/etc/systemd/system/{e2e.REPROVISION_UNIT}"], [f"{e2e.REPROVISION_DIR}/config.json"]):
            with self.subTest(leftover=leftover), self.assertRaises(SystemExit):
                self._check(leftover)

    def test_a_running_handoff_fails(self):
        with self.assertRaises(SystemExit):
            self._check([], state="activating")


class ResetOnHostTests(unittest.TestCase):
    def test_reset_runs_on_the_host_and_everything_is_checked(self):
        calls = []
        with patch.object(e2e, "ssh_cmd", side_effect=lambda command: calls.append(("ssh", command))), \
                patch.object(e2e, "run_quiet", side_effect=lambda args, **kw: calls.append(("run", args[1]))), \
                patch.object(e2e, "wait_for_node_absent", side_effect=lambda name: calls.append(("absent",))), \
                patch.object(e2e, "validate_node_stack_removed", side_effect=lambda: calls.append(("stack",))), \
                patch.object(e2e, "validate_reset_cleanup", side_effect=lambda: calls.append(("files",))), \
                patch.object(e2e, "validate_nftables_flush_removed", side_effect=lambda: calls.append(("nft",))), \
                patch.object(e2e, "validate_no_reprovision_handoff", side_effect=lambda: calls.append(("handoff",))):
            e2e.reset_agent_on_host()
        self.assertEqual(calls[0], ("ssh", f"sudo {e2e.DAEMON_BINARY} reset"))
        self.assertEqual([c[0] for c in calls[1:]], ["run", "absent", "stack", "files", "nft", "handoff"])

    def test_nothing_left_of_the_nftables_flush_passes(self):
        self.assertEqual(e2e.nftables_flush_leftover([], ""), "")
        self.assertEqual(e2e.nftables_flush_leftover([], "not-found\n"), "")

    def test_a_leftover_nftables_flush_is_reported(self):
        problem = e2e.nftables_flush_leftover(["/etc/unbounded/kube/nftables-clear.nft"], "")
        self.assertIn("nftables-clear.nft", problem)
        for state in ("enabled\n", "disabled", "static", "masked"):
            self.assertIn(repr(state.strip()), e2e.nftables_flush_leftover([], state), state)

    def test_the_nftables_flush_check_covers_every_mode(self):
        """The unit is enabled by multi-user.target in none and by the nspawn
        machines in nspawn; both links and the rules file are checked."""
        for path in ("/etc/systemd/system/multi-user.target.wants/nftables-flush.service",
                     "/etc/systemd/system/systemd-nspawn@.service.requires/nftables-flush.service",
                     "/etc/unbounded/kube/nftables-clear.nft"):
            self.assertIn(path, e2e.NFTABLES_FLUSH_ARTIFACTS)
        with patch.object(e2e, "present_on_host", return_value=[]), \
                patch.object(e2e, "ssh_capture_quiet", return_value=unittest.mock.Mock(stdout="enabled\n")):
            with self.assertRaises(SystemExit):
                e2e.validate_nftables_flush_removed()
        with patch.object(e2e, "present_on_host", return_value=[]), \
                patch.object(e2e, "ssh_capture_quiet", return_value=unittest.mock.Mock(stdout="")):
            e2e.validate_nftables_flush_removed()

    def test_the_none_node_stack_must_be_gone(self):
        with patch.object(e2e, "ISOLATION_MODE", "none"), \
                patch.object(e2e, "present_on_host", return_value=[e2e.NONE_NODE_BASE]):
            with self.assertRaises(SystemExit):
                e2e.validate_node_stack_removed()
        with patch.object(e2e, "ISOLATION_MODE", "none"), patch.object(e2e, "present_on_host", return_value=[]):
            e2e.validate_node_stack_removed()


def pod(name, ip, phase="Running", host_network=False, deleting=False, ips=None):
    metadata = {"namespace": "ns", "name": name}
    if deleting:
        metadata["deletionTimestamp"] = "2026-01-01T00:00:00Z"
    status = {"phase": phase, "podIP": ip}
    if ips is not None:
        status["podIPs"] = [{"ip": i} for i in ips]
    return {"metadata": metadata, "spec": {"hostNetwork": host_network}, "status": status}


class NoneRepavePodCIDRTests(unittest.TestCase):
    def test_only_running_pod_network_pods_outside_the_range_are_reported(self):
        pods = [
            pod("inside", "10.244.1.5"),
            pod("outside", "10.244.0.7"),
            pod("host", "192.168.100.10", host_network=True),
            pod("pending", "10.244.0.8", phase="Pending"),
            pod("deleting", "10.244.0.9", deleting=True),
            pod("no-ip", ""),
        ]
        self.assertEqual(e2e.pod_ips_outside_cidrs(pods, ["10.244.1.0/24"]), ["ns/outside 10.244.0.7"])

    def test_every_family_of_a_dual_stack_pod_is_checked(self):
        cidrs = ["10.244.1.0/24", "fd00:10:244:1::/64"]
        good = pod("good", "10.244.1.5", ips=["10.244.1.5", "fd00:10:244:1::5"])
        bad = pod("bad", "10.244.1.6", ips=["10.244.1.6", "fd00:10:244:0::6"])
        self.assertEqual(e2e.pod_ips_outside_cidrs([good, bad], cidrs), ["ns/bad fd00:10:244:0::6"])

    def test_node_pod_cidrs_prefers_the_list(self):
        self.assertEqual(e2e.node_pod_cidrs({"spec": {"podCIDR": "10.244.1.0/24"}}), ["10.244.1.0/24"])
        self.assertEqual(e2e.node_pod_cidrs({"spec": {"podCIDR": "10.244.1.0/24", "podCIDRs": ["10.244.1.0/24", "fd00::/64"]}}),
                         ["10.244.1.0/24", "fd00::/64"])
        self.assertEqual(e2e.node_pod_cidrs({"spec": {}}), [])

    def test_the_daemonset_pod_is_on_the_pod_network_of_the_agent_node(self):
        spec = e2e.pod_cidr_daemonset()["spec"]["template"]["spec"]
        self.assertFalse(spec.get("hostNetwork", False))
        terms = spec["affinity"]["nodeAffinity"]["requiredDuringSchedulingIgnoredDuringExecution"]["nodeSelectorTerms"]
        self.assertEqual(terms[0]["matchFields"][0]["values"], [e2e.AGENT_MACHINE_NAME])

    def test_other_modes_skip_it(self):
        with patch.object(e2e, "ISOLATION_MODE", "nspawn"), patch.object(e2e, "kubectl") as kubectl:
            e2e.validate_none_repave_pod_cidr(e2e.load_node_config(""))
        kubectl.assert_not_called()

    def test_it_runs_in_the_operations_suite_after_the_repave_upgrade(self):
        steps = e2e.SUITES["isolation-operations"]
        self.assertLess(steps.index("validate-node-repave-upgrade"), steps.index("validate-none-repave-pod-cidr"))


class NoneRepaveDaemonKillTests(unittest.TestCase):
    def test_the_script_kills_the_daemon_once_the_repave_started(self):
        script = e2e.daemon_kill_during_repave_script(1700000000, 300)
        self.assertIn("--since @1700000000", script)
        self.assertIn("systemctl show -p MainPID --value unbounded-agent-daemon.service", script)
        self.assertIn('kill -KILL "$pid"', script)
        self.assertNotIn("systemctl kill", script)
        # A repave that is already over is reported, not killed after the fact.
        self.assertLess(script.index(e2e.NONE_REPAVE_COMPLETED), script.index(e2e.NONE_REPAVE_STARTED))

    def test_the_script_is_valid_shell(self):
        script = e2e.daemon_kill_during_repave_script(1, 1)
        self.assertEqual(subprocess.run(["sh", "-n", "-c", script], check=False).returncode, 0)

    def test_the_node_must_come_back_after_the_kill(self):
        calls = []
        killed = unittest.mock.Mock(returncode=0, stdout="killed\n", stderr="")
        with patch.object(e2e, "ISOLATION_MODE", "none"), \
                patch.object(e2e, "ssh_capture", return_value="1700000000"), \
                patch.object(e2e, "ssh_capture_quiet", return_value=killed), \
                patch.object(e2e, "repave_with_config_change", side_effect=lambda nc, purpose: calls.append("repave")), \
                patch.object(e2e, "wait_for_daemon_active", side_effect=lambda: calls.append("daemon")), \
                patch.object(e2e, "wait_for_node", side_effect=lambda: calls.append("node")), \
                patch.object(e2e, "validate_isolation_mode", side_effect=lambda: calls.append("units")):
            e2e.validate_none_repave_survives_daemon_kill(e2e.load_node_config(""))
        self.assertEqual(calls, ["repave", "daemon", "node", "units"])

    def test_a_repave_that_was_not_interrupted_fails(self):
        finished = unittest.mock.Mock(returncode=2, stdout="finished\n", stderr="")
        with patch.object(e2e, "ISOLATION_MODE", "none"), \
                patch.object(e2e, "ssh_capture", return_value="1700000000"), \
                patch.object(e2e, "ssh_capture_quiet", return_value=finished), \
                patch.object(e2e, "repave_with_config_change"):
            with self.assertRaises(SystemExit):
                e2e.validate_none_repave_survives_daemon_kill(e2e.load_node_config(""))

    def test_other_modes_skip_it(self):
        with patch.object(e2e, "ISOLATION_MODE", "nspawn"), patch.object(e2e, "repave_with_config_change") as repave:
            e2e.validate_none_repave_survives_daemon_kill(e2e.load_node_config(""))
        repave.assert_not_called()


class RestoreDeletedNodeTests(unittest.TestCase):
    def test_a_node_reboot_brings_the_node_back(self):
        with patch.object(e2e, "run_quiet"), patch.object(e2e, "create_machine_operation") as created, \
                patch.object(e2e, "wait_for_machine_operation_complete") as waited, patch.object(e2e, "wait_for_node") as node:
            e2e.restore_deleted_node()
        self.assertEqual(created.call_args.args[2], "NodeReboot")
        waited.assert_called_once()
        node.assert_called_once()


class AppliedConfigFilesTests(unittest.TestCase):
    def test_only_regular_files_are_listed(self):
        commands = []

        def ssh(command):
            commands.append(command)
            return unittest.mock.Mock(stdout="/etc/unbounded/agent/kube1-applied-config.json\n", returncode=0)

        with patch.object(e2e, "ssh_capture_quiet", side_effect=ssh):
            files = e2e.applied_config_files()
        self.assertEqual(files, ["/etc/unbounded/agent/kube1-applied-config.json"])
        self.assertIn("[ -f", commands[0], "a directory named like an applied config must not count as a slot")


if __name__ == "__main__":
    unittest.main()
