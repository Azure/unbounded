# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0

import argparse
import contextlib
import copy
import importlib.util
import io
import json
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest.mock import patch


SPEC = importlib.util.spec_from_file_location("sampler", Path(__file__).with_name("racer-s3-benchmark.py"))
sampler = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(sampler)


def metrics(value=0):
    return f'''# HELP irrelevant ignored
process_start_time_seconds 100
racer_loadgen_verified_bytes_total {value * 2**30}
racer_loadgen_received_bytes_total {value * 2**31}
racer_loadgen_applied_concurrency 8
racer_loadgen_in_flight 7
racer_loadgen_pulls_total{{result="success"}} {value * 10}
racer_loadgen_pull_failures_total{{reason="timeout"}} {value}
racer_loadgen_pull_duration_seconds_bucket{{result="success",le="1"}} {value * 5}
racer_loadgen_pull_duration_seconds_bucket{{result="success",le="2"}} {value * 10}
racer_loadgen_pull_duration_seconds_bucket{{result="success",le="+Inf"}} {value * 10}
'''


def snapshot(value, midpoint):
    return {"text": metrics(value), "midpoint": midpoint, "duration": 0.2}


POD = {
    "metadata": {"name": "load-1", "uid": "uid-1"},
    "spec": {"nodeName": "node-1", "containers": [{"name": "load", "image": "load:v1"}]},
    "status": {"containerStatuses": [{"name": "load", "imageID": "sha256:abc",
                                      "containerID": "containerd://abc", "restartCount": 0, "ready": True}]},
}


class MetricsTests(unittest.TestCase):
    def test_per_pod_interval_and_histogram(self):
        identity = sampler.identity(POD)
        result, _ = sampler.pod_result(snapshot(1, 20), snapshot(3, 24), identity, identity)
        self.assertEqual(result["verified_gib_per_second"], 0.5)
        self.assertEqual(result["received_gib_per_second"], 1)
        self.assertEqual(result["successful_operations_per_second"], 5)
        self.assertEqual(result["failure_deltas_by_reason"], {"timeout": 2})
        self.assertEqual(result["applied_concurrency"], {"before": 8, "after": 8})
        self.assertEqual(result["operation_latency_seconds"],
                         {"observations": 20, "p50": 1, "p95": 1.9, "p99": 1.98})

    def test_lazy_counters_and_label_escapes(self):
        before = sampler.parse_metrics(metrics())
        after = sampler.parse_metrics(metrics(1) + 'racer_loadgen_pull_failures_total{reason="a\\\"b\\\\c\\nd"} 3\n')
        delta = sampler.counter_deltas(before, after)
        self.assertEqual(delta[("racer_loadgen_pull_failures_total", (("reason", 'a"b\\c\nd'),))], 3)

    def test_invalid_scrapes(self):
        for text in ("", metrics().replace("in_flight 7", "in_flight NaN"),
                     metrics() + 'racer_loadgen_bad{broken} 1', metrics() + 'racer_loadgen_bad -1',
                     metrics() + 'racer_loadgen_in_flight 7'):
            with self.subTest(text=text), self.assertRaises(ValueError):
                sampler.parse_metrics(text)

    def test_resets_and_missing_series(self):
        before = sampler.parse_metrics(metrics(2))
        for text in (metrics(1), metrics(3).replace("seconds 100", "seconds 101"),
                     metrics(3).replace('racer_loadgen_pull_failures_total{reason="timeout"} 3\n', "")):
            with self.subTest(text=text), self.assertRaises(ValueError):
                sampler.counter_deltas(before, sampler.parse_metrics(text))

    def test_identity_changes_and_scrape_failure(self):
        identity = sampler.identity(POD)
        for field, value in (("uid", "uid-2"), ("containers", [])):
            changed = dict(identity, **{field: value})
            with self.subTest(field=field), self.assertRaises(ValueError):
                sampler.pod_result(snapshot(1, 1), snapshot(3, 3), identity, changed)
        restarted = copy.deepcopy(identity)
        restarted["containers"][0]["restarts"] = 1
        with self.assertRaises(ValueError):
            sampler.pod_result(snapshot(1, 1), snapshot(3, 3), identity, restarted)
        with self.assertRaises(ValueError):
            sampler.pod_result({"error": "timeout"}, snapshot(3, 3), identity, identity)
        with self.assertRaises(ValueError):
            sampler.pod_result(snapshot(1, 1), snapshot(3, 1), identity, identity)

    def test_histogram_edges(self):
        self.assertIsNone(sampler.quantiles({1: 0, float("inf"): 0})["p99"])
        self.assertEqual(sampler.quantiles({1: 1, float("inf"): 10})["p99"], 1)
        for buckets in ({1: 2}, {1: 4, float("inf"): 2}):
            with self.assertRaises(ValueError):
                sampler.quantiles(buckets)


