# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0
import json
import logging
import os
import struct
import sys
import types

import pytest
from conftest import FakeTensor, make_safetensors
from fakeserver import Obj

import racer
import racer_vllm as rv

ALL = {"w1": b"\1" * 300, "w2": b"\2" * 50, "w3": b"\3" * 900, "w4": b"\0" * 8}
HERE = os.path.dirname(__file__)


@pytest.fixture
def uri():
    return rv.parse_uri("racer://cache1/models/org/llama")


def put(server, uri, name, data, etag='"v1"'):
    server.objects[uri.request(name).hex] = Obj(data, etag)


def replace(server, uri, name, etag='"v2"'):
    """Swap in a new version of an object, as the origin would."""
    put(server, uri, name, server.objects[uri.request(name).hex].data, etag)


def setup_model(server, uri, index=True):
    s1 = make_safetensors([("w1", "U8", (300,), ALL["w1"]), ("w2", "U8", (50,), ALL["w2"])])
    s2 = make_safetensors([("w3", "U8", (900,), ALL["w3"]), ("w4", "F32", (2,), ALL["w4"])])
    put(server, uri, "config.json", b'{"model_type":"llama"}')
    put(server, uri, "tokenizer.json", b"{}")
    if not index:
        put(server, uri, rv.SINGLE, s1)
        return
    idx = {
        "metadata": {"total_size": 1258, "extra": "chat_template.jinja", "bad": "../x.json"},
        "weight_map": {"w1": "a.safetensors", "w2": "a.safetensors", "w3": "b.safetensors", "w4": "b.safetensors"},
    }
    put(server, uri, rv.INDEX, json.dumps(idx).encode())
    put(server, uri, "chat_template.jinja", b"{{x}}")
    put(server, uri, "a.safetensors", s1)
    put(server, uri, "b.safetensors", s2)


def client(server):
    return racer.Client(socket_path=server.path, staging_bytes=4096)


def chunks_for(c, uri, staging=400, pins=None):
    files = rv.weight_files(c, uri, pins)
    pinned = [(n, rv.stat_pinned(c, uri, n, (pins or {}).get(n))) for n in files]
    return rv.read_chunks(c, uri, pinned, staging)


def weights(c, uri, depth=2, **kw):
    return rv.iter_weights(c, uri, chunks_for(c, uri), 400, depth, use_vmm=False, **kw)


def range_reads(server):
    out = []
    for s in server.seen:
        if s.method == "POST" and "range" in s.headers:
            first, last = (int(x) for x in s.headers["range"][len("bytes=") :].split("-"))
            out.append((s, first, last))
    return out


# URIs and racer-model.json


def test_parse_uri():
    u = rv.parse_uri("racer://c/b/a/b/c/")
    assert (u.cache, u.bucket, u.prefix) == ("c", "b", "a/b/c")
    assert u.key("config.json") == "a/b/c/config.json"
    assert str(u) == "racer://c/b/a/b/c"
    assert rv.parse_uri("racer://c/b").key("x") == "x"
    for bad in ["s3://c/b", "racer://c", "racer:///b", "racer://c/"]:
        with pytest.raises(ValueError):
            rv.parse_uri(bad)


def test_fetch_and_marker(server, uri, tmp_path):
    setup_model(server, uri)
    replace(server, uri, "b.safetensors", '"vb"')
    dest = tmp_path / "m"
    got = rv.fetch(uri, str(dest), client=client(server))
    assert set(got) == {"config.json", "tokenizer.json", rv.INDEX, "chat_template.jinja"}
    assert (dest / "chat_template.jinja").read_bytes() == b"{{x}}"
    assert not (tmp_path / "x.json").exists()
    doc = json.loads((dest / rv.MARKER).read_text())
    assert doc["uri"] == str(uri)
    assert doc["etags"]["config.json"] == '"v1"' and doc["etags"]["a.safetensors"] == '"v1"'
    assert doc["etags"]["b.safetensors"] == '"vb"'
    assert rv.resolve_uri(str(dest), {}) == uri
    assert rv.resolve_pins(str(dest), uri) == doc["etags"]
    # Pins belong to the fetched URI only.
    assert rv.resolve_pins(str(dest), rv.parse_uri("racer://x/y/z")) == {}
    assert rv.resolve_pins(str(tmp_path / "nowhere"), uri) == {}


