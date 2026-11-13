# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0
"""Staging buffers, zero-copy views and delivery selection."""

import gc
import types

import pytest
from conftest import _fixtures
from modelfix import client, publish

import racer
from racer_vllm import manifest as mf
from racer_vllm import reader
from racer_vllm.planner import PlannedTensor, Read, plan


class Alloc:
    """Host memory from bytearrays; counts allocations and frees."""

    kind = reader.HOST

    def __init__(self):
        self.allocated = 0
        self.freed = 0

    def allocate(self, size):
        self.allocated += 1
        return bytearray(size)

    def owner(self, raw, length):
        return reader.HostOwner(memoryview(raw), length)

    def as_tensor(self, owner):
        import torch

        return torch.frombuffer(owner.anchor)

    def free(self, raw):
        self.freed += 1


def read_of(*sizes, dtype="U8"):
    r = Read("f", 0, sum(sizes), False)
    pos = 0
    for i, n in enumerate(sizes):
        r.tensors.append(PlannedTensor(f"t{i}", dtype, (n,), pos, n))
        pos += n
    return r


def pool(alloc, staging=16, cap=32, clone="auto", events=None):
    return reader.StagingPool(staging, cap, 2, alloc, clone, events)


def test_buffer_is_reused_once_views_are_gone(torch):
    a = Alloc()
    p = pool(a)
    b = p.acquire(8)
    b.raw[:8] = b"abcdefgh"
    views = list(b.tensor_views(read_of(4, 4), clone=False))
    assert [t.bytes() for _, t in views] == [b"abcd", b"efgh"]
    p.release(b)
    assert not b.ready()  # the views still share the buffer
    b2 = p.acquire(8)
    assert b2 is not b and a.allocated == 2
    p.release(b2)
    del views
    gc.collect()
    assert b.ready()
    assert p.acquire(8) in (b, b2) and a.allocated == 2


def test_views_share_staging_memory(torch):
    a = Alloc()
    p = pool(a)
    b = p.acquire(4)
    b.raw[:4] = b"wxyz"
    (_, t), = list(b.tensor_views(read_of(4), clone=False))
    b.raw[0:1] = b"W"
    assert t.bytes() == b"Wxyz"  # zero copy
    (_, c), = list(b.tensor_views(read_of(4), clone=True))
    b.raw[0:1] = b"Q"
    assert c.bytes() == b"Wxyz"  # the clone kept its own copy


def test_cap_with_live_views_switches_to_clone(torch, caplog):
    a = Alloc()
    p = pool(a, staging=16, cap=32)
    held = []
    for _ in range(2):
        b = p.acquire(16)
        held += list(b.tensor_views(read_of(16), clone=False))
        p.release(b)
    assert not p.cloning
    b = p.acquire(16)  # both buffers are held by views: past the cap
    assert p.cloning and a.allocated == 3
    assert any("cloning remaining tensors" in r.getMessage() for r in caplog.records)
    p.release(b)


def test_cap_with_clone_never_fails(torch):
    p = pool(Alloc(), staging=16, cap=16, clone="never")
    b = p.acquire(16)
    held = list(b.tensor_views(read_of(16), clone=False))
    p.release(b)
    with pytest.raises(MemoryError, match="max_staging_bytes"):
        p.acquire(16)
    assert held


def test_dedicated_buffers_are_retired_and_freed(torch):
    a = Alloc()
    p = pool(a, staging=16, cap=1000)
    b = p.acquire(100, owned=True)
    assert b.size == 100 and b.owned
    p.release(b)
    p.acquire(8)  # sweeps the retired buffer
    assert a.freed == 1


def test_release_records_an_event_and_close_fences(torch):
    a = Alloc()
    p = reader.StagingPool(16, 64, 2, a, "auto", torch.cuda.Event)
    b = p.acquire(8)
    p.release(b)
    assert torch.events[-1].recorded and not torch.events[-1].synced
    p.close()
    assert torch.events[-1].synced and a.freed == 1


def test_misaligned_views_are_copied(torch):
    a = Alloc()
    p = pool(a)
    b = p.acquire(6)
    r = Read("f", 0, 6, False, [PlannedTensor("x", "F32", (1,), 1, 4)])
    (_, t), = list(b.tensor_views(r, clone=False))
    b.raw[1:2] = b"Z"
    assert t.bytes() == b"\0\0\0\0"  # cloned before the write
    assert t.dtype == "float32"


# Delivery selection


