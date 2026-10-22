# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0
import json
import os
import sys
import types

import pytest
from conftest import make_safetensors
from fakeserver import Obj

import racer
from racer_vllm import loader, model
from racer_vllm.weights import MemoryBackend, iter_weights


def put(server, uri, name, data):
    server.objects[uri.request(name).hex] = Obj(data)


@pytest.fixture
def uri():
    return model.parse_uri("racer://cache1/models/org/llama")


def test_parse_uri():
    u = model.parse_uri("racer://c/b/a/b/c/")
    assert (u.cache, u.bucket, u.prefix) == ("c", "b", "a/b/c")
    assert u.key("config.json") == "a/b/c/config.json"
    assert str(u) == "racer://c/b/a/b/c"
    assert model.parse_uri("racer://c/b").key("x") == "x"
    for bad in ["s3://c/b", "racer://c", "racer:///b", "racer://c/"]:
        with pytest.raises(ValueError):
            model.parse_uri(bad)


def shards():
    s1 = make_safetensors([("w1", "U8", (300,), b"\1" * 300), ("w2", "U8", (50,), b"\2" * 50)])
    s2 = make_safetensors([("w3", "U8", (900,), b"\3" * 900), ("w4", "F32", (2,), b"\0" * 8)])
    return s1, s2


def setup_model(server, uri, index=True):
    s1, s2 = shards()
    put(server, uri, "config.json", b'{"model_type":"llama"}')
    put(server, uri, "tokenizer.json", b"{}")
    if index:
        idx = {
            "metadata": {"total_size": 1258, "extra": "chat_template.jinja", "bad": "../x.json"},
            "weight_map": {"w1": "a.safetensors", "w2": "a.safetensors", "w3": "b.safetensors", "w4": "b.safetensors"},
        }
        put(server, uri, model.INDEX, json.dumps(idx).encode())
        put(server, uri, "chat_template.jinja", b"{{x}}")
        put(server, uri, "a.safetensors", s1)
        put(server, uri, "b.safetensors", s2)
    else:
        put(server, uri, model.SINGLE, s1)


def test_fetch_and_marker(server, uri, tmp_path):
    setup_model(server, uri)
    c = racer.Client(socket_path=server.path)
    got = model.fetch(uri, str(tmp_path / "m"), client=c)
    assert set(got) == {"config.json", "tokenizer.json", model.INDEX, "chat_template.jinja"}
    assert (tmp_path / "m" / "chat_template.jinja").read_bytes() == b"{{x}}"
    assert model.resolve_uri(str(tmp_path / "m"), {}) == uri
    assert model.resolve_uri("/nowhere", {"uri": "racer://x/y/z"}) == model.parse_uri("racer://x/y/z")
    with pytest.raises(ValueError):
        model.resolve_uri(str(tmp_path), {})


def test_fetch_requires_config(server, uri, tmp_path):
    c = racer.Client(socket_path=server.path)
    with pytest.raises(FileNotFoundError):
        model.fetch(uri, str(tmp_path / "m"), client=c)


def test_weight_files(server, uri):
    c = racer.Client(socket_path=server.path)
    setup_model(server, uri)
    assert model.weight_files(c, uri) == ["a.safetensors", "b.safetensors"]


def test_weight_files_single(server, uri):
    c = racer.Client(socket_path=server.path)
    setup_model(server, uri, index=False)
    assert model.weight_files(c, uri) == [model.SINGLE]
    server.objects.clear()
    with pytest.raises(racer.NotFound):
        model.weight_files(c, uri)


@pytest.mark.parametrize("depth", [1, 2, 3])
def test_iter_weights(server, uri, depth):
    setup_model(server, uri)
    c = racer.Client(socket_path=server.path, staging_bytes=4096)
    got = dict(iter_weights(c, uri, MemoryBackend(), staging_bytes=400, depth=depth))
    assert got == {"w1": b"\1" * 300, "w2": b"\2" * 50, "w3": b"\3" * 900, "w4": b"\0" * 8}
    # w3 is larger than staging, so it was read on its own.
    sizes = []
    for s in server.seen:
        if s.method == "POST" and "range" in s.headers:
            first, last = s.headers["range"][len("bytes=") :].split("-")
            sizes.append(int(last) - int(first) + 1)
    assert 900 in sizes


class _Param:
    pass


def install_fake_vllm(monkeypatch):
    registry = {}
    vllm = types.ModuleType("vllm")
    me = types.ModuleType("vllm.model_executor")
    ml = types.ModuleType("vllm.model_executor.model_loader")
    bl = types.ModuleType("vllm.model_executor.model_loader.base_loader")

    class BaseModelLoader:
        def __init__(self, load_config):
            self.load_config = load_config

    def register_model_loader(name):
        def wrap(cls):
            assert issubclass(cls, BaseModelLoader)
            registry[name] = cls
            return cls

        return wrap

    ml.register_model_loader = register_model_loader
    bl.BaseModelLoader = BaseModelLoader
    for name, mod in {
        "vllm": vllm,
        "vllm.model_executor": me,
        "vllm.model_executor.model_loader": ml,
        "vllm.model_executor.model_loader.base_loader": bl,
    }.items():
        monkeypatch.setitem(sys.modules, name, mod)
    monkeypatch.setattr(loader, "_registered", False)
    return registry


def test_register_and_load(server, uri, monkeypatch, tmp_path):
    registry = install_fake_vllm(monkeypatch)
    loader.register()
    loader.register()
    cls = registry["racer"]
    setup_model(server, uri)
    model.fetch(uri, str(tmp_path / "m"), client=racer.Client(socket_path=server.path))
    monkeypatch.setenv("RACER_SOCKET", server.path)
    cfg = types.SimpleNamespace(model_loader_extra_config={"staging_bytes": 400, "concurrency": 2})
    inst = cls(cfg)
    mc = types.SimpleNamespace(model=str(tmp_path / "m"), quantization=None)
    inst.download_model(mc)

    # Swap the torch backend for the memory one, as there is no GPU here.
    monkeypatch.setattr(loader, "TorchBackend", lambda **kw: MemoryBackend())

    class Model:
        def __init__(self):
            self.got = {}

        def load_weights(self, weights):
            for name, t in weights:
                self.got[name] = t
            return set(self.got)

        def named_parameters(self):
            return [(n, _Param()) for n in ("w1", "w2", "w3", "w4")]

    m = Model()
    inst.load_weights(m, mc)
    assert m.got["w3"] == b"\3" * 900

    class Missing(Model):
        def named_parameters(self):
            return super().named_parameters() + [("w5", _Param())]

    with pytest.raises(ValueError, match="w5"):
        inst.load_weights(Missing(), mc)


def test_entry_point_declared():
    here = os.path.dirname(__file__)
    text = open(os.path.join(here, "..", "pyproject.toml")).read()
    assert '[project.entry-points."vllm.general_plugins"]' in text
    assert 'racer = "racer_vllm:register"' in text
