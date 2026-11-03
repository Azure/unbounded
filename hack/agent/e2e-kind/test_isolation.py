# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0

import json
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
        self.assertEqual(e2e.parse_applied_isolation(json.dumps({"Isolation": "container"})), "container")

    def test_the_strategy_follows_from_the_pair(self):
        """In place only between nspawn and container; any pair involving none
        reprovisions (pkg/agent/isolation/migrate.Plan). The harness no longer
        chooses it."""
        self.assertEqual(e2e.expected_change_strategy("nspawn", "container"), "inplace")
        self.assertEqual(e2e.expected_change_strategy("container", "nspawn"), "inplace")
        for pair in (("nspawn", "none"), ("none", "nspawn"), ("container", "none"), ("none", "container")):
            self.assertEqual(e2e.expected_change_strategy(*pair), "reprovision", pair)


class ContainerUnitTests(unittest.TestCase):
    def test_slots_come_from_the_unit_names(self):
        out = (
            "unbounded-node-kube1.service loaded active running Unbounded node kube1\n"
            "unbounded-node-kube2.service loaded active running Unbounded node kube2\n"
        )
        self.assertEqual(e2e.container_slots_from_units(out), ["kube1", "kube2"])

    def test_other_units_and_blank_output_are_ignored(self):
        self.assertEqual(e2e.container_slots_from_units(""), [])
        self.assertEqual(e2e.container_slots_from_units("unbounded-containerd.service loaded active running x\n"), [])


class SuiteTests(unittest.TestCase):
    def test_all_suite_steps_exist(self):
        e2e.validate_suites()

    def test_default_lifecycle_has_no_isolation_steps(self):
        self.assertFalse([s for s in e2e.SUITES["lifecycle"] if "isolation" in s or s.startswith("migrate")])

    def test_the_checks_of_the_removed_mode_change_suite_live_in_the_surviving_ones(self):
        """isolation-mode-change is gone. Its changes run in the matrix, the failed
        change with rollback in drift (nspawn to container) and rollback (container
        to nspawn), and the same-mode declaration in rollback."""
        self.assertNotIn("isolation-mode-change", e2e.SUITES)
        for step in ("change-to-container", "change-to-nspawn", "change-to-none"):
            self.assertIn(step, e2e.SUITES["isolation-matrix"])
        self.assertIn("validate-change-rollback-to-container", e2e.SUITES["isolation-drift"])
        self.assertIn("validate-change-rollback-config", e2e.SUITES["isolation-rollback"])
        self.assertIn("validate-same-mode-declaration", e2e.SUITES["isolation-rollback"])

    def test_the_drift_suite_makes_no_successful_change(self):
        """isolation-matrix asserts AppliedModeMatches after every change, so the
        drift suite only checks the declaration states and a failed change."""
        steps = e2e.SUITES["isolation-drift"]
        self.assertFalse([s for s in steps if s.startswith("change-to-")])
        for step in ("declare-isolation-mismatch", "reconcile-machine-isolation-spec",
                     "validate-change-rollback-to-container"):
            self.assertIn(step, steps)

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
        self.assertEqual(e2e.isolation_drift_expectation("none", "container"), ("True", "AppliedModeDiffers"))

    def test_drift_expectation_reports_the_change_for_the_declared_mode(self):
        failed = {"targetMode": "container", "phase": "Failed"}
        running = {"targetMode": "container", "phase": "InProgress"}
        done = {"targetMode": "container", "phase": "Succeeded"}
        self.assertEqual(e2e.isolation_drift_expectation("container", "nspawn", failed), ("True", "ChangeFailed"))
        self.assertEqual(e2e.isolation_drift_expectation("container", "nspawn", running), ("True", "ChangeInProgress"))
        # A change for another mode, or one that succeeded, does not describe this drift.
        self.assertEqual(e2e.isolation_drift_expectation("none", "nspawn", failed), ("True", "AppliedModeDiffers"))
        self.assertEqual(e2e.isolation_drift_expectation("container", "nspawn", done), ("True", "AppliedModeDiffers"))
        # Once the modes agree the old failure no longer matters.
        self.assertEqual(e2e.isolation_drift_expectation("container", "container", failed), ("False", "AppliedModeMatches"))

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
                "none", {"checks": [{"name": "none-backend"}, {"name": "container-backend"}]})
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

    def test_digest_differences(self):
        before = {"/a": "1", "/b": "2", "/c": "3"}
        after = {"/a": "1", "/b": "9", "/d": "4"}
        self.assertEqual(e2e.digest_differences(before, after), ["changed /b", "removed /c", "added /d"])
        self.assertEqual(e2e.digest_differences(before, dict(before)), [])

    def test_uninstall_asset_has_the_placeholder_the_harness_replaces(self):
        self.assertIn(e2e.UNINSTALL_SCRIPT_PLACEHOLDER, e2e.UNINSTALL_SCRIPT_ASSET.read_text())

    def test_new_steps_and_suites_exist(self):
        for step in ("validate-isolation-status", "reconcile-machine-isolation-spec", "validate-isolation-preflight",
                     "validate-isolation-uninstall", "validate-change-rollback-config",
                     "change-to-container", "change-to-none", "change-to-nspawn", "declare-isolation-mismatch",
                     "validate-same-mode-declaration", "validate-change-needs-nspawn-runtime",
                     "collect-isolation-evidence"):
            self.assertIn(step, e2e.COMMANDS)
        for suite in ("isolation-operations", "isolation-uninstall", "isolation-matrix",
                      "isolation-drift", "isolation-rollback"):
            self.assertIn(suite, e2e.SUITES)

    def test_existing_suites_are_unchanged(self):
        self.assertEqual(e2e.SUITES["lifecycle"][0], "run-agent")
        self.assertNotIn("validate-isolation-status", e2e.SUITES["lifecycle"])

    def test_nspawn_only_checks_stay_out_of_the_mode_neutral_suites(self):
        for suite in ("isolation-operations", "isolation-uninstall", "isolation-matrix",
                      "isolation-drift", "isolation-rollback"):
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
        for suite in ("isolation-matrix", "isolation-drift", "isolation-rollback"):
            self.assertEqual(e2e.SUITES[suite][0], "run-agent")