class Caps:
    def __init__(self, caps):
        self._caps = caps

    def read(self):
        pass

    def host_read(self):
        pass

    def gpu_read(self):
        pass

    def capabilities(self):
        if isinstance(self._caps, Exception):
            raise self._caps
        return self._caps


def test_candidates_follow_capabilities():
    assert reader.candidates(Caps({"delivery": ["host", "stream"]})) == ["host", "stream"]
    assert reader.candidates(Caps({"delivery": ["gpu", "host", "stream"]}), "host") == ["host", "stream"]
    assert reader.candidates(Caps(racer.NotFound(404))) == ["gpu", "host", "stream"]
    assert reader.candidates(Caps({"schema": 1})) == ["gpu", "host", "stream"]
    with pytest.raises(RuntimeError, match="no usable delivery"):
        reader.candidates(Caps({"delivery": ["stream"]}), "gpu")


def test_probe_drops_modes_whose_allocator_fails():
    reader.forget()

    def make(mode):
        if mode == reader.GPU:
            raise RuntimeError("no CUDA")
        return object()

    assert reader.probe(Caps({"delivery": ["gpu", "host", "stream"]}), "auto", make) == ["host", "stream"]


# Reads through the fake dataplane


def jobs_for(c, u, m, staging):
    out = []
    for sf in mf.safetensors_files(m):
        req = u.request(sf.name)
        out += [(req, r, None) for r in plan(sf.name, sf.tensors, None, staging, file_size=sf.size)]
    return out


@pytest.mark.parametrize("gpu_status", [501, 404])
def test_reader_falls_back_from_gpu_to_host(server, torch, gpu_status):
    reader.forget()
    server.gpu_status = gpu_status
    u, files, m = publish(server)
    c = client(server)
    allocs = {reader.GPU: reader.HostAllocator(pin=False), reader.HOST: reader.HostAllocator(pin=False)}
    # A GPU "allocation" that is really a memfd (the racer tests' FakeGpu).
    gpu = types.SimpleNamespace(kind=reader.GPU, allocate=_fixtures.FakeGpu, free=lambda raw: raw.close())
    allocs[reader.GPU] = gpu
    p = reader.StagingPool(4096, 1 << 20, 2, gpu, "always", reader.DoneEvent)
    r = reader.WeightReader(c, p, ["gpu", "host"], allocs)
    got = {n: t.bytes() for n, t in r.iter(jobs_for(c, u, m, 4096))}
    assert got == {"w1": b"\1" * 300, "w2": b"\2" * 50, "w3": b"\3" * 900, "w4": b"\4" * 8}
    assert r.mode == "host" and reader.cached_mode(c) == "host"
    assert r.stats["reads"]["host"] == 2 and r.stats["reads"]["gpu"] == 0
    p.close()


def test_reader_stream_mode(server, torch):
    reader.forget()
    u, files, m = publish(server)
    c = client(server)
    alloc = reader.HostAllocator(pin=False)
    p = reader.StagingPool(4096, 1 << 20, 2, alloc, "auto", reader.DoneEvent)
    r = reader.WeightReader(c, p, ["stream"], {"stream": alloc})
    got = dict((n, t.bytes()) for n, t in r.iter(jobs_for(c, u, m, 4096)))
    assert got["w3"] == b"\3" * 900 and r.stats["bytes"]["stream"] > 0


@pytest.mark.parametrize("stop", ["close", "error"])
def test_reader_returns_buffers_on_early_stop(server, torch, stop):
    reader.forget()
    u, files, m = publish(server)
    c = client(server)
    alloc = reader.HostAllocator(pin=False)
    p = reader.StagingPool(512, 1 << 20, 2, alloc, "always", reader.DoneEvent)
    r = reader.WeightReader(c, p, ["host"], {"host": alloc})
    it = r.iter(jobs_for(c, u, m, 512))
    next(it)
    if stop == "close":
        it.close()
    else:
        with pytest.raises(ValueError):
            it.throw(ValueError("consumer failed"))
    assert len(p.idle) + sum(1 for b in p.buffers if b.retired) == len(p.buffers)
    p.close()
    assert p.buffers == []


def test_reader_fails_when_the_object_changes(server, torch):
    reader.forget()
    u, files, m = publish(server)
    c = client(server)
    alloc = reader.HostAllocator(pin=False)
    p = reader.StagingPool(4096, 1 << 20, 2, alloc, "auto", reader.DoneEvent)
    r = reader.WeightReader(c, p, ["host"], {"host": alloc})
    jobs = [(req, rd, '"old"') for req, rd, _ in jobs_for(c, u, m, 4096)]
    with pytest.raises(racer.VersionUnavailable):
        list(r.iter(jobs))
