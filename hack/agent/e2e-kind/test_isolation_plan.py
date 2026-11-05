# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0

import json
import re
import tempfile
import unittest
from pathlib import Path

import e2e
import isolation_plan as plan

REPO_ROOT = Path(__file__).resolve().parents[3]
PLAN_DOC = REPO_ROOT / "designs" / "agent-isolation-modes-test-plan.md"
TOKEN = "abcdef.0123456789abcdef"


class FakeRunner:
    """Records every command and answers the ones the driver depends on."""

    def __init__(self, suite_results=None, fail=None):
        self.calls = []
        self.suite_results = suite_results or {}  # suite -> (rc, [lines])
        self.fail = fail or set()  # command words that exit 1
        self.env_seen = []

    @staticmethod
    def _word(argv):
        for word in ("run-local.sh", "launch-vm", "retire-lifecycle-vm", "cleanup", "run-suite", "list-suite",
                     "collect-isolation-evidence", "collect-logs"):
            if any(word in a for a in argv):
                return word
        return argv[0]

    def run(self, argv, env, cwd, on_line, timeout):
        word = self._word(argv)
        self.calls.append(word)
        self.env_seen.append((word, env.get("ISOLATION_MODE"), env.get("HOST_BASE_OS")))
        if word == "run-suite":
            suite = argv[argv.index("--suite") + 1]
            rc, lines = self.suite_results[suite]
            for line in lines:
                on_line(line)
            return rc
        on_line(f"[INFO]  {word} output")
        return 1 if word in self.fail else 0

    def capture(self, argv, env, cwd, timeout):
        word = self._word(argv)
        if argv[0] == "sh" or argv[0] == "git" or "--version" in argv or argv[0] in ("kind", "kubectl", "docker"):
            return 0, "tool 1.0\n"
        self.calls.append(word)
        if word == "list-suite":
            return 0, "\n".join(e2e.SUITES[argv[argv.index("--suite") + 1]]) + "\n"
        if word == "collect-isolation-evidence":
            return 0, 'noise\nISOLATION_EVIDENCE {"machine": {"observed": "nspawn"}}\n'
        return 0, ""


def passing(suite):
    steps = e2e.SUITES[suite]
    lines = [f"[INFO]  [{i}/{len(steps)}] {suite}: {s}" for i, s in enumerate(steps, start=1)]
    lines.append(f"[INFO]  suite '{suite}' PASSED ({len(steps)} steps)")
    return 0, lines


def failing(suite, at):
    steps = e2e.SUITES[suite][:at]
    lines = [f"[INFO]  [{i}/{len(e2e.SUITES[suite])}] {suite}: {s}" for i, s in enumerate(steps, start=1)]
    lines.append(f"[ERROR] boom with token {TOKEN}")
    return 1, lines


class Clock:
    def __init__(self):
        self.now = 1_700_000_000.0

    def __call__(self):
        self.now += 7.0
        return self.now


class CatalogTests(unittest.TestCase):
    def test_ids_are_unique(self):
        ids = [s.id for s in plan.CATALOG]
        self.assertEqual(len(ids), len(set(ids)))

    def test_every_suite_exists_and_has_known_steps(self):
        e2e.validate_suites()
        for scenario in plan.CATALOG:
            if scenario.suite is not None:
                self.assertIn(scenario.suite, e2e.SUITES, scenario.id)

    def test_modes_are_valid_and_unset_means_nspawn(self):
        for scenario in plan.CATALOG:
            self.assertIn(scenario.isolation_mode, ("", *e2e.ISOLATION_MODES), scenario.id)
            self.assertEqual(scenario.start_mode, scenario.isolation_mode or "nspawn")

    def test_scenarios_without_a_suite_are_skipped(self):
        for scenario in plan.CATALOG:
            if scenario.suite is None:
                self.assertTrue(plan.skip_reason(scenario, "ubuntu2404"), scenario.id)

    def test_default_and_every_mode_are_covered(self):
        modes = {s.isolation_mode for s in plan.CATALOG if s.suite == "isolation-operations"}
        self.assertEqual(modes, {"", *e2e.ISOLATION_MODES})

    def test_every_scenario_is_in_the_plan_document(self):
        text = PLAN_DOC.read_text()
        for scenario in plan.CATALOG:
            self.assertRegex(text, rf"\b{scenario.id}\b", scenario.id)

    def test_plan_document_lists_every_suite_it_uses(self):
        text = PLAN_DOC.read_text()
        for suite in {s.suite for s in plan.CATALOG if s.suite}:
            self.assertIn(suite, text)

    def test_the_rollback_scenario_starts_in_the_default_mode(self):
        """The refusal check moves the node from nspawn to none, which needs a
        reprovision; it starts in the default mode like the other transitions."""
        t03 = next(s for s in plan.CATALOG if s.id == "T03")
        self.assertEqual(t03.suite, "isolation-rollback")
        self.assertEqual(t03.isolation_mode, "")

    def test_reset_scenarios_cover_every_mode_and_a_reprovisioned_node(self):
        """The uninstall script is gone; `unbounded-agent reset` on the host is
        what the reset scenarios check, in every mode and after a reprovision."""
        resets = [s for s in plan.CATALOG if s.category == "reset"]
        self.assertEqual({s.start_mode for s in resets if s.suite == "isolation-reset"}, set(e2e.ISOLATION_MODES))
        self.assertIn("isolation-reset-after-reprovision", {s.suite for s in resets})
        for scenario in plan.CATALOG:
            self.assertNotIn("uninstall", " ".join([scenario.title, scenario.description, scenario.suite or ""]))

    def test_text_rules(self):
        for scenario in plan.CATALOG:
            blob = " ".join([scenario.title, scenario.description, scenario.expected, scenario.notes])
            self.assertNotIn("\u2014", blob)
            self.assertIsNone(re.search(r"behaviour|initialis|labelled|catalogue", blob), scenario.id)

    def test_matrix_suite_covers_every_ordered_pair(self):
        mode, pairs = "nspawn", set()
        for step in e2e.SUITES["isolation-matrix"]:
            target = plan.report.parse_change_step(step)
            if target:
                pairs.add((mode, target))
                mode = target
        want = {(a, b) for a in e2e.ISOLATION_MODES for b in e2e.ISOLATION_MODES if a != b}
        self.assertEqual(pairs & want, want)


