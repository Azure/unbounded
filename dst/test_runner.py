# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0
import argparse
import contextlib
import io
import hashlib
import json
from pathlib import Path
import unittest
from unittest import mock

import run as runner

from run import campaign_summary, classify, coverage_weights, deletions, execute, exploration_input, failure_identity, gate, kernel_outcome, nightly_samples, preserves_witnesses, reductions, sampled_input, selected_names, witness_signature


def causal_history(target="/sized/8192/shared", ids=(10, 20, 30), cancel=10, cohort=None):
    def history(kind, **fields):
        return {"kind": "history", "value": {"node": 0, "worker": 0, "incarnation": 1,
                                             "transition": {kind: fields}}}

    return [*(history("Invoke", request=request, target=target, head=index == 1)
              for index, request in enumerate(ids)),
            history("FaultArmed", fault=7, target=target),
            history("FaultEffective", fault=7),
            history("ActionFaultOverlap", fault=7, target=target,
                    requests=list(ids[:2] if cohort is None else cohort)),
            history("Cancel", request=cancel),
            history("Response", request=ids[1], status=200),
            history("RemoteFailureSubmitted", cause="remote", initiated=True),
            {"kind": "terminal"}]


def lifecycle_history():
    records = []

    def emit(kind, node=None, incarnation=0, **fields):
        records.append({"node": node, "incarnation": incarnation, "worker": 0,
                        "transition": {kind: fields}})

    def invoke(request, node, target, head=False, incarnation=0):
        emit("Invoke", node=node, incarnation=incarnation, request=request, target=target, head=head)

    def response(request):
        invocation = next(r for r in records if r["transition"].get("Invoke", {}).get("request") == request)
        emit("Response", node=invocation["node"], incarnation=invocation["incarnation"], request=request, status=200)

    emit("LifecycleRoundPlanned", round=0, crash_node=0, callers=2, object_bytes=257,
         first_source=0, persistence_stride=2)
    for i in (0, 1):
        invoke(i, 0, "/retained")
        response(i)
    emit("DurabilityWitness", target="/retained")
    for source, ids in ((0, [2, 3]), (1, [4, 5])):
        emit("FaultArmed", fault=source, target=f"/held/{source}")
        for i in ids:
            invoke(i, source, f"/held/{source}", head=i == ids[-1])
        emit("FaultEffective", fault=source)
        emit("LifecycleFaultCohort", round=0, fault=source, source=source,
             destination=1-source, target=f"/held/{source}", requests=ids)
    invoke(6, 0, "/healthy/0")
    invoke(7, 1, "/healthy/1")
    for node in (0, 1):
        emit("Publish", node=node, revision=2)
    emit("LifecyclePublicationOverlap", round=0, faults=[0, 1], requests=[2, 3, 4, 5], revisions=[2, 2])
    response(6)
    response(7)
    invoke(8, 0, "/healthy/0")
    response(8)
    emit("LifecycleHealthyProgress", round=0, faults=[0, 1], requests=[6, 7, 8])
    emit("DirtyCheckpointCrash", dirty=5, persisted=[3, 9])
    emit("LifecycleCrashOverlap", round=0, node=0, faults=[0, 1], requests=[2, 3, 4, 5], dirty=5, persisted=[3, 9])
    for i in (4, 5):
        emit("Cancel", request=i)
    for i in (2, 3):
        emit("ProcessLost", request=i)
    emit("LifecycleRestarted", round=0, node=0, incarnation=1, lost=[2, 3])
    # node/incarnation above are both transition fields and observation scope in
    # real records; explicitly retain them in the transition for this fixture.
    records[-1]["transition"]["LifecycleRestarted"].update(node=0, incarnation=1)
    crash = next(r["transition"]["LifecycleCrashOverlap"] for r in records if "LifecycleCrashOverlap" in r["transition"])
    crash["node"] = 0
    for fault in (0, 1):
        emit("FaultReleased", fault=fault)
    invoke(9, 0, "/retained", incarnation=1)
    response(9)
    emit("DurableRecovery", target="/retained")
    invoke(10, 0, "/cold/0", incarnation=1)
    invoke(11, 1, "/cold/1")
    response(10)
    response(11)
    emit("LifecycleRoundRecovered", round=0, target="/retained", cold_requests=[10, 11])
    return records


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

    def test_composition_campaign_persists_resolved_input_and_enforces_missing_witness(self):
        self.args.tier, self.args.timeout = "nightly", 30
        self.args.sampler, self.args.samples, self.args.coverage_from = "composition-v2", 1, None
        self.files[str(runner.ROOT / "dst/scenarios/campaign.json")] = json.dumps({"schema": 1, "cells": []})
        expected = runner.generated_samples(19, 1)[0]

        def record(args, *unused):
            self.assertEqual(json.loads(self.files[str(args.input)]), expected["resolved_input"])
            runner.save(args.artifacts / "result.json", {"runs": [{"outcome": "pass", "attempted": True}]})
            runner.save(args.artifacts / "semantic.json", {"status": "pass"})

        def replay(directory, *unused, **options):
            runner.save(directory / "replay-result.json", {"outcome": "pass"})
            return 0

        witnesses = {"terminal_record_present": True, "transitions": {"ActionExecuted": 80, "Response": 20}}
        with mock.patch.object(runner, "run", side_effect=record) as run, \
                mock.patch.object(runner, "replay", side_effect=replay), \
                mock.patch.object(runner, "coverage", return_value=witnesses):
            self.assertEqual(runner.run_campaign(self.args), 1)
            run.assert_called_once()
        definition = self.result("campaign-manifest.json")
        self.assertEqual(definition["sampling"]["policy"], "composition-v2")
        self.assertEqual(definition["cells"], [expected])
        result = self.result("campaign-result.json")
        self.assertTrue(result["complete"])
        self.assertEqual(result["runs"][0]["outcome"], "unexercised")
        self.assertEqual(result["coverage"]["exercised"], 0)
        self.assertIn("action_fault_overlap", result["coverage"]["gaps"][0]["missing_transitions"])

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

    def test_v3_campaign_persists_root_inputs_and_requires_certified_lifecycle_rounds(self):
        self.args.tier, self.args.timeout = "nightly", 30
        self.args.sampler, self.args.samples, self.args.coverage_from = "composition-v3", 2, None
        self.files[str(runner.ROOT / "dst/scenarios/campaign.json")] = json.dumps({"schema": 1, "cells": []})
        expected = runner.lifecycle_samples(19, 2)
        by_id = {cell["id"]: cell for cell in expected}

        def record(args, *unused):
            cell = by_id[args.artifacts.name]
            self.assertEqual(args.scenario, "artifact")
            self.assertEqual(json.loads(self.files[str(args.input)]), cell["resolved_input"])
            runner.save(args.artifacts / "result.json", {"runs": [{"outcome": "pass", "attempted": True}]})
            runner.save(args.artifacts / "semantic.json", {"status": "pass"})

        def replay(directory, *unused, **options):
            runner.save(directory / "replay-result.json", {"outcome": "pass"})
            return 0

        def witnesses(directory):
            counts = dict(by_id[directory.name]["minimum_transitions"])
            counts.pop("lifecycle_round_certified", None)
            return {"terminal_record_present": True, "transitions": counts}

        with mock.patch.object(runner, "run", side_effect=record), \
                mock.patch.object(runner, "replay", side_effect=replay), \
                mock.patch.object(runner, "coverage", side_effect=witnesses):
            self.assertEqual(runner.run_campaign(self.args), 1)
        self.assertEqual(self.result("campaign-manifest.json")["cells"], expected)
        result = self.result("campaign-result.json")
        self.assertEqual([item["outcome"] for item in result["runs"]], ["pass", "unexercised"])
        self.assertEqual(result["coverage"]["gaps"][0]["missing_transitions"],
                         {"lifecycle_round_certified": expected[1]["resolved_input"]["generated_lifecycle"]["rounds"]})

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

    def test_size_reduction_acceptance_requires_causal_witnesses_oracle_and_exact_replay(self):
        self.args.directory = runner.ROOT / "dst/source-not-created"
        self.args.timeout, self.args.max_candidates = 30, 2
        source = {"nodes": 2, "actions": [
            {"Get": {"node": 0, "target": "/sized/8192/shared", "range": [0, 15]}},
            {"Head": {"node": 0, "target": "/sized/8192/shared"}}]}
        semantic = {"status": "product_failure", "failure": {"oracle": "response.status"}}
        original_open = Path.open
        original_reductions = runner.reductions

        def journal(directory, records):
            self.files[str(directory / "journal.jsonl")] = "\n".join(
                json.dumps({"payload": json.dumps(record)}) for record in records)

        def open_file(path, *args, **kwargs):
            if path.name == "journal.jsonl":
                return io.StringIO(self.files[str(path)])
            return original_open(path, *args, **kwargs)

        def proposals(current):
            target_size = 4096 if "8192" in current["actions"][0]["Get"]["target"] else 257
            for dimension, proposal in original_reductions(current):
                if dimension == "object_bytes" and proposal["actions"][0]["Get"]["target"] == f"/sized/{target_size}/shared":
                    yield dimension, proposal
                    return

        for case in ("accepted", "cause", "unrelated_cancel", "oracle", "replay"):
            with self.subTest(case=case):
                runner.save(self.args.directory / "input.json", source)
                runner.save(self.args.directory / "semantic.json", semantic)
                runner.save(self.args.directory / "build.json", {"binary": "retained", "binary_sha256": "hash"})
                journal(self.args.directory, causal_history())

                def execute(command, seconds, env):
                    candidate = Path(env["RACER_DST_INPUT"]).parent
                    proposal = json.loads(self.files[str(candidate / "input.json")])
                    target = proposal["actions"][0]["Get"]["target"]
                    records = causal_history(target, ids=(1, 2, 3), cancel=3 if case == "unrelated_cancel" else 1)
                    if case == "cause":
                        records[-2]["value"]["transition"]["RemoteFailureSubmitted"]["cause"] = "local"
                    journal(candidate, records)
                    runner.save(candidate / "semantic.json", semantic if case != "oracle" else
                                {"status": "product_failure", "failure": {"oracle": "other"}})
                    return 101, "test result: FAILED.", False, 0.01

                def replay(directory, *args, **kwargs):
                    return int(case == "replay" and directory != self.args.directory)

                with mock.patch.object(Path, "open", open_file), \
                        mock.patch.object(runner, "reductions", side_effect=proposals), \
                        mock.patch.object(runner, "replay", side_effect=replay) as exact, \
                        mock.patch.object(runner.os, "link"), \
                        mock.patch.object(runner, "execute", side_effect=execute):
                    self.assertEqual(runner.reduce_artifact(self.args), 0)
                result = self.result("reduction.json")
                if case == "accepted":
                    self.assertEqual(len(result["attempts"]), 2)
                    self.assertTrue(all(attempt["accepted"] for attempt in result["attempts"]))
                    self.assertEqual(result["remaining_input"]["actions"][0]["Get"]["target"], "/sized/257/shared")
                    self.assertEqual(exact.call_count, 3)  # Source and both fresh candidates.
                    self.assertEqual(result["attempts"][1]["target_mapping"],
                                     {"/sized/4096/shared": "/sized/257/shared"})
                else:
                    self.assertIsNone(result["accepted"])
                    self.assertEqual(result["remaining_input"], source)
                    self.assertEqual(exact.call_count, 2 if case == "replay" else 1)

    def test_cli_rejects_nonpositive_minimum_before_memory_enforcement(self):
        with mock.patch.object(runner.sys, "argv", ["run.py", "run", "--min-free-disk-bytes", "0"]), \
                mock.patch.object(runner, "enforce_memory") as memory:
            with self.assertRaises(SystemExit) as raised:
                runner.main()
            self.assertEqual(raised.exception.code, 2)
            memory.assert_not_called()


