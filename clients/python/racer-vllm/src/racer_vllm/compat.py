# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0
"""Adapters between vLLM releases (tested shapes: v0.30 and v0.33).

Every helper detects features with hasattr and inspect.signature. Nothing
here compares version strings.
"""

from __future__ import annotations

import importlib
import inspect
import re


class VllmMissing(ImportError):
    """vLLM (or torch) is not importable in this environment."""


def _import(name: str):
    try:
        return importlib.import_module(name)
    except ImportError as exc:
        raise VllmMissing(
            f"racer-vllm needs {name.split('.')[0]} from the host image, but importing {name} failed: {exc}"
        ) from exc


def vllm_version() -> str:
    return str(getattr(_import("vllm"), "__version__", "unknown"))


def register_model_loader():
    """vLLM's register_model_loader, from wherever this release exposes it."""
    for mod in ("vllm.model_executor.model_loader", "vllm.model_executor.models.registry"):
        try:
            fn = getattr(importlib.import_module(mod), "register_model_loader", None)
        except ImportError:
            continue
        if callable(fn):
            return fn
    raise VllmMissing("this vLLM does not expose register_model_loader")


def default_model_loader():
    for mod in ("vllm.model_executor.model_loader.default_loader", "vllm.model_executor.model_loader"):
        try:
            cls = getattr(importlib.import_module(mod), "DefaultModelLoader", None)
        except ImportError:
            continue
        if cls is not None:
            return cls
    raise VllmMissing("this vLLM does not expose DefaultModelLoader")


def params(fn) -> list:
    """Parameter names after self, or [] when the signature is unavailable."""
    try:
        names = list(inspect.signature(fn).parameters)
    except (TypeError, ValueError):
        return []
    return names[1:] if names and names[0] == "self" else names


def source_has_is_unused_weight(source) -> bool:
    """Does vLLM's Source carry is_unused_weight (a field or attribute)?"""
    fields = getattr(source, "__dataclass_fields__", None) or {}
    return "is_unused_weight" in fields or hasattr(source, "is_unused_weight")


def source_is_unused(source):
    """A name -> bool predicate from source.is_unused_weight, or None.

    The value may be a callable or a collection snapshot of names.
    """
    v = getattr(source, "is_unused_weight", None)
    if v is None:
        return None
    if callable(v):
        return v
    names = frozenset(v)
    return names.__contains__


def prepare_weights_params(loader_cls) -> list:
    return params(getattr(loader_cls, "_prepare_weights", None))


def get_all_weights_params(loader_cls) -> list:
    return params(getattr(loader_cls, "get_all_weights", None))


def adapt_call(fn, args: tuple, kwargs: dict) -> dict:
    """Map positional and keyword args onto fn's named parameters, dropping unknown keywords."""
    names = params(fn)
    out = dict(zip(names, args))
    for k, v in kwargs.items():
        if not names or k in names:
            out[k] = v
    return out


def has_mm_encoder_only_filter(loader) -> bool:
    return hasattr(loader, "_init_mm_encoder_only_weight_filter")


def model_has_is_unused_checkpoint_weight(model) -> bool:
    return callable(getattr(model, "is_unused_checkpoint_weight", None))


_EXPERT = re.compile(r"\.experts\.(\d+)\.")


def expert_filter(local_expert_ids):
    """name -> bool keep-predicate for expert parallel loads, or None.

    Uses vLLM's ep_weight_filter helper when importable, else matches
    ".experts.<id>." in the name.
    """
    if local_expert_ids is None:
        return None
    local = frozenset(int(i) for i in local_expert_ids)
    try:
        mod = importlib.import_module("vllm.model_executor.model_loader.ep_weight_filter")
    except ImportError:
        mod = None
    for attr in ("should_skip_weight", "skip_non_local_expert"):
        fn = getattr(mod, attr, None) if mod else None
        if callable(fn) and len(params(fn)) >= 2:
            return lambda name: not fn(name, local)

    def keep(name: str) -> bool:
        m = _EXPERT.search(name)
        return m is None or int(m.group(1)) in local

    return keep


def argument_parser():
    """vLLM's FlexibleArgumentParser (it accepts --flag=value and YAML configs), else argparse."""
    for mod in ("vllm.utils.argparse_utils", "vllm.utils"):
        try:
            cls = getattr(importlib.import_module(mod), "FlexibleArgumentParser", None)
        except ImportError:
            continue
        if cls is not None:
            return cls()
    import argparse

    return argparse.ArgumentParser()


def prepare_weights_arity(loader_cls) -> int:
    """How many values DefaultModelLoader._prepare_weights returns: 3 (v0.30) or 4 (v0.33, adds the index file).

    Read from the return annotation of the first class in the MRO that is not
    ours; 3 when there is none.
    """
    for klass in loader_cls.__mro__:
        fn = klass.__dict__.get("_prepare_weights")
        if fn is None or klass.__module__.startswith("racer_vllm"):
            continue
        ann = getattr(fn, "__annotations__", {}).get("return")
        text = ann if isinstance(ann, str) else repr(ann)
        if "tuple" not in text.lower():
            return 3
        inner = text[text.index("[") + 1 : text.rindex("]")] if "[" in text else ""
        depth, parts = 0, 1
        for ch in inner:
            if ch == "[":
                depth += 1
            elif ch == "]":
                depth -= 1
            elif ch == "," and depth == 0:
                parts += 1
        return parts if inner else 3
    return 3


def mm_encoder_only_filter():
    """vLLM's filter_mm_encoder_only_safetensors_files, or None on releases without it."""
    try:
        mod = importlib.import_module("vllm.model_executor.model_loader.weight_utils")
    except ImportError:
        return None
    fn = getattr(mod, "filter_mm_encoder_only_safetensors_files", None)
    return fn if callable(fn) else None
