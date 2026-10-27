# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0
"""vLLM sharded_state checkpoints: each parallel rank reads only its own files.

Mirrors vllm/model_executor/model_loader/sharded_state_loader.py
(ShardedStateLoader). A checkpoint saved with ShardedStateLoader.save_model
has files named by a pattern such as model-rank-{rank}-part-{part}.safetensors.
Each file holds tensors of one rank's state dict, already split, so they are
copied straight into the parameters instead of going through
model.load_weights.
"""

from __future__ import annotations

import logging
import string
from dataclasses import dataclass

import racer

from .model import ModelURI

log = logging.getLogger("vllm.racer_vllm.sharded")

TP_ONLY_PATTERN = "model-rank-{rank}-part-{part}.safetensors"
PP_AND_TP_PATTERN = "model-pp-{pp_rank}-tp-{tp_rank}-part-{part}.safetensors"
MAX_PARTS = 100_000


@dataclass(frozen=True)
class Ranks:
    tp_rank: int = 0
    tp_size: int = 1
    pp_rank: int = 0
    pp_size: int = 1


def vllm_ranks() -> Ranks:
    """This worker's ranks from vllm.distributed."""
    from vllm.distributed import (
        get_pipeline_model_parallel_rank,
        get_pp_group,
        get_tensor_model_parallel_rank,
        get_tp_group,
    )

    return Ranks(
        tp_rank=get_tensor_model_parallel_rank(),
        tp_size=get_tp_group().world_size,
        pp_rank=get_pipeline_model_parallel_rank(),
        pp_size=get_pp_group().world_size,
    )


def render(pattern: str | None, ranks: Ranks, part: int) -> str:
    """File name of one part for these ranks, with vLLM's pattern checks."""
    if pattern is None:
        pattern = PP_AND_TP_PATTERN if ranks.pp_size > 1 else TP_ONLY_PATTERN
    fields = {f for _, f, _, _ in string.Formatter().parse(pattern) if f is not None}
    unsupported = fields - {"pp_rank", "tp_rank", "rank", "part"}
    if unsupported:
        raise ValueError(f"pattern {pattern!r} uses unsupported placeholder(s) {sorted(unsupported)}")
    if "part" not in fields:
        raise ValueError(f"pattern {pattern!r} needs a {{part}} placeholder")
    has_tp = "tp_rank" in fields or "rank" in fields
    if ranks.pp_size > 1 and "pp_rank" not in fields:
        raise ValueError(f"pattern {pattern!r} does not embed the pipeline parallel rank")
    if ranks.pp_size == 1 and "pp_rank" in fields:
        raise ValueError(f"pattern {pattern!r} requires pipeline parallelism, but the pipeline parallel size is 1")
    if ranks.tp_size > 1 and not has_tp:
        raise ValueError(f"pattern {pattern!r} does not embed the tensor parallel rank")
    name = pattern.format(pp_rank=ranks.pp_rank, tp_rank=ranks.tp_rank, rank=ranks.tp_rank, part=part)
    if name.startswith("/") or ".." in name.split("/"):
        raise ValueError(f"unsafe file name from pattern: {name!r}")
    return name


def rank_files(client: racer.Client, uri: ModelURI, pattern: str | None, ranks: Ranks) -> list:
    """This rank's files: part 0, 1, ... until one is missing.

    Racer has no listing, so parts are probed in order. save_model numbers
    parts from 0 with no gaps.
    """
    files = []
    for part in range(MAX_PARTS):
        name = render(pattern, ranks, part)
        try:
            client.stat(uri.request(name))
        except racer.NotFound:
            break
        files.append(name)
    if not files:
        raise ValueError(
            f"racer loader: no sharded_state files for this rank in {uri} (looked for {uri.key(render(pattern, ranks, 0))})"
        )
    return files


def load_state(state_dict: dict, weights) -> None:
    """Copy (name, tensor) pairs into state_dict parameters, as ShardedStateLoader does.

    A tensor smaller than its parameter in some dimension (for example LoRA
    padding) is copied into a narrowed view. Every key must be loaded once.
    """
    remaining = dict(state_dict)
    for key, tensor in weights:
        param = remaining.pop(key, None)
        if param is None:
            if key in state_dict:
                raise ValueError(f"racer loader: tensor {key!r} appears twice in the sharded checkpoint")
            raise ValueError(f"racer loader: unexpected tensor {key!r} in the sharded checkpoint")
        if getattr(param, "dtype", None) != getattr(tensor, "dtype", None):
            log.warning(
                "racer: loading weight %s with dtype %s into parameter with dtype %s",
                key,
                getattr(tensor, "dtype", None),
                getattr(param, "dtype", None),
            )
        data = param.data
        shape = tuple(param.shape)
        if len(tuple(tensor.shape)) != len(shape):
            raise ValueError(f"racer loader: tensor {key!r} has shape {tuple(tensor.shape)}, parameter has {shape}")
        for dim, size in enumerate(tensor.shape):
            if size < shape[dim]:
                data = data.narrow(dim, 0, size)
        if tuple(tensor.shape) != shape:
            log.warning("racer: loading tensor of shape %s into parameter %r of shape %s", tuple(tensor.shape), key, shape)
        data.copy_(tensor)
    if remaining:
        raise ValueError(f"racer loader: missing keys {tuple(sorted(remaining))[:10]} in the sharded checkpoint")


def filter_subtensors(state_dict: dict) -> dict:
    """vLLM's filter for tensors that share memory, when vLLM is present."""
    try:
        from vllm.model_executor.model_loader.sharded_state_loader import ShardedStateLoader
    except ImportError:
        return state_dict
    return ShardedStateLoader._filter_subtensors(state_dict)
