#!/usr/bin/env python3
# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0

import argparse
import base64
import hashlib
import json
import os
import re
import signal
import ssl
import subprocess
import sys
import time
import urllib.error
import urllib.request
from datetime import datetime, timezone
from decimal import Decimal

MODEL_IDENTITY = (
    "EleutherAI/pythia-70m-deduped@"
    "sha256:52b9b0fa1a8a67424d2e8f7994675698c60e3eb1a7925091930ecc4958dbcd7e"
)
MODEL_REVISION = "e93a9faa9c77e5d09219f6c868bfc7a1bd65593c"
MODEL_MANIFEST_DIGEST = (
    "sha256:52b9b0fa1a8a67424d2e8f7994675698c60e3eb1a7925091930ecc4958dbcd7e"
)
MODEL_WEIGHTS_DIGEST = (
    "sha256:3da388330e4549156d76b58d6d268c63cd005e9336b4f4d2d378421e7b7a33fd"
)
PROMPT_FIXTURE = {
    "max_tokens": 8,
    "prompt": "The capital of France is",
    "seed": 1,
    "temperature": 0,
}
PROMPT_FIXTURE_DIGEST = (
    "sha256:87c87bfc4863e490bdef827c5aa1c0b48f47311e2fed3fd002c3e05dfad23d99"
)
RESULT_SCHEMA = "signal.unbounded-cloud.io/invocation-result/v1"
TENSOR_PARALLEL_SIZE = 8
SERVER_PORT = 8000
MAX_MESSAGE_LENGTH = 1024


class HarnessError(Exception):
    def __init__(self, message, reason="TestHarnessFailure"):
        super().__init__(message)
        self.reason = reason


def canonical_json(value):
    return json.dumps(value, separators=(",", ":"), sort_keys=True)


def sha256_text(value):
    return "sha256:" + hashlib.sha256(value.encode("utf-8")).hexdigest()


def parse_duration(value):
    match = re.fullmatch(r"([0-9]+(?:\.[0-9]+)?)(ms|s|m|h)", value)
    if not match:
        raise HarnessError(f"unsupported duration {value!r}", "InvalidResult")

    amount = Decimal(match.group(1))
    unit = match.group(2)
    multiplier = {
        "ms": Decimal("0.001"),
        "s": Decimal("1"),
        "m": Decimal("60"),
        "h": Decimal("3600"),
    }[unit]

    return float(amount * multiplier)


def parameter_values(parameters):
    values = {}
    for parameter in parameters:
        name = parameter["name"]
        if name in values:
            raise HarnessError(f"parameter {name!r} is duplicated", "InvalidResult")

        value = parameter["value"]
        value_type = value["type"]
        field = {
            "String": "stringValue",
            "Integer": "integerValue",
            "Decimal": "decimalValue",
            "Boolean": "booleanValue",
            "Duration": "durationValue",
            "Quantity": "quantityValue",
        }.get(value_type)
        if not field or field not in value:
            raise HarnessError(f"parameter {name!r} has an invalid typed value", "InvalidResult")

        values[name] = value[field]

    return values


def typed_boolean(value):
    return {"type": "Boolean", "booleanValue": bool(value)}


def typed_integer(value):
    return {"type": "Integer", "integerValue": int(value)}


def typed_string(value):
    return {"type": "String", "stringValue": str(value)}


def typed_decimal(value):
    return {"type": "Decimal", "decimalValue": format(Decimal(str(value)), "f")}


def typed_duration(seconds):
    milliseconds = max(0, int(round(seconds * 1000)))
    return {"type": "Duration", "durationValue": f"{milliseconds}ms"}


def measurement(name, value, unit=""):
    result = {"name": name, "value": value}
    if unit:
        result["unit"] = unit

    return result