class NodeStackDiagnosticsTests(unittest.TestCase):
    def _run(self, mode, ssh):
        with patch.object(e2e, "ISOLATION_MODE", mode), patch.object(e2e, "ssh_capture_quiet", side_effect=ssh), \
                patch.object(e2e.subprocess, "run"):
            e2e.dump_node_stack_diagnostics()

    def test_container_mode_asks_for_the_in_container_journals(self):
        commands = []

        def ssh(command):
            commands.append(command)
            return unittest.mock.Mock(stdout="out", stderr="", returncode=0)

        self._run("container", ssh)
        joined = "\n".join(commands)
        self.assertIn("/opt/unbounded/runtime/runc list", joined)
        self.assertIn("journalctl --no-pager -n 150 -u kubelet -u containerd", joined)

    def test_other_modes_do_not_query_runc(self):
        for mode in ("nspawn", "none"):
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

        self._run("container", ssh)


class UninstallSuiteTests(unittest.TestCase):
    def test_the_suite_stops_at_the_script_because_reset_is_the_way_back(self):
        """The uninstall script is not a reset; `unbounded-agent reset` is the
        only way back to a clean host, and the operations suites cover reset and
        reinstall. This suite only checks what the script removes."""
        steps = e2e.SUITES["isolation-uninstall"]
        self.assertEqual(steps[-1], "validate-isolation-uninstall")
        self.assertNotIn("reinstall-agent", steps)
        self.assertNotIn("reset-host", e2e.COMMANDS)

    def test_the_uninstall_check_only_asks_for_what_the_script_owns(self):
        for mode, leftover in (("container", ["/var/lib/unbounded/containers/kube1"]),
                               ("none", [e2e.NONE_NODE_BASE])):
            with self.subTest(mode=mode), patch.object(e2e, "ISOLATION_MODE", mode), \
                    patch.object(e2e, "present_on_host", return_value=leftover):
                with self.assertRaises(SystemExit):
                    e2e.validate_uninstall_removed_node_stack("kube1")

    def test_layout_directories_are_not_the_scripts_business(self):
        with patch.object(e2e, "ISOLATION_MODE", "container"), \
                patch.object(e2e, "present_on_host", return_value=[]) as present:
            e2e.validate_uninstall_removed_node_stack("kube1")
        asked = present.call_args.args[0]
        self.assertNotIn(e2e.CONTAINER_ROOT_BASE, asked)

    def test_an_nspawn_machine_that_is_still_closing_is_waited_for(self):
        """The script returns after its own bounded wait; a node container with
        running pods can still be State=closing. e2e showed the check failing
        on that, so the machine is given time to go."""
        answers = [unittest.mock.Mock(returncode=0), unittest.mock.Mock(returncode=0),
                   unittest.mock.Mock(returncode=1)]
        with patch.object(e2e, "ISOLATION_MODE", "nspawn"), patch.object(e2e.time, "sleep") as slept, \
                patch.object(e2e, "ssh_capture_quiet", side_effect=answers):
            e2e.validate_uninstall_removed_node_stack("kube1")
        self.assertEqual(slept.call_count, 2)

    def test_an_nspawn_machine_that_never_goes_fails(self):
        clock = iter(range(0, 10000, 100))
        with patch.object(e2e, "ISOLATION_MODE", "nspawn"), patch.object(e2e.time, "sleep"), \
                patch.object(e2e.time, "monotonic", side_effect=lambda: next(clock)), \
                patch.object(e2e, "ssh_capture_quiet", return_value=unittest.mock.Mock(returncode=0, stdout="", stderr="")):
            with self.assertRaises(SystemExit):
                e2e.validate_uninstall_removed_node_stack("kube1", "script output")


