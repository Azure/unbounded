# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0

import json
import os
import unittest
from pathlib import Path
from unittest.mock import patch

import isolation_report as report

TESTDATA = Path(__file__).resolve().parent / "testdata"


def log_lines(*texts, start=1000.0, step=10.0):
    return [(start + i * step, text) for i, text in enumerate(texts)]


class RedactTests(unittest.TestCase):
    def test_bootstrap_token_and_json_secret_are_removed(self):
        text = 'token abcdef.0123456789abcdef and "BootstrapToken": "s3cret" and "token": "x"'
        out = report.redact(text)
        self.assertNotIn("abcdef.0123456789abcdef", out)
        self.assertNotIn("s3cret", out)
        self.assertIn('"BootstrapToken": "<redacted>"', out)

    def test_plain_text_is_unchanged(self):
        self.assertEqual(report.redact("node ready in 12.5s"), "node ready in 12.5s")


class ParseSuiteLogTests(unittest.TestCase):
    PLANNED = ["run-agent", "wait-for-node", "validate-workload"]

    def test_passing_suite_has_every_step_passed_with_durations(self):
        lines = log_lines(
            "[INFO]  [1/3] s: run-agent",
            "[INFO]  [2/3] s: wait-for-node",
            "[INFO]  [3/3] s: validate-workload",
            "[INFO]  suite 's' PASSED (3 steps)",
        )
        parsed = report.parse_suite_log(lines, self.PLANNED, finished=1045.0, suite="s", returncode=0)
        self.assertTrue(parsed["suite_passed"])
        self.assertIsNone(parsed["failure"])
        self.assertEqual([s["status"] for s in parsed["steps"]], ["pass"] * 3)
        self.assertEqual([s["duration_s"] for s in parsed["steps"]], [10.0, 10.0, 25.0])

    def test_failure_marks_the_running_step_and_the_rest_not_run(self):
        lines = log_lines(
            "[INFO]  [1/3] s: run-agent",
            "[INFO]  [2/3] s: wait-for-node",
            "[ERROR] node did not become Ready token abcdef.0123456789abcdef",
        )
        parsed = report.parse_suite_log(lines, self.PLANNED, finished=1100.0, suite="s", returncode=1)
        self.assertFalse(parsed["suite_passed"])
        self.assertEqual([s["status"] for s in parsed["steps"]], ["pass", "fail", "not-run"])
        self.assertEqual(parsed["failure"]["step"], "wait-for-node")
        self.assertIn("did not become Ready", parsed["failure"]["message"])
        self.assertNotIn("abcdef.0123456789abcdef", parsed["failure"]["log_excerpt"])

    def test_nonzero_exit_without_passed_line_is_a_failure(self):
        lines = log_lines("[INFO]  [1/3] s: run-agent", "[INFO]  suite 's' PASSED (3 steps)")
        parsed = report.parse_suite_log(lines, self.PLANNED, finished=1100.0, suite="s", returncode=1)
        self.assertFalse(parsed["suite_passed"])
        self.assertEqual(parsed["steps"][0]["status"], "fail")

    def test_lines_from_other_suites_are_ignored(self):
        lines = log_lines("[INFO]  [1/9] other: run-agent")
        parsed = report.parse_suite_log(lines, self.PLANNED, finished=1010.0, suite="s", returncode=1)
        self.assertEqual([s["status"] for s in parsed["steps"]], ["not-run"] * 3)

    def test_empty_output_fails_with_a_message(self):
        parsed = report.parse_suite_log([], self.PLANNED, finished=1.0, suite="s", returncode=1)
        self.assertEqual(parsed["failure"]["message"], "no output")