def test_resolve_uri_order(tmp_path):
    assert rv.resolve_uri("/nowhere", {"uri": "racer://x/y/z"}) == rv.parse_uri("racer://x/y/z")
    assert rv.resolve_uri("racer://a/b/c", {}) == rv.parse_uri("racer://a/b/c")
    with pytest.raises(ValueError, match="no model URI"):
        rv.resolve_uri(str(tmp_path), {})


def test_fetch_single_file(server, uri, tmp_path):
    setup_model(server, uri, index=False)
    rv.fetch(uri, str(tmp_path), client=client(server))
    assert json.loads((tmp_path / rv.MARKER).read_text())["etags"][rv.SINGLE] == '"v1"'


def test_fetch_without_weights_still_writes_marker(server, uri, tmp_path):
    put(server, uri, "config.json", b"{}")
    rv.fetch(uri, str(tmp_path), client=client(server))
    assert json.loads((tmp_path / rv.MARKER).read_text())["etags"] == {"config.json": '"v1"'}


def test_fetch_requires_config(server, uri, tmp_path):
    with pytest.raises(FileNotFoundError):
        rv.fetch(uri, str(tmp_path), client=client(server))


def test_main(server, uri, tmp_path, monkeypatch, capsys):
    setup_model(server, uri)
    monkeypatch.setenv("RACER_SOCKET", server.path)
    assert rv.main([str(uri), str(tmp_path)]) == 0
    assert "config.json" in capsys.readouterr().out.split()
    assert (tmp_path / rv.MARKER).exists()
    with pytest.raises(SystemExit):
        rv.main([str(uri)])


def test_bad_pins_rejected(uri, tmp_path):
    (tmp_path / rv.MARKER).write_text(json.dumps({"uri": str(uri), "etags": {"a": 1}}))
    with pytest.raises(ValueError, match="bad etags"):
        rv.resolve_pins(str(tmp_path), uri)


def test_read_pinned(server, uri):
    setup_model(server, uri)
    c = client(server)
    assert rv.read_pinned(c, uri, "config.json") == (b'{"model_type":"llama"}', '"v1"')
    assert rv.read_pinned(c, uri, "config.json", '"v1"')[1] == '"v1"'
    assert rv.read_pinned(c, uri, "missing.json") is None
    with pytest.raises(rv.ModelChangedError, match="config.json"):
        rv.read_pinned(c, uri, "config.json", '"v0"')
    # A pinned file that was deleted also counts as a change.
    with pytest.raises(rv.ModelChangedError):
        rv.read_pinned(c, uri, "missing.json", '"v1"')


def test_weight_files(server, uri):
    c = client(server)
    with pytest.raises(racer.NotFound):
        rv.weight_files(c, uri)
    setup_model(server, uri, index=False)
    assert rv.weight_files(c, uri) == [rv.SINGLE]
    setup_model(server, uri)
    assert rv.weight_files(c, uri) == ["a.safetensors", "b.safetensors"]
    replace(server, uri, rv.INDEX)
    with pytest.raises(rv.ModelChangedError, match="index"):
        rv.weight_files(c, uri, {rv.INDEX: '"v1"'})


@pytest.mark.parametrize("weight_map", [{}, {"w": "../x.safetensors"}, {"w": "/x.safetensors"}])
def test_weight_files_rejects_bad_index(server, uri, weight_map):
    put(server, uri, rv.INDEX, json.dumps({"weight_map": weight_map}).encode())
    with pytest.raises(ValueError):
        rv.weight_files(client(server), uri)


# safetensors headers and chunk planning


def test_parse_header():
    f = make_safetensors(
        [
            ("a", "F32", (2, 3), struct.pack("<6f", *range(6))),
            ("b", "BF16", (4,), b"\1\2" * 4),
            ("c", "U8", (0,), b""),
            ("d", "I64", (), struct.pack("<q", -7)),
        ],
        metadata={"format": "pt"},
        pad=3,
    )
    n = rv.header_length(f[:8])
    tensors = rv.parse_header(f[8 : 8 + n], len(f))
    assert [(t.name, t.dtype, t.shape) for t in tensors] == [
        ("a", "F32", (2, 3)),
        ("b", "BF16", (4,)),
        ("c", "U8", (0,)),
        ("d", "I64", ()),
    ]
    assert tensors[0].start == 8 + n and tensors[0].end == 8 + n + 24
    assert f[tensors[3].start : tensors[3].end] == struct.pack("<q", -7)
    with pytest.raises(ValueError, match="past end"):
        rv.parse_header(f[8 : 8 + n], len(f) - 1)


