#!/usr/bin/env python3
# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0

import argparse
import json
import math
import os
import pathlib
import re
import signal
import subprocess
import sys
import tempfile


AIPERF_VERSION = "0.13.0"
DEFAULT_EXECUTABLE = "/opt/aiperf/venv/bin/aiperf"
MAX_MESSAGE_LENGTH = 1024


class AIPerfError(Exception):
    def __init__(self, message, reason="InvalidResult"):
        super().__init__(message)
        self.reason = reason


def canonical_json(value):
    return json.dumps(value, separators=(",", ":"), sort_keys=True)


def bounded_message(message):
    return str(message).replace("\n", " ")[:MAX_MESSAGE_LENGTH]


def positive_integer(config, name):
    value = config.get(name)
    if isinstance(value, bool) or not isinstance(value, int) or value <= 0:
        raise AIPerfError(f"{name} must be a positive integer")

    return value


def positive_number(config, name):
    value = config.get(name)
    if (
        isinstance(value, bool)
        or not isinstance(value, (int, float))
        or not math.isfinite(value)
        or value <= 0
    ):
        raise AIPerfError(f"{name} must be a positive finite number")

    return value


def required_string(config, name):
    value = config.get(name)
    if not isinstance(value, str) or not value or "\x00" in value:
        raise AIPerfError(f"{name} must be a nonempty string")

    return value


def validate_config(config):
    required = {
        "concurrency",
        "max_tokens",
        "model",
        "prompt",
        "request_count",
        "timeout_seconds",
        "tokenizer",
        "url",
    }
    optional = {
        "endpoint",
        "extra_inputs",
        "random_seed",
        "tokenizer_revision",
        "warmup_request_count",
    }
    if not isinstance(config, dict) or set(config) - required - optional:
        raise AIPerfError("configuration contains unsupported fields")
    if required - set(config):
        raise AIPerfError("configuration is missing required fields")

    normalized = {
        "model": required_string(config, "model"),
        "tokenizer": required_string(config, "tokenizer"),
        "url": required_string(config, "url"),
        "prompt": required_string(config, "prompt"),
        "max_tokens": positive_integer(config, "max_tokens"),
        "request_count": positive_integer(config, "request_count"),
        "concurrency": positive_integer(config, "concurrency"),
        "timeout_seconds": positive_number(config, "timeout_seconds"),
        "endpoint": config.get("endpoint", "/v1/completions"),
        "warmup_request_count": config.get("warmup_request_count", 1),
        "random_seed": config.get("random_seed", 1),
        "extra_inputs": config.get(
            "extra_inputs",
            {"temperature": 0, "ignore_eos": True, "seed": 1},
        ),
    }
    normalized["endpoint"] = required_string(normalized, "endpoint")
    normalized["warmup_request_count"] = positive_integer(
        normalized,
        "warmup_request_count",
    )
    normalized["random_seed"] = positive_integer(normalized, "random_seed")

    tokenizer_revision = config.get("tokenizer_revision")
    if tokenizer_revision is not None:
        normalized["tokenizer_revision"] = required_string(
            config,
            "tokenizer_revision",
        )

    extra_inputs = normalized["extra_inputs"]
    if not isinstance(extra_inputs, dict) or not all(
        isinstance(name, str)
        and name
        and isinstance(value, (str, int, float, bool))
        for name, value in extra_inputs.items()
    ):
        raise AIPerfError("extra_inputs must be a scalar JSON object")

    return normalized


def metric(summary, name, unit):
    value = summary.get(name)
    if not isinstance(value, dict) or value.get("unit") != unit:
        raise AIPerfError(f"AIPerf result omitted valid metric {name}")

    average = value.get("avg")
    if (
        isinstance(average, bool)
        or not isinstance(average, (int, float))
        or not math.isfinite(average)
        or average < 0
    ):
        raise AIPerfError(f"AIPerf metric {name} is not finite and nonnegative")

    return average


def stop_process(process):
    if process.poll() is not None:
        return

    os.killpg(os.getpgid(process.pid), signal.SIGKILL)