class CollectorTests(unittest.TestCase):
    def test_parallel_aggregate_uses_individual_intervals(self):
        args = argparse.Namespace(namespace="ns", selector="app=load", parallel=8, seconds=60, top=False)
        collector = sampler.Collector(args, Path("unused"))
        pods = {"a": sampler.identity(POD), "b": sampler.identity(POD)}
        summary = {"errors": [], "pods": {}}

        def scrape(name, phase, due=0):
            return snapshot(0, 10) if phase == "before" else snapshot(60, 70 if name == "a" else 130)

        with patch.object(collector, "pods", return_value=pods), patch.object(collector, "scrape", side_effect=scrape):
            collector.collect(summary)
        self.assertEqual(summary["aggregate"]["verified_gib_per_second"], 1.5)
        self.assertEqual(summary["aggregate"]["per_pod_verified_gib_per_second"],
                         {"min": 0.5, "median": 0.75, "max": 1})
        self.assertEqual(summary["aggregate"]["applied_concurrency"]["after"], 16)
        self.assertEqual(summary["errors"], [])

    def test_main_saves_raw_and_summary(self):
        # Temporary test artifacts remain within the repository, not host /tmp.
        with tempfile.TemporaryDirectory(dir=Path(__file__).parent) as directory:
            output = Path(directory) / "result"
            calls = []

            class Process:
                returncode = 0

                def __init__(self, command, **kwargs):
                    calls.append(command)
                    self.command = command

                def communicate(self, timeout=None):
                    if "--raw" in self.command:
                        self_test.assertLessEqual(timeout, 30)
                        return metrics(sum("--raw" in c for c in calls)), ""
                    return json.dumps({"items": [POD]}), ""

            self_test = self
            with patch.object(sampler.subprocess, "Popen", Process), contextlib.redirect_stdout(io.StringIO()):
                code = sampler.main(["--seconds", "0.001", "--output", str(output)])
            summary = json.loads((output / "summary.json").read_text())
            self.assertEqual(code, 0)
            self.assertTrue(summary["complete"])
            self.assertTrue((output / "before-load-1-metrics.txt").exists())
            self.assertTrue((output / "after-load-1-metrics.json").exists())
            self.assertIn("/api/v1/namespaces/unbounded-system/pods/load-1:9090/proxy/metrics", calls[1])

    def test_partial_results_exclude_failed_and_replaced_pods(self):
        args = argparse.Namespace(namespace="ns", selector="app=load", parallel=8, seconds=60, top=False)
        collector = sampler.Collector(args, Path("unused"))
        identity = sampler.identity(POD)
        before = {name: identity for name in ("good", "failed", "replaced")}
        after = dict(before, replaced=dict(identity, uid="new-uid"), added=identity)
        summary = {"errors": [], "pods": {}}

        def scrape(name, phase, due=0):
            if name == "failed":
                return {"error": "timeout", "midpoint": 0}
            return snapshot(0, 10) if phase == "before" else snapshot(60, 70)

        with patch.object(collector, "pods", side_effect=[before, after]), \
                patch.object(collector, "scrape", side_effect=scrape):
            collector.collect(summary)
        self.assertEqual(summary["aggregate"]["valid_pods"], 1)
        self.assertEqual(summary["aggregate"]["selected_pods"], 3)
        self.assertEqual(summary["aggregate"]["verified_gib_per_second"], 1)
        self.assertEqual(len(summary["errors"]), 3)

    def test_no_pods_is_error(self):
        collector = sampler.Collector(None, Path("unused"))
        with patch.object(collector, "pods", return_value={}), self.assertRaisesRegex(ValueError, "no pods"):
            collector.collect({})

    def test_deadline_does_not_start_more_subprocesses(self):
        with tempfile.TemporaryDirectory(dir=Path(__file__).parent) as directory:
            collector = sampler.Collector(None, Path(directory))
            collector.deadline = 0
            with patch.object(sampler.subprocess, "Popen") as popen:
                record = collector.command("expired", ["get", "pods"])
            popen.assert_not_called()
            self.assertIn("deadline exhausted", record["error"])

    def test_missing_kubectl_is_loud_and_saved(self):
        with tempfile.TemporaryDirectory(dir=Path(__file__).parent) as directory:
            output = Path(directory) / "result"
            with patch.object(sampler.subprocess, "Popen", side_effect=FileNotFoundError("kubectl missing")), \
                    contextlib.redirect_stdout(io.StringIO()), contextlib.redirect_stderr(io.StringIO()):
                code = sampler.main(["--output", str(output)])
            summary = json.loads((output / "summary.json").read_text())
            self.assertEqual(code, 1)
            self.assertFalse(summary["complete"])
            self.assertIn("kubectl missing", summary["errors"][0])

    def test_timeout_terminates_process_group_and_saves_partial_output(self):
        with tempfile.TemporaryDirectory(dir=Path(__file__).parent) as directory:
            collector = sampler.Collector(None, Path(directory))
            with patch.object(sampler.subprocess, "Popen") as popen, patch.object(sampler.os, "killpg") as kill:
                process = popen.return_value
                process.pid = 1234
                process.communicate.side_effect = [subprocess.TimeoutExpired("kubectl", 28), ("partial", "error")]
                record = collector.command("timeout", ["get", "pods"])
            kill.assert_called_once_with(1234, sampler.signal.SIGTERM)
            self.assertIn("exceeded", record["error"])
            self.assertEqual((Path(directory) / "timeout.txt").read_text(), "partial")

    def test_timeout_escalation_is_bounded(self):
        with tempfile.TemporaryDirectory(dir=Path(__file__).parent) as directory:
            collector = sampler.Collector(None, Path(directory))
            with patch.object(sampler.subprocess, "Popen") as popen, patch.object(sampler.os, "killpg") as kill:
                process = popen.return_value
                process.pid = 1234
                process.communicate.side_effect = [subprocess.TimeoutExpired("kubectl", 28),
                                                   subprocess.TimeoutExpired("kubectl", 1), ("", "")]
                record = collector.command("killed", ["get", "pods"])
            self.assertEqual([c.args[1] for c in kill.call_args_list], [sampler.signal.SIGTERM, sampler.signal.SIGKILL])
            self.assertEqual([c.kwargs["timeout"] for c in process.communicate.call_args_list], [28, 1, 1])
            self.assertIn("exceeded", record["error"])


if __name__ == "__main__":
    unittest.main()