class RunnerTests(unittest.TestCase):
    def test_v3_sampling_preserves_v2_and_resolves_only_supported_lifecycle_fields(self):
        legacy = runner.generated_samples(19, 8)
        self.assertEqual(hashlib.sha256(json.dumps(legacy, sort_keys=True).encode()).hexdigest(),
                         "7648720536549919d1870c96fe48e49c8faae90161e4192630eed93595f3ceda")
        samples = runner.lifecycle_samples(19, 128)
        self.assertEqual(samples[:8], runner.lifecycle_samples(19, 8))
        self.assertNotEqual(samples[:8], runner.lifecycle_samples(71, 8))
        rounds, capacities = set(), set()
        for index, sample in enumerate(samples):
            source = sample["resolved_input"]
            if index % 2 == 0:
                self.assertEqual(source, runner.generated_samples(19, index // 2 + 1)[-1]["resolved_input"])
                continue
            self.assertEqual(set(source), {"seeds", "nodes", "rdma", "actions", "generated_lifecycle",
                                           "socket_capacity", "phase_policy", "callback_policy"})
            self.assertEqual((source["nodes"], source["rdma"], source["actions"]), (2, False, []))
            actor = source["generated_lifecycle"]
            self.assertEqual(set(actor), {"seed", "rounds"})
            self.assertEqual(len({actor["seed"], *source["seeds"].values()}), 7)
            self.assertTrue(all(0 <= seed < 2**64 for seed in [actor["seed"], *source["seeds"].values()]))
            rounds.add(actor["rounds"])
            capacities.add(source["socket_capacity"])
            for kind, minimum in runner.LIFECYCLE_PER_ROUND.items():
                self.assertEqual(sample["minimum_transitions"][kind], actor["rounds"] * minimum)
            self.assertEqual(sample["minimum_transitions"]["lifecycle_round_certified"], actor["rounds"])
        self.assertEqual(rounds, {1, 2, 3})
        self.assertEqual(capacities, {4096, 16384, 65536})
        script = ("import sys,json; sys.path.insert(0,'dst'); import run; "
                  "print(json.dumps(run.lifecycle_samples(19,8),sort_keys=True))")
        code, output, expired, _ = execute([runner.sys.executable, "-B", "-c", script], 5,
                                           dict(runner.os.environ, PYTHONHASHSEED="37"))
        self.assertEqual(code, 0, output)
        self.assertFalse(expired)
        self.assertEqual(json.loads(output), samples[:8])
        for seed, count in ((-1, 1), (2**64, 1), (19, 0), (19, 257)):
            with self.assertRaises(ValueError):
                runner.lifecycle_samples(seed, count)

    def test_lifecycle_certification_requires_causal_live_cohorts_and_ordered_recovery(self):
        records = lifecycle_history()
        self.assertEqual(runner.lifecycle_coverage(records), 1)
        text = "\n".join(json.dumps({"payload": json.dumps(item)}) for item in
                         [*({"kind": "history", "value": r} for r in records), {"kind": "terminal"}])
        with mock.patch.object(Path, "exists", return_value=True), \
                mock.patch.object(Path, "open", mock.mock_open(read_data=text)):
            witnesses = runner.coverage(Path("unused"))
        cell = {"expected": "pass", "minimum_transitions": dict(runner.LIFECYCLE_PER_ROUND,
                                                                lifecycle_round_certified=1)}
        self.assertEqual(gate(cell, "pass", {}, witnesses, True, {}), "pass")
        for kind, field, value in (("LifecycleFaultCohort", "requests", [2, 99]),
                                  ("LifecycleFaultCohort", "destination", 0),
                                  ("LifecyclePublicationOverlap", "faults", [1, 0]),
                                  ("LifecyclePublicationOverlap", "requests", [2, 3, 4, 99]),
                                  ("LifecycleHealthyProgress", "requests", [0, 1, 6]),
                                  ("LifecycleCrashOverlap", "requests", [3, 2, 4, 5]),
                                  ("LifecycleRestarted", "lost", [4, 5]),
                                  ("LifecycleRoundRecovered", "cold_requests", [6, 7])):
            changed = json.loads(json.dumps(records))
            next(r["transition"][kind] for r in changed if kind in r["transition"])[field] = value
            with self.subTest(kind=kind, field=field):
                self.assertEqual(runner.lifecycle_coverage(changed), 0)
        for kind in ("FaultEffective", "Publish", "DurabilityWitness", "Cancel", "ProcessLost", "DurableRecovery"):
            changed = list(records)
            del changed[next(i for i, r in enumerate(changed) if kind in r["transition"])]
            self.assertEqual(runner.lifecycle_coverage(changed), 0, kind)
        changed = list(records)
        cancel = next(r for r in changed if "Cancel" in r["transition"])
        changed.remove(cancel)
        changed.insert(next(i for i, r in enumerate(changed) if "LifecyclePublicationOverlap" in r["transition"]), cancel)
        self.assertEqual(runner.lifecycle_coverage(changed), 0)
        witnesses["transitions"].pop("lifecycle_round_certified")
        self.assertEqual(gate(cell, "pass", {}, witnesses, True, {}), "unexercised")
        # Duplicate evidence cannot certify an additional round, and counts from
        # another round cannot repair the incomplete first round's contract.
        self.assertEqual(runner.lifecycle_coverage(records + records), 1)
        second = json.loads(json.dumps(records))
        for record in second:
            kind, fields = next(iter(record["transition"].items()))
            if "round" in fields:
                fields["round"] = 1
            if "request" in fields:
                fields["request"] += 100
            for field in runner.request_list_fields(kind):
                fields[field] = [i + 100 for i in fields[field]]
            if "fault" in fields:
                fields["fault"] += 10
            if "faults" in fields:
                fields["faults"] = [i + 10 for i in fields["faults"]]
        self.assertEqual(runner.lifecycle_coverage(records + second), 2)
        incomplete = [r for r in records if "LifecycleHealthyProgress" not in r["transition"]]
        self.assertEqual(runner.lifecycle_coverage(incomplete + second), 1)

    def test_lifecycle_durability_evidence_requires_order_and_restarted_process(self):
        records = lifecycle_history()
        self.assertEqual(runner.lifecycle_coverage(records), 1)

        def move(kind, boundary, after):
            changed = json.loads(json.dumps(records))
            record = next(r for r in changed if kind in r["transition"])
            changed.remove(record)
            index = next(i for i, r in enumerate(changed) if boundary in r["transition"])
            changed.insert(index + int(after), record)
            return changed

        for kind, boundary, after in (("DurabilityWitness", "LifecycleRestarted", True),
                                      ("DurabilityWitness", "LifecycleCrashOverlap", True),
                                      ("DurableRecovery", "LifecycleCrashOverlap", False),
                                      ("DurableRecovery", "LifecycleRestarted", False),
                                      ("DurableRecovery", "LifecycleRestarted", True)):
            with self.subTest(kind=kind, boundary=boundary, after=after):
                self.assertEqual(runner.lifecycle_coverage(move(kind, boundary, after)), 0)
        for kind in ("Invoke", "Response"):
            for field, value in (("node", 1), ("incarnation", 0), ("incarnation", 2)):
                changed = json.loads(json.dumps(records))
                record = next(r for r in changed if r["transition"].get(kind, {}).get("request") == 9)
                record[field] = value
                with self.subTest(kind=kind, field=field, value=value):
                    self.assertEqual(runner.lifecycle_coverage(changed), 0)
        changed = json.loads(json.dumps(records))
        for record in changed:
            for kind in ("Invoke", "Response"):
                if record["transition"].get(kind, {}).get("request") == 9:
                    record.update(node=1, incarnation=0)
        self.assertEqual(runner.lifecycle_coverage(changed), 0, "surviving process is not durable recovery")
        changed = json.loads(json.dumps(records))
        next(r["transition"]["Invoke"] for r in changed
             if r["transition"].get("Invoke", {}).get("request") == 9)["head"] = True
        self.assertEqual(runner.lifecycle_coverage(changed), 0, "HEAD is not a retained-byte probe")

    def test_lifecycle_reducer_preserves_actor_seed_and_causal_lists(self):
        source = {"nodes": 2, "rdma": False, "actions": [{"Turn": 2}],
                  "generated_lifecycle": {"seed": 71, "rounds": 3}}
        proposals = list(reductions(source))
        shrunk = [p for d, p in proposals if d == "generated_lifecycle.rounds"]
        self.assertEqual([p["generated_lifecycle"] for p in shrunk],
                         [{"seed": 71, "rounds": 1}, {"seed": 71, "rounds": 2}])
        removed = next(p for d, p in proposals if d == "generated_lifecycle")
        self.assertIsNone(removed["generated_lifecycle"])
        self.assertEqual(removed["actions"], source["actions"])
        records = [{"kind": "history", "value": r} for r in lifecycle_history()] + [{"kind": "terminal"}]
        required = witness_signature(records)
        changed = json.loads(json.dumps(records))
        for record in changed[:-1]:
            kind, fields = next(iter(record["value"]["transition"].items()))
            if "request" in fields:
                fields["request"] += 100
            for field in runner.request_list_fields(kind):
                fields[field] = [i + 100 for i in fields[field]]
            if "fault" in fields:
                fields["fault"] += 10
            if "faults" in fields:
                fields["faults"] = [i + 10 for i in fields["faults"]]
        self.assertEqual(required, witness_signature(changed))
        lost = next(r["value"]["transition"]["LifecycleRestarted"] for r in changed[:-1]
                    if "LifecycleRestarted" in r["value"]["transition"])
        lost["lost"] = [104, 105]
        self.assertFalse(preserves_witnesses(required, witness_signature(changed)))

    def test_legacy_sampler_seed_golden_is_unchanged(self):
        cells = [{"id": "fixture", "expected": "pass", "input": "fixture.json"},
                 {"id": "generated", "expected": "pass"}]
        actual = hashlib.sha256(json.dumps(nightly_samples(cells, 19, 4), sort_keys=True).encode()).hexdigest()
        self.assertEqual(actual, "d05236b257877ef9d13f876fbac7cfe6df17ad455f0fe442e939ff7737adb2ab")

    def test_composition_reproduces_in_fresh_python_process(self):
        script = ("import sys,json; sys.path.insert(0,'dst'); import run; "
                  "print(json.dumps(run.generated_samples(19,2),sort_keys=True))")
        env = dict(runner.os.environ, PYTHONHASHSEED="123")
        code, output, expired, _ = execute([runner.sys.executable, "-B", "-c", script], 5, env)
        self.assertEqual(code, 0, output)
        self.assertFalse(expired)
        self.assertEqual(json.loads(output), runner.generated_samples(19, 2))

    def test_generated_owner_matches_blake3_known_vectors_and_rejects_long_keys(self):
        # Official BLAKE3 empty-input and ASCII abc digest prefixes, independent
        # of the runner compression implementation and corpus topology.
        for key, prefix in [("", "af1349b9f5f9a1a6"), ("abc", "6437b3ac38465133")]:
            for nodes in range(2, 9):
                self.assertEqual(runner.short_target_owner(key, nodes),
                                 int.from_bytes(bytes.fromhex(prefix), "little") % nodes)
        with self.assertRaises(ValueError):
            runner.short_target_owner("a" * 65, 2)

    def test_generated_inputs_are_deterministic_prefix_stable_and_bounded(self):
        samples = runner.generated_samples(19, 128)
        self.assertEqual(samples[:2], runner.generated_samples(19, 2))
        self.assertNotEqual(samples[:2], runner.generated_samples(20, 2))
        # Mutating returned inputs cannot affect later generations.
        saved = json.dumps(samples, sort_keys=True)
        configs, sizes = set(), set()
        for cell in samples:
            source = cell["resolved_input"]
            runner.validate_generated_input(source)
            self.assertEqual(len(set(source["seeds"].values())), 6)
            self.assertLessEqual(len(source["actions"]), 80)
            configs.add((source["nodes"], source["rdma"], source["phase_policy"],
                         source["callback_policy"], source["socket_capacity"]))
            sizes.update(window["object_bytes"] for window in cell["windows"])
            self.assertIn(source["socket_capacity"], (4096, 16384, 65536))
            pressure = cell["pressure"]
            self.assertEqual(pressure["evidence"], "configuration-only")
            self.assertEqual(pressure["socket_capacity_bytes"], source["socket_capacity"])
            self.assertEqual(pressure["capacity_policy"], "page-fill-deadline-floor-v1")
            floor = 65536 if any(window["object_bytes"] >= runner.BUFFER_SIZE - 1
                                for window in cell["windows"]) else 4096
            self.assertEqual(pressure["minimum_socket_capacity_bytes"], floor)
            prefix = f"dst/composition-v2/19/{cell['generation_index']}"
            capacity_draw = int.from_bytes(hashlib.sha256(f"{prefix}/config/socket_capacity".encode()).digest()[:8], "little")
            sampled = (4096, 16384, 65536)[capacity_draw % 3]
            self.assertEqual(pressure["sampled_socket_capacity_bytes"], sampled)
            self.assertEqual(source["socket_capacity"], max(sampled, floor))
            for domain, seed in source["seeds"].items():
                self.assertEqual(seed, int.from_bytes(hashlib.sha256(f"{prefix}/{domain}".encode()).digest()[:8], "little"))
            self.assertEqual(pressure["page_to_queue_ratio"], runner.BUFFER_SIZE // source["socket_capacity"])
            self.assertEqual(pressure["object_queue_chunks"],
                             [(window["object_bytes"] + source["socket_capacity"] - 1) // source["socket_capacity"]
                              for window in cell["windows"]])
            self.assertEqual(pressure["windows_exceeding_queue"],
                             sum(window["object_bytes"] > source["socket_capacity"] for window in cell["windows"]))
            # Independently check lifecycle and the crucial no-drain held window.
            held, requests, windows, barrier = None, [], 0, False
            for action in source["actions"]:
                if action == "AwaitOverlap":
                    self.assertGreaterEqual(sum(kind == "Get" for kind, _ in requests), 2)
                    self.assertTrue(any(kind == "Head" for kind, _ in requests))
                    self.assertTrue(all(request["target"] == held[2] and request["node"] == held[0]
                                        for _, request in requests))
                    barrier = True
                elif action == "Release":
                    windows += 1
                    held, requests = None, []
                elif action == "Drain":
                    self.assertIsNone(held)
                elif isinstance(action, dict):
                    kind, value = next(iter(action.items()))
                    if kind == "Hold":
                        self.assertIsNone(held)
                        held = value
                        barrier = False
                        self.assertNotEqual(value[0], value[1])
                        self.assertLessEqual(int(value[2].split("/")[2]), 4096)
                    elif kind in {"Get", "Head"} and held:
                        requests.append((kind, value))
                    elif kind == "Cancel":
                        self.assertTrue(barrier)
                    elif kind == "Turn":
                        barrier = False
            self.assertEqual(windows, cell["minimum_transitions"]["action_fault_overlap"])
        self.assertGreater(len(configs), 40)
        self.assertEqual(sizes, {0, 1, 257, 4095, 4096, runner.BUFFER_SIZE - 1,
                                runner.BUFFER_SIZE, runner.BUFFER_SIZE + 1})
        self.assertEqual(saved, json.dumps(samples, sort_keys=True))
        samples[0]["resolved_input"]["actions"].clear()
        self.assertTrue(runner.generated_samples(19, 1)[0]["resolved_input"]["actions"])

    def test_integration_regression_retains_small_queues_and_overlap_barrier(self):
        # POLL supports small queues, but page fills through them can exhaust
        # production deadlines. Short external ranges do not avoid those fills.
        for size in (runner.BUFFER_SIZE - 1, runner.BUFFER_SIZE, runner.BUFFER_SIZE + 1):
            source = {"nodes": 2, "socket_capacity": 4096, "actions": [
                {"Get": {"node": 0, "target": f"/sized/{size}/regression",
                         "range": [size - 9, size + 7]}}, "Drain"]}
            for capacity in (4096, 16384):
                with self.assertRaisesRegex(ValueError, "page fill requires"):
                    runner.validate_generated_input(dict(source, socket_capacity=capacity))
            runner.validate_generated_input(dict(source, socket_capacity=65536))
            for capacity in (0, 4095, 65537, size + 4096):
                with self.assertRaisesRegex(ValueError, "socket capacity bounds"):
                    runner.validate_generated_input(dict(source, socket_capacity=capacity))
        for size in (0, 1, 257, 4095, 4096):
            for capacity in (4096, 16384):
                runner.validate_generated_input({"nodes": 2, "socket_capacity": capacity, "actions": [
                    {"Get": {"node": 0, "target": f"/sized/{size}/small", "range": None}}, "Drain"]})
        source = runner.generated_samples(19, 1)[0]["resolved_input"]
        actions = source["actions"]
        barrier = actions.index("AwaitOverlap")
        self.assertIn("Cancel", actions[barrier + 1])
        for broken in (actions[:barrier] + actions[barrier + 1:],
                       actions[:barrier] + ["AwaitGate"] + actions[barrier + 1:],
                       actions[:barrier + 1] + [{"Turn": 1}] + actions[barrier + 1:]):
            with self.assertRaisesRegex(ValueError, "accepted-overlap barrier"):
                runner.validate_generated_input(dict(source, actions=broken))
        samples = runner.generated_samples(19, 8)
        variety = runner.generated_samples(19, 128)
        self.assertEqual({sample["resolved_input"]["socket_capacity"] for sample in variety},
                         {4096, 16384, 65536})
        for index in (3, 5, 6):
            self.assertEqual(samples[index]["resolved_input"]["socket_capacity"], 65536)
        self.assertTrue(any(sample["pressure"]["windows_exceeding_queue"] for sample in samples))
        self.assertTrue(any(max(sample["pressure"]["object_queue_chunks"]) >= 64 for sample in samples))
        for sample in samples:
            self.assertEqual(sample["generator"], "composition-v2")
            self.assertEqual(sample["bounds"]["overlap_wait_ticks"], 500)
            self.assertEqual(sample["model_limits"]["required_action"], "AwaitOverlap")
            self.assertEqual(sample["model_limits"]["uncovered"],
                             ["arbitrary simultaneous action gates and live-request reloads"])
            self.assertEqual(sample["resolved_input"]["actions"].count("AwaitOverlap"),
                             sample["minimum_transitions"]["action_fault_overlap"])

    def test_generated_invalid_lifecycle_and_resource_bounds_fail_closed(self):
        base = runner.generated_samples(19, 1)[0]["resolved_input"]
        bad_actions = [["AwaitGate"], ["AwaitOverlap"], [{"Cancel": 0}], [base["actions"][0]],
                       [base["actions"][0], "Drain"], [{"Turn": 5}],
                       [base["actions"][0], base["actions"][0]],
                       [base["actions"][1]] * 7 + ["Drain"],
                       [{"WallOffset": [0, 86400001]}], [{"Reload": 0}],
                       [{"Get": {"node": base["nodes"], "target": "/sized/1/x"}}],
                       [{"Get": {"node": 0, "target": f"/sized/{runner.BUFFER_SIZE + 1}/x"}}],
                       [{"Turn": 0}] * 81]
        for actions in bad_actions:
            with self.subTest(actions=actions), self.assertRaises(ValueError):
                runner.validate_generated_input(dict(base, actions=actions))
        for seed, count in [(-1, 1), (2**64, 1), (19, 0), (19, 257)]:
            with self.assertRaises(ValueError):
                runner.generated_samples(seed, count)

    def test_generated_overlap_requires_live_ids_effective_fault_and_target(self):
        def history(kind, **fields):
            return {"kind": "history", "value": {"transition": {kind: fields}}}

        records = [history("Invoke", request=1, target="/x", head=False),
                   history("Invoke", request=2, target="/x", head=True),
                   history("FaultArmed", fault=7, target="/x"),
                   history("FaultEffective", fault=7)]
        overlap = history("ActionFaultOverlap", fault=7, target="/x", requests=[1, 2],
                          boundary="peer-request", phase="Request", source=0, destination=1)

        def observed(items):
            text = "\n".join(json.dumps({"payload": json.dumps(item)}) for item in
                             [*items, {"kind": "terminal"}])
            with mock.patch.object(Path, "exists", return_value=True), \
                    mock.patch.object(Path, "open", mock.mock_open(read_data=text)):
                return runner.coverage(Path("unused"))

        self.assertNotIn("action_fault_overlap", observed(records)["transitions"])
        self.assertNotIn("action_fault_overlap", observed([*records[:-1], overlap])["transitions"])
        self.assertEqual(observed([*records, overlap, overlap])["transitions"]["action_fault_overlap"], 1)
        for ending in (history("Response", request=1, status=200),
                       history("Cancel", request=1), history("ProcessLost", request=1),
                       history("FaultReleased", fault=7)):
            self.assertNotIn("action_fault_overlap", observed([*records, ending, overlap])["transitions"])
        for fields in ({"requests": [1, 1]}, {"requests": [1, 3]}, {"target": "/other"},
                       {"boundary": "unrelated"}, {"phase": "Connect"}):
            changed = history("ActionFaultOverlap", **dict(overlap["value"]["transition"]["ActionFaultOverlap"], **fields))
            self.assertNotIn("action_fault_overlap", observed([*records, changed])["transitions"])
        cell = runner.generated_samples(19, 1)[0]
        witnesses = {"terminal_record_present": True, "transitions": dict(cell["minimum_transitions"])}
        self.assertEqual(gate(cell, "pass", {}, witnesses, True, {}), "pass")
        witnesses["transitions"].pop("action_fault_overlap")
        witnesses["transitions"]["ActionExecuted"] = 1000
        self.assertEqual(gate(cell, "pass", {}, witnesses, True, {}), "unexercised")
        failed = observed([records[0], history("Response", request=1, status=503)])
        self.assertNotIn("successful_get", failed["transitions"])
        success = observed([*records[:2], history("Response", request=1, status=206),
                            history("Response", request=2, status=200)])
        self.assertEqual(success["transitions"]["successful_get"], 1)
        self.assertEqual(success["transitions"]["successful_head"], 1)

    def test_object_reduction_rewrites_shared_targets_and_preserves_other_keys(self):
        target = "/sized/8192/shared?exact=%2f"
        source = {"nodes": 2, "seeds": {"scheduler": 19}, "actions": [
            {"Hold": [0, 1, target]}, {"Get": {"node": 0, "target": target, "range": [8180, 8200]}},
            {"Head": {"node": 0, "target": target}}, "AwaitGate", "Release",
            {"Durable": [1, target]}, {"Get": {"node": 0, "target": "/other"}}]}
        original = json.dumps(source, sort_keys=True)
        proposals = [proposal for dimension, proposal in reductions(source) if dimension == "object_bytes"]
        self.assertTrue(proposals)
        for proposal in proposals:
            actions = proposal["actions"]
            replacement = actions[1]["Get"]["target"]
            self.assertLess(int(replacement.split("/")[2]), 8192)
            self.assertEqual(actions[0]["Hold"][2], replacement)
            self.assertEqual(runner.short_target_owner(replacement, 2), actions[0]["Hold"][1])
            self.assertEqual(actions[2]["Head"]["target"], replacement)
            self.assertEqual(actions[5]["Durable"][1], replacement)
            self.assertEqual(actions[1]["Get"]["range"], [8180, 8200])
            self.assertEqual(actions[6], source["actions"][6])
            self.assertEqual(proposal["seeds"], source["seeds"])
        self.assertEqual(json.dumps(source, sort_keys=True), original)

    def test_causal_witnesses_preserve_cohort_order_and_terminal_request_links(self):
        required = witness_signature(causal_history())
        # The unused third admission disappears, and all allocated IDs change.
        renumbered = causal_history(ids=(101, 99), cancel=101)
        self.assertEqual(required, witness_signature(renumbered))
        for records in (causal_history(cancel=30), causal_history(cohort=[20, 10]),
                        causal_history(cohort=[10, 30])):
            self.assertFalse(preserves_witnesses(required, witness_signature(records)))
        changed_response = causal_history()
        changed_response[-3]["value"]["transition"]["Response"]["request"] = 30
        self.assertFalse(preserves_witnesses(required, witness_signature(changed_response)))
        for field, value in (("head", True), ("target", "/unrelated"), ("range", [1, 2])):
            records = causal_history()
            records[0]["value"]["transition"]["Invoke"][field] = value
            self.assertFalse(preserves_witnesses(required, witness_signature(records)))
        reordered = causal_history()
        reordered[0], reordered[1] = reordered[1], reordered[0]
        self.assertFalse(preserves_witnesses(required, witness_signature(reordered)))
        with self.assertRaisesRegex(ValueError, "preceding Invoke"):
            witness_signature(causal_history()[1:])
        with self.assertRaisesRegex(ValueError, "duplicate request"):
            witness_signature(causal_history(cohort=[10, 10]))

    def test_size_mapping_preservation_is_exact_except_authorized_size(self):
        old, new = "/sized/8192/shared", "/sized/4096/shared"
        source = {"nodes": 2, "seeds": {"workload": 19}, "actions": [
            {"Get": {"node": 0, "target": old, "range": [1, 15]}},
            {"Head": {"node": 0, "target": old}}, {"Hold": [0, 1, old]}]}
        proposal = json.loads(json.dumps(source).replace(old, new))
        mapping = runner.size_target_mapping(source, proposal)
        required = witness_signature(causal_history(old))
        observed = witness_signature(causal_history(new, ids=(1, 2), cancel=1))
        self.assertFalse(preserves_witnesses(required, observed))
        mapped = runner.mapped_witnesses(required, mapping)
        self.assertTrue(preserves_witnesses(mapped, observed))
        self.assertEqual(required, witness_signature(causal_history(old)))
        for replacement in ("/sized/9000/shared", "/sized/4096/other", "/other/4096/shared",
                            "/sized/4096/shared?changed=1"):
            changed = json.loads(json.dumps(proposal).replace(new, replacement))
            with self.assertRaises(ValueError):
                runner.size_target_mapping(source, changed)
        for mutate in (lambda p: p["actions"][0]["Get"].update(range=[0, 15]),
                       lambda p: p["actions"][1]["Head"].update(target=old),
                       lambda p: p.update(nodes=3),
                       lambda p: p["seeds"].update(workload=20),
                       lambda p: p["actions"][2]["Hold"].__setitem__(1, 0)):
            changed = json.loads(json.dumps(proposal))
            mutate(changed)
            with self.assertRaises(ValueError):
                runner.size_target_mapping(source, changed)
        collision = dict(source, actions=[*source["actions"], {"Get": {"node": 1, "target": new}}])
        changed = json.loads(json.dumps(collision).replace(old, new))
        with self.assertRaises(ValueError):
            runner.size_target_mapping(collision, changed)
        # A target-looking cause is not a typed target and must remain exact.
        before = causal_history(old)
        before[-2]["value"]["transition"]["RemoteFailureSubmitted"]["cause"] = old
        after = causal_history(new)
        after[-2]["value"]["transition"]["RemoteFailureSubmitted"]["cause"] = new
        self.assertFalse(preserves_witnesses(
            runner.mapped_witnesses(witness_signature(before), mapping), witness_signature(after)))

    def test_supported_actor_knob_reduction_preserves_dependencies(self):
        source = {"nodes": 2, "actions": [], "checkpoint_overlap": True, "checkpoint_versions": True,
                  "socket_capacity": 8192, "callback_policy": "ReadyBatch"}
        proposals = dict(reductions(source))
        self.assertFalse(proposals["checkpoint_overlap"]["checkpoint_versions"])
        self.assertTrue(proposals["checkpoint_versions"]["checkpoint_overlap"])
        self.assertEqual(proposals["callback_policy"]["callback_policy"], "Fifo")
        self.assertLess(proposals["socket_capacity"]["socket_capacity"], 8192)

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
                  history("Invoke", {"request": 7, "target": "/a", "head": False}),
                  history("Response", {"request": 7, "status": 201}), terminal]
        required = witness_signature(source)
        self.assertEqual(required, witness_signature([
            history("ActionExecuted", {"index": 12}), *source]))
        reindexed = [history("FaultArmed", {"fault": 0, "target": "/a"}, 20),
                     history("FaultEffective", {"fault": 0}, 21),
                     source[2], history("Invoke", {"request": 1, "target": "/a", "head": False}),
                     history("Response", {"request": 1, "status": 201}), terminal]
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