class ActiveSlotTests(unittest.TestCase):
    def test_a_container_host_names_its_slot_from_the_unit(self):
        units = unittest.mock.Mock(returncode=0, stdout="unbounded-node-kube2.service loaded active running x\n")
        with patch.object(e2e, "ISOLATION_MODE", "container"), patch.object(e2e, "ssh_capture_quiet", return_value=units):
            self.assertEqual(e2e.active_node_slot(), "kube2")
            self.assertEqual(e2e.alternate_nspawn_slot(), "kube1")

    def test_none_always_uses_kube1(self):
        with patch.object(e2e, "ISOLATION_MODE", "none"):
            self.assertEqual(e2e.active_node_slot(), "kube1")
            self.assertEqual(e2e.alternate_nspawn_slot(), "kube2")


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
        for mode in ("nspawn", "none", "container"):
            with self.subTest(mode=mode):
                self._run(mode, ["100\n", "250\n"])

    def test_a_node_reboot_that_did_nothing_fails_even_though_the_node_is_ready(self):
        with self.assertRaises(SystemExit):
            self._run("container", ["100\n"] * 8)

    def test_an_unreadable_marker_before_the_reboot_fails(self):
        with self.assertRaises(SystemExit):
            self._run("nspawn", [""])

    def test_the_marker_reads_the_kubelet_unit_of_the_mode(self):
        for mode, unit in (("none", "unbounded-kubelet.service"), ("nspawn", "kubelet.service"),
                           ("container", "kubelet.service")):
            seen = []

            def ssh(command, seen=seen):
                seen.append(command)
                return unittest.mock.Mock(returncode=0, stdout="7\n", stderr="")

            with self.subTest(mode=mode), patch.object(e2e, "ISOLATION_MODE", mode), \
                    patch.object(e2e, "ssh_capture_quiet", ssh):
                self.assertEqual(e2e.node_stack_start_marker(), "7")
            self.assertIn(f"exec -- systemctl show -p ActiveEnterTimestampMonotonic --value {unit}", seen[0])


