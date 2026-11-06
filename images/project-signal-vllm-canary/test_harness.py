# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0

import argparse
import base64
import json
import os
import unittest
from unittest import mock

import harness


class HarnessTest(unittest.TestCase):
    def test_monotonic_duration_never_rounds_above_timestamp_window(self):
        self.assertEqual(harness.monotonic_duration(0.010999), "0.010s")
        self.assertEqual(harness.monotonic_duration(-1), "0.000s")

    def test_prompt_fixture_digest_is_canonical(self):
        self.assertEqual(
            harness.sha256_text(harness.canonical_json(harness.PROMPT_FIXTURE)),
            harness.PROMPT_FIXTURE_DIGEST,
        )

    def test_contract_rejects_mutable_identity(self):
        parameters = {
            "expectedModelIdentity": "EleutherAI/pythia-70m-deduped:latest",
            "expectedModelRevision": harness.MODEL_REVISION,
            "expectedModelWeightsDigest": harness.MODEL_WEIGHTS_DIGEST,
            "promptFixtureDigest": harness.PROMPT_FIXTURE_DIGEST,
            "requestTimeout": "2m",
        }
        test = {
            "identity": {
                "name": "project-signal-vllm-canary-v1",
                "trust": "Approved",
            }
        }

        with self.assertRaisesRegex(harness.HarnessError, "immutable harness contract"):
            harness.verify_contract(parameters, test, approved_model_input())

    def test_contract_rejects_unpinned_model_input(self):
        parameters = approved_parameters()
        test = {
            "identity": {
                "name": "project-signal-vllm-canary-v1",
                "trust": "Approved",
            }
        }

        with self.assertRaisesRegex(harness.HarnessError, "approved model cache manifest"):
            harness.verify_contract(
                parameters,
                test,
                [{"kind": "Model", "state": "Complete", "digest": "sha256:" + "0" * 64}],
            )

    def test_parameter_values_rejects_duplicates(self):
        values = [
            parameter("requestTimeout", "Duration", "2m"),
            parameter("requestTimeout", "Duration", "3m"),
        ]

        with self.assertRaisesRegex(harness.HarnessError, "duplicated"):
            harness.parameter_values(values)

    def test_server_command_uses_vllm_0251_supported_offline_flags(self):
        command = harness.server_command("/models/model")

        self.assertEqual(command[:3], ["vllm", "serve", "/models/model"])
        self.assertIn("--tensor-parallel-size", command)
        self.assertIn("--load-format", command)
        self.assertIn("--enforce-eager", command)
        self.assertNotIn("--disable-log-requests", command)

    def test_verify_model_cache_checks_approved_architecture(self):
        config = json.dumps(
            {
                "architectures": ["GPTNeoXForCausalLM"],
                "hidden_size": 512,
                "num_attention_heads": 8,
            }
        )

        with (
            mock.patch.object(harness.subprocess, "run"),
            mock.patch("builtins.open", mock.mock_open(read_data=config)),
        ):
            harness.verify_model_cache("/models/model")

    def test_verify_model_cache_rejects_non_tp8_shape(self):
        config = json.dumps(
            {
                "architectures": ["GPTNeoXForCausalLM"],
                "hidden_size": 512,
                "num_attention_heads": 4,
            }
        )

        with (
            mock.patch.object(harness.subprocess, "run"),
            mock.patch("builtins.open", mock.mock_open(read_data=config)),
            self.assertRaisesRegex(harness.HarnessError, "TP=8 shape"),
        ):
            harness.verify_model_cache("/models/model")

    def test_run_harness_with_offline_mocks(self):
        args = argparse.Namespace(
            invocation_id="invocation-1",
            run_uid="run-uid",
            plan_digest="sha256:" + "a" * 64,
            stage_id="vllm-canary",
            stage_order=0,
            test_json=json.dumps(
                {
                    "identity": {
                        "name": "project-signal-vllm-canary-v1",
                        "uid": "test-uid",
                        "generation": 1,
                        "contentDigest": "sha256:" + "b" * 64,
                        "trust": "Approved",
                    }
                }
            ),
            parameters_json=json.dumps(
                [
                    parameter("expectedModelIdentity", "String", harness.MODEL_IDENTITY),
                    parameter("expectedModelRevision", "String", harness.MODEL_REVISION),
                    parameter(
                        "expectedModelWeightsDigest",
                        "String",
                        harness.MODEL_WEIGHTS_DIGEST,
                    ),
                    parameter(
                        "promptFixtureDigest",
                        "String",
                        harness.PROMPT_FIXTURE_DIGEST,
                    ),
                    parameter("requestTimeout", "Duration", "2m"),
                ]
            ),
            participants_json=json.dumps([{"nodeName": "redacted", "nodeUID": "node-uid"}]),
            participant_roles_json=json.dumps({"0": "primary"}),
            resolved_inputs_json=json.dumps(
                approved_model_input()
            ),
            result_configmap="result",
            model_cache="/models/model",
        )
        process = mock.Mock()
        process.poll.return_value = None

        with (
            mock.patch.object(harness, "verify_model_cache"),
            mock.patch.object(harness, "visible_gpu_count", return_value=8),
            mock.patch.object(harness.subprocess, "Popen", return_value=process),
            mock.patch.object(harness, "wait_for_service", return_value=(True, 0)),
            mock.patch.object(harness, "active_gpu_count", return_value=8),
            mock.patch.object(harness, "gpu_memory_used_bytes", return_value=1024),
            mock.patch.object(
                harness,
                "issue_completion",
                return_value={
                    "promptTokens": 5,
                    "completionTokens": 8,
                    "endToEndLatency": 1.0,
                    "timeToFirstToken": 0.2,
                    "outputTokensPerSecond": 10.0,
                },
            ),
            mock.patch.object(harness, "stop_server", return_value=True),
            mock.patch("builtins.open", mock.mock_open()),
        ):
            envelope = harness.run_harness(args)

        self.assertEqual(envelope["outcome"], "Passed")
        self.assertEqual(envelope["test"]["uid"], "test-uid")
        self.assertEqual(
            measurement_value(envelope, "activeGpuCount")["integerValue"],
            8,
        )
        self.assertEqual(
            measurement_value(envelope, "promptTokens")["integerValue"],
            5,
        )
        self.assertNotIn(
            "restartSourceCounts",
            [measurement["name"] for measurement in envelope["measurements"]],
        )
        self.assertNotIn("redacted", json.dumps(envelope))

    def test_run_harness_fails_when_all_visible_gpus_are_not_active(self):
        args = passing_args()
        process = mock.Mock()
        process.poll.return_value = None

        with (
            mock.patch.object(harness, "verify_model_cache"),
            mock.patch.object(harness, "visible_gpu_count", return_value=8),
            mock.patch.object(harness.subprocess, "Popen", return_value=process),
            mock.patch.object(harness, "wait_for_service", return_value=(True, 0)),
            mock.patch.object(harness, "active_gpu_count", return_value=7),
            mock.patch.object(harness, "gpu_memory_used_bytes", return_value=1024),
            mock.patch.object(harness, "stop_server", return_value=True),
            mock.patch("builtins.open", mock.mock_open()),
        ):
            envelope = harness.run_harness(args)

        self.assertEqual(envelope["outcome"], "Failed")
        self.assertEqual(envelope["reasonCode"], "AssertionFailed")
        self.assertEqual(
            measurement_value(envelope, "activeGpuCount")["integerValue"],
            7,
        )

    def test_cleanup_failure_makes_passing_run_inconclusive(self):
        args = passing_args()
        process = mock.Mock()
        process.poll.return_value = None

        with (
            mock.patch.object(harness, "verify_model_cache"),
            mock.patch.object(harness, "visible_gpu_count", return_value=8),
            mock.patch.object(harness.subprocess, "Popen", return_value=process),
            mock.patch.object(harness, "wait_for_service", return_value=(True, 0)),
            mock.patch.object(harness, "active_gpu_count", return_value=8),
            mock.patch.object(harness, "gpu_memory_used_bytes", return_value=1024),
            mock.patch.object(
                harness,
                "issue_completion",
                return_value={
                    "promptTokens": 5,
                    "completionTokens": 8,
                    "endToEndLatency": 1.0,
                    "timeToFirstToken": 0.2,
                    "outputTokensPerSecond": 10.0,
                },
            ),
            mock.patch.object(harness, "stop_server", return_value=False),
            mock.patch("builtins.open", mock.mock_open()),
        ):
            envelope = harness.run_harness(args)

        self.assertEqual(envelope["outcome"], "Inconclusive")
        self.assertEqual(envelope["reasonCode"], "InfrastructureFailure")
        self.assertIn("did not terminate cleanly", envelope["message"])

    def test_result_configmap_is_immutable_and_contains_canonical_envelope(self):
        response = mock.MagicMock()
        response.status = 201
        response.__enter__.return_value = response
        envelope = {"schemaVersion": harness.RESULT_SCHEMA, "outcome": "Passed"}

        with (
            mock.patch.dict(
                os.environ,
                {
                    "KUBERNETES_SERVICE_HOST": "kubernetes.invalid",
                    "KUBERNETES_SERVICE_PORT_HTTPS": "443",
                },
                clear=False,
            ),
            mock.patch("builtins.open", mock.mock_open(read_data="token")),
            mock.patch.object(harness.ssl, "create_default_context"),
            mock.patch.object(harness.urllib.request, "urlopen", return_value=response) as urlopen,
        ):
            harness.create_result_configmap("signal", "result", "run-uid", envelope)

        request = urlopen.call_args.args[0]
        body = json.loads(request.data)
        self.assertTrue(body["immutable"])
        self.assertEqual(
            base64.b64decode(body["binaryData"]["result.json"]),
            harness.canonical_json(envelope).encode(),
        )
        self.assertEqual(
            body["metadata"]["labels"]["signal.unbounded-cloud.io/run-uid"],
            "run-uid",
        )