def verify_contract(parameters, test, resolved_inputs):
    expected = {
        "expectedModelIdentity": MODEL_IDENTITY,
        "expectedModelRevision": MODEL_REVISION,
        "expectedModelWeightsDigest": MODEL_WEIGHTS_DIGEST,
        "promptFixtureDigest": PROMPT_FIXTURE_DIGEST,
    }
    if set(parameters) != set(expected) | {"requestTimeout"}:
        raise HarnessError("parameters do not match the immutable harness contract", "IdentityMismatch")

    for name, value in expected.items():
        if parameters.get(name) != value:
            raise HarnessError(
                f"parameter {name} does not match the immutable harness contract",
                "IdentityMismatch",
            )

    if parse_duration(parameters.get("requestTimeout", "")) <= 0:
        raise HarnessError("requestTimeout must be positive", "InvalidResult")

    identity = test.get("identity", {})
    if identity.get("name") != "project-signal-vllm-canary-v1":
        raise HarnessError("resolved Test identity is not the canonical vLLM canary", "IdentityMismatch")

    if identity.get("trust") != "Approved":
        raise HarnessError("resolved Test is not approved", "IdentityMismatch")

    if not any(
        resolved_input.get("kind") == "Model"
        and resolved_input.get("state") == "Complete"
        and resolved_input.get("digest") == MODEL_MANIFEST_DIGEST
        for resolved_input in resolved_inputs
    ):
        raise HarnessError(
            "resolved inputs do not pin the approved model cache manifest",
            "IdentityMismatch",
        )


def verify_model_cache(model_cache):
    subprocess.run(
        [
            "/usr/local/bin/project-signal-model-cache",
            "verify",
            f"--dir={model_cache}",
            f"--manifest-digest={MODEL_MANIFEST_DIGEST}",
        ],
        check=True,
        stdin=subprocess.DEVNULL,
        stdout=subprocess.DEVNULL,
    )

    with open(os.path.join(model_cache, "config.json"), encoding="utf-8") as config_file:
        config = json.load(config_file)

    if config.get("architectures") != ["GPTNeoXForCausalLM"]:
        raise HarnessError("model architecture is not GPTNeoXForCausalLM", "IdentityMismatch")

    if config.get("hidden_size") != 512 or config.get("num_attention_heads") != 8:
        raise HarnessError(
            "model configuration is not the approved TP=8 shape",
            "IdentityMismatch",
        )


def run_nvidia_smi(query):
    completed = subprocess.run(
        ["nvidia-smi", f"--query-{query}", "--format=csv,noheader,nounits"],
        check=True,
        capture_output=True,
        text=True,
        timeout=30,
    )

    return [line.strip() for line in completed.stdout.splitlines() if line.strip()]


def visible_gpu_count():
    return len(run_nvidia_smi("gpu=uuid"))


def active_gpu_count():
    return len(set(run_nvidia_smi("compute-apps=gpu_uuid")))


def gpu_memory_used_bytes():
    total_mib = sum(int(value) for value in run_nvidia_smi("gpu=memory.used"))
    return total_mib * 1024 * 1024


def server_command(model_cache):
    return [
        "vllm",
        "serve",
        model_cache,
        "--served-model-name",
        MODEL_IDENTITY,
        "--tensor-parallel-size",
        str(TENSOR_PARALLEL_SIZE),
        "--dtype",
        "half",
        "--load-format",
        "safetensors",
        "--tokenizer",
        model_cache,
        "--max-model-len",
        "256",
        "--max-num-seqs",
        "1",
        "--enforce-eager",
        "--host",
        "127.0.0.1",
        "--port",
        str(SERVER_PORT),
    ]


def request_json(url, payload=None, timeout=30):
    data = None
    headers = {"Accept": "application/json"}
    if payload is not None:
        data = canonical_json(payload).encode("utf-8")
        headers["Content-Type"] = "application/json"

    request = urllib.request.Request(url, data=data, headers=headers)
    with urllib.request.urlopen(request, timeout=timeout) as response:
        return json.load(response)


def request_status(url, timeout=30):
    request = urllib.request.Request(url, headers={"Accept": "*/*"})
    with urllib.request.urlopen(request, timeout=timeout) as response:
        return response.status


def wait_for_service(process, timeout):
    deadline = time.monotonic() + timeout
    server_errors = 0

    while time.monotonic() < deadline:
        if process.poll() is not None:
            return False, server_errors + 1

        try:
            if request_status(f"http://127.0.0.1:{SERVER_PORT}/health", timeout=2) != 200:
                time.sleep(1)
                continue

            models = request_json(f"http://127.0.0.1:{SERVER_PORT}/v1/models", timeout=2)
            model_ids = [entry.get("id") for entry in models.get("data", [])]
            if MODEL_IDENTITY in model_ids:
                return True, server_errors
        except (OSError, ValueError, urllib.error.URLError):
            time.sleep(1)

    return False, server_errors + 1


