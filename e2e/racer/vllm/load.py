"""Exercise vLLM's real S3 weight iterator and parameter loader on CPU."""

import json

import torch
from vllm.model_executor.model_loader.weight_utils import (
    default_weight_loader,
    runai_safetensors_weights_iterator,
)


model = torch.nn.Linear(1024, 1024)
parameters = dict(model.named_parameters())
loaded = set()
for name, tensor in runai_safetensors_weights_iterator(
    ["s3://models/model.safetensors"], use_tqdm_on_load=False
):
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
torch.testing.assert_close(
    output, expected_weight.sum(dim=1).unsqueeze(0) + expected_bias, rtol=0, atol=0
)
print(json.dumps({"loaded": sorted(loaded), "output_sum": output.sum().item()}))