class RollbackCheckTests(unittest.TestCase):
    def test_a_none_source_is_refused_because_the_rollback_would_never_run(self):
        with patch.object(e2e, "applied_isolation", return_value="none"), \
                patch.object(e2e, "shared_config_digest") as digest:
            with self.assertRaises(SystemExit):
                e2e.validate_change_rollback_config(None)
        digest.assert_not_called()

    def test_a_rollback_message_must_name_the_rollback_and_the_injected_fault(self):
        good = "persist applied config: x (rolled back to container)"
        e2e.assert_rollback_ran(good, "container")
        for bad in ("", "preflight nspawn: no (the container node was not changed)",
                    "start nspawn: boom (rolled back to container)"):
            with self.subTest(message=bad), self.assertRaises(SystemExit):
                e2e.assert_rollback_ran(bad, "container")

    def test_a_custom_fault_only_needs_the_rollback(self):
        e2e.assert_rollback_ran("boom (rolled back to nspawn)", "nspawn", custom_fault=True)
        with self.assertRaises(SystemExit):
            e2e.assert_rollback_ran("boom (the nspawn node was not changed)", "nspawn", custom_fault=True)


class DigestTests(unittest.TestCase):
    def test_a_kubelet_client_certificate_rotation_is_not_a_change(self):
        import pathlib
        import subprocess
        import tempfile

        (e2e.REPO_ROOT / "tmp").mkdir(exist_ok=True)

        with tempfile.TemporaryDirectory(dir=str(e2e.REPO_ROOT / "tmp")) as tmp:
            pki = pathlib.Path(tmp) / "pki"
            pki.mkdir()
            (pki / "ca.crt").write_text("ca")
            (pki / "kubelet-client-2026-01-01-00-00-00.pem").write_text("old")
            (pki / "kubelet-client-current.pem").symlink_to("kubelet-client-2026-01-01-00-00-00.pem")
            (pathlib.Path(tmp) / "config.yaml").write_text("cfg")

            def digest():
                with patch.object(e2e, "SHARED_CONFIG_PATHS", (str(pki), str(pathlib.Path(tmp) / "config.yaml"))):
                    script = e2e.shared_config_digest_script()

                return e2e.parse_digest(subprocess.run(["sh", "-c", script], capture_output=True, text=True,
                                                       check=True).stdout)

            before = digest()
            self.assertIn(str(pki / "ca.crt"), before)
            self.assertIn(str(pathlib.Path(tmp) / "config.yaml"), before)
            self.assertFalse([p for p in before if "kubelet-client-" in p], before)

            # The kubelet rotates its certificate.
            (pki / "kubelet-client-current.pem").unlink()
            (pki / "kubelet-client-2026-02-01-00-00-00.pem").write_text("new")
            (pki / "kubelet-client-current.pem").symlink_to("kubelet-client-2026-02-01-00-00-00.pem")
            self.assertEqual(e2e.digest_differences(before, digest()), [])

            # A real configuration change is still seen.
            (pki / "ca.crt").write_text("changed")
            self.assertEqual(e2e.digest_differences(before, digest()), [f"changed {pki / 'ca.crt'}"])


class DiagnosticsRedactionTests(unittest.TestCase):
    def test_tokens_and_kubeconfig_credentials_are_removed(self):
        text = ("kubelet: token abcdef.0123456789abcdef\n"
                "users:\n- user:\n    token: s3cr3t\n    client-key-data: QUJD\n"
                '{"BootstrapToken": "zzzzzz.yyyyyyyyyyyyyyyy"}\nplain line\n')
        out = e2e.redact_diagnostics(text)
        for secret in ("abcdef.0123456789abcdef", "s3cr3t", "QUJD", "zzzzzz.yyyyyyyyyyyyyyyy"):
            self.assertNotIn(secret, out)
        self.assertIn("plain line", out)

    def test_the_dump_prints_redacted_output_and_reads_the_bundle_directory(self):
        import contextlib
        import io

        commands = []

        def ssh(command):
            commands.append(command)
            return unittest.mock.Mock(stdout="token abcdef.0123456789abcdef", stderr="", returncode=0)

        out = io.StringIO()
        with patch.object(e2e, "ISOLATION_MODE", "container"), patch.object(e2e, "ssh_capture_quiet", ssh), \
                patch.object(e2e.subprocess, "run"), contextlib.redirect_stdout(out):
            e2e.dump_node_stack_diagnostics()
        self.assertNotIn("abcdef.0123456789abcdef", out.getvalue())
        self.assertIn("<redacted-token>", out.getvalue())
        self.assertTrue(any("containers/bundles/*/config.json" in c for c in commands))


