# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0
"""vLLM sharded_state checkpoints."""

import sys
import types

import pytest
from conftest import make_safetensors
from fakeserver import Obj
from test_loader import install_fake_vllm

import racer
from racer_vllm import loader, model, sharded
from racer_vllm.sharded import Ranks, render
from racer_vllm.weights import MemoryBackend


@pytest.fixture
def uri():
    return model.parse_uri("racer://cache1/models/org/llama-sharded")


def put(server, uri, name, data):
    server.objects[uri.request(name).hex] = Obj(data)


def test_render_defaults():
    assert render(None, Ranks(), 0) == "model-rank-0-part-0.safetensors"
    assert render(None, Ranks(tp_rank=1, tp_size=2), 3) == "model-rank-1-part-3.safetensors"
    assert render(None, Ranks(1, 2, 1, 2), 0) == "model-pp-1-tp-1-part-0.safetensors"
    assert render("w/{tp_rank}-{part}.st", Ranks(1, 2), 0) == "w/1-0.st"


@pytest.mark.parametrize(
    "pattern,ranks,match",
    [
        ("m-{x}-{part}", Ranks(), "unsupported"),
        ("m-{rank}", Ranks(), "part"),
        ("m-{rank}-{part}", Ranks(0, 1, 0, 2), "pipeline parallel rank"),
        ("m-{pp_rank}-{rank}-{part}", Ranks(), "pipeline parallel size is 1"),
        ("m-{part}", Ranks(0, 2), "tensor parallel rank"),
        ("../m-{rank}-{part}", Ranks(), "unsafe"),
        ("/m-{rank}-{part}", Ranks(), "unsafe"),
    ],
)
def test_render_rejects(pattern, ranks, match):
    with pytest.raises(ValueError, match=match):
        render(pattern, ranks, 0)


def test_rank_files_probes_parts(server, uri):
    for name in ("model-rank-0-part-0.safetensors", "model-rank-0-part-1.safetensors", "model-rank-1-part-0.safetensors"):
        put(server, uri, name, b"x")
    c = racer.Client(socket_path=server.path)
    assert sharded.rank_files(c, uri, None, Ranks(0, 2)) == [
        "model-rank-0-part-0.safetensors",
        "model-rank-0-part-1.safetensors",
    ]
    assert sharded.rank_files(c, uri, None, Ranks(1, 2)) == ["model-rank-1-part-0.safetensors"]
    with pytest.raises(ValueError, match="model-rank-2-part-0"):
        sharded.rank_files(c, uri, None, Ranks(2, 4))


class FakeTensor:
    """Just enough of torch.Tensor for load_state."""

    def __init__(self, shape, dtype="F32", raw=None, base=None):
        self.shape = tuple(shape)
        self.dtype = dtype
        self.raw = raw
        self.base = base
        self.copied = None

    @property
    def data(self):
        return self

    def narrow(self, dim, start, size):
        shape = list(self.shape)
        shape[dim] = size
        return FakeTensor(shape, self.dtype, base=self.base or self)

    def copy_(self, src):
        if tuple(src.shape) != self.shape:
            raise RuntimeError("shape mismatch")
        (self.base or self).copied = (self.shape, src.raw)


def test_load_state_copies_and_narrows(caplog):
    params = {"a": FakeTensor((2, 3)), "b": FakeTensor((4,))}
    load = [("a", FakeTensor((2, 3), raw=b"a")), ("b", FakeTensor((3,), raw=b"b"))]
    with caplog.at_level("WARNING"):
        sharded.load_state(params, iter(load))
    assert params["a"].copied == ((2, 3), b"a")
    # b is padded (as with LoRA): only the first 3 elements are written.
    assert params["b"].copied == ((3,), b"b")
    assert any("shape" in r.getMessage() for r in caplog.records)


def test_load_state_errors():
    with pytest.raises(ValueError, match="missing keys"):
        sharded.load_state({"a": FakeTensor((1,)), "b": FakeTensor((1,))}, iter([("a", FakeTensor((1,)))]))
    with pytest.raises(ValueError, match="unexpected"):
        sharded.load_state({"a": FakeTensor((1,))}, iter([("z", FakeTensor((1,)))]))
    with pytest.raises(ValueError, match="twice"):
        sharded.load_state({"a": FakeTensor((1,))}, iter([("a", FakeTensor((1,))), ("a", FakeTensor((1,)))]))
    with pytest.raises(ValueError, match="shape"):
        sharded.load_state({"a": FakeTensor((1,))}, iter([("a", FakeTensor((1, 1)))]))


def test_load_state_dtype_mismatch_warns(caplog):
    params = {"a": FakeTensor((1,), dtype="BF16")}
    with caplog.at_level("WARNING"):
        sharded.load_state(params, iter([("a", FakeTensor((1,), dtype="F32"))]))
    assert any("dtype" in r.getMessage() for r in caplog.records)


class ShapedBackend(MemoryBackend):
    def tensor(self, target, rel, info, staged):
        return FakeTensor(info.shape, info.dtype, bytes(target.buf[rel : rel + info.nbytes]))

    def joined_tensor(self, joined, info):
        return FakeTensor(info.shape, info.dtype, bytes(joined))


def install_ranks(monkeypatch, tp_rank, tp_size):
    dist = types.ModuleType("vllm.distributed")
    dist.get_tensor_model_parallel_rank = lambda: tp_rank
    dist.get_pipeline_model_parallel_rank = lambda: 0
    dist.get_tp_group = lambda: types.SimpleNamespace(world_size=tp_size)
    dist.get_pp_group = lambda: types.SimpleNamespace(world_size=1)
    monkeypatch.setitem(sys.modules, "vllm.distributed", dist)


@pytest.mark.parametrize("rank", [0, 1])
def test_loader_reads_only_own_rank(server, uri, monkeypatch, rank):
    registry = install_fake_vllm(monkeypatch)
    loader.register()
    install_ranks(monkeypatch, rank, 2)
    for r in (0, 1):
        put(server, uri, f"model-rank-{r}-part-0.safetensors", make_safetensors([("w", "U8", (4,), bytes([r]) * 4)]))
        put(server, uri, f"model-rank-{r}-part-1.safetensors", make_safetensors([("v", "U8", (2,), bytes([r + 10]) * 2)]))
    monkeypatch.setenv("RACER_SOCKET", server.path)
    monkeypatch.setattr(loader, "TorchBackend", lambda **kw: ShapedBackend())
    cfg = types.SimpleNamespace(model_loader_extra_config={"uri": str(uri), "sharded_state": True})
    inst = registry["racer"](cfg)
    mc = types.SimpleNamespace(model="/unused", quantization=None)
    params = {"w": FakeTensor((4,), "U8"), "v": FakeTensor((2,), "U8")}

    class Model:
        def state_dict(self):
            return params

        def load_weights(self, weights):  # pragma: no cover - must not be used
            raise AssertionError("sharded_state copies into the state dict")

    inst.download_model(mc)
    inst.load_weights(Model(), mc)
    assert params["w"].copied == ((4,), bytes([rank]) * 4)
    assert params["v"].copied == ((2,), bytes([rank + 10]) * 2)
    other = {uri.request(f"model-rank-{1 - rank}-part-{p}.safetensors").hex for p in (0, 1)}
    assert not [s for s in server.seen if s.method == "POST" and s.target.split("/")[-1] in other]
