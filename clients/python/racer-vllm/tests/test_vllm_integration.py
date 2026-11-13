# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0
"""Checks against a real vLLM install. CI runs these with ``pytest -m vllm``.

Each test skips without vLLM, rather than the whole module, so ``-m vllm``
still collects tests and exits 0 instead of 5 ("no tests collected").
"""

import inspect

import pytest

import racer_vllm
from racer_vllm import compat, loader

pytestmark = pytest.mark.vllm


@pytest.fixture(scope="module")
def base():
    pytest.importorskip("vllm")
    return compat.default_model_loader()


def test_loader_subclass_registers(base):
    racer_vllm.register()
    racer_vllm.register()
    cls = loader.loader_class()
    assert issubclass(cls, base) and issubclass(cls, loader.RacerLoaderMixin)
    from vllm.model_executor import model_loader as ml

    get = getattr(ml, "get_model_loader", None)
    formats = getattr(ml, "_LOAD_FORMAT_TO_MODEL_LOADER", None)
    if isinstance(formats, dict):
        assert formats.get(loader.LOAD_FORMAT) is cls
    else:
        assert callable(get)


def _bind_like(base_fn, ours):
    """Call ours's signature with every argument base_fn's callers may pass."""
    params = list(inspect.signature(base_fn).parameters.values())[1:]
    args = [object() for p in params if p.kind in (p.POSITIONAL_ONLY, p.POSITIONAL_OR_KEYWORD)]
    kwargs = {p.name: object() for p in params if p.kind == p.KEYWORD_ONLY}
    inspect.signature(ours).bind(object(), *args, **kwargs)
    by_name = {p.name: object() for p in params if p.kind in (p.POSITIONAL_OR_KEYWORD, p.KEYWORD_ONLY)}
    inspect.signature(ours).bind(object(), **by_name)


def test_prepare_weights_accepts_vllms_signature(base):
    assert compat.prepare_weights_params(base)[0] in ("model_name_or_path", "model")
    _bind_like(base._prepare_weights, loader.RacerLoaderMixin._prepare_weights)


def test_get_all_weights_accepts_vllms_signature(base):
    names = compat.get_all_weights_params(base)
    assert names[:2] == ["model_config", "model"]
    _bind_like(base.get_all_weights, loader.RacerLoaderMixin.get_all_weights)


def test_base_calls_the_hooks_the_loader_overrides(base):
    src = inspect.getsource(base.get_all_weights)
    assert "_get_weights_iterator" in src
    assert "secondary_weights" in src
    source = getattr(base, "Source", None)
    assert source is not None
    fields = getattr(source, "__dataclass_fields__", None) or inspect.signature(source).parameters
    assert {"model_or_path", "prefix"} <= set(fields)
    for name in ("load_weights", "download_model", "load_model"):
        assert callable(getattr(base, name, None))
    for name in ("load_weights", "load_model"):
        params = compat.params(getattr(base, name))
        assert params[:1] in (["model"], ["vllm_config"]), (name, params)


def test_loader_accepts_racer_settings(base):
    from vllm.config import LoadConfig

    extra = {"uri": "racer://c/b/p", "sharded": "auto", "verify": "size"}
    cfg = LoadConfig(load_format="racer", model_loader_extra_config=dict(extra))
    ld = loader.loader_class()(cfg)
    assert ld.racer.uri == "racer://c/b/p"
    assert cfg.model_loader_extra_config == extra
