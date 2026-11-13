# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0
"""Model fixtures shared by the racer-vllm tests: checkpoints, manifests, a fake vLLM."""

import hashlib
import json
import os
import sys
import types

from conftest import FakeTensor, make_safetensors
from fakeserver import Obj

import racer
import racer_vllm
from racer_vllm import importer, loader
from racer_vllm import manifest as mf

URI = "racer://cache1/models/org/llama"
W = {"w1": b"\1" * 300, "w2": b"\2" * 50, "w3": b"\3" * 900, "w4": b"\4" * 8}


def checkpoint():
    """{name: bytes} of a small two-file checkpoint with an index and aux files."""
    a = make_safetensors([("w1", "U8", (300,), W["w1"]), ("w2", "U8", (50,), W["w2"])])
    b = make_safetensors([("w3", "U8", (900,), W["w3"]), ("w4", "F32", (2,), W["w4"])])
    idx = {"metadata": {"total_size": 1258}, "weight_map": {"w1": "a.safetensors", "w2": "a.safetensors",
                                                             "w3": "b.safetensors", "w4": "b.safetensors"}}
    return {
        "config.json": b'{"model_type":"llama"}',
        "tokenizer.json": b"{}",
        "chat_template.jinja": b"{{x}}",
        importer.INDEX_NAME: json.dumps(idx).encode(),
        "a.safetensors": a,
        "b.safetensors": b,
    }


def manifest_for(files: dict, source=None) -> dict:
    entries = []
    for name, data in sorted(files.items()):
        e = {"name": name, "size": len(data), "sha256": hashlib.sha256(data).hexdigest(), "kind": "aux"}
        if name.endswith(".safetensors"):
            n = mf.header_length(data[:8])
            e.update(kind="safetensors", header_bytes=n, tensors=mf.parse_header_json(data[8 : 8 + n], len(data)))
        entries.append(e)
    doc = {"schema": mf.SCHEMA, "source": source or {"hub": "huggingface", "repo": "org/llama", "revision": "r1"},
           "files": entries}
    if importer.INDEX_NAME in files:
        doc["index"] = importer.INDEX_NAME
    return mf.validate(doc)


def put(server, uri, name, data, etag='"v1"'):
    server.objects[uri.request(name).hex] = Obj(bytes(data), etag)


def publish(server, uri=URI, files=None, etag='"v1"'):
    """Put a checkpoint and its manifest into the fake Racer. Returns (ModelURI, files, manifest)."""
    u = racer_vllm.parse_uri(uri) if isinstance(uri, str) else uri
    files = checkpoint() if files is None else files
    m = manifest_for(files)
    for name, data in files.items():
        put(server, u, name, data, etag)
    put(server, u, mf.MANIFEST_NAME, mf.dumps(m).encode(), etag)
    return u, files, m


def serve_dir(server, uri, root):
    """Put every file of a LocalStore root into the fake Racer under uri."""
    for dirpath, _, names in os.walk(root):
        for n in names:
            if n.endswith((".sha256", ".tmp")):
                continue
            p = os.path.join(dirpath, n)
            with open(p, "rb") as f:
                put(server, uri, os.path.relpath(p, root), f.read())


def client(server, **kw):
    return racer.Client(socket_path=server.path, **kw)


# A fake vLLM ------------------------------------------------------------------


class Source:
    """vLLM v0.33's DefaultModelLoader.Source."""

    def __init__(self, model_or_path, revision=None, subfolder=None, prefix="", fall_back_to_pt=True,
                 allow_patterns_overrides=None, is_unused_weight=None):
        self.model_or_path = model_or_path
        self.revision = revision
        self.subfolder = subfolder
        self.prefix = prefix
        self.fall_back_to_pt = fall_back_to_pt
        self.allow_patterns_overrides = allow_patterns_overrides
        self.is_unused_weight = is_unused_weight


