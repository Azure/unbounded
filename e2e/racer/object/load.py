# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0
"""Real vLLM plugin/Run:ai load through the production loopback frontend."""

import importlib.metadata
import json
import socket
import tempfile
import time

import torch
from vllm.config.load import LoadConfig
from vllm.model_executor.model_loader import get_model_loader
from vllm.model_executor.model_loader.weight_utils import default_weight_loader
from vllm.plugins import load_general_plugins

assert importlib.metadata.version("vllm").split("+")[0] == "0.29.0"
assert importlib.metadata.version("runai-model-streamer") == "0.16.1"
load_general_plugins()

deadline = time.monotonic() + 30
while True:
    try:
        with socket.create_connection(("127.0.0.1", 8000), timeout=1):
            break
    except OSError:
        if time.monotonic() >= deadline:
            raise
        time.sleep(0.1)

loader = get_model_loader(LoadConfig(
    load_format="racer_object",
    model_loader_extra_config={"files": ["s3://models/model.safetensors"], "concurrency": 2},
))
model = torch.nn.Linear(1024, 1024)
parameters, loaded = dict(model.named_parameters()), set()
# No checkpoint is present locally. Discovery would fail against racer-object.
with tempfile.TemporaryDirectory() as local:
    for name, tensor in loader._get_weights_iterator(local, None):
        assert name not in loaded, f"duplicate tensor: {name}"
        default_weight_loader(parameters[name], tensor)
        loaded.add(name)
assert loaded == set(parameters), f"missing weights: {set(parameters) - loaded}"
expected_weight = (torch.arange(1024 * 1024) % 97).float().reshape(1024, 1024) / 16
expected_bias = torch.arange(1024).float() / 16
torch.testing.assert_close(model.weight, expected_weight, rtol=0, atol=0)
torch.testing.assert_close(model.bias, expected_bias, rtol=0, atol=0)
with torch.no_grad():
    output = model(torch.ones(1, 1024))
torch.testing.assert_close(output, expected_weight.sum(dim=1).unsqueeze(0) + expected_bias,
                           rtol=0, atol=0)
print(json.dumps({"loader": "racer_object", "loaded": sorted(loaded),
                  "output_sum": output.sum().item()}))