@pytest.mark.parametrize(
    "header",
    [
        b"[]",
        b"{not json",
        b"\xff",
        b'{"a":{"dtype":"F4","shape":[1],"data_offsets":[0,1]}}',
        b'{"a":{"dtype":"F32","shape":[2],"data_offsets":[0,4]}}',
        b'{"a":{"dtype":"U8","shape":[2],"data_offsets":[0,2]},"b":{"dtype":"U8","shape":[2],"data_offsets":[1,3]}}',
        b'{"a":{"dtype":"U8","shape":[-1],"data_offsets":[0,0]}}',
        b'{"a":{"dtype":"U8"}}',
        b'{"a":{"dtype":"U8","shape":[1.9],"data_offsets":[0.9,1.9]}}',
        b'{"a":{"dtype":"U8","shape":["1"],"data_offsets":[0,1]}}',
        b'{"a":{"dtype":"U8","shape":[true],"data_offsets":[0,1]}}',
        b'{"a":{"dtype":"U8","shape":[1],"data_offsets":[1,0]}}',
        b'{"a":{"dtype":"U8","shape":[1],"data_offsets":[0,1,2]}}',
        b'{"a":[]}',
    ],
)
def test_bad_headers(header):
    with pytest.raises(ValueError):
        rv.parse_header(header)


def test_bad_header_length():
    for prefix in (struct.pack("<Q", 1 << 40), struct.pack("<Q", 1), b"abc"):
        with pytest.raises(ValueError):
            rv.header_length(prefix)


def plan(sizes, staging, page=0):
    f = make_safetensors([(f"t{i}", "U8", (n,), bytes([i]) * n) for i, n in enumerate(sizes)])
    tensors = rv.parse_header(f[8 : 8 + rv.header_length(f[:8])])
    return tensors, rv.plan_chunks("f", '"e"', tensors, staging, page)


def test_plan_chunks_split_and_dedicated():
    _, chunks = plan([100] * 5 + [500], 250)
    assert [[t.name for t in c.tensors] for c in chunks] == [["t0", "t1"], ["t2", "t3"], ["t4"], ["t5"]]
    assert [c.dedicated for c in chunks] == [False, False, False, True]
    assert all(c.end - c.start <= 250 and not c.parts for c in chunks[:3])


