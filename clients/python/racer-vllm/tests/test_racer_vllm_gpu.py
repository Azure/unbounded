# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0
"""The weight pipeline and loader with real torch, against the fake server.

tensor_device "cpu" needs only torch; "cuda" needs torch with CUDA and a GPU.
See the gpu fixtures in ../../racer/tests/conftest.py for when these skip or
fail. The fake server emulates gpu-read by importing the exported staging
allocation, as the dataplane does.
"""

import copy
import ctypes
import gc
import json
import types
import weakref

import pytest
import realcuda
from conftest import make_safetensors
from test_racer_vllm import Model, make_loader, model_config, put, spec_config

import racer
import racer.cuda as rcuda
import racer_vllm as rv

pytestmark = pytest.mark.gpu

STAGING = 4096  # loader staging; smaller than a.big
CLIENT_STAGING = 8192  # host-read staging; also smaller than a.big
PLAN_PAGE = 1536  # chunk ends fall on these, so tensors split across chunks

# (name, safetensors dtype, shape), stored in this order. The odd sizes leave
# later tensors at offsets that are not a multiple of their element size.
FILES = {
    "a.safetensors": [
        ("embed.i8", "I8", (37,)),
        ("layers.0.bf16", "BF16", (3, 5)),
        ("layers.0.f32", "F32", (7,)),
        ("layers.0.scalar", "F32", ()),
        ("layers.0.empty", "F16", (0, 4)),
        ("layers.0.f16", "F16", (301,)),
        ("layers.0.u8", "U8", (999,)),
        ("layers.0.i64", "I64", (9,)),
        ("layers.0.big", "F32", (3001,)),
        ("layers.0.wide", "BF16", (1700,)),
        ("layers.0.i32", "I32", (3, 333)),
    ],
    "b.safetensors": [
        ("mtp.0.f16", "F16", (2, 511)),
        ("layers.1.bf16", "BF16", (1999,)),
        ("mtp.0.i8", "I8", (3,)),
        ("mtp.0.big", "BF16", (4097,)),
        ("layers.1.f32", "F32", (5,)),
        ("mtp.0.f32", "F32", (1023,)),
        ("mtp.0.f8", "F8_E4M3", (13,)),
        ("layers.1.bool", "BOOL", (9,)),
        ("layers.1.f64", "F64", (5,)),
    ],
}
SHARDS = {"a.safetensors": "model-rank-0-part-0.safetensors", "b.safetensors": "model-rank-0-part-1.safetensors"}
MODES = {racer.GPU: 0, racer.HOST: 0, racer.STREAM: 0}

# (tensor_device, use_vmm, delivery). gpu-read needs exportable (VMM) memory.
PATHS = [
    ("cuda", True, racer.GPU),
    ("cuda", True, racer.HOST),
    ("cuda", True, racer.STREAM),
    ("cuda", False, racer.HOST),
    ("cuda", False, racer.STREAM),
    ("cpu", False, racer.HOST),
    ("cpu", False, racer.STREAM),
]


@pytest.fixture
def uri():
    return rv.parse_uri("racer://cache1/models/org/gpu")


@pytest.fixture
def plan_pages(monkeypatch):
    monkeypatch.setattr(racer, "PAGE_SIZE", PLAN_PAGE)


@pytest.fixture
def vmm(monkeypatch):
    """Weak references to every GpuAllocation made during the test."""
    made = []

    class Tracked(rcuda.GpuAllocation):
        def __init__(self, *args, **kwargs):
            super().__init__(*args, **kwargs)
            made.append(weakref.ref(self))

    monkeypatch.setattr(rcuda, "GpuAllocation", Tracked)
    return made


def torch_for(request, tensor_device):
    return request.getfixturevalue("cuda_torch" if tensor_device == "cuda" else "real_torch")