class UninstallSlotTests(unittest.TestCase):
    def test_the_uninstall_validation_uses_the_active_slot(self):
        seen = {}

        def validate(slot, output=""):
            seen["slot"] = slot

        with patch.object(e2e, "ISOLATION_MODE", "container"), patch.object(e2e, "active_node_slot", return_value="kube2"), \
                patch.object(e2e, "ssh_capture", return_value="Isolation mode: container\n"), \
                patch.object(e2e, "ssh_capture_quiet"), patch.object(e2e, "scp_cmd"), \
                patch.object(e2e, "validate_uninstall_removed_node_stack", validate), \
                patch.object(e2e, "run_quiet"), patch.object(e2e, "delete_machine_cr"), \
                patch.object(e2e, "VM_DIR", e2e.REPO_ROOT / "tmp"):
            (e2e.REPO_ROOT / "tmp").mkdir(exist_ok=True)
            e2e.validate_isolation_uninstall()
        self.assertEqual(seen["slot"], "kube2")


def view(spec="", observed="nspawn", drift=None, change=None):
    return {"spec": spec, "observed": observed, "drift": drift, "change": change or {}}


def drift(status, reason, message=""):
    return {"status": status, "reason": reason, "message": message}


class WaitForIsolationChangeTests(unittest.TestCase):
    def _wait(self, views, target="container", phase="Succeeded", previous=None, timeout_secs=None):
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
        done = {"targetMode": "container", "phase": "Succeeded", "lastTransitionTime": "t2"}
        got, _ = self._wait([{}, {"targetMode": "container", "phase": "InProgress", "lastTransitionTime": "t1"}, done])
        self.assertEqual(got, done)

    def test_a_stale_result_of_the_same_target_is_not_taken_for_the_new_one(self):
        old = {"targetMode": "container", "phase": "Succeeded", "lastTransitionTime": "t1"}
        new = {"targetMode": "container", "phase": "Succeeded", "lastTransitionTime": "t2"}
        got, _ = self._wait([old, old, new], previous=old)
        self.assertEqual(got["lastTransitionTime"], "t2")

    def test_a_change_for_another_mode_is_ignored(self):
        other = {"targetMode": "none", "phase": "Succeeded", "lastTransitionTime": "t9"}
        mine = {"targetMode": "container", "phase": "Succeeded", "lastTransitionTime": "t2"}
        got, _ = self._wait([other, mine])
        self.assertEqual(got, mine)

    def test_the_wrong_terminal_phase_fails(self):
        failed = {"targetMode": "container", "phase": "Failed", "lastTransitionTime": "t2", "message": "boom"}
        with self.assertRaises(SystemExit):
            self._wait([failed], phase="Succeeded")
        succeeded = {"targetMode": "container", "phase": "Succeeded", "lastTransitionTime": "t2"}
        with self.assertRaises(SystemExit):
            self._wait([succeeded], phase="Failed")

    def test_no_result_in_time_fails_with_diagnostics(self):
        seen = iter([{}] * 50)
        clock = iter(range(0, 100000, 400))
        with patch.object(e2e, "isolation_change_view", side_effect=lambda: next(seen)), patch.object(e2e.time, "sleep"), \
                patch.object(e2e.time, "monotonic", side_effect=lambda: next(clock)), \
                patch.object(e2e, "dump_node_stack_diagnostics") as dumped:
            with self.assertRaises(SystemExit):
                e2e.wait_for_isolation_change("container", "Succeeded", {}, timeout_secs=600)
        dumped.assert_called_once()


