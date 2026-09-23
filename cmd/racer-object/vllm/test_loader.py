# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0
"""Execute in the pinned vLLM image against the Go test's live frontend."""

import importlib.metadata
import tempfile
from unittest.mock import patch

import torch
from vllm.config.load import LoadConfig
from vllm.model_executor.model_loader import get_model_loader
from racer_object_vllm import register

assert importlib.metadata.version("vllm").split("+")[0] == "0.29.0"
assert importlib.metadata.version("runai-model-streamer") == "0.16.1"
register()
files = ["s3://models/model.safetensors"]
with tempfile.TemporaryDirectory() as local:
    config = LoadConfig(load_format="racer_object", model_loader_extra_config={"files": files})
    loader = get_model_loader(config)
    assert config.model_loader_extra_config == {"files": files}
    with patch("vllm.model_executor.model_loader.runai_streamer_loader.list_safetensors",
               side_effect=AssertionError("discovery is forbidden")):
        weights = dict(loader._get_weights_iterator(local, None))
    torch.testing.assert_close(weights["weight"],
        (torch.arange(1024 * 1024) % 97).float().reshape(1024, 1024) / 16, rtol=0, atol=0)
    torch.testing.assert_close(weights["bias"], torch.arange(1024).float() / 16, rtol=0, atol=0)
    for remote in ["s3://models", "not-a-local-directory"]:
        try:
            loader._prepare_weights(remote, None)
        except ValueError:
            pass
        else:
            raise AssertionError("accepted remote config")
for value in [None, [], "s3://models/a", ["s3://models/"], ["https://host/a"], ["s3://models/a?x=1"], files * 2]:
    try:
        get_model_loader(LoadConfig(load_format="racer_object", model_loader_extra_config={"files": value}))
    except ValueError:
        pass
    else:
        raise AssertionError(f"accepted invalid file list: {value!r}")
print("Explicit-file plugin: real tensors loaded, discovery forbidden, invalid configs rejected")