def raw(t):
    """The bytes of a tensor, as safetensors stores them."""
    t = t.detach().to("cpu").contiguous()
    n = t.numel() * t.element_size()
    return ctypes.string_at(t.data_ptr(), n) if n else b""


def block(t):
    """Bytes the CUDA caching allocator counts for a tensor of its own (512-byte blocks)."""
    n = t.numel() * t.element_size()
    return -(-n // 512) * 512


def put_checkpoint(torch, server, uri, sharded=False):
    """Store FILES (or the same tensors as a sharded_state rank) with an index. Returns {name: CPU tensor}."""
    gen = torch.Generator().manual_seed(1234)
    want = {}
    weight_map = {}
    for f, specs in FILES.items():
        name = SHARDS[f] if sharded else f
        items = []
        for tensor, dtype, shape in specs:
            dt = getattr(torch, rv.DTYPES[dtype][1])
            if dt.is_floating_point:
                t = torch.randn(shape, generator=gen).to(dt)
            elif dt == torch.bool:
                t = torch.randint(0, 2, shape, generator=gen) == 1
            elif dt == torch.uint8:
                t = torch.randint(0, 256, shape, generator=gen, dtype=dt)
            else:
                bound = min(torch.iinfo(dt).max, 1 << 40)
                t = torch.randint(-bound, bound, shape, generator=gen, dtype=dt)
            want[tensor] = t
            weight_map[tensor] = name
            items.append((tensor, dtype, shape, raw(t)))
        put(server, uri, name, make_safetensors(items))
    put(server, uri, "config.json", b"{}")
    if not sharded:
        put(server, uri, rv.INDEX, json.dumps({"weight_map": weight_map}).encode())
    return want


def deliver(server, mode):
    """Make the fake server's reads end in delivery mode."""
    if mode == racer.GPU:
        server.gpu_status = 200
        server.gpu_write = realcuda.gpu_writer()
    elif mode == racer.STREAM:
        server.host_status = 501


def check(got, want, tensor_device):
    assert sorted(got) == sorted(want)
    for name, t in got.items():
        w = want[name]
        assert (t.dtype, tuple(t.shape), t.device.type) == (w.dtype, tuple(w.shape), tensor_device), name
        assert raw(t) == raw(w), name


def nbytes(tensors):
    return sum(t.numel() * t.element_size() for t in tensors)


def messages(caplog, text):
    return [r.getMessage() for r in caplog.records if text in r.getMessage()]


def plan(c, uri, select=None):
    pinned = [(f, c.stat(uri.request(f))) for f in FILES]
    return rv.read_chunks(c, uri, pinned, STAGING, select)


def test_layout_covers_the_cases(real_torch, server, uri, plan_pages):
    # The other tests rely on FILES producing each of these.
    put_checkpoint(real_torch, server, uri)
    with racer.Client(socket_path=server.path) as c:
        chunks = plan(c, uri)
    size = {t: rv.DTYPES[d][0] for specs in FILES.values() for t, d, _ in specs}
    assert any(c.dedicated and c.end - c.start > CLIENT_STAGING for c in chunks)
    assert any((t.start - c.start) % size[t.name] for c in chunks for t in c.tensors)
    split = [t.name for c in chunks for t, _, _ in c.parts]
    assert split and any(split.count(n) > 2 for n in split)
    assert any(t.end == t.start for c in chunks for t in c.tensors)


@pytest.mark.parametrize("clone", [True, False])
@pytest.mark.parametrize("tensor_device,use_vmm,mode", PATHS)
def test_iter_weights(request, server, uri, plan_pages, vmm, caplog, tensor_device, use_vmm, mode, clone):
    torch = torch_for(request, tensor_device)
    on_gpu = tensor_device == "cuda"
    want = put_checkpoint(torch, server, uri)
    deliver(server, mode)
    stats = {"reads": dict(MODES), "bytes": dict(MODES)}
    if on_gpu:
        torch.cuda.synchronize()
        base = torch.cuda.memory_allocated()
    got = {}
    with racer.Client(socket_path=server.path, staging_bytes=CLIENT_STAGING, pin_staging=on_gpu) as c:
        chunks = plan(c, uri)
        weights = rv.iter_weights(c, uri, chunks, STAGING, 2, clone, use_vmm, stats, tensor_device)
        for name, t in weights:
            # Without clone a tensor may view a staging buffer that a later chunk reuses.
            got[name] = t if clone else t.clone()
        del t
    check(got, want, tensor_device)
    data = [c for c in chunks if c.end > c.start]
    assert stats["reads"] == {**MODES, mode: len(data)}
    assert stats["bytes"] == {**MODES, mode: nbytes(want.values())}
    assert not messages(caplog, "exportable GPU memory unavailable")
    if on_gpu:
        gc.collect()
        torch.cuda.synchronize()
        alive = [a for a in (r() for r in vmm) if a is not None]
        if use_vmm:
            assert vmm
            # Only dedicated buffers stay, while the tensors over them do.
            assert len(alive) == (sum(c.dedicated for c in chunks) if clone else 0)

        def in_vmm(t):
            p = t.untyped_storage().data_ptr()
            return any(a.ptr <= p < a.ptr + a.size for a in alive)

        # Staging buffers are freed; what remains is the loaded weights.
        assert torch.cuda.memory_allocated() - base <= sum(block(t) for t in got.values() if not in_vmm(t))
        del alive, in_vmm
    del got
    gc.collect()
    assert all(r() is None for r in vmm)
    if on_gpu:
        assert torch.cuda.memory_allocated() == base
    assert not messages(caplog, "leaking GPU allocation")


class TorchModel(Model):
    """Copies each weight as it arrives, as vLLM models do."""

    def load_weights(self, weights):
        for name, t in weights:
            self.got[name] = t.clone()
        return set(self.got)


class DSparkDeepseekV4Torch(TorchModel):
    """Named as vLLM's DeepSeek-V4 DSpark drafter, which loads only mtp.* tensors."""


class StateDict:
    def __init__(self, params):
        self.params = params

    def state_dict(self):
        return self.params


@pytest.fixture
def summaries(monkeypatch):
    """The stats of every load summary."""
    out = []
    real = rv.summary

    def summary(stats, seconds, uri):
        out.append(copy.deepcopy(stats))
        return real(stats, seconds, uri)

    monkeypatch.setattr(rv, "summary", summary)
    return out


@pytest.mark.parametrize("sharded,clone", [(False, True), (False, False), (True, True)])
@pytest.mark.parametrize("tensor_device,use_vmm,mode", [p for p in PATHS if p[2] != racer.STREAM])
def test_load_weights(
    request, server, uri, monkeypatch, plan_pages, vmm, summaries, caplog, tensor_device, use_vmm, mode, sharded, clone
):
    torch = torch_for(request, tensor_device)
    on_gpu = tensor_device == "cuda"
    want = put_checkpoint(torch, server, uri, sharded)
    deliver(server, mode)
    inst = make_loader(
        monkeypatch,
        server,
        uri=str(uri),
        staging_bytes=STAGING,
        use_vmm=use_vmm,
        clone=clone,
        sharded_state=sharded,
        tensor_device=tensor_device,
    )
    mc = types.SimpleNamespace(model="/unused", quantization=None)
    if sharded:
        params = {n: torch.zeros_like(t, device=tensor_device) for n, t in want.items()}
    if on_gpu:
        torch.cuda.synchronize()
        base = torch.cuda.memory_allocated()
    if sharded:
        inst.load_weights(StateDict(params), mc)
        got = params
    else:
        model = TorchModel(tuple(want))
        inst.load_weights(model, mc)
        got = model.got
    check(got, want, tensor_device)
    (stats,) = summaries
    assert stats["files"] == 2
    assert stats["bytes"] == {**MODES, mode: nbytes(want.values())}
    assert stats["reads"][mode] > 0 and sum(stats["reads"].values()) == stats["reads"][mode]
    assert not messages(caplog, "exportable GPU memory unavailable")
    if use_vmm and on_gpu:
        assert vmm
    gc.collect()
    assert all(r() is None for r in vmm)
    if on_gpu:
        torch.cuda.synchronize()
        # Staging is freed: only the copies the model kept remain.
        assert torch.cuda.memory_allocated() - base <= (0 if sharded else sum(block(t) for t in got.values()))
        if not sharded:
            del got, model
            gc.collect()
            assert torch.cuda.memory_allocated() == base
    assert not messages(caplog, "leaking GPU allocation")


@pytest.mark.parametrize(
    "extra,select",
    [
        ({"tensor_include": ["^mtp\\."]}, lambda n: n.startswith("mtp.")),
        ({"tensor_exclude": ["bf16$", "^mtp\\.0\\.big$"]}, lambda n: not n.endswith("bf16") and n != "mtp.0.big"),
        (
            {"tensor_include": ["big", "i8"], "tensor_exclude": ["^embed"]},
            lambda n: n.endswith(("big", "i8")) and n != "embed.i8",
        ),
    ],
)
@pytest.mark.parametrize("tensor_device,use_vmm,mode", [PATHS[0], PATHS[3], PATHS[5]])
def test_tensor_selection(
    request, server, uri, monkeypatch, plan_pages, summaries, tensor_device, use_vmm, mode, extra, select
):
    torch = torch_for(request, tensor_device)
    want = put_checkpoint(torch, server, uri)
    deliver(server, mode)
    names = tuple(n for n in want if select(n))
    model = TorchModel(names)
    opts = {"staging_bytes": STAGING, "use_vmm": use_vmm, "tensor_device": tensor_device, **extra}
    make_loader(monkeypatch, server, uri=str(uri), **opts).load_weights(model, model_config("/unused", Model, ()))
    check(model.got, {n: want[n] for n in names}, tensor_device)
    (stats,) = summaries
    skipped = [t for n, t in want.items() if n not in names]
    assert (stats["skipped_tensors"], stats["skipped_bytes"]) == (len(skipped), nbytes(skipped))
    files = {f for f, specs in FILES.items() if any(select(t) for t, _, _ in specs)}
    assert (stats["files"], stats["skipped_files"]) == (len(files), len(FILES) - len(files))
    assert sum(stats["bytes"].values()) == stats["bytes"][mode] == nbytes(want[n] for n in names)


@pytest.mark.parametrize("tensor_device,use_vmm,mode", [PATHS[0], PATHS[3], PATHS[5]])
def test_dspark_draft_reads_only_mtp_tensors(
    request, server, uri, monkeypatch, plan_pages, summaries, tensor_device, use_vmm, mode
):
    torch = torch_for(request, tensor_device)
    want = put_checkpoint(torch, server, uri)
    deliver(server, mode)
    opts = {"staging_bytes": STAGING, "use_vmm": use_vmm, "tensor_device": tensor_device}
    target_loader = make_loader(monkeypatch, server, uri=str(uri), **opts)
    draft_loader = type(target_loader)(target_loader.load_config)
    mtp = tuple(n for n in want if n.startswith("mtp."))
    target = model_config("/models/m", TorchModel, tuple(want))
    draft = model_config("/models/m", DSparkDeepseekV4Torch, mtp)
    vc = spec_config(target, draft, draft_load_config=None)
    check(target_loader.load_model(vllm_config=vc, model_config=target).got, want, tensor_device)
    check(draft_loader.load_model(vllm_config=vc, model_config=draft).got, {n: want[n] for n in mtp}, tensor_device)
    stats = summaries[-1]
    assert stats["files"] == 1 and stats["skipped_files"] == 1
    assert sum(stats["bytes"].values()) == nbytes(want[n] for n in mtp)