class ChangePendingTests(unittest.TestCase):
    def _pending(self, views, marker_now="kube1", applied="nspawn", previous=None, target="container"):
        seen = iter(views)
        clock = iter(range(0, 100000, 100))
        with patch.object(e2e, "machine_isolation_view", side_effect=lambda: next(seen)), \
                patch.object(e2e, "isolation_node_marker", return_value=marker_now), \
                patch.object(e2e, "applied_isolation", return_value=applied), \
                patch.object(e2e.time, "sleep"), patch.object(e2e.time, "monotonic", side_effect=lambda: next(clock)):
            e2e.validate_change_pending("nspawn", target, "kube1", previous or {})

    def test_a_declared_mode_only_shows_a_drift(self):
        self._pending([view("", "nspawn", drift("False", "NoModeDeclared")),
                       view("container", "nspawn", drift("True", "AppliedModeDiffers"))])

    def test_an_old_failure_for_the_same_target_may_still_show_as_change_failed(self):
        previous = {"targetMode": "container", "phase": "Failed"}
        self._pending([view("container", "nspawn", drift("True", "ChangeFailed"))], previous=previous)

    def test_change_failed_is_not_accepted_without_an_old_failure(self):
        with self.assertRaises(SystemExit):
            self._pending([view("container", "nspawn", drift("True", "ChangeFailed"))] * 5)

    def test_a_node_that_moved_on_declaration_fails(self):
        with self.assertRaises(SystemExit):
            self._pending([view("container", "nspawn", drift("True", "AppliedModeDiffers"))], marker_now="kube2")
        with self.assertRaises(SystemExit):
            self._pending([view("container", "nspawn", drift("True", "AppliedModeDiffers"))], applied="container")

    def test_a_drift_that_never_appears_fails(self):
        with self.assertRaises(SystemExit):
            self._pending([view("container", "nspawn", drift("False", "AppliedModeMatches"))] * 5)


class ChangeIsolationModeTests(unittest.TestCase):
    def _run(self, source="nspawn", target="container", strategy="InPlace", after=None, change_phase="Succeeded"):
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

    def test_declare_then_check_then_delete_then_wait_in_that_order(self):
        calls = self._run()
        self.assertEqual(calls[:5], ["declare", "pending", "delete-node", "wait", "effective"])
        self.assertEqual(calls[5:], ["node", "mode", "status", "config", "workload"])

    def test_a_reprovision_pair_must_record_the_reprovision_strategy(self):
        self._run(source="nspawn", target="none", strategy="Reprovision",
                  after=view("none", "none", drift("False", "AppliedModeMatches")))
        with self.assertRaises(SystemExit):
            self._run(source="nspawn", target="none", strategy="InPlace",
                      after=view("none", "none", drift("False", "AppliedModeMatches")))

    def test_an_in_place_pair_must_not_record_a_reprovision(self):
        with self.assertRaises(SystemExit):
            self._run(source="nspawn", target="container", strategy="Reprovision")

    def test_a_drift_that_survives_the_change_fails(self):
        with self.assertRaises(SystemExit):
            self._run(after=view("container", "container", drift("True", "AppliedModeDiffers")))
        with self.assertRaises(SystemExit):
            self._run(after=view("container", "nspawn", drift("False", "AppliedModeMatches")))


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
            self._run(applied_after="container")

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
    CHANGE = {"targetMode": "nspawn", "phase": "Failed", "message": "persist applied config: x (rolled back to container)"}

    def _check(self, views):
        seen = iter(views)
        clock = iter(range(0, 100000, 100))
        with patch.object(e2e, "machine_isolation_view", side_effect=lambda: next(seen)), patch.object(e2e.time, "sleep"), \
                patch.object(e2e.time, "monotonic", side_effect=lambda: next(clock)):
            e2e.validate_change_failed_condition("nspawn", self.CHANGE)

    def test_the_condition_carrying_the_failure_passes(self):
        self._check([view("nspawn", "container", drift("True", "AppliedModeDiffers")),
                     view("nspawn", "container", drift("True", "ChangeFailed", "persist applied config: x"))])

    def test_a_condition_that_never_reports_the_failure_fails(self):
        with self.assertRaises(SystemExit):
            self._check([view("nspawn", "container", drift("True", "AppliedModeDiffers"))] * 5)

    def test_a_condition_without_the_failure_text_fails(self):
        with self.assertRaises(SystemExit):
            self._check([view("nspawn", "container", drift("True", "ChangeFailed", "something else"))])


