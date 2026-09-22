# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0
import argparse
import contextlib
import io
import json
from pathlib import Path
import unittest
from unittest import mock

import run as runner

from run import campaign_summary, classify, coverage_weights, deletions, execute, exploration_input, failure_identity, gate, kernel_outcome, nightly_samples, preserves_witnesses, reductions, sampled_input, selected_names, witness_signature


class DiskCapacityTests(unittest.TestCase):
    def setUp(self):
        self.stack = contextlib.ExitStack()
        self.addCleanup(self.stack.close)
        self.files = {}
        for name in ("baseline", "campaign"):
            path = runner.ROOT / f"dst/scenarios/{name}.json"
            self.files[str(path)] = path.read_text()
        original_read = Path.read_text
        original_exists = Path.exists
        self.stack.enter_context(mock.patch.object(Path, "exists",
            lambda path: str(path) in self.files or original_exists(path)))

        def read(path, *args, **kwargs):
            return self.files[str(path)] if str(path) in self.files else original_read(path, *args, **kwargs)

        self.stack.enter_context(mock.patch.object(Path, "read_text", read))
        self.stack.enter_context(mock.patch.object(Path, "write_text",
            lambda path, value: self.files.__setitem__(str(path), value)))
        self.stack.enter_context(mock.patch.object(Path, "mkdir"))
        self.stack.enter_context(mock.patch.object(Path, "open", mock.mock_open()))
        self.stack.enter_context(mock.patch.dict(runner.os.environ, {"TMPDIR": str(runner.ROOT)}, clear=True))
        self.stack.enter_context(contextlib.redirect_stdout(io.StringIO()))
        self.stderr = self.stack.enter_context(contextlib.redirect_stderr(io.StringIO()))
        self.usage = self.stack.enter_context(mock.patch.object(runner.shutil, "disk_usage",
            return_value=argparse.Namespace(total=1000, used=800, free=200)))
        self.build = self.stack.enter_context(mock.patch.object(runner, "build",
            return_value=(runner.ROOT / "fake-libtest", 1)))
        self.execute = self.stack.enter_context(mock.patch.object(runner, "execute",
            return_value=(0, "test result: ok. 1 passed; 0 failed; 0 ignored;", False, 1)))
        self.stack.enter_context(mock.patch.object(runner, "checked", return_value="test"))
        self.stack.enter_context(mock.patch.object(runner, "digest", return_value="hash"))
        self.stack.enter_context(mock.patch.object(runner, "inventory", return_value=[
            {"selector": "simulation::test", "suite": "simulator-contracts", "ignored": False}]))
        self.stack.enter_context(mock.patch.object(runner, "coverage", return_value={"transitions": {}}))
        self.directory = runner.ROOT / "dst/disk-unit-test-not-created"
        self.args = argparse.Namespace(artifacts=self.directory, scenario="simulator-contracts",
            profile="pr", input=None, seed=19, min_free_disk_bytes=100, disk_path=[])

    def result(self, name="result.json"):
        return json.loads(self.files[str(self.directory / name)])

    def test_boundary_and_missing_destination_do_not_create_files(self):
        evidence = runner.disk_snapshot(self.directory / "nested", "build", 200, building=True)
        self.assertTrue(evidence["sufficient"])
        self.assertEqual(evidence["paths"][0]["observed_path"], str(runner.ROOT / "dst"))
        self.assertEqual(evidence["paths"][0]["free_bytes"], 200)
        self.assertFalse(runner.disk_snapshot(self.directory, "test", 201)["sufficient"])
        Path.mkdir.assert_not_called()

    def test_checks_scratch_target_cargo_home_and_additional_destinations(self):
        with mock.patch.dict(runner.os.environ, {"CARGO_TARGET_DIR": "custom-target",
                "CARGO_HOME": str(runner.ROOT / "custom-cargo"),
                "CARGO_BUILD_BUILD_DIR": "custom-intermediates"}):
            evidence = runner.disk_snapshot(self.directory, "build", 100, True,
                                             [runner.ROOT / "custom-storage"])
        paths = {p["role"]: p["path"] for p in evidence["paths"]}
        self.assertEqual(paths["scratch"], str(runner.ROOT))
        self.assertEqual(paths["cargo-target"], str(runner.ROOT / "custom-target"))
        self.assertEqual(paths["cargo-build"], str(runner.ROOT / "custom-intermediates"))
        self.assertEqual(paths["cargo-home"], str(runner.ROOT / "custom-cargo"))
        self.assertEqual(paths["additional"], str(runner.ROOT / "custom-storage"))
        self.assertEqual({p["role"] for p in runner.disk_snapshot(self.directory, "test", 100)["paths"]},
                         {"artifacts", "scratch"})

    def test_low_scratch_is_not_hidden_by_free_artifact_storage(self):
        self.usage.side_effect = [argparse.Namespace(total=1000, used=800, free=200),
                                 argparse.Namespace(total=1000, used=999, free=1)]
        with self.assertRaises(runner.DiskPrerequisiteError) as raised:
            runner.disk_check(self.directory, "test", 100)
        self.assertTrue(raised.exception.evidence["paths"][0]["sufficient"])
        self.assertFalse(raised.exception.evidence["paths"][1]["sufficient"])

    def test_disk_stat_error_and_file_destination_fail_closed(self):
        self.usage.side_effect = PermissionError("disk stat denied")
        evidence = runner.disk_snapshot(self.directory, "test", 100)
        self.assertFalse(evidence["sufficient"])
        self.assertIn("disk stat denied", evidence["paths"][0]["error"])
        self.usage.side_effect = None
        evidence = runner.disk_snapshot(runner.MANIFEST / "child", "test", 100)
        self.assertFalse(evidence["paths"][0]["sufficient"])
        self.assertIn(str(runner.MANIFEST), evidence["paths"][0]["error"])

    def test_evidence_write_failure_blocks_admission_and_prints_json(self):
        with mock.patch.object(Path, "open", side_effect=OSError("disk full")):
            with self.assertRaises(runner.DiskPrerequisiteError):
                runner.disk_check(self.directory, "build", 100)
        emitted = json.loads(self.stderr.getvalue())
        self.assertEqual(emitted["outcome"], "infrastructure_failure")
        self.assertEqual(emitted["disk_capacity"]["evidence_write_error"], "disk full")

    def test_prebuild_failure_preserves_native_denominator_without_attempts(self):
        self.args.profile, self.args.scenario = "native", None
        self.usage.return_value.free = 99
        self.assertEqual(runner.run(self.args), 1)
        result = self.result()
        self.assertEqual(result["planned"], 3)
        self.assertFalse(result["complete"])
        self.assertFalse(result["runs"][0]["attempted"])
        self.assertEqual(result["runs"][0]["outcome"], "infrastructure_failure")
        self.assertEqual(result["runs"][0]["disk_capacity"]["phase"], "build")
        self.build.assert_not_called()
        self.execute.assert_not_called()

    def test_build_consumption_blocks_tests_and_records_evidence(self):
        def build(*args):
            self.usage.return_value.free = 99
            return runner.ROOT / "fake-libtest", 1
        self.build.side_effect = build
        self.assertEqual(runner.run(self.args), 1)
        self.assertEqual(self.result()["planned"], 1)
        self.assertEqual(self.result()["runs"][0]["disk_capacity"]["phase"], "prepare-tests")
        self.execute.assert_not_called()

    def test_post_execution_pressure_does_not_reclassify_assertions_or_passes(self):
        for code, output, expected in [(101, "assertion failed: HTTP503\ntest result: FAILED.", "product_failure"),
                (0, "test result: ok. 1 passed; 0 failed; 0 ignored;", "pass")]:
            with self.subTest(expected=expected):
                self.usage.return_value.free = 200
                def execute(*args):
                    self.usage.return_value.free = 1
                    return code, output, False, 1
                self.execute.side_effect = execute
                self.assertEqual(runner.run(self.args), int(expected != "pass"))
                record = self.result()["runs"][0]
                self.assertEqual(record["outcome"], expected)
                self.assertTrue(record["disk_capacity_before"]["sufficient"])
                self.assertFalse(record["disk_capacity_after"]["sufficient"])

    def test_replay_admission_retains_input_and_hash_contract(self):
        self.usage.return_value.free = 1
        with mock.patch.object(Path, "unlink") as unlink, mock.patch.object(runner, "discover") as discover:
            self.assertEqual(runner.replay(self.directory, minimum=100), 1)
            self.assertEqual(self.result("replay-result.json")["outcome"], "infrastructure_failure")
            unlink.assert_not_called()
            discover.assert_not_called()
        self.execute.assert_not_called()
        self.usage.return_value.free = 200
        self.files[str(self.directory / "build.json")] = json.dumps({"binary": "retained", "binary_sha256": "wrong"})
        with mock.patch.object(runner, "discover") as discover:
            self.assertEqual(runner.replay(self.directory, minimum=100), 1)
            self.assertIn("recorded binary hash", self.result("replay-result.json")["error"])
            discover.assert_not_called()

    def test_campaign_admission_retains_all_cells_as_unattempted(self):
        self.args.tier, self.args.timeout = "pr", 30
        self.usage.return_value.free = 1
        self.assertEqual(runner.run_campaign(self.args), 1)
        result = self.result("campaign-result.json")
        self.assertEqual(result["outcome"], "infrastructure_failure")
        self.assertFalse(result["complete"])
        self.assertGreater(result["planned"], 0)
        self.assertEqual(result["coverage"]["planned"], result["planned"])
        self.assertEqual(result["coverage"]["attempted"], 0)
        self.assertEqual(result["coverage"]["exercised"], 0)
        self.assertTrue(all(gap["outcome"] == "not_attempted" for gap in result["coverage"]["gaps"]))
        self.build.assert_not_called()
        self.execute.assert_not_called()

    def test_admitted_replay_still_requires_matching_recorded_failure(self):
        self.files[str(self.directory / "build.json")] = json.dumps({"binary": "retained", "binary_sha256": "hash"})
        semantic = {"status": "product_failure", "failure": {"oracle": "response.status"}}
        self.files[str(self.directory / "semantic.json")] = json.dumps(semantic)
        self.files[str(self.directory / "replay-semantic.json")] = json.dumps(semantic)
        self.execute.return_value = (101, "test result: FAILED.", False, 1)
        with mock.patch.object(Path, "unlink"), mock.patch.object(runner, "discover", return_value=[runner.ADAPTER]):
            self.assertEqual(runner.replay(self.directory, minimum=100), 0)
            self.assertEqual(self.result("replay-result.json")["outcome"], "pass")
            self.files[str(self.directory / "replay-semantic.json")] = json.dumps({"status": "different"})
            self.assertEqual(runner.replay(self.directory, minimum=100), 1)
            self.assertEqual(self.result("replay-result.json")["outcome"], "product_failure")
        self.assertEqual(self.execute.call_args.args[2]["RACER_DST_MODE"], "exact")
        self.assertEqual(self.files[str(self.directory / "semantic.json")], json.dumps(semantic))

    def test_rejected_cell_is_not_counted_as_attempted(self):
        cells = [{"id": "blocked", "scenario": "artifact", "minimum_transitions": {}}]
        summary = runner.campaign_summary(cells, [{"cell": "blocked", "attempted": False,
                                                   "outcome": "infrastructure_failure"}])
        self.assertEqual((summary["planned"], summary["attempted"], summary["exercised"]), (1, 0, 0))

    def test_default_options_support_existing_namespace_callers(self):
        self.assertEqual(runner.disk_options(argparse.Namespace()),
                         {"minimum": 10 * 1024**3, "extra_paths": ()})
        with self.assertRaises(ValueError):
            runner.disk_snapshot(self.directory, "test", 0)

    def test_result_write_failure_emits_unsaved_result(self):
        result = {"planned": 3, "complete": False, "runs": []}
        with mock.patch.object(Path, "write_text", side_effect=OSError("no space")):
            with self.assertRaises(OSError):
                runner.save(self.directory / "result.json", result)
        emitted = json.loads(self.stderr.getvalue())
        self.assertEqual(emitted["outcome"], "infrastructure_failure")
        self.assertEqual(emitted["unsaved"], result)

    def test_log_write_failure_retains_product_assertion_and_attempt_count(self):
        original_write = Path.write_text
        def write(path, value):
            if path.suffix == ".log":
                raise OSError("no space for log")
            return original_write(path, value)
        self.execute.return_value = (101, "assertion failed\ntest result: FAILED.", False, 1)
        with mock.patch.object(Path, "write_text", write):
            self.assertEqual(runner.run(self.args), 1)
        runs = self.result()["runs"]
        self.assertEqual([r["outcome"] for r in runs], ["product_failure", "infrastructure_failure"])
        self.assertEqual(sum(r.get("attempted", True) for r in runs), 1)

    def test_semantic_postprocessing_failure_retains_executed_artifact(self):
        self.args.scenario = "artifact"
        entries = [{"selector": runner.ADAPTER, "suite": "cluster", "ignored": False}]
        self.execute.return_value = (101, "assertion failed\ntest result: FAILED.", False, 1)
        for semantic in ('{"status":', '{}', '[]', '{"status": "unknown"}'):
            with self.subTest(semantic=semantic), \
                    mock.patch.object(runner.shutil, "copy2"), \
                    mock.patch.object(runner, "inventory", return_value=entries):
                self.files[str(self.directory / "semantic.json")] = semantic
                self.assertEqual(runner.run(self.args), 1)
                result = self.result()
                self.assertFalse(result["complete"])
                self.assertEqual(result["planned"], 1)
                self.assertEqual([r["outcome"] for r in result["runs"]],
                                 ["product_failure", "infrastructure_failure"])
                self.assertEqual([r["attempted"] for r in result["runs"]], [True, False])
                self.assertEqual(result["runs"][0]["selectors"], [runner.ADAPTER])
                self.assertEqual(result["runs"][0]["exit_code"], 101)

    def test_postexecution_disk_observation_error_retains_suite_result(self):
        original_check = runner.disk_check
        def check(directory, phase, **options):
            if phase.startswith("after-suite:"):
                raise OSError("post-execution observation failed")
            return original_check(directory, phase, **options)
        self.execute.return_value = (101, "assertion failed\ntest result: FAILED.", False, 1)
        with mock.patch.object(runner, "disk_check", side_effect=check):
            self.assertEqual(runner.run(self.args), 1)
        result = self.result()
        self.assertEqual([r["outcome"] for r in result["runs"]],
                         ["product_failure", "infrastructure_failure"])
        self.assertEqual([r["attempted"] for r in result["runs"]], [True, False])

    def test_native_reporting_failures_preserve_probe_outcome_and_attempt(self):
        self.args.profile, self.args.scenario = "native", None
        selector = "uring::tests::kernel_integration"
        entries = [{"selector": selector, "suite": "remaining-library", "ignored": False}]
        original_write = Path.write_text
        for failed_path in ("kernel-capability.log", "capabilities.json", "result.json"):
            for code, output, outcome in [(101, "assertion failed\ntest result: FAILED.", "product_failure"),
                    (0, "test result: ok. 1 passed; 0 failed; 0 ignored;", "pass")]:
                with self.subTest(path=failed_path, outcome=outcome):
                    failed = False
                    def write(path, value):
                        nonlocal failed
                        if path.name == failed_path and self.execute.called and not failed:
                            failed = True
                            raise OSError("no space for native diagnostics")
                        return original_write(path, value)
                    self.execute.reset_mock()
                    self.execute.return_value = (code, output, False, 1)
                    with mock.patch.object(Path, "write_text", write), \
                            mock.patch.object(runner, "inventory", return_value=entries):
                        self.assertEqual(runner.run(self.args), 1)
                    result = self.result()
                    self.assertTrue(failed)
                    self.assertFalse(result["complete"])
                    self.assertEqual(result["planned"], 3)
                    self.assertEqual([r["outcome"] for r in result["runs"]],
                                     [outcome, "infrastructure_failure"])
                    self.assertEqual([r["attempted"] for r in result["runs"]], [True, False])
                    self.assertEqual(result["runs"][0]["suite"], "required-kernel")
                    self.assertEqual(result["runs"][0]["exit_code"], code)
                    self.execute.assert_called_once()

    def test_campaign_retains_cell_when_replay_cannot_save_diagnostics(self):
        self.args.tier, self.args.timeout = "pr", 30
        definition = {"schema": 1, "cells": [
            {"id": name, "scenario": "artifact", "seed": 19, "expected": "pass",
             "minimum_transitions": {"Response": 1}} for name in ("first", "pending")]}
        self.files[str(runner.ROOT / "dst/scenarios/campaign.json")] = json.dumps(definition)
        witnesses = {"terminal_record_present": True, "transitions": {"Response": 2}}
        original_write, original_read = Path.write_text, Path.read_text
        for diagnostics in (None, '{"outcome":'):
            with self.subTest(diagnostics=diagnostics):
                self.files.pop(str(self.directory / "first/replay-result.json"), None)
                def record(args, *unused):
                    runner.save(args.artifacts / "result.json", {"runs": [{"outcome": "product_failure", "attempted": True}]})
                    runner.save(args.artifacts / "semantic.json", {"status": "product_failure"})
                    runner.save(args.artifacts / "build.json", {"binary": "retained", "binary_sha256": "hash"})
                def write(path, value):
                    if path.name == "replay-result.json":
                        if diagnostics is not None:
                            self.files[str(path)] = diagnostics
                        raise OSError("no space for replay result")
                    return original_write(path, value)
                def read(path, *args, **kwargs):
                    if path.name == "replay-result.json" and str(path) not in self.files:
                        raise FileNotFoundError(str(path))
                    return original_read(path, *args, **kwargs)
                with mock.patch.object(runner, "run", side_effect=record) as run, \
                        mock.patch.object(Path, "write_text", write), \
                        mock.patch.object(Path, "read_text", read), mock.patch.object(Path, "unlink"), \
                        mock.patch.object(runner, "discover", return_value=[runner.ADAPTER]), \
                        mock.patch.object(runner, "coverage", return_value=witnesses):
                    self.assertEqual(runner.run_campaign(self.args), 1)
                    run.assert_called_once()
                result = self.result("campaign-result.json")
                self.assertFalse(result["complete"])
                self.assertEqual(result["outcome"], "infrastructure_failure")
                cell = result["runs"][0]
                self.assertTrue(cell["attempted"])
                self.assertEqual(cell["outcome"], "infrastructure_failure")
                self.assertEqual(cell["record_outcome"], "product_failure")
                self.assertFalse(cell["exact_replay"])
                self.assertEqual(cell["coverage"], witnesses)
                self.assertIn("replay diagnostics unavailable", cell["replay_failure"]["error"])
                self.assertEqual((result["coverage"]["planned"], result["coverage"]["attempted"],
                                  result["coverage"]["exercised"]), (2, 1, 0))
                self.assertEqual(result["coverage"]["transitions"], {"Response": 2})
                self.assertEqual(result["coverage"]["gaps"][1]["outcome"], "not_attempted")

    def test_campaign_replay_admission_failure_is_not_divergence(self):
        self.args.tier, self.args.timeout = "pr", 30
        definition = {"schema": 1, "cells": [
            {"id": name, "scenario": "artifact", "seed": 19, "expected": "pass", "minimum_transitions": {}}
            for name in ("first", "pending")]}
        self.files[str(runner.ROOT / "dst/scenarios/campaign.json")] = json.dumps(definition)
        def record(args, *unused):
            runner.save(args.artifacts / "result.json", {"runs": [{"outcome": "pass"}]})
            runner.save(args.artifacts / "semantic.json", {"status": "pass"})
            self.usage.return_value.free = 1
        with mock.patch.object(runner, "run", side_effect=record) as run:
            self.assertEqual(runner.run_campaign(self.args), 1)
            self.assertEqual(run.call_count, 1)
        result = self.result("campaign-result.json")
        self.assertFalse(result["complete"])
        self.assertEqual(result["runs"][0]["outcome"], "infrastructure_failure")
        self.assertEqual(result["runs"][0]["record_outcome"], "pass")
        self.assertEqual(result["disk_capacity"]["phase"], "replay")
        self.assertEqual((result["coverage"]["planned"], result["coverage"]["attempted"],
                          result["coverage"]["exercised"]), (2, 1, 0))
        self.assertEqual(result["coverage"]["gaps"][1]["outcome"], "not_attempted")
        self.execute.assert_not_called()

    def test_reduction_admission_does_not_create_candidates(self):
        self.args.directory = runner.ROOT / "dst/source-not-created"
        self.args.timeout, self.args.max_candidates = 30, 2
        self.usage.return_value.free = 1
        with mock.patch.object(runner.os, "link") as link, mock.patch.object(runner, "replay") as replay:
            self.assertEqual(runner.reduce_artifact(self.args), 1)
            link.assert_not_called()
            replay.assert_not_called()
        result = self.result("reduction.json")
        self.assertEqual(result["attempts"], [])
        self.assertFalse(result["complete"])
        self.assertEqual(result["outcome"], "infrastructure_failure")
        self.assertEqual(result["disk_capacity"]["phase"], "reduction")

    def test_cli_rejects_nonpositive_minimum_before_memory_enforcement(self):
        with mock.patch.object(runner.sys, "argv", ["run.py", "run", "--min-free-disk-bytes", "0"]), \
                mock.patch.object(runner, "enforce_memory") as memory:
            with self.assertRaises(SystemExit) as raised:
                runner.main()
            self.assertEqual(raised.exception.code, 2)
            memory.assert_not_called()