def issue_completion(timeout):
    payload = dict(PROMPT_FIXTURE)
    payload.update(
        {
            "model": MODEL_IDENTITY,
            "stream": True,
            "stream_options": {"include_usage": True},
        }
    )
    request = urllib.request.Request(
        f"http://127.0.0.1:{SERVER_PORT}/v1/completions",
        data=canonical_json(payload).encode("utf-8"),
        headers={"Accept": "text/event-stream", "Content-Type": "application/json"},
    )

    request_started = time.monotonic()
    first_token_at = None
    usage = None

    with urllib.request.urlopen(request, timeout=timeout) as response:
        for raw_line in response:
            line = raw_line.decode("utf-8").strip()
            if not line.startswith("data: "):
                continue

            data = line.removeprefix("data: ")
            if data == "[DONE]":
                break

            chunk = json.loads(data)
            if first_token_at is None and any(
                choice.get("text") for choice in chunk.get("choices", [])
            ):
                first_token_at = time.monotonic()

            if chunk.get("usage"):
                usage = chunk["usage"]

    request_finished = time.monotonic()
    if first_token_at is None or usage is None:
        raise HarnessError("vLLM response omitted token timing or usage", "InvalidResult")

    return {
        "promptTokens": int(usage["prompt_tokens"]),
        "completionTokens": int(usage["completion_tokens"]),
        "endToEndLatency": request_finished - request_started,
        "timeToFirstToken": first_token_at - request_started,
        "outputTokensPerSecond": (
            int(usage["completion_tokens"]) / max(request_finished - first_token_at, 0.001)
        ),
    }


def stop_server(process):
    if process is None or process.poll() is not None:
        return True

    os.killpg(os.getpgid(process.pid), signal.SIGTERM)
    try:
        process.wait(timeout=15)
    except subprocess.TimeoutExpired:
        os.killpg(os.getpgid(process.pid), signal.SIGKILL)
        process.wait(timeout=5)

    return process.returncode in (0, -signal.SIGTERM, -signal.SIGKILL)


def count_server_errors(path):
    try:
        with open(path, encoding="utf-8", errors="replace") as log_file:
            return sum(
                1
                for line in log_file
                if "[ERROR]" in line
                or " ERROR " in line
                or "Traceback (most recent call last)" in line
            )
    except FileNotFoundError:
        return 1


def create_result_configmap(namespace, name, run_uid, envelope):
    content = canonical_json(envelope).encode("utf-8")
    digest = "sha256:" + hashlib.sha256(content).hexdigest()
    object_body = {
        "apiVersion": "v1",
        "kind": "ConfigMap",
        "metadata": {
            "name": name,
            "namespace": namespace,
            "labels": {"signal.unbounded-cloud.io/run-uid": run_uid},
            "annotations": {
                "signal.unbounded-cloud.io/invocation-result-digest": digest,
                "signal.unbounded-cloud.io/model-manifest-digest": MODEL_MANIFEST_DIGEST,
                "signal.unbounded-cloud.io/model-revision": MODEL_REVISION,
            },
        },
        "immutable": True,
        "binaryData": {"result.json": base64.b64encode(content).decode("ascii")},
    }

    host = os.environ["KUBERNETES_SERVICE_HOST"]
    port = os.environ.get("KUBERNETES_SERVICE_PORT_HTTPS", "443")
    with open(
        "/var/run/secrets/kubernetes.io/serviceaccount/token",
        encoding="utf-8",
    ) as token_file:
        token = token_file.read().strip()

    context = ssl.create_default_context(
        cafile="/var/run/secrets/kubernetes.io/serviceaccount/ca.crt"
    )
    request = urllib.request.Request(
        f"https://{host}:{port}/api/v1/namespaces/{namespace}/configmaps",
        data=canonical_json(object_body).encode("utf-8"),
        headers={
            "Authorization": f"Bearer {token}",
            "Content-Type": "application/json",
        },
        method="POST",
    )

    with urllib.request.urlopen(request, context=context, timeout=30) as response:
        if response.status != 201:
            raise HarnessError(
                f"create result ConfigMap returned HTTP {response.status}",
                "InfrastructureFailure",
            )