class SelectTests(unittest.TestCase):
    def test_filters(self):
        self.assertEqual([s.id for s in plan.select(["T01", "D01"], [], [], [])], ["D01", "T01"])
        self.assertTrue(all(s.category == "reset" for s in plan.select([], ["reset"], [], [])))
        self.assertTrue(plan.select([], ["reset"], [], []))
        self.assertTrue(all("none" in s.tags for s in plan.select([], [], ["none"], [])))
        self.assertFalse([s for s in plan.select([], [], [], ["transition"]) if "transition" in s.tags])

    def test_unknown_id_is_an_error(self):
        with self.assertRaises(SystemExit):
            plan.select(["Z99"], [], [], [])

    def test_skip_reasons(self):
        acl = plan.Scenario("Z01", "needs acl", "operations", "isolation-operations", "", "", "",
                            requires=("host-os:acl",))
        self.assertIn("acl", plan.skip_reason(acl, "ubuntu2404"))
        self.assertEqual(plan.skip_reason(acl, "acl"), "")
        self.assertIn("GPU", plan.skip_reason(plan.SCENARIOS_BY_ID["G01"], "acl"))


class DriverTests(unittest.TestCase):
    def setUp(self):
        scratch = REPO_ROOT / "tmp"
        scratch.mkdir(exist_ok=True)
        self.tmp = tempfile.TemporaryDirectory(dir=scratch)
        self.addCleanup(self.tmp.cleanup)
        self.out = Path(self.tmp.name)

    def driver(self, runner, **kw):
        return plan.Driver(runner, out_root=self.out, clock=Clock(), base_env={"PATH": "/usr/bin"}, **kw)

    def scenarios(self, *ids):
        return [plan.SCENARIOS_BY_ID[i] for i in ids]

    def test_runs_every_scenario_after_a_failure_and_writes_the_report(self):
        runner = FakeRunner({"isolation-matrix": passing("isolation-matrix"),
                             "isolation-operations": failing("isolation-operations", 3)})
        results = self.driver(runner).execute(self.scenarios("T01", "X01", "G01"), "r1")
        status = {s["id"]: s["status"] for s in results["scenarios"]}
        self.assertEqual(status, {"T01": "pass", "X01": "fail", "G01": "skip"})
        self.assertEqual(results["summary"]["fail"], 1)

        run_dir = self.out / "r1"
        for name in ("results.json", "report.md", "report.html", "infra-up.log", "infra-down.log", "T01/suite.log"):
            self.assertTrue((run_dir / name).exists(), name)
        saved = json.loads((run_dir / "results.json").read_text())
        self.assertEqual(saved["run_id"], "r1")

        failed = results["scenarios"][1]
        self.assertEqual([s["status"] for s in failed["steps"]][:4], ["pass", "pass", "fail", "not-run"])
        self.assertEqual(failed["failure"]["step"], e2e.SUITES["isolation-operations"][2])
        self.assertNotIn(TOKEN, json.dumps(saved))
        self.assertNotIn(TOKEN, (run_dir / "X01" / "suite.log").read_text())
        self.assertEqual(results["scenarios"][0]["evidence"], {"machine": {"observed": "nspawn"}})
        self.assertIn("X01", (run_dir / "report.md").read_text())

    def test_vm_lifecycle_and_environment_per_scenario(self):
        runner = FakeRunner({"isolation-matrix": passing("isolation-matrix"),
                             "isolation-operations": passing("isolation-operations")})
        self.driver(runner).execute(self.scenarios("T01", "X01"), "r2")
        self.assertEqual([c for c in runner.calls if c in ("run-local.sh", "launch-vm", "retire-lifecycle-vm", "cleanup")],
                         ["run-local.sh", "launch-vm", "retire-lifecycle-vm", "launch-vm", "cleanup"])
        modes = [m for word, m, _ in runner.env_seen if word == "run-suite"]
        self.assertEqual(modes, [None, "none"], "an unset ISOLATION_MODE stays unset")
        self.assertTrue(all(os == "ubuntu2404" for _, _, os in runner.env_seen))

    def test_a_stale_isolation_mode_in_the_environment_is_removed(self):
        runner = FakeRunner({"isolation-matrix": passing("isolation-matrix")})
        driver = plan.Driver(runner, out_root=self.out, clock=Clock(), base_env={"ISOLATION_MODE": "none"})
        driver.execute(self.scenarios("T01"), "r3")
        self.assertEqual([m for word, m, _ in runner.env_seen if word == "run-suite"], [None])

    def test_fail_fast_stops_after_the_first_failure(self):
        runner = FakeRunner({"isolation-operations": failing("isolation-operations", 2)})
        results = self.driver(runner, fail_fast=True).execute(self.scenarios("X01", "N01"), "r4")
        self.assertEqual([s["status"] for s in results["scenarios"]], ["fail", "skip"])
        self.assertIn("fail-fast", results["scenarios"][1]["skip_reason"])
        self.assertEqual(runner.calls.count("run-suite"), 1)

    def test_infrastructure_failure_marks_scenarios_as_errors_and_skips_the_rest(self):
        runner = FakeRunner(fail={"run-local.sh"})
        results = self.driver(runner).execute(self.scenarios("T01", "G01"), "r5")
        self.assertEqual([s["status"] for s in results["scenarios"]], ["error", "skip"])
        self.assertIn("infra-up.log", results["scenarios"][0]["failure"]["message"])
        self.assertNotIn("run-suite", runner.calls)
        self.assertNotIn("cleanup", runner.calls)

    def test_vm_launch_failure_is_an_error_not_a_test_failure(self):
        runner = FakeRunner(fail={"launch-vm"})
        results = self.driver(runner).execute(self.scenarios("T01"), "r6")
        self.assertEqual(results["scenarios"][0]["status"], "error")
        self.assertEqual(results["scenarios"][0]["failure"]["step"], "launch-vm")

    def test_no_up_and_keep_env_create_and_remove_nothing(self):
        runner = FakeRunner({"isolation-matrix": passing("isolation-matrix")})
        self.driver(runner, no_up=True).execute(self.scenarios("T01"), "r7")
        self.assertNotIn("run-local.sh", runner.calls)
        self.assertNotIn("cleanup", runner.calls)
        runner = FakeRunner({"isolation-matrix": passing("isolation-matrix")})
        self.driver(runner, keep_env=True).execute(self.scenarios("T01"), "r8")
        self.assertIn("run-local.sh", runner.calls)
        self.assertNotIn("cleanup", runner.calls)

    def test_only_skipped_scenarios_need_no_infrastructure(self):
        runner = FakeRunner()
        results = self.driver(runner).execute(self.scenarios("G01"), "r9")
        self.assertEqual([s["status"] for s in results["scenarios"]], ["skip"])
        self.assertNotIn("run-local.sh", runner.calls)

    def test_steps_carry_the_mode_they_ran_in(self):
        runner = FakeRunner({"isolation-matrix": passing("isolation-matrix")})
        results = self.driver(runner).execute(self.scenarios("T01"), "r10")
        steps = results["scenarios"][0]["steps"]
        names = e2e.SUITES["isolation-matrix"]
        # The last change to none is made from nspawn; the steps after it run in none.
        last = max(i for i, name in enumerate(names) if name == "change-to-none")
        self.assertEqual(steps[last]["name"], "change-to-none")
        self.assertEqual(steps[last]["mode"], "nspawn")
        self.assertEqual(steps[last + 1]["mode"], "none")


class CommandLineTests(unittest.TestCase):
    def test_run_defaults(self):
        args = plan.parse_args(["run"])
        self.assertEqual((args.scenario, args.fail_fast, args.no_up, args.keep_env), ([], False, False, False))

    def test_plan_table_lists_every_scenario(self):
        table = plan.plan_markdown()
        for scenario in plan.CATALOG:
            self.assertIn(f"| {scenario.id} |", table)


if __name__ == "__main__":
    unittest.main()
