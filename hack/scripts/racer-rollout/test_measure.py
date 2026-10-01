# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0
import contextlib
import io
import json
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest.mock import patch
from urllib.parse import parse_qs, urlsplit

import measure


def payload(values):
    return {"status": "success", "data": {"resultType": "vector", "result": [
        {"metric": {"node": node}, "value": [123, str(value)]} for node, value in values]}}


class MeasurementTests(unittest.TestCase):
    def fixture(self):
        raw = {name: payload([("a", 0), ("b", 2)])
               for name in measure.queries("unbounded-system", ".+")}
        raw["racer_ready"] = payload([("a", 1), ("b", 0), ("c", 1)])
        raw["applied_concurrency"] = payload([("a", 2), ("b", 2)])
        raw["process_cpu_cores_by_app"] = payload([])
        return raw

    def test_summary_missing_is_not_zero(self):
        report = measure.summarize(self.fixture())
        self.assertEqual(report["node_count"], 3)
        self.assertEqual(report["zero_progress_nodes"], ["a"])
        self.assertEqual(report["active_zero_progress_nodes"], ["a"])
        self.assertEqual(report["missing_nodes_by_metric"]["verified_goodput_gbps"], ["c"])
        self.assertEqual(report["readiness"]["not_ready_nodes"], ["b"])
        self.assertEqual(report["applied_concurrency_distribution"], {"2.0": 2})
        self.assertEqual(report["summary"]["verified_goodput_gbps"]["sum"], 2)
        self.assertIsNone(report["completion_fractions"]["success"])

    def test_statistics(self):
        self.assertEqual(measure.statistics([1])["p95"], 1)
        self.assertIsNone(measure.statistics([])["sum"])
        stats = measure.statistics([0, 10])
        self.assertEqual((stats["p05"], stats["median"], stats["p95"]), (.5, 5, 9.5))

    def test_scrape_failure_cannot_report_stale_readiness_as_ready(self):
        raw = self.fixture()
        raw["dataplane_up"] = payload([("a", 0), ("b", 1), ("d", 0)])
        report = measure.summarize(raw)
        self.assertEqual(report["node_count"], 4)
        self.assertEqual(report["readiness"]["ready_nodes"], 0)
        self.assertEqual(report["readiness"]["scrape_down_nodes"], ["a", "d"])
        self.assertEqual(report["readiness"]["missing_nodes"], ["c", "d"])

    def test_host_nodes_outside_campaign_do_not_enter_aggregate(self):
        raw = self.fixture()
        raw["eth0_egress_gbps"] = payload([("a", 1), ("b", 2), ("unrelated", 100)])
        self.assertEqual(measure.summarize(raw)["summary"]["eth0_egress_gbps"]["sum"], 3)

    def test_resource_job_is_configurable_and_process_cpu_is_separate(self):
        expr = measure.queries("ns", ".+", "resource")
        self.assertIn('job="resource"', expr["resource_cpu_cores"])
        self.assertIn('container_cpu_usage_seconds_total', expr["resource_cpu_cores"])
        raw = self.fixture()
        raw["resource_cpu_cores"] = payload([])
        raw["process_cpu_cores_by_app"] = payload([("a", 2)])
        raw["process_cpu_cores_by_app"]["data"]["result"][0]["metric"]["app_kubernetes_io_name"] = "gantry"
        report = measure.summarize(raw)
        self.assertIsNone(report["summary"]["resource_cpu_cores"]["sum"])
        self.assertEqual(report["process_cpu_cores_by_app"], [{"node": "a", "app": "gantry", "cores": 2}])

    def test_completion_fractions_and_idle(self):
        raw = self.fixture()
        raw["racer_ready"] = payload([("a", 1), ("b", 1)])
        raw["success_pulls_per_second"] = payload([("a", 1), ("b", 3)])
        raw["error_pulls_per_second"] = payload([("a", 0), ("b", 1)])
        self.assertEqual(measure.summarize(raw)["completion_fractions"], {"success": .8, "error": .2})
        for name in ("success_pulls_per_second", "error_pulls_per_second"):
            raw[name] = payload([("a", 0), ("b", 0)])
        self.assertIsNone(measure.summarize(raw)["completion_fractions"]["success"])

    def test_vector_rejects_ambiguous_or_nonfinite(self):
        for values in ([("a", 0), ("a", 1)], [("", 1)], [("a", "NaN")], [("a", "Inf")]):
            with self.subTest(values=values), self.assertRaises(ValueError):
                measure.vector(payload(values))
        with self.assertRaises(ValueError):
            measure.summarize({name: payload([]) for name in self.fixture()})

    def test_query_semantics(self):
        expressions = measure.queries('test"namespace', 'aks-.*')
        for direction in ("egress", "ingress"):
            expr = expressions[f"eth0_{direction}_gbps"]
            self.assertIn('device="eth0"', expr)
            self.assertNotIn('device=~', expr)
            self.assertIn('[5m]', expr)
        self.assertIn('verified_bytes_total', expressions["verified_goodput_gbps"])
        self.assertNotIn('received_bytes', expressions["verified_goodput_gbps"])
        self.assertIn('result!="success"', expressions["error_pulls_per_second"])
        self.assertIn('or on (node)', expressions["error_pulls_per_second"])
        self.assertIn('namespace="test\\"namespace"', expressions["inflight"])

    @patch("measure.bounded_command")
    def test_only_read_proxy_with_explicit_context_and_shared_time(self, command):
        command.return_value = json.dumps(payload([]))
        args = measure.parse_args(["--context", "other", "--timestamp", "2026-10-01T00:00:00Z"])
        measure.query(args, 'a{node="x"}', args.timestamp, 20)
        argv, seconds = command.call_args.args
        self.assertEqual(argv[:4], ["kubectl", "--context=other", "--request-timeout=20s", "get"])
        self.assertEqual(argv[4], "--raw")
        self.assertEqual(seconds, 20)
        params = parse_qs(urlsplit(argv[5]).query)
        self.assertEqual(params["time"], [args.timestamp])
        self.assertEqual(params["query"], ['a{node="x"}'])
        command.return_value = '{"status":"error"}'
        with self.assertRaises(ValueError):
            measure.query(args, "up", args.timestamp, 20)

    @patch("measure.subprocess.Popen")
    def test_subprocess_bounded_and_errors_redacted(self, popen):
        process = popen.return_value
        process.communicate.return_value = ("", "secret plugin output")
        process.returncode = 124
        with self.assertRaisesRegex(RuntimeError, "exit 124") as raised:
            measure.bounded_command(["kubectl", "get"], 20)
        self.assertNotIn("secret", str(raised.exception))
        self.assertEqual(popen.call_args.args[0][:4], ["timeout", "--signal=TERM", "--kill-after=10s", "20s"])
        process.communicate.assert_called_once_with(timeout=32)

    @patch("measure.os.killpg")
    @patch("measure.subprocess.Popen")
    def test_timeout_cleanup(self, popen, killpg):
        process = popen.return_value
        process.communicate.side_effect = [subprocess.TimeoutExpired("kubectl", 32), ("", "")]
        with self.assertRaises(subprocess.TimeoutExpired):
            measure.bounded_command(["kubectl", "get"], 20)
        killpg.assert_called_once_with(process.pid, measure.signal.SIGTERM)

    @patch("measure.os.killpg")
    @patch("measure.subprocess.Popen")
    def test_cancellation_escalates_and_reaps(self, popen, killpg):
        process = popen.return_value
        process.communicate.side_effect = [KeyboardInterrupt(), subprocess.TimeoutExpired("kubectl", 10), ("", "")]
        with self.assertRaises(KeyboardInterrupt):
            measure.bounded_command(["kubectl", "get"], 20)
        self.assertEqual([call.args[1] for call in killpg.call_args_list],
                         [measure.signal.SIGTERM, measure.signal.SIGKILL])
        self.assertEqual(process.communicate.call_args.kwargs, {"timeout": 2})

    def test_raw_output_and_validation(self):
        # Keep fixtures inside the project boundary, not the host temp directory.
        with tempfile.TemporaryDirectory(dir=Path(__file__).parent) as directory:
            target = Path(directory) / "raw.json"
            with patch("measure.query", side_effect=list(self.fixture().values())), contextlib.redirect_stdout(io.StringIO()):
                self.assertEqual(measure.main(["--raw-output", str(target)]), 0)
            self.assertIn("responses", json.loads(target.read_text()))
            for argv in (["--raw-output", str(target)], ["--raw-output", str(target / "missing.json")],
                         ["--prometheus-service", "../secrets"], ["--timestamp", "2026-10-01"]):
                with self.subTest(argv=argv), contextlib.redirect_stderr(io.StringIO()), self.assertRaises(SystemExit):
                    measure.parse_args(argv)


if __name__ == "__main__":
    unittest.main()