def bounded_message(message):
    return str(message).replace("\n", " ")[:MAX_MESSAGE_LENGTH]


def utc_now():
    return datetime.now(timezone.utc).isoformat(timespec="milliseconds").replace("+00:00", "Z")


def build_envelope(args, test, resolved_inputs, participants, roles, result):
    participant = participants[0]
    role = roles.get("0") or roles.get(0) or "primary"
    outcome = result["outcome"]

    return {
        "schemaVersion": RESULT_SCHEMA,
        "invocationID": args.invocation_id,
        "runUID": args.run_uid,
        "stageID": args.stage_id,
        "stageOrder": args.stage_order,
        "planDigest": args.plan_digest,
        "test": test["identity"],
        "resolvedInputs": resolved_inputs,
        "startedAt": result["startedAt"],
        "finishedAt": result["finishedAt"],
        "monotonicDuration": f"{max(result['duration'], 0):.3f}s",
        "outcome": outcome,
        "reasonCode": result["reasonCode"],
        "message": bounded_message(result["message"]),
        "exitCode": result["exitCode"],
        "measurements": result["measurements"],
        "participants": [
            {
                "index": 0,
                "role": role,
                "outcome": outcome,
                "start": {"nodeUID": participant["nodeUID"]},
                "end": {"nodeUID": participant["nodeUID"]},
                "visibleDevices": [f"gpu-{index}" for index in range(result["visibleGpuCount"])],
                "coveredDevices": [f"gpu-{index}" for index in range(result["activeGpuCount"])],
            }
        ],
        "activity": {
            "sampleCount": 1,
            "sampleSequenceDigest": sha256_text(canonical_json(result["measurements"])),
        },
        "cleanup": {"outcome": "NotRequired"},
    }