def run_profile(
    config,
    *,
    artifact_dir=None,
    executable=DEFAULT_EXECUTABLE,
    log_path=None,
):
    config = validate_config(config)
    artifact_path = pathlib.Path(
        artifact_dir or tempfile.mkdtemp(prefix="aiperf-", dir="/tmp")
    )
    artifact_path.mkdir(parents=True, exist_ok=True)
    input_path = artifact_path / "inputs.jsonl"
    input_path.write_text(
        canonical_json(
            {
                "text_input": config["prompt"],
                "output_length": config["max_tokens"],
            }
        )
        + "\n",
        encoding="utf-8",
    )

    command = [
        executable,
        "profile",
        "--model",
        config["model"],
        "--endpoint-type",
        "completions",
        "--endpoint",
        config["endpoint"],
        "--streaming",
        "--tokenizer",
        config["tokenizer"],
        "--url",
        config["url"],
        "--input-file",
        str(input_path),
        "--custom-dataset-type",
        "mooncake_trace",
        "--request-count",
        str(config["request_count"]),
        "--concurrency",
        str(config["concurrency"]),
        "--warmup-request-count",
        str(config["warmup_request_count"]),
        "--workers-max",
        str(config["concurrency"]),
        "--random-seed",
        str(config["random_seed"]),
        "--request-timeout-seconds",
        str(config["timeout_seconds"]),
        "--wait-for-model-timeout",
        "0",
        "--no-server-metrics",
        "--no-gpu-telemetry",
        "--ui",
        "none",
        "--artifact-dir",
        str(artifact_path),
    ]
    if "tokenizer_revision" in config:
        command.extend(["--tokenizer-revision", config["tokenizer_revision"]])
    for name, value in sorted(config["extra_inputs"].items()):
        command.extend(
            [
                "--extra-inputs",
                f"{name}:{str(value).lower() if isinstance(value, bool) else value}",
            ]
        )

    process = subprocess.Popen(
        command,
        stdin=subprocess.DEVNULL,
        stdout=subprocess.PIPE,
        stderr=subprocess.STDOUT,
        text=True,
        start_new_session=True,
    )
    try:
        output, _ = process.communicate(timeout=config["timeout_seconds"] + 60)
    except subprocess.TimeoutExpired as error:
        stop_process(process)
        output, _ = process.communicate(timeout=5)
        raise AIPerfError(
            f"AIPerf profile timed out: {bounded_message(output)}",
            "AssertionFailed",
        ) from error

    if log_path:
        pathlib.Path(log_path).write_text(output[-65536:], encoding="utf-8")
    if process.returncode != 0:
        raise AIPerfError(
            f"AIPerf exited with code {process.returncode}: {bounded_message(output)}",
            "AssertionFailed",
        )

    result_path = artifact_path / "profile_export_aiperf.json"
    try:
        summary = json.loads(result_path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as error:
        raise AIPerfError("AIPerf result is missing or invalid") from error
    if summary.get("aiperf_version") != AIPERF_VERSION:
        raise AIPerfError("AIPerf result version is not approved", "IdentityMismatch")
    if not re.fullmatch(r"1\.[0-9]+", str(summary.get("schema_version", ""))):
        raise AIPerfError("AIPerf result schema is not supported")

    request_count = metric(summary, "request_count", "requests")
    error_count = (
        metric(summary, "error_request_count", "requests")
        if "error_request_count" in summary
        else 0
    )
    if (
        request_count != config["request_count"]
        or error_count != 0
        or summary.get("was_cancelled") is True
    ):
        raise AIPerfError(
            f"AIPerf completed requests = {request_count}, errors = {error_count}",
            "AssertionFailed",
        )

    return {
        "aiperfVersion": AIPERF_VERSION,
        "schemaVersion": summary["schema_version"],
        "requestCount": int(request_count),
        "errorCount": int(error_count),
        "inputSequenceLength": int(metric(summary, "input_sequence_length", "tokens")),
        "outputSequenceLength": int(metric(summary, "output_sequence_length", "tokens")),
        "requestLatencyMs": metric(summary, "request_latency", "ms"),
        "timeToFirstTokenMs": metric(summary, "time_to_first_token", "ms"),
        "outputTokenThroughputPerUser": metric(
            summary,
            "output_token_throughput_per_user",
            "tokens/sec/user",
        ),
        "summary": summary,
    }


def parse_args(argv):
    parser = argparse.ArgumentParser()
    parser.add_argument("--config", required=True)
    parser.add_argument("--output", required=True)
    parser.add_argument("--artifact-dir", required=True)
    parser.add_argument("--log", required=True)
    return parser.parse_args(argv)


def main(argv=None):
    args = parse_args(argv)
    try:
        config = json.loads(pathlib.Path(args.config).read_text(encoding="utf-8"))
        result = run_profile(
            config,
            artifact_dir=args.artifact_dir,
            log_path=args.log,
        )
        pathlib.Path(args.output).write_text(
            canonical_json(result) + "\n",
            encoding="utf-8",
        )
    except (AIPerfError, OSError, json.JSONDecodeError) as error:
        print(bounded_message(error), file=sys.stderr)
        return 1

    return 0


if __name__ == "__main__":
    raise SystemExit(main())