class ChangeRollbackTests(unittest.TestCase):
    def _run(self, source, target, env=None, wait_raises=False):
        failed = {"targetMode": target, "phase": "Failed", "lastTransitionTime": "t2",
                  "message": f"persist applied config: x (rolled back to {source})"}
        calls = []
        patches = [
            patch.dict("os.environ", env or {}, clear=False),
            patch.object(e2e, "applied_isolation", return_value=source),
            patch.object(e2e, "isolation_change_view", return_value={}),
            patch.object(e2e, "isolation_node_marker", return_value="m"),
            patch.object(e2e, "declare_isolation_spec", side_effect=lambda mode: calls.append(("declare", mode))),
            patch.object(e2e, "validate_change_pending"),
            patch.object(e2e, "kubectl", side_effect=lambda args: calls.append(("kubectl", args[0]))),
            patch.object(e2e, "alternate_nspawn_slot", return_value="kube2"),
            patch.object(e2e, "inject_migration_target_fault", side_effect=lambda: calls.append(("inject",)) or "/p"),
            patch.object(e2e, "clear_migration_target_fault", side_effect=lambda path: calls.append(("clear", path))),
            patch.object(e2e, "wait_for_isolation_change",
                         side_effect=(SystemExit(1) if wait_raises else lambda *a, **k: failed)),
            patch.object(e2e, "validate_change_failed_condition"),
            patch.object(e2e, "assert_no_automatic_retry"),
            patch.object(e2e, "set_effective_isolation"),
            patch.object(e2e, "wait_for_node"),
            patch.object(e2e, "validate_isolation_mode"),
            patch.object(e2e, "validate_isolation_status"),
            patch.object(e2e, "kubectl_capture", return_value="{}"),
            patch.object(e2e, "_assert_expected_node_config"),
            patch.object(e2e, "validate_workload"),
        ]
        for item in patches:
            item.start()
        try:
            e2e.validate_change_rollback(None, target)
        finally:
            for item in reversed(patches):
                item.stop()
        return calls, None

    def test_every_target_gets_the_fault_up_front_and_it_is_cleared(self):
        for source, target in (("nspawn", "container"), ("container", "nspawn")):
            calls, _ = self._run(source, target)
            self.assertIn(("inject",), calls, target)
            self.assertIn(("clear", "/p"), calls, target)
            order = [c[0] for c in calls]
            self.assertLess(order.index("inject"), order.index("declare"), "the fault is in place before the change is declared")

    def test_a_custom_fault_command_is_used(self):
        calls, _ = self._run("container", "nspawn", env={"MIGRATION_FAULT_CMD": "true"})
        self.assertIn(("inject",), calls)

    def test_the_spec_is_set_back_to_the_source_afterwards(self):
        calls, _ = self._run("nspawn", "container")
        declares = [call[1] for call in calls if call[0] == "declare"]
        self.assertEqual(declares, ["container", "nspawn"])

    def test_the_fault_is_removed_even_when_the_wait_fails(self):
        with self.assertRaises(SystemExit):
            self._run("container", "nspawn", wait_raises=True)
        with self.assertRaises(SystemExit):
            self._run("nspawn", "container", wait_raises=True)