def parameter(name, value_type, value):
    field = {
        "String": "stringValue",
        "Duration": "durationValue",
    }[value_type]

    return {"name": name, "value": {"type": value_type, field: value}}


def approved_parameters():
    return {
        "expectedModelIdentity": harness.MODEL_IDENTITY,
        "expectedModelRevision": harness.MODEL_REVISION,
        "expectedModelWeightsDigest": harness.MODEL_WEIGHTS_DIGEST,
        "promptFixtureDigest": harness.PROMPT_FIXTURE_DIGEST,
        "requestTimeout": "2m",
    }


def approved_model_input():
    return [
        {
            "name": "model-cache",
            "kind": "Model",
            "state": "Complete",
            "digest": harness.MODEL_MANIFEST_DIGEST,
        }
    ]


def passing_args():
    return argparse.Namespace(
        invocation_id="invocation-1",
        run_uid="run-uid",
        plan_digest="sha256:" + "a" * 64,
        stage_id="vllm-canary",
        stage_order=0,
        test_json=json.dumps(
            {
                "identity": {
                    "name": "project-signal-vllm-canary-v1",
                    "uid": "test-uid",
                    "generation": 1,
                    "contentDigest": "sha256:" + "b" * 64,
                    "trust": "Approved",
                }
            }
        ),
        parameters_json=json.dumps(
            [
                parameter("expectedModelIdentity", "String", harness.MODEL_IDENTITY),
                parameter("expectedModelRevision", "String", harness.MODEL_REVISION),
                parameter(
                    "expectedModelWeightsDigest",
                    "String",
                    harness.MODEL_WEIGHTS_DIGEST,
                ),
                parameter("promptFixtureDigest", "String", harness.PROMPT_FIXTURE_DIGEST),
                parameter("requestTimeout", "Duration", "2m"),
            ]
        ),
        participants_json=json.dumps([{"nodeName": "redacted", "nodeUID": "node-uid"}]),
        participant_roles_json=json.dumps({"0": "primary"}),
        resolved_inputs_json=json.dumps(approved_model_input()),
        result_configmap="result",
        model_cache="/models/model",
    )


def measurement_value(envelope, name):
    return next(
        measurement["value"]
        for measurement in envelope["measurements"]
        if measurement["name"] == name
    )


if __name__ == "__main__":
    unittest.main()
