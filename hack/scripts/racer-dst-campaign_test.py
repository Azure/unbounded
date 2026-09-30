# SPDX-License-Identifier: Apache-2.0
"""Campaign control-flow tests with fake commands; no Cargo or real DST runs."""

import contextlib
import importlib.util
import io
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import patch


ROOT = Path(__file__).resolve().parents[2]
SPEC = importlib.util.spec_from_file_location("campaign", Path(__file__).with_name("racer-dst-campaign.py"))
MODULE = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(MODULE)


class FakeRunner:
    def __init__(self, fail=None, duration=1):
        self.calls = []
        self.messages = []
        self.now = 0
        self.fail = fail
        self.duration = duration

    def report(self, message):
        self.messages.append(message)

    def run(self, command, environment, label, limit=None):
        self.calls.append((command, environment, label, limit))
        self.now += self.duration
        if label == self.fail:
            raise MODULE.Failure("fake failure " + label)
        count = 2 if label.startswith("seed-") else 1
        return f"test result: ok. {count} passed; 0 failed; 0 ignored; 0 measured;"


class CampaignTests(unittest.TestCase):
    def run_campaign(self, runner, seconds=8):
        tests = [("binary", name) for name in ["regression", *MODULE.GENERATED]]
        MODULE.campaign(runner, tests, "binary", {}, seconds, clock=lambda: runner.now)

    def test_baseline_then_distinct_paired_seeds_with_graceful_finish(self):
        runner = FakeRunner(duration=2)
        self.run_campaign(runner, seconds=11)
        self.assertEqual([c[2] for c in runner.calls], [
            "baseline-1", "baseline-2", "baseline-3", "seed-100", "seed-101", "seed-102",
        ])
        for command, environment, label, _ in runner.calls:
            self.assertIn("--exact", command)
            if label.startswith("baseline"):
                self.assertNotIn("RACER_DST_SEEDS", environment)
                self.assertNotIn("RACER_DST_STEPS", environment)
            else:
                self.assertEqual(environment["RACER_DST_SEEDS"], label.removeprefix("seed-"))
                self.assertEqual(command[1:3], list(MODULE.GENERATED))
        self.assertIn("completed_seed_pairs=3", runner.messages[-1])
        self.assertIn("elapsed_seconds=12.000", runner.messages[-1])

    def test_failure_stops_immediately_and_counts_only_complete_pairs(self):
        for label, count in [("baseline-2", 0), ("seed-101", 1)]:
            with self.subTest(label=label):
                runner = FakeRunner(fail=label)
                with self.assertRaisesRegex(MODULE.Failure, "fake failure"):
                    self.run_campaign(runner)
                self.assertEqual(runner.calls[-1][2], label)
                self.assertIn(f"completed_seed_pairs={count}", runner.messages[-1])

    def test_budget_cannot_silently_skip_baseline_or_all_fresh_seeds(self):
        for seconds, message in [(1, "baseline incomplete"), (3, "no fresh")]:
            with self.subTest(seconds=seconds), self.assertRaisesRegex(MODULE.Failure, message):
                self.run_campaign(FakeRunner(), seconds)

    def test_zero_tests_or_ignored_tests_are_not_success(self):
        runner = FakeRunner()
        for result in ["", "test result: ok. 0 passed; 0 failed; 1 ignored;"]:
            with patch.object(runner, "run", return_value=result), \
                    self.assertRaisesRegex(MODULE.Failure, "test count"):
                MODULE.run_tests(runner, "binary", ["test"], {}, "missing")

    def test_build_once_and_discovery_requires_both_oracles(self):
        runner = FakeRunner()
        artifact = json.dumps({"reason": "compiler-artifact", "profile": {"test": True},
                               "executable": "binary"})
        listing = "\n".join(name + ": test" for name in ["regression_dst", *MODULE.GENERATED])
        with patch.object(runner, "run", side_effect=[artifact, listing]) as run:
            tests, binary = MODULE.discover(runner, ["cargo"], "target", {})
        self.assertEqual(binary, "binary")
        self.assertEqual(len(tests), 3)
        self.assertEqual(run.call_count, 2)
        self.assertIn("--no-run", run.call_args_list[0].args[0])
        self.assertEqual(run.call_args_list[0].kwargs["limit"], 300)
        for output in ["", MODULE.GENERATED[0] + ": test"]:
            with patch.object(runner, "run", side_effect=[artifact, output]), \
                    self.assertRaisesRegex(MODULE.Failure, "both generated"):
                MODULE.discover(runner, ["cargo"], "target", {})

    def test_rejects_invalid_configuration(self):
        for option in ["--seconds", "--case-timeout"]:
            for value in ["0", "-1", "nan", "inf", "601"]:
                with self.subTest(option=option, value=value), \
                        contextlib.redirect_stderr(io.StringIO()), self.assertRaises(SystemExit):
                    MODULE.parser().parse_args(["--target-dir", "target", option, value])
        for setting in ["RACER_DST_SEEDS", "RACER_DST_STEPS", "RACER_DST_TYPO", "RACER_TEST_ARGS"]:
            with patch.dict(os.environ, {setting: "1"}), \
                    patch.object(sys, "argv", ["runner", "--target-dir", "target", "--", "cargo"]), \
                    patch.object(MODULE.subprocess, "run") as command, \
                    self.assertRaisesRegex(MODULE.Failure, "overrides"):
                MODULE.main()
            command.assert_not_called()

    def test_guard_failure_prevents_build(self):
        with patch.dict(os.environ, {}, clear=True), \
                patch.object(sys, "argv", ["runner", "--target-dir", "target", "--", "cargo"]), \
                patch.object(MODULE.subprocess, "run", side_effect=subprocess.CalledProcessError(125, "guard")) as guard, \
                patch.object(MODULE, "discover") as build, self.assertRaises(subprocess.CalledProcessError):
            MODULE.main()
        self.assertIn("--verify-exec", guard.call_args.args[0])
        build.assert_not_called()


class CommandTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(dir=ROOT / "tmp")
        self.addCleanup(self.temp.cleanup)
        self.runner = MODULE.Runner(Path(self.temp.name), 0.2)
        self.environment = dict(os.environ)

    def command(self, source):
        return self.runner.run([sys.executable, "-c", source], self.environment, "fake")

    def test_success_preserves_logs_and_reproduction(self):
        self.assertEqual(self.command("print('fake success')"), "fake success\n")
        progress = self.runner.progress.read_text()
        self.assertIn("reproduce: env -u RACER_DST_SEEDS -u RACER_DST_STEPS", progress)
        self.assertIn("--kill-after=10s", progress)
        self.assertIn("PASS fake", progress)

    def test_failure_is_not_budget_exhaustion(self):
        with self.assertRaisesRegex(MODULE.Failure, "COMMAND FAILURE.*exit=7"):
            self.command("import sys; print('oracle failure'); sys.exit(7)")
        self.assertIn("oracle failure", (Path(self.temp.name) / "0001-fake.log").read_text())

    def test_kill_is_not_assumed_to_be_timeout(self):
        with self.assertRaisesRegex(MODULE.Failure, "COMMAND KILLED.*external/OOM"):
            self.command("import sys; sys.exit(137)")

    def test_interruption_cleans_up_running_command(self):
        original = subprocess.Popen
        children = []

        def interrupting_process(*args, **kwargs):
            process = original(*args, **kwargs)
            children.append(process)
            wait = process.wait
            first = True

            def interrupt_once(*args, **kwargs):
                nonlocal first
                if first:
                    first = False
                    raise KeyboardInterrupt()
                return wait(*args, **kwargs)

            process.wait = interrupt_once
            return process

        with patch.object(MODULE.subprocess, "Popen", side_effect=interrupting_process), \
                self.assertRaises(KeyboardInterrupt):
            self.command("import time; time.sleep(60)")
        self.assertIsNotNone(children[0].poll())

    def test_timeout_kills_descendants(self):
        pidfile = Path(self.temp.name) / "child.pid"
        source = ("import subprocess, sys, time; "
                  "p = subprocess.Popen([sys.executable, '-c', 'import time; time.sleep(60)']); "
                  f"open({str(pidfile)!r}, 'w').write(str(p.pid)); time.sleep(60)")
        with self.assertRaisesRegex(MODULE.Failure, "PER-COMMAND TIMEOUT"):
            self.command(source)
        pid = int(pidfile.read_text())
        # A zombie is dead but may await reaping by the container's init.
        status = Path(f"/proc/{pid}/stat")
        if status.exists():
            self.assertEqual(status.read_text().split()[2], "Z")


if __name__ == "__main__":
    unittest.main()