class RunnerTests(unittest.TestCase):
    def test_exploration_keeps_prefix_identity_and_only_changes_scheduler_seed(self):
        source = {"actions": [], "nodes": 2, "seeds": {"scheduler": 19, "entropy": 71}}
        choices = [{"index": i, "enabled": 2, "selected": i % 2,
                    "fingerprint": [i] * 32} for i in range(4)]
        records = [{"kind": "choice", "value": choice} for choice in choices]
        with self.assertRaises(ValueError):
            exploration_input(source, records, 2, 3)
        records.append({"kind": "terminal"})
        with self.assertRaises(ValueError):
            exploration_input(source, records, 5, 3)
        resolved = exploration_input(source, records, 3, 3)
        self.assertEqual(resolved["schedule_prefix"], choices[:3])
        self.assertEqual(resolved["seeds"], {"scheduler": 3, "entropy": 71})
        self.assertEqual(source["seeds"]["scheduler"], 19)
        proposals = [value["schedule_prefix"] for kind, value in reductions(resolved)
                     if kind == "schedule_prefix"]
        self.assertEqual(proposals, [[], choices[:1], choices[:2]])

    def test_coverage_weights_preserve_fairness_and_prior_sample_identity(self):
        cells = [{"id": name, "expected": "pass", "minimum_transitions": {"Response": 2}}
                 for name in ("covered", "missing")]
        manifest = {"cells": cells + [{"id": "sample-0000", "template": "covered"}]}
        result = {"runs": [{"cell": "sample-0000", "outcome": "pass", "exact_replay": True,
                            "coverage": {"transitions": {"Response": 2}}}]}
        weights = coverage_weights(cells, manifest, result)
        self.assertEqual(weights, {"covered": 1, "missing": 3})
        samples = nightly_samples(cells, 19, 8, weights)
        self.assertEqual(samples, nightly_samples(list(reversed(cells)), 19, 8, weights))
        self.assertEqual([s["template"] for s in samples].count("missing"), 6)
        self.assertEqual([s["template"] for s in samples].count("covered"), 2)
        self.assertEqual(samples[0]["template"], "missing")
        result["runs"][0]["exact_replay"] = False
        self.assertEqual(coverage_weights(cells, manifest, result)["covered"], 2)
        result["runs"][0]["cell"] = "unknown"
        with self.assertRaises(ValueError):
            coverage_weights(cells, manifest, result)

    def test_exact_opt_in_selection_never_expands_to_ignored_neighbors(self):
        entries = [{"selector": "x", "ignored": True, "suite": "a"},
                   {"selector": "xy", "ignored": True, "suite": "a"},
                   {"selector": "xyz", "ignored": False, "suite": "a"}]
        suite = {"selector": "x", "id": "scale", "exact": True, "ignored": True}
        self.assertEqual(selected_names(entries, suite), ["x"])
        self.assertEqual(selected_names(entries, dict(suite, ignored=False)), [])
        self.assertEqual(selected_names(entries, {"selector": "x", "id": "a"}), ["xyz"])
        self.assertEqual(selected_names(entries, {"selector": "", "id": "b"}), [])

    def test_kernel_setup_is_distinct_from_contract_failure(self):
        success = "test result: ok. 1 passed; 0 failed; 0 ignored;"
        self.assertEqual(kernel_outcome(0, success, False), "pass")
        self.assertEqual(kernel_outcome(101, "ring setup: Operation not permitted\ntest result: FAILED.", False), "infrastructure_failure")
        self.assertEqual(kernel_outcome(0, "SKIP io_uring kernel tests: unsupported\n" + success, False), "infrastructure_failure")
        self.assertEqual(kernel_outcome(101, "assertion failed: bytes\ntest result: FAILED.", False), "product_failure")
        self.assertEqual(kernel_outcome(0, success, True), "infrastructure_failure")
        self.assertEqual(kernel_outcome(0, "test result: ok. 0 passed; 0 failed; 0 ignored;", False), "unexercised")

    def test_nightly_samples_cover_templates_and_replace_fixture_seeds(self):
        cells = [{"id": "generated", "expected": "pass"},
                 {"id": "fixture", "expected": "pass", "input": "fixture.json"},
                 {"id": "control", "expected": "pass", "disable_mutant": True},
                 {"id": "mutant", "expected": "product_failure"}]
        samples = nightly_samples(cells, 19, 4)
        self.assertEqual(samples, nightly_samples(list(reversed(cells)), 19, 4))
        self.assertEqual([cell["template"] for cell in samples],
                         ["fixture", "generated", "fixture", "generated"])
        self.assertNotEqual(samples, nightly_samples(cells, 71, 4))
        self.assertNotIn("resolved_seeds", samples[1])
        original = {"seeds": {"scheduler": 1}, "actions": [], "rdma": True}
        resolved = sampled_input(samples[0], original)
        self.assertEqual(resolved["seeds"], samples[0]["resolved_seeds"])
        self.assertEqual(len(set(resolved["seeds"].values())), 6)
        self.assertEqual(original["seeds"], {"scheduler": 1})
        self.assertTrue(resolved["rdma"])
        self.assertNotEqual(resolved["seeds"], samples[2]["resolved_seeds"])
        with self.assertRaises(ValueError):
            nightly_samples([], 19, 1)

    def test_zero_matches_and_ignored_tests_are_not_passes(self):
        self.assertEqual(classify(0, "test result: ok. 0 passed; 0 failed; 0 ignored;", False, 0), "unexercised")
        self.assertEqual(classify(0, "test result: ok. 1 passed; 0 failed; 1 ignored;", False, 2), "unexercised")
        self.assertEqual(classify(0, "test result: ok. 2 passed; 0 failed; 0 ignored;", False, 2), "pass")
        child = "test result: ok. 1 passed; 0 failed; 0 ignored;\n"
        parent = "test result: ok. 2 passed; 0 failed; 0 ignored;"
        self.assertEqual(classify(0, child + parent, False, 2), "pass")
        self.assertEqual(classify(0, child + parent, False, 1), "unexercised")
        self.assertEqual(classify(0, child + "test result: ok. 1 passed; 0 failed; 1 ignored;", False, 1), "unexercised")

    def test_failure_classes(self):
        self.assertEqual(classify(101, "test result: FAILED.", False, 1), "product_failure")
        self.assertEqual(classify(-9, "", False, 1), "infrastructure_failure")
        self.assertEqual(classify(0, "", True, 1), "infrastructure_failure")
        self.assertEqual(classify(101, "invalid scenario: input JSON", False, 1), "invalid_scenario")

    def test_reduction_requires_named_product_failure(self):
        self.assertIsNone(failure_identity({"status": "simulator_failure", "failure": "response.status"}))
        self.assertIsNone(failure_identity({"status": "product_failure", "failure": "arbitrary panic"}))
        self.assertEqual(failure_identity({"status": "product_failure", "failure": {"oracle": "response.status"}}), "response.status")
        proposals = list(deletions([1, 2, 3, 4]))
        self.assertIn([1, 3, 4], proposals)
        self.assertTrue(all(len(proposal) < 4 for proposal in proposals))
        self.assertEqual(list(deletions([1])), [[]])
        self.assertEqual(list(deletions([])), [])

    def test_deadline_kills_process_group(self):
        code, _, expired, seconds = execute(["python3", "-c", "import time; time.sleep(30)"], 0.1)
        self.assertTrue(expired)
        self.assertLess(code, 0)
        self.assertLess(seconds, 5)

    def test_reduction_dimensions_preserve_seeds_and_do_not_mutate_input(self):
        source = {"nodes": 8, "rdma": True, "zc_retirement": True,
                  "phase_policy": "Permuted", "peer_failure_delay": 17,
                  "seeds": {"scheduler": 19}, "mutant": "PrematureZcRetirement",
                  "actions": [{"Turn": 8}, {"WallOffset": [0, -7]},
                              {"CrashSectors": [0, [3, 9, 15]]}]}
        proposals = list(reductions(source))
        dimensions = {dimension for dimension, _ in proposals}
        self.assertEqual(dimensions, {"actions", "nodes", "rdma", "zc_retirement",
                                     "phase_policy", "peer_failure_delay", "actions.0.Turn",
                                     "actions.1.WallOffset", "actions.2.CrashSectors"})
        self.assertEqual([proposal["nodes"] for dimension, proposal in proposals
                          if dimension == "nodes"], [2, 4, 7])
        self.assertTrue(all(proposal["seeds"] == source["seeds"] and
                            proposal["mutant"] == source["mutant"] for _, proposal in proposals))
        self.assertIn(("actions.1.WallOffset", dict(source, actions=[
            {"Turn": 8}, {"WallOffset": [0, -3]}, {"CrashSectors": [0, [3, 9, 15]]}])), proposals)
        self.assertEqual(source["actions"][2], {"CrashSectors": [0, [3, 9, 15]]})
        self.assertEqual(list(reductions({"nodes": 2, "actions": []})), [])

    def test_reduction_preserves_fault_scope_and_mutant_activation(self):
        def history(kind, fields, tick=1):
            return {"kind": "history", "value": {"node": 2, "worker": 0,
                    "incarnation": 1, "tick": tick, "transition": {kind: fields}}}

        terminal = {"kind": "terminal"}
        source = [history("FaultArmed", {"fault": 3, "target": "/a"}),
                  history("FaultEffective", {"fault": 3}),
                  history("MutantActivated", {"mutant": "SuccessfulGetStatus"}),
                  history("Response", {"request": 7, "status": 201}), terminal]
        required = witness_signature(source)
        self.assertEqual(required, witness_signature([
            history("ActionExecuted", {"index": 12}), *source]))
        reindexed = [history("FaultArmed", {"fault": 0, "target": "/a"}, 20),
                     history("FaultEffective", {"fault": 0}, 21),
                     source[2], history("Response", {"request": 1, "status": 201}), terminal]
        self.assertEqual(required, witness_signature(reindexed))
        self.assertFalse(preserves_witnesses(required, witness_signature(source[:2] + source[3:])))
        wrong_target = [history("FaultArmed", {"fault": 3, "target": "/b"}), *source[1:]]
        self.assertFalse(preserves_witnesses(required, witness_signature(wrong_target)))
        with self.assertRaises(ValueError):
            witness_signature(source[:-1])
        with self.assertRaises(ValueError):
            witness_signature(source[1:])

    def test_reduction_preserves_observation_order_and_multiplicity(self):
        def record(kind):
            return {"kind": "history", "value": {"node": 0, "incarnation": 0,
                    "transition": {kind: {}}}}

        terminal = {"kind": "terminal"}
        required = witness_signature([record("Publish"), record("Response"),
                                      record("Response"), terminal])
        self.assertEqual(len(required), 3)
        self.assertTrue(preserves_witnesses(required, required))
        self.assertTrue(preserves_witnesses(required, ["extra", *required, "extra"]))
        self.assertFalse(preserves_witnesses(required, required[:2]))
        self.assertFalse(preserves_witnesses(required, list(reversed(required))))
        self.assertTrue(preserves_witnesses([], required))
        self.assertFalse(preserves_witnesses(required, []))

    def test_required_cell_needs_witnesses_replay_and_control(self):
        cell = {"expected": "product_failure", "oracle": "response.status",
                "control": "control", "minimum_transitions": {"MutantActivated": 1}}
        semantic = {"status": "product_failure", "failure": {"oracle": "response.status"}}
        witnesses = {"terminal_record_present": True, "transitions": {"MutantActivated": 1}}
        self.assertEqual(gate(cell, "product_failure", semantic, witnesses, True, {"control": "pass"}), "pass")
        self.assertEqual(gate(cell, "product_failure", semantic, witnesses, True, {}), "unexercised")
        self.assertEqual(gate(cell, "product_failure", semantic, witnesses, False, {"control": "pass"}), "replay_divergence")
        self.assertEqual(gate(cell, "product_failure", semantic, dict(witnesses, transitions={}), True, {"control": "pass"}), "unexercised")
        self.assertEqual(gate(cell, "pass", {}, witnesses, True, {"control": "pass"}), "unexercised")
        self.assertEqual(gate(cell, "simulator_failure", semantic, witnesses, True, {"control": "pass"}), "simulator_failure")
        self.assertEqual(gate(cell, "product_failure", semantic, dict(witnesses, terminal_record_present=False), True, {"control": "pass"}), "unexercised")

    def test_campaign_denominator_preserves_unattempted_and_failed_cells(self):
        cells = [{"id": name, "scenario": "artifact", "minimum_transitions": {"Response": 2}}
                 for name in ("passed", "partial", "pending")]
        cells.append({"id": "unavailable", "scenario": "absent", "minimum_transitions": {}})
        runs = [{"cell": "passed", "outcome": "pass", "exact_replay": True,
                 "coverage": {"transitions": {"Response": 2}}},
                {"cell": "partial", "outcome": "unexercised", "exact_replay": True,
                 "coverage": {"transitions": {"Response": 1}}}]
        summary = campaign_summary(cells, runs)
        self.assertEqual([summary[key] for key in ("planned", "feasible", "attempted", "exercised")], [4, 3, 2, 1])
        self.assertEqual(summary["transitions"], {"Response": 3})
        self.assertEqual(summary["gaps"][0]["missing_transitions"], {"Response": 1})
        self.assertEqual(summary["gaps"][1]["outcome"], "not_attempted")
        self.assertFalse(summary["gaps"][2]["feasible"])


if __name__ == "__main__":
    unittest.main()