class NSpawnRuntimeRefusalTests(unittest.TestCase):
    def test_the_rollback_suite_refuses_then_installs_the_runtime_before_the_rollback_check(self):
        steps = e2e.SUITES["isolation-rollback"]
        refuse = steps.index("validate-change-needs-nspawn-runtime")
        install = steps.index("install-nspawn-runtime")
        rollback = steps.index("validate-change-rollback-config")
        self.assertLess(refuse, install)
        self.assertLess(install, rollback)

    def _refuse(self, lacks=True, message="host cannot run isolation mode nspawn: nspawn-runtime: machinectl",
                marker_after="kube1"):
        change = {"targetMode": "nspawn", "phase": "Failed", "message": message, "lastTransitionTime": "t2"}
        markers = iter(["kube1", marker_after])
        with patch.object(e2e, "container_host_lacks_nspawn_runtime", return_value=lacks), \
                patch.object(e2e, "applied_isolation", return_value="container"), \
                patch.object(e2e, "isolation_change_view", return_value={}), \
                patch.object(e2e, "isolation_node_marker", side_effect=lambda: next(markers)), \
                patch.object(e2e, "declare_isolation_spec") as declared, \
                patch.object(e2e, "validate_change_pending"), patch.object(e2e, "kubectl") as kubectl, \
                patch.object(e2e, "wait_for_isolation_change", return_value=change) as waited, \
                patch.object(e2e, "validate_change_failed_condition"), patch.object(e2e, "wait_for_node"), \
                patch.object(e2e, "validate_isolation_status"):
            e2e.validate_change_needs_nspawn_runtime()
        return declared, kubectl, waited

    def test_a_host_that_has_the_runtime_has_nothing_to_refuse(self):
        with patch.object(e2e, "container_host_lacks_nspawn_runtime", return_value=False), \
                patch.object(e2e, "declare_isolation_spec") as declared:
            e2e.validate_change_needs_nspawn_runtime()
        declared.assert_not_called()

    def test_the_expected_refusal_passes_and_the_spec_is_set_back(self):
        declared, kubectl, waited = self._refuse()
        self.assertEqual([c.args[0] for c in declared.call_args_list], ["nspawn", "container"])
        self.assertEqual(waited.call_args.kwargs["timeout_secs"], 180, "a refusal is quick")
        self.assertIn("node", kubectl.call_args.args[0])

    def test_a_refusal_for_another_reason_fails(self):
        with self.assertRaises(SystemExit):
            self._refuse(message="something unrelated")

    def test_a_refusal_that_changed_the_node_fails(self):
        with self.assertRaises(SystemExit):
            self._refuse(marker_after="kube2")


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

    def test_the_drift_suite_covers_a_failed_change(self):
        self.assertIn("validate-change-rollback-to-container", e2e.SUITES["isolation-drift"])

    def test_the_mismatch_is_a_mode_other_than_the_applied_one(self):
        for applied, other in (("none", "nspawn"), ("container", "nspawn"), ("nspawn", "none")):
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


class MigrationTargetFaultTests(unittest.TestCase):
    def test_the_fault_is_a_directory_where_the_checksum_sidecar_belongs(self):
        """Persisting writes the config and then its checksum sidecar, so a
        directory at the sidecar path fails the persist for a target of any mode
        and leaves the target's own pre-start hook, which reads only the config
        file, alone."""
        for slot in ("kube1", "kube2"):
            with patch.dict("os.environ", {}, clear=False), patch.object(e2e, "alternate_nspawn_slot", return_value=slot), \
                    patch.object(e2e, "ssh_cmd") as ssh:
                path = e2e.inject_migration_target_fault()
            self.assertEqual(path, f"/etc/unbounded/agent/{slot}-applied-config.json.sha256")
            self.assertIn("mkdir -p", ssh.call_args.args[0])
            self.assertIn(path, ssh.call_args.args[0])

    def test_a_custom_fault_command_replaces_it(self):
        with patch.dict("os.environ", {"MIGRATION_FAULT_CMD": "true"}), patch.object(e2e, "ssh_cmd") as ssh:
            self.assertEqual(e2e.inject_migration_target_fault(), "")
        ssh.assert_called_once_with("true")

    def test_clearing_removes_only_the_empty_directory(self):
        with patch.dict("os.environ", {}, clear=False), patch.object(e2e, "ssh_cmd") as ssh:
            e2e.clear_migration_target_fault("/etc/unbounded/agent/kube2-applied-config.json.sha256")
        self.assertIn("rmdir", ssh.call_args.args[0])


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
