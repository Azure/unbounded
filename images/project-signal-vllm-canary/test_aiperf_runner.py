# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0

import json
import pathlib
import tempfile
import unittest
from unittest import mock

import aiperf_runner


class Process:
    def __init__(self, returncode=0, output="profile complete"):
        self.returncode = returncode
        self.output = output

    def communicate(self, timeout):
        del timeout
        return self.output, None

    def poll(self):
        return self.returncode


class AIPerfRunnerTest(unittest.TestCase):
    def test_run_profile_returns_normalized_metrics(self):
        with tempfile.TemporaryDirectory() as directory:
            artifact_dir = pathlib.Path(directory)
            (artifact_dir / "profile_export_aiperf.json").write_text(
                json.dumps(summary()),
                encoding="utf-8",
            )
            with mock.patch.object(
                aiperf_runner.subprocess,
                "Popen",
                return_value=Process(),
            ) as popen:
                result = aiperf_runner.run_profile(
                    config(),
                    artifact_dir=artifact_dir,
                    executable="/fixture/aiperf",
                    log_path=artifact_dir / "aiperf.log",
                )

        command = popen.call_args.args[0]
        self.assertEqual(command[:2], ["/fixture/aiperf", "profile"])
        self.assertIn("--tokenizer-revision", command)
        self.assertEqual(result["requestCount"], 4)
        self.assertEqual(result["errorCount"], 0)
        self.assertEqual(result["inputSequenceLength"], 9)
        self.assertEqual(result["outputSequenceLength"], 8)
        self.assertEqual(result["requestLatencyMs"], 794.5)
        self.assertEqual(result["timeToFirstTokenMs"], 550.4)
        self.assertEqual(result["outputTokenThroughputPerUser"], 311.8)

    def test_run_profile_rejects_incomplete_requests(self):
        invalid = summary()
        invalid["request_count"]["avg"] = 3
        with tempfile.TemporaryDirectory() as directory:
            artifact_dir = pathlib.Path(directory)
            (artifact_dir / "profile_export_aiperf.json").write_text(
                json.dumps(invalid),
                encoding="utf-8",
            )
            with (
                mock.patch.object(
                    aiperf_runner.subprocess,
                    "Popen",
                    return_value=Process(),
                ),
                self.assertRaisesRegex(
                    aiperf_runner.AIPerfError,
                    "completed requests = 3",
                ),
            ):
                aiperf_runner.run_profile(
                    config(),
                    artifact_dir=artifact_dir,
                    executable="/fixture/aiperf",
                )

    def test_validate_config_rejects_unknown_fields(self):
        value = config()
        value["metric_name"] = "outputTokensPerSecond"

        with self.assertRaisesRegex(
            aiperf_runner.AIPerfError,
            "unsupported fields",
        ):
            aiperf_runner.validate_config(value)


def config():
    return {
        "model": "example-model",
        "tokenizer": "example/model",
        "tokenizer_revision": "a" * 40,
        "url": "http://127.0.0.1:8000",
        "prompt": "Reply with only 42.",
        "max_tokens": 8,
        "request_count": 4,
        "concurrency": 4,
        "timeout_seconds": 120,
    }


def summary():
    return {
        "aiperf_version": "0.13.0",
        "schema_version": "1.0",
        "request_count": {"unit": "requests", "avg": 4},
        "error_request_count": {"unit": "requests", "avg": 0},
        "input_sequence_length": {"unit": "tokens", "avg": 9},
        "output_sequence_length": {"unit": "tokens", "avg": 8},
        "request_latency": {"unit": "ms", "avg": 794.5},
        "time_to_first_token": {"unit": "ms", "avg": 550.4},
        "output_token_throughput_per_user": {
            "unit": "tokens/sec/user",
            "avg": 311.8,
        },
        "was_cancelled": False,
    }


if __name__ == "__main__":
    unittest.main()