def run_harness(args):
    started_wall = utc_now()
    started = time.monotonic()
    server = None
    log_file = None
    log_path = "/tmp/vllm-server.log"
    service_ready = False
    request_succeeded = False
    server_errors = 0
    request_errors = 0
    visible_count = 0
    active_count = 0
    memory_used = 0
    completion = {
        "promptTokens": 0,
        "completionTokens": 0,
        "endToEndLatency": 0,
        "timeToFirstToken": 0,
        "outputTokensPerSecond": 0,
    }
    outcome = "Inconclusive"
    reason = "TestHarnessFailure"
    message = "vLLM canary did not complete"
    exit_code = 1

    parameters = parameter_values(json.loads(args.parameters_json))
    test = json.loads(args.test_json)
    resolved_inputs = json.loads(args.resolved_inputs_json)
    participants = json.loads(args.participants_json)
    roles = json.loads(args.participant_roles_json)

    try:
        verify_contract(parameters, test, resolved_inputs)
        verify_model_cache(args.model_cache)

        visible_count = visible_gpu_count()
        if visible_count != TENSOR_PARALLEL_SIZE:
            raise HarnessError(
                f"visible GPU count = {visible_count}, want {TENSOR_PARALLEL_SIZE}",
                "AssertionFailed",
            )

        log_file = open(log_path, "w", encoding="utf-8")
        server = subprocess.Popen(
            server_command(args.model_cache),
            stdin=subprocess.DEVNULL,
            stdout=log_file,
            stderr=subprocess.STDOUT,
            start_new_session=True,
        )

        service_ready, readiness_errors = wait_for_service(
            server,
            min(parse_duration(parameters["requestTimeout"]), 300),
        )
        server_errors += readiness_errors
        if not service_ready:
            raise HarnessError("vLLM service did not become ready", "InfrastructureFailure")

        active_count = active_gpu_count()
        memory_used = gpu_memory_used_bytes()
        if active_count != TENSOR_PARALLEL_SIZE:
            raise HarnessError(
                f"active GPU count = {active_count}, want {TENSOR_PARALLEL_SIZE}",
                "AssertionFailed",
            )

        try:
            completion = issue_completion(parse_duration(parameters["requestTimeout"]))
            request_succeeded = True
        except (HarnessError, OSError, ValueError, urllib.error.URLError) as error:
            request_errors += 1
            raise HarnessError(f"deterministic request failed: {error}", "AssertionFailed") from error

        outcome = "Passed"
        reason = "Completed"
        message = "bounded offline vLLM canary completed"
        exit_code = 0
    except HarnessError as error:
        outcome = "Failed" if error.reason in ("AssertionFailed", "IdentityMismatch") else "Inconclusive"
        reason = error.reason
        message = str(error)
    except (OSError, subprocess.SubprocessError, ValueError, KeyError, json.JSONDecodeError) as error:
        message = str(error)
    finally:
        cleanup_succeeded = stop_server(server)
        if log_file is not None:
            log_file.close()
            server_errors += count_server_errors(log_path)

        if not cleanup_succeeded and outcome == "Passed":
            outcome = "Inconclusive"
            reason = "InfrastructureFailure"
            message = "vLLM server process group did not terminate cleanly"
            exit_code = 1
        elif server_errors > 0 and outcome == "Passed":
            outcome = "Failed"
            reason = "AssertionFailed"
            message = f"vLLM server error source count = {server_errors}"
            exit_code = 1

    finished = time.monotonic()
    measurements = [
        measurement("serviceReady", typed_boolean(service_ready)),
        measurement("modelIdentity", typed_string(MODEL_IDENTITY)),
        measurement("modelRevision", typed_string(MODEL_REVISION)),
        measurement("modelWeightsDigest", typed_string(MODEL_WEIGHTS_DIGEST)),
        measurement("requestSucceeded", typed_boolean(request_succeeded)),
        measurement("promptTokens", typed_integer(completion["promptTokens"])),
        measurement("completionTokens", typed_integer(completion["completionTokens"])),
        measurement(
            "errorSourceCounts",
            typed_string(canonical_json({"request": request_errors, "server": server_errors})),
        ),
        measurement(
            "restartSourceCounts",
            typed_string(canonical_json({"container": 0, "workflowPod": 0})),
        ),
        measurement("tensorParallelSize", typed_integer(TENSOR_PARALLEL_SIZE)),
        measurement("observedGpuCount", typed_integer(visible_count), "devices"),
        measurement("activeGpuCount", typed_integer(active_count), "devices"),
        measurement(
            "endToEndLatency",
            typed_duration(completion["endToEndLatency"]),
            "ms",
        ),
        measurement(
            "timeToFirstToken",
            typed_duration(completion["timeToFirstToken"]),
            "ms",
        ),
        measurement(
            "outputTokensPerSecond",
            typed_decimal(completion["outputTokensPerSecond"]),
            "tokens/s",
        ),
        measurement("gpuMemoryUsed", typed_integer(memory_used), "bytes"),
    ]

    return build_envelope(
        args,
        test,
        resolved_inputs,
        participants,
        roles,
        {
            "startedAt": started_wall,
            "finishedAt": utc_now(),
            "duration": finished - started,
            "outcome": outcome,
            "reasonCode": reason,
            "message": message,
            "exitCode": exit_code,
            "measurements": measurements,
            "visibleGpuCount": visible_count,
            "activeGpuCount": active_count,
        },
    )


def parse_args(argv):
    parser = argparse.ArgumentParser()
    parser.add_argument("--invocation-id", required=True)
    parser.add_argument("--run-uid", required=True)
    parser.add_argument("--plan-digest", required=True)
    parser.add_argument("--stage-id", required=True)
    parser.add_argument("--stage-order", required=True, type=int)
    parser.add_argument("--test-json", required=True)
    parser.add_argument("--parameters-json", required=True)
    parser.add_argument("--participants-json", required=True)
    parser.add_argument("--participant-roles-json", required=True)
    parser.add_argument("--resolved-inputs-json", required=True)
    parser.add_argument("--result-configmap", required=True)
    parser.add_argument("--model-cache", required=True)

    return parser.parse_args(argv)


def main(argv=None):
    args = parse_args(argv)
    envelope = run_harness(args)
    namespace = os.environ.get("POD_NAMESPACE")
    if not namespace:
        raise HarnessError("POD_NAMESPACE is required", "InfrastructureFailure")

    create_result_configmap(
        namespace,
        args.result_configmap,
        args.run_uid,
        envelope,
    )

    print(canonical_json(envelope), flush=True)

    return 0 if envelope["outcome"] == "Passed" else 1


if __name__ == "__main__":
    sys.exit(main())