class AnnotateTests(unittest.TestCase):
    def test_mode_follows_successful_changes(self):
        steps = [{"name": n, "status": "pass"} for n in (
            "validate-workload", "change-to-none", "validate-workload",
            "change-to-nspawn", "validate-isolation-status")]
        report.annotate_steps("nspawn", steps)
        self.assertEqual([s["mode"] for s in steps], ["nspawn", "nspawn", "none", "none", "nspawn"])
        self.assertEqual(steps[1]["transition"], {"from": "nspawn", "to": "none", "strategy": "reprovision"})
        self.assertEqual(steps[3]["transition"], {"from": "none", "to": "nspawn", "strategy": "reprovision"})
        self.assertEqual(steps[0]["operation"], "Workload and DNS")

    def test_the_strategy_follows_from_the_pair(self):
        steps = [{"name": n, "status": "pass"} for n in ("change-to-none", "change-to-nspawn")]
        report.annotate_steps("nspawn", steps)
        self.assertEqual([s["transition"]["strategy"] for s in steps], ["reprovision", "reprovision"])
        self.assertEqual(steps[0]["operation"], "Mode change to none (reprovision)")

    def test_failed_change_keeps_the_source_mode(self):
        steps = [{"name": "change-to-none", "status": "fail"},
                 {"name": "validate-workload", "status": "not-run"}]
        report.annotate_steps("nspawn", steps)
        self.assertEqual(steps[1]["mode"], "nspawn")

    def test_parse_change_step(self):
        self.assertEqual(report.parse_change_step("change-to-nspawn"), "nspawn")
        self.assertEqual(report.parse_change_step("change-to-none"), "none")
        self.assertIsNone(report.parse_change_step("validate-workload"))
        self.assertIsNone(report.parse_change_step("change-to-vm"))
        # A mode this harness does not know is not a change step.
        self.assertIsNone(report.parse_change_step("change-to-container"))
        self.assertIsNone(report.parse_change_step("migrate-to-none-reprovision"))

    def test_change_strategy(self):
        # none has no slot, so no pair of nspawn and none changes in place.
        for pair in (("nspawn", "none"), ("none", "nspawn")):
            self.assertEqual(report.change_strategy(*pair), "reprovision", pair)
        self.assertEqual(report.STRATEGIES, ("reprovision",))

    def test_an_in_place_pair_is_reported_as_in_place(self):
        """IN_PLACE_PAIRS is where a mode with slots adds its pairs."""
        with patch.object(report, "IN_PLACE_PAIRS", frozenset({("nspawn", "slotted")})):
            self.assertEqual(report.change_strategy("nspawn", "slotted"), "inPlace")
            self.assertEqual(report.change_strategy("slotted", "nspawn"), "reprovision")


class MatrixTests(unittest.TestCase):
    def scenarios(self):
        steps = [
            {"name": "validate-workload", "status": "pass"},
            {"name": "change-to-none", "status": "pass"},
            {"name": "validate-workload", "status": "fail"},
            {"name": "change-to-nspawn", "status": "not-run"},
        ]
        report.annotate_steps("nspawn", steps)
        return [{"id": "T", "status": "fail", "steps": steps}]

    def test_coverage_uses_the_worst_status_per_mode(self):
        cov = report.coverage_matrix(self.scenarios())
        self.assertEqual(cov["Workload and DNS"], {"nspawn": "pass", "none": "fail"})

    def test_transitions_are_listed_by_pair_and_strategy(self):
        trans = report.transition_matrix(self.scenarios())
        self.assertEqual(trans["nspawn -> none"], {"reprovision": "pass"})
        self.assertEqual(trans["none -> nspawn"], {"reprovision": "not-run"})

    def test_summarize_counts(self):
        counts = report.summarize([{"status": "pass"}, {"status": "fail"}, {"status": "skip"}, {"status": "pass"}])
        self.assertEqual((counts["pass"], counts["fail"], counts["skip"], counts["error"], counts["total"]), (2, 1, 1, 0, 4))


class RenderTests(unittest.TestCase):
    def results(self):
        return json.loads((TESTDATA / "isolation_results.json").read_text())

    def test_markdown_matches_golden(self):
        got = report.render_markdown(self.results())
        golden = TESTDATA / "isolation_report.md"
        if os.environ.get("UPDATE_GOLDEN") == "1":
            golden.write_text(got)
        self.assertEqual(got, golden.read_text())

    def test_markdown_has_no_em_dash_and_names_every_scenario(self):
        got = report.render_markdown(self.results())
        self.assertNotIn("\u2014", got)
        for scenario in self.results()["scenarios"]:
            self.assertIn(scenario["id"], got)

    def test_html_escapes_content_and_colors_results(self):
        results = self.results()
        results["scenarios"][1]["failure"]["message"] = "<script>alert(1)</script>"
        page = report.render_html(results)
        self.assertNotIn("<script>alert(1)</script>", page)
        self.assertIn("&lt;script&gt;", page)
        self.assertIn('class="PASS"', page)
        self.assertIn('class="FAIL"', page)

    def test_main_writes_markdown_and_html(self):
        import tempfile

        scratch = Path(__file__).resolve().parents[3] / "tmp"
        scratch.mkdir(exist_ok=True)
        with tempfile.TemporaryDirectory(dir=scratch) as tmp:
            target = Path(tmp) / "results.json"
            target.write_text((TESTDATA / "isolation_results.json").read_text())
            self.assertEqual(report.main([str(target), "--html"]), 0)
            self.assertTrue((Path(tmp) / "report.md").exists())
            self.assertTrue((Path(tmp) / "report.html").exists())


if __name__ == "__main__":
    unittest.main()