class DefaultModelLoader:
    """The parts of vLLM v0.33's DefaultModelLoader (and BaseModelLoader) the Racer loader builds on."""

    Source = Source

    def __init__(self, load_config):
        self.load_config = load_config
        self.local_expert_ids = None
        self.prepared = []
        self.events = []

    def _prepare_weights(
        self, model_name_or_path, subfolder, revision, fall_back_to_pt, allow_patterns_overrides
    ) -> "tuple[str, list[str], bool, str]":
        raise AssertionError("the racer loader must override _prepare_weights")

    def _get_weights_iterator(self, source):
        self.prepared.append(
            self._prepare_weights(source.model_or_path, source.subfolder, source.revision,
                                  source.fall_back_to_pt, source.allow_patterns_overrides)
        )
        raise AssertionError("the racer loader must override _get_weights_iterator")

    def get_all_weights(self, model_config, model):
        primary = Source(model_config.model, model_config.revision, prefix="",
                         is_unused_weight=getattr(model, "is_unused_checkpoint_weight", None))
        self.prepared.append(self._prepare_weights(primary.model_or_path, primary.subfolder, primary.revision,
                                                   True, None))
        yield from self._get_weights_iterator(primary)
        for s in getattr(model, "secondary_weights", ()):
            yield from self._get_weights_iterator(s)

    def load_weights(self, model, model_config):
        model.load_weights(self.get_all_weights(model_config, model))

    def download_model(self, model_config):
        raise AssertionError("override")

    def create_model(self, vllm_config, model_config, prefix=""):
        return vllm_config.make_model()

    def load_model(self, vllm_config, model_config, prefix=""):
        model = self.create_model(vllm_config=vllm_config, model_config=model_config, prefix=prefix)
        self.load_weights(model, model_config)
        self.events.append("process_weights_after_loading")
        return model


class DefaultModelLoaderV030(DefaultModelLoader):
    """v0.30: _prepare_weights returns three values."""

    def _prepare_weights(
        self, model_name_or_path, subfolder, revision, fall_back_to_pt, allow_patterns_overrides
    ) -> "tuple[str, list[str], bool]":
        raise AssertionError("override")


def install_fake_vllm(monkeypatch, base=DefaultModelLoader):
    """Put a fake vLLM in sys.modules. Returns the loader registry."""
    registry = {"calls": []}
    names = ("vllm", "vllm.model_executor", "vllm.model_executor.model_loader",
             "vllm.model_executor.model_loader.default_loader",
             "vllm.model_executor.model_loader.sharded_state_loader", "vllm.distributed")
    mods = {n: types.ModuleType(n) for n in names}
    mods["vllm"].__version__ = "0.33.0"

    def register_model_loader(name):
        def wrap(cls):
            assert issubclass(cls, DefaultModelLoader)
            registry["calls"].append(name)
            registry[name] = cls
            return cls

        return wrap

    mods["vllm.model_executor.model_loader"].register_model_loader = register_model_loader
    mods["vllm.model_executor.model_loader.default_loader"].DefaultModelLoader = base
    mods["vllm.model_executor.model_loader.sharded_state_loader"].ShardedStateLoader = types.SimpleNamespace(
        _filter_subtensors=lambda sd: dict(sd)
    )
    set_parallel(mods["vllm.distributed"])
    mods["vllm"].distributed = mods["vllm.distributed"]
    for n, m in mods.items():
        monkeypatch.setitem(sys.modules, n, m)
    monkeypatch.setattr(racer_vllm, "_registered", False)
    monkeypatch.setattr(loader, "_built", {})
    return registry


def set_parallel(dist, tp=1, pp=1, rank=0):
    """Give a fake vllm.distributed module a parallel layout."""
    dist.get_tensor_model_parallel_world_size = lambda: tp
    dist.get_pp_group = lambda: types.SimpleNamespace(world_size=pp)
    dist.get_world_group = lambda: types.SimpleNamespace(rank_in_group=rank)


class Model:
    """Records loaded weights. state holds FakeTensors for the sharded path."""

    def __init__(self, state=None, secondary=()):
        self.got = {}
        self.state = state or {}
        self.secondary_weights = list(secondary)

    def load_weights(self, weights):
        for name, t in weights:
            self.got[name] = t.bytes()
        return set(self.got)

    def state_dict(self):
        return dict(self.state)


def model_state(values: dict):
    """{name: FakeTensor} holding a copy of each value's bytes."""
    return {n: FakeTensor(bytearray(v)) for n, v in values.items()}


def model_config(model, **kw):
    fields = {"revision": None, "dtype": "bfloat16", "quantization": None, "hf_config": None, "hf_overrides": None}
    return types.SimpleNamespace(model=model, **{**fields, **kw})


def make_loader(monkeypatch, server, base=DefaultModelLoader, **extra):
    registry = install_fake_vllm(monkeypatch, base)
    racer_vllm.register()
    racer_vllm.register()
    assert registry["calls"] == ["racer"]
    cfg = {"socket": server.path, "staging_bytes": 4096, "max_staging_bytes": 1 << 20, "concurrency": 2,
           "delivery": "host", "sharded": "off", **extra}
    return registry["racer"](types.SimpleNamespace(model_loader_extra_config=cfg))