def test_plan_chunks_on_page_boundaries():
    tensors, chunks = plan([90] * 8 + [0], 250, 64)
    assert all(c.end % 64 == 0 for c in chunks[:-1])
    assert all(c.end - c.start <= 250 for c in chunks)
    seen = set()
    for c in chunks:
        pages = set(range(c.start // 64, (c.end - 1) // 64 + 1)) if c.end > c.start else set()
        assert not pages & seen
        seen |= pages
    # Each tensor is whole in one chunk, or covered exactly once by its parts.
    covered = {t.name: [] for t in tensors}
    for c in chunks:
        for t in c.tensors:
            assert c.start <= t.start and t.end <= c.end
            covered[t.name].append((0, t.end - t.start))
        for t, off, n in c.parts:
            assert c.start <= t.start + off and t.start + off + n <= c.end
            covered[t.name].append((off, off + n))
    for t in tensors:
        spans = sorted(covered[t.name])
        assert spans[0][0] == 0 and spans[-1][1] == t.end - t.start
        assert all(a[1] == b[0] for a, b in zip(spans, spans[1:]))
    assert any(c.parts for c in chunks)


def test_plan_chunks_splits_across_several_chunks():
    _, chunks = plan([100, 127], 127, 64)
    parts = [(off, n) for c in chunks for t, off, n in c.parts if t.name == "t1"]
    assert len(parts) >= 3 and parts[0][0] == 0
    assert all(a[0] + a[1] == b[0] for a, b in zip(parts, parts[1:]))
    assert sum(n for _, n in parts) == 127
    assert all(c.end - c.start <= 127 for c in chunks)


def test_plan_chunks_ignores_pages_larger_than_staging():
    _, paged = plan([100] * 5, 250, 1024)
    _, plain = plan([100] * 5, 250)
    assert paged == plain


# The weight pipeline, with a fake torch


@pytest.mark.parametrize("depth", [1, 2, 3, 4])
def test_iter_weights(server, uri, torch, depth):
    setup_model(server, uri)
    c = client(server)
    got = {n: t for n, t in weights(c, uri, depth)}
    assert {n: t.bytes() for n, t in got.items()} == ALL
    assert got["w4"].dtype == "float32" and got["w4"].shape == (2,)
    # w3 is larger than staging, so it was read on its own.
    assert 900 in [last + 1 - first for _, first, last in range_reads(server)]
    # Every recorded event was waited on before its buffer was reused or dropped.
    assert torch.events and all(e.synced for e in torch.events)


def test_iter_weights_reads_each_page_once(server, uri, torch, monkeypatch):
    monkeypatch.setattr(racer, "PAGE_SIZE", 64)
    setup_model(server, uri)
    c = client(server)
    assert {n: t.bytes() for n, t in weights(c, uri)} == ALL
    pages = {}
    reads = 0
    for s, first, last in range_reads(server):
        if first in (0, 8) or last + 1 - first == 900:
            continue  # header reads and the dedicated w3 read
        reads += 1
        mine = set(range(first // 64, last // 64 + 1))
        assert not mine & pages.setdefault(s.target, set())
        pages[s.target] |= mine
    assert reads >= 2


def test_iter_weights_clone_off_gives_staging_views(server, uri, torch):
    setup_model(server, uri)
    c = client(server)
    got = dict(rv.iter_weights(c, uri, chunks_for(c, uri), 400, 1, clone=False, use_vmm=False))
    # w1 and w2 share one staging buffer; with clone=True they would not.
    assert got["w1"].buf is got["w2"].buf


def test_iter_weights_falls_back_from_vmm(server, uri, torch, monkeypatch, caplog):
    def no_vmm(size, device):
        raise racer.RacerError("no VMM")

    monkeypatch.setattr("racer.cuda.GpuAllocation", no_vmm)
    setup_model(server, uri)
    c = client(server)
    with caplog.at_level(logging.INFO, logger="vllm.racer_vllm.loader"):
        got = {n: t.bytes() for n, t in rv.iter_weights(c, uri, chunks_for(c, uri), 400, 2)}
    assert got == ALL
    assert [r for r in caplog.records if "exportable GPU memory unavailable" in r.getMessage()]


def test_iter_weights_counts_reads(server, uri, torch):
    setup_model(server, uri)
    stats = {"reads": dict.fromkeys((racer.GPU, racer.HOST, racer.STREAM), 0)}
    stats["bytes"] = dict(stats["reads"])
    assert {n: t.bytes() for n, t in weights(client(server), uri, stats=stats)} == ALL
    assert sum(stats["bytes"].values()) == sum(len(v) for v in ALL.values())
    # Fake torch memory cannot be exported, so host-read is used.
    assert stats["reads"][racer.HOST] >= 2 and stats["reads"][racer.GPU] == 0


@pytest.mark.parametrize("stop", ["w1", "w3", None])
def test_iter_weights_fences_on_early_close_and_error(server, uri, torch, stop):
    setup_model(server, uri)
    gen = weights(client(server), uri)
    if stop is None:
        next(gen)
        with pytest.raises(RuntimeError):
            gen.throw(RuntimeError("consumer failed"))
    else:
        for name, _ in gen:
            if name == stop:
                break
        gen.close()
    assert torch.events and all(e.synced for e in torch.events)


def test_iter_weights_rejects_zero_concurrency(server, uri, torch):
    setup_model(server, uri)
    with pytest.raises(ValueError):
        list(weights(client(server), uri, depth=0))


@pytest.mark.parametrize("stop", ["w1", "w3"])
def test_model_changes_mid_load(server, uri, torch, stop):
    setup_model(server, uri)
    gen = weights(client(server), uri, depth=1)
    for name, _ in gen:
        if name == stop:
            break
    replace(server, uri, "b.safetensors")
    with pytest.raises(rv.ModelChangedError, match="b.safetensors"):
        for _ in gen:
            pass


def test_summary():
    stats = {"files": 2, "reads": {racer.GPU: 1, racer.HOST: 0, racer.STREAM: 1}}
    stats["bytes"] = {racer.GPU: 1 << 30, racer.HOST: 0, racer.STREAM: 1 << 29}
    text = rv.summary(stats, 2.0, "racer://c/b/p")
    assert "loaded 2 files, 1.50 GiB in 2.00s (0.75 GiB/s) from racer://c/b/p" in text
    assert "gpu=1 (1.00 GiB) host=0 (0.00 GiB) stream=1 (0.50 GiB)" in text
    assert "(0.00 GiB/s)" in rv.summary(stats, 0.0, "x")


# sharded_state


def test_render_pattern():
    assert rv.render_pattern(None, 0) == "model-rank-0-part-0.safetensors"
    assert rv.render_pattern(None, 3, tp_rank=1, tp_size=2) == "model-rank-1-part-3.safetensors"
    assert rv.render_pattern(None, 0, 1, 2, 1, 2) == "model-pp-1-tp-1-part-0.safetensors"
    assert rv.render_pattern("w/{tp_rank}-{part}.st", 0, 1, 2) == "w/1-0.st"


@pytest.mark.parametrize(
    "pattern,ranks,match",
    [
        ("m-{x}-{part}", {}, "unsupported"),
        ("m-{rank}", {}, "part"),
        ("m-{rank}-{part}", {"pp_size": 2}, "pipeline parallel rank"),
        ("m-{pp_rank}-{rank}-{part}", {}, "pipeline parallel size is 1"),
        ("m-{part}", {"tp_size": 2}, "tensor parallel rank"),
        ("../m-{rank}-{part}", {}, "unsafe"),
        ("/m-{rank}-{part}", {}, "unsafe"),
    ],
)
def test_render_pattern_rejects(pattern, ranks, match):
    with pytest.raises(ValueError, match=match):
        rv.render_pattern(pattern, 0, **ranks)


def test_rank_files(server, uri):
    for name in (
        "model-rank-0-part-0.safetensors",
        "model-rank-0-part-1.safetensors",
        "model-rank-1-part-0.safetensors",
    ):
        put(server, uri, name, b"x")
    c = client(server)
    assert rv.rank_files(c, uri, None, {"tp_rank": 0, "tp_size": 2}) == [
        "model-rank-0-part-0.safetensors",
        "model-rank-0-part-1.safetensors",
    ]
    assert rv.rank_files(c, uri, None, {"tp_rank": 1, "tp_size": 2}) == ["model-rank-1-part-0.safetensors"]
    with pytest.raises(ValueError, match="model-rank-2-part-0"):
        rv.rank_files(c, uri, None, {"tp_rank": 2, "tp_size": 4})


def ft(data, dtype="uint8"):
    return FakeTensor(bytearray(data), dtype=dtype)


def test_load_state_copies_and_narrows(caplog):
    params = {"a": ft(b"\0" * 3), "b": ft(b"\0" * 4)}
    with caplog.at_level(logging.WARNING):
        rv.load_state(params, [("a", ft(b"abc")), ("b", ft(b"xyz"))])
    assert params["a"].bytes() == b"abc"
    # b is padded (as with LoRA): only the first 3 bytes are written.
    assert params["b"].bytes() == b"xyz\0"
    assert any("shape" in r.getMessage() for r in caplog.records)


def test_load_state_errors(caplog):
    with pytest.raises(ValueError, match="missing keys"):
        rv.load_state({"a": ft(b"1"), "b": ft(b"1")}, [("a", ft(b"1"))])
    with pytest.raises(ValueError, match="unexpected"):
        rv.load_state({"a": ft(b"1")}, [("z", ft(b"1"))])
    with pytest.raises(ValueError, match="twice"):
        rv.load_state({"a": ft(b"1")}, [("a", ft(b"1")), ("a", ft(b"1"))])
    with pytest.raises(ValueError, match="shape"):
        rv.load_state({"a": ft(b"1")}, [("a", ft(b"1").view((1, 1)))])
    with caplog.at_level(logging.WARNING):
        rv.load_state({"a": ft(b"1", "bfloat16")}, [("a", ft(b"2", "float32"))])
    assert any("dtype" in r.getMessage() for r in caplog.records)


# The vLLM loader


def install_fake_vllm(monkeypatch, tp_rank=0, tp_size=1):
    registry = {}
    mods = {
        n: types.ModuleType(n)
        for n in (
            "vllm",
            "vllm.distributed",
            "vllm.model_executor",
            "vllm.model_executor.model_loader",
            "vllm.model_executor.model_loader.base_loader",
            "vllm.model_executor.model_loader.sharded_state_loader",
        )
    }

    class BaseModelLoader:
        def __init__(self, load_config):
            self.load_config = load_config

    def register_model_loader(name):
        def wrap(cls):
            assert issubclass(cls, BaseModelLoader)
            registry.setdefault("calls", []).append(name)
            registry[name] = cls
            return cls

        return wrap

    mods["vllm.model_executor.model_loader"].register_model_loader = register_model_loader
    mods["vllm.model_executor.model_loader.base_loader"].BaseModelLoader = BaseModelLoader
    mods["vllm.model_executor.model_loader.sharded_state_loader"].ShardedStateLoader = types.SimpleNamespace(
        _filter_subtensors=lambda sd: sd
    )
    dist = mods["vllm.distributed"]
    dist.get_tensor_model_parallel_rank = lambda: tp_rank
    dist.get_pipeline_model_parallel_rank = lambda: 0
    dist.get_tp_group = lambda: types.SimpleNamespace(world_size=tp_size)
    dist.get_pp_group = lambda: types.SimpleNamespace(world_size=1)
    for name, mod in mods.items():
        monkeypatch.setitem(sys.modules, name, mod)
    monkeypatch.setattr(rv, "_registered", False)
    return registry


class Model:
    def __init__(self, names=tuple(ALL)):
        self.names = names
        self.got = {}

    def load_weights(self, weights):
        for name, t in weights:
            self.got[name] = t.bytes()
        return set(self.got)

    def named_parameters(self):
        return [(n, None) for n in self.names]


def make_loader(monkeypatch, server, **extra):
    registry = install_fake_vllm(monkeypatch)
    rv.register()
    rv.register()
    assert registry["calls"] == ["racer"]
    monkeypatch.setenv("RACER_SOCKET", server.path)
    extra = {"staging_bytes": 400, "concurrency": 2, "use_vmm": False, **extra}
    return registry["racer"](types.SimpleNamespace(model_loader_extra_config=extra))


def test_loader_loads_logs_and_uses_pins(server, uri, torch, monkeypatch, tmp_path, caplog):
    setup_model(server, uri)
    rv.fetch(uri, str(tmp_path), client=client(server))
    inst = make_loader(monkeypatch, server)
    mc = types.SimpleNamespace(model=str(tmp_path), quantization=None)
    inst.download_model(mc)
    m = Model()
    with caplog.at_level(logging.INFO, logger="vllm.racer_vllm.loader"):
        inst.load_weights(m, mc)
    assert m.got == ALL
    msgs = [r.getMessage() for r in caplog.records if r.name == "vllm.racer_vllm.loader"]
    assert len(msgs) == 1 and "loaded 2 files" in msgs[0] and "host=" in msgs[0] and str(uri) in msgs[0]

    with pytest.raises(ValueError, match="w5"):
        inst.load_weights(Model(tuple(ALL) + ("w5",)), mc)
    # Quantized models skip the check.
    inst.load_weights(Model(tuple(ALL) + ("w5",)), types.SimpleNamespace(model=str(tmp_path), quantization="fp8"))

    # The model was replaced after racer-vllm-fetch: the load fails before reading data.
    replace(server, uri, "a.safetensors")
    before = len(range_reads(server))
    with pytest.raises(rv.ModelChangedError, match="racer-vllm-fetch"):
        inst.load_weights(Model(), mc)
    assert len(range_reads(server)) == before


def test_loader_stats_every_file_before_data_reads(server, uri, torch, monkeypatch):
    setup_model(server, uri)
    inst = make_loader(monkeypatch, server, uri=str(uri))
    inst.load_weights(Model(), types.SimpleNamespace(model="/unused", quantization=None))
    a, b = uri.request("a.safetensors").hex, uri.request("b.safetensors").hex
    heads = [i for i, s in enumerate(server.seen) if s.method == "HEAD" and s.target.endswith((a, b))]
    posts = [i for i, s in enumerate(server.seen) if s.method == "POST" and (a in s.target or b in s.target)]
    assert len(heads) == 2 and max(heads) < min(posts)
    assert all(server.seen[i].headers.get("if-match") == '"v1"' for i in posts)


@pytest.mark.parametrize("fail", [True, False])
def test_loader_closes_weights_before_client(server, uri, torch, monkeypatch, fail):
    setup_model(server, uri)
    inst = make_loader(monkeypatch, server, uri=str(uri))
    events = []
    real_iter = rv.iter_weights

    def tracking_iter(*a, **kw):
        try:
            yield from real_iter(*a, **kw)
        finally:
            events.append("weights closed")

    real_close = racer.Client.close
    monkeypatch.setattr(rv, "iter_weights", tracking_iter)
    monkeypatch.setattr(racer.Client, "close", lambda self: (events.append("client closed"), real_close(self)))

    class Partial:
        def load_weights(self, weights):
            next(weights)
            if fail:
                raise RuntimeError("boom")

    mc = types.SimpleNamespace(model="/unused", quantization=None)
    if fail:
        with pytest.raises(RuntimeError, match="boom"):
            inst.load_weights(Partial(), mc)
    else:
        inst.load_weights(Partial(), mc)
    assert events[:2] == ["weights closed", "client closed"]


def test_loader_without_uri_fails(server, monkeypatch, tmp_path):
    inst = make_loader(monkeypatch, server)
    with pytest.raises(ValueError, match="no model URI"):
        inst.download_model(types.SimpleNamespace(model=str(tmp_path)))


@pytest.mark.parametrize("rank", [0, 1])
def test_loader_sharded_state_reads_only_own_rank(server, uri, torch, monkeypatch, rank):
    for r in (0, 1):
        put(server, uri, f"model-rank-{r}-part-0.safetensors", make_safetensors([("w", "U8", (4,), bytes([r]) * 4)]))
        put(
            server,
            uri,
            f"model-rank-{r}-part-1.safetensors",
            make_safetensors([("v", "U8", (2,), bytes([r + 10]) * 2)]),
        )
    inst = make_loader(monkeypatch, server, uri=str(uri), sharded_state=True)
    install_fake_vllm(monkeypatch, tp_rank=rank, tp_size=2)
    params = {"w": ft(b"\0" * 4), "v": ft(b"\0" * 2)}

    class Sharded:
        def state_dict(self):
            return params

    mc = types.SimpleNamespace(model="/unused", quantization=None)
    inst.download_model(mc)
    inst.load_weights(Sharded(), mc)
    assert params["w"].bytes() == bytes([rank]) * 4
    assert params["v"].bytes() == bytes([rank + 10]) * 2
    other = {uri.request(f"model-rank-{1 - rank}-part-{p}.safetensors").hex for p in (0, 1)}
    assert not [s for s in server.seen if s.method == "POST" and s.target.split("/")[-1] in other]


def test_settings():
    s = rv.settings({})
    assert (s["staging_bytes"], s["concurrency"], s["clone"], s["use_vmm"], s["sharded_state"]) == (
        256 << 20,
        4,
        True,
        True,
        False,
    )
    assert s["mode_retry_seconds"] == racer.DEFAULT_MODE_RETRY_SECONDS
    assert rv.settings({"mode_retry_seconds": None})["mode_retry_seconds"] is None
    assert rv.settings({"mode_retry_seconds": "30"})["mode_retry_seconds"] == 30.0


# Packaging


def test_pyproject():
    tomllib = pytest.importorskip("tomllib")
    with open(os.path.join(HERE, "..", "pyproject.toml"), "rb") as f:
        proj = tomllib.load(f)
    with open(os.path.join(HERE, "..", "..", "racer", "pyproject.toml"), "rb") as f:
        client_proj = tomllib.load(f)["project"]
    vllm_proj = proj["project"]
    assert vllm_proj["entry-points"]["vllm.general_plugins"] == {"racer": "racer_vllm:register"}
    assert vllm_proj["scripts"] == {"racer-vllm-fetch": "racer_vllm:main"}
    assert proj["tool"]["setuptools"]["py-modules"] == ["racer_vllm"]
    # racer-vllm needs the racer-client release built from the same tree.
    assert [d for d in vllm_proj["dependencies"] if d.startswith("racer-client")] == [
        f"racer-client>={client_proj['version']},<0.3"
    ]
    for p in (vllm_proj, client_proj):
        assert p["license"] == "Apache-2.0" and p["license-files"] == ["LICENSE"]
        assert p["urls"]["Source"].startswith("https://github.com/Azure/unbounded")
    for d in ("racer", "racer-vllm"):
        assert os.path.exists(os.path.join(HERE, "..", "..", d, "LICENSE"))


def test_loader_rejects_index_added_after_single_file_fetch(server, uri, torch, monkeypatch, tmp_path):
    setup_model(server, uri, index=False)
    rv.fetch(uri, str(tmp_path), client=client(server))
    setup_model(server, uri)  # an index and shards are uploaded later
    inst = make_loader(monkeypatch, server)
    mc = types.SimpleNamespace(model=str(tmp_path), quantization=None)
    with pytest.raises(rv.ModelChangedError, match=f"{rv.INDEX}.*not pinned.*racer-vllm-fetch"):
        inst.download_model(mc)
    assert not range_reads(server)


def test_loader_rejects_unpinned_weight_file(server, uri, torch, monkeypatch, tmp_path):
    # Fetch found no weights, so the marker pins none; a single file appears later.
    put(server, uri, "config.json", b"{}")
    rv.fetch(uri, str(tmp_path), client=client(server))
    setup_model(server, uri, index=False)
    inst = make_loader(monkeypatch, server)
    with pytest.raises(rv.ModelChangedError, match=f"{rv.SINGLE}.*not pinned"):
        inst.download_model(types.SimpleNamespace(model=str(tmp_path), quantization=None))


def test_loader_without_marker_is_unpinned(server, uri, torch, monkeypatch, tmp_path):
    setup_model(server, uri, index=False)
    inst = make_loader(monkeypatch, server, uri=str(uri))
    mc = types.SimpleNamespace(model=str(tmp_path), quantization=None)
    setup_model(server, uri)  # layout changes, but nothing was pinned
    inst.load_weights(Model(), mc)
    # A marker for another URI is ignored too.
    (tmp_path / rv.MARKER).write_text(json.dumps({"uri": "racer://cache1/models/other", "etags": {}}))
    inst.load_weights(Model(), mc)


def put_sharded(server, uri, ranks=2, parts=2, etag='"v1"'):
    for r in range(ranks):
        for p in range(parts):
            data = make_safetensors([(f"w{p}", "U8", (2,), bytes([r * 10 + p]) * 2)])
            put(server, uri, f"model-rank-{r}-part-{p}.safetensors", data, etag)


def test_fetch_pins_sharded_state_files(server, uri, tmp_path):
    put(server, uri, "config.json", b"{}")
    put_sharded(server, uri)
    rv.fetch(uri, str(tmp_path), client=client(server))
    etags = json.loads((tmp_path / rv.MARKER).read_text())["etags"]
    shards = [f"model-rank-{r}-part-{p}.safetensors" for r in (0, 1) for p in (0, 1)]
    assert sorted(etags) == sorted(["config.json", *shards])

    pp = tmp_path / "pp"
    put(server, uri, "model-pp-0-tp-0-part-0.safetensors", b"x")
    put(server, uri, "model-pp-1-tp-0-part-0.safetensors", b"x")
    pattern = "model-pp-{pp_rank}-tp-{tp_rank}-part-{part}.safetensors"
    rv.fetch(uri, str(pp), client=client(server), pattern=pattern)
    etags = json.loads((pp / rv.MARKER).read_text())["etags"]
    assert sorted(etags) == ["config.json", "model-pp-0-tp-0-part-0.safetensors", "model-pp-1-tp-0-part-0.safetensors"]


@pytest.mark.parametrize("change", ["none", "new part", "deleted part", "replaced"])
def test_loader_sharded_state_uses_pins(server, uri, torch, monkeypatch, tmp_path, change):
    put(server, uri, "config.json", b"{}")
    put_sharded(server, uri)
    rv.fetch(uri, str(tmp_path), client=client(server))
    if change == "new part":
        put(server, uri, "model-rank-1-part-2.safetensors", make_safetensors([("w2", "U8", (2,), b"zz")]))
    elif change == "deleted part":
        del server.objects[uri.request("model-rank-1-part-1.safetensors").hex]
    elif change == "replaced":
        replace(server, uri, "model-rank-1-part-0.safetensors")
    inst = make_loader(monkeypatch, server, sharded_state=True)
    install_fake_vllm(monkeypatch, tp_rank=1, tp_size=2)
    params = {"w0": ft(b"\0" * 2), "w1": ft(b"\0" * 2)}

    class Sharded:
        def state_dict(self):
            return params

    mc = types.SimpleNamespace(model=str(tmp_path), quantization=None)
    if change == "none":
        inst.load_weights(Sharded(), mc)
        assert params["w0"].bytes() == b"\x0a\x0a" and params["w1"].bytes() == b"\x0b\x0b"
        return
    match = {"new part": "part-2.*not pinned", "deleted part": "part-1", "replaced": "part-0"}[change]
    with pytest.raises(rv.ModelChangedError, match=match):
        inst.load_weights(Sharded(), mc)
