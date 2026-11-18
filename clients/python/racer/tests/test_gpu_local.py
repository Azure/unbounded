# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0
"""racer.cuda, pinned host buffers and GPU reads on a real GPU, with the fake server.

These need libcuda and a GPU, and the tensor tests torch with CUDA; see the
gpu fixtures in conftest.py. The fake server emulates gpu-read by importing
the exported allocation and writing through a second mapping, as the
dataplane does.
"""

import ctypes
import gc
import mmap
import os
import random
import time
import weakref

import pytest
import realcuda
from fakeserver import Obj
from test_host import mapped, memfds

from racer import GPU, HOST, STREAM, Client, HostBuffer, RacerError, Request, cuda, object_request, tensors

pytestmark = pytest.mark.gpu

MiB = 1 << 20
PAGE = 4096  # the server fixture's page size
DATA = random.Random(7).randbytes(5 * PAGE + 321)
FILL = 0xA5


def add(server, data=DATA):
    req = Request(object_request("bucket", "gpu-object").key)
    server.objects[req.hex] = Obj(data)
    return req


def deliver(server, mode, device=0):
    """Make the fake server's reads end in delivery mode."""
    if mode == GPU:
        server.gpu_status = 200
        server.gpu_write = realcuda.gpu_writer(device)
    elif mode == STREAM:
        server.host_status = 501


def client(server):
    # Smaller than DATA, so host-read fallback takes several chunks.
    return Client(socket_path=server.path, staging_bytes=2 * PAGE)


def contents(alloc, offset=0, n=None):
    out = bytearray(alloc.size - offset if n is None else n)
    alloc.copy_to_host(offset, out)
    return bytes(out)


# Device allocations


def test_allocation_size_rounds_up_to_granularity(gpu):
    g = cuda.granularity()
    assert g > 0 and g & (g - 1) == 0
    for size, want in [(1, g), (g - 1, g), (g, g), (g + 1, 2 * g), (3 * g + 7, 4 * g)]:
        with cuda.GpuAllocation(size) as a:
            assert (a.requested, a.size) == (size, want)
            assert a.ptr and a.ptr % g == 0
            assert a.__cuda_array_interface__["shape"] == (want,)
            assert a.__cuda_array_interface__["data"] == (a.ptr, False)
            assert a.gpu_uuid() == cuda.device_uuid(0)
    with pytest.raises(RacerError):
        cuda.GpuAllocation(0)


def test_copies_at_offsets_and_across_granules(gpu):
    g = cuda.granularity()
    data = random.Random(1).randbytes(3 * PAGE + 5)
    with cuda.GpuAllocation(2 * g) as a:
        expect = bytearray([FILL]) * a.size
        a.copy_from_host(0, bytes(expect))
        writes = [
            (0, data),  # bytes are read-only, so copy_from_host copies them first
            (1, bytearray(data[:999])),
            (g - 7, memoryview(bytearray(b"\0" + data))[1:]),  # across the granule boundary
            (a.size - 5, b"tail!"),
            (12345, b""),
        ]
        for offset, src in writes:
            a.copy_from_host(offset, src)
            expect[offset : offset + len(src)] = bytes(src)
        assert contents(a) == expect
        # Read back into a view at an offset of a larger buffer.
        out = bytearray(len(data) + 6)
        a.copy_to_host(g - 7, memoryview(out)[3:-3])
        assert out == b"\0" * 3 + expect[g - 7 : g - 7 + len(data)] + b"\0" * 3
        for offset, n in [(a.size - 1, 2), (-1, 1)]:
            with pytest.raises(RacerError):
                a.copy_from_host(offset, b"x" * n)
            with pytest.raises(RacerError):
                a.copy_to_host(offset, bytearray(n))


def test_export_fd_maps_the_same_memory(gpu):
    with cuda.GpuAllocation(PAGE) as a:
        a.copy_from_host(100, b"from the owner")
        fd = a.export_fd()
        try:
            assert fd >= 0
            assert realcuda.read(fd, a.size, 100, 14) == b"from the owner"
            # What a dataplane writes through the fd lands in this allocation.
            realcuda.write(fd, a.size, a.size - 5, b"tail!")
            assert contents(a, a.size - 5) == b"tail!"
            second = a.export_fd()
            try:
                assert second != fd and realcuda.read(second, a.size, a.size - 5, 5) == b"tail!"
            finally:
                os.close(second)
        finally:
            os.close(fd)


def wait_for_free(baseline, slack, timeout=10.0):
    """Free device memory, once it is back within slack of baseline or the timeout passes."""
    deadline = time.monotonic() + timeout
    while True:
        free, _ = realcuda.mem_get_info()
        if free >= baseline - slack or time.monotonic() > deadline:
            return free
        time.sleep(0.05)


def test_close_and_collection_free_device_memory(gpu, server):
    size = 128 * MiB
    slack = 64 * MiB
    req = add(server)
    deliver(server, GPU)
    cuda.GpuAllocation(size).close()  # the driver's own first-use allocations
    baseline, _ = realcuda.mem_get_info()
    with cuda.GpuAllocation(size):
        # The measure sees these allocations, so a leak would show.
        assert realcuda.mem_get_info()[0] <= baseline - size + slack
    with client(server) as c:
        for _ in range(8):
            a = cuda.GpuAllocation(size)
            a.copy_from_host(size - 3, b"end")
            a.close()
            a.close()
            b = cuda.GpuAllocation(size)
            os.close(b.export_fd())
            del b
            with cuda.GpuAllocation(size) as d:
                # gpu-read sends an exported fd; the client must close its copy.
                assert c.read_into_gpu(req, d, 7).delivery == GPU
    # The fake server may still hold its copy of the last fd for a moment.
    free = wait_for_free(baseline, slack)
    assert free >= baseline - slack, f"{(baseline - free) / MiB:.0f} MiB of device memory not freed"


# Host registration


def test_host_register_pins_and_unpins(gpu):
    with mmap.mmap(-1, MiB) as m:
        addr = ctypes.addressof(ctypes.c_char.from_buffer(m))
        assert not realcuda.registered(addr, MiB)
        reg = cuda.host_register(m)
        assert realcuda.registered(addr, MiB)
        reg.close()
        reg.close()
        assert not realcuda.registered(addr, MiB)
    with pytest.raises(RacerError, match="writable"):
        cuda.host_register(b"read-only")


@pytest.mark.parametrize("release", ["close", "collect", "collect-keep-view"])
def test_pinned_host_buffer_unpins_before_unmapping(gpu, monkeypatch, release):
    name = f"racer-gpu-{release}"
    size = MiB
    seen = []
    real_close = cuda.HostRegistration.close

    def close(reg):
        addr = reg._addr
        if addr:
            seen.append(("before", mapped(name), realcuda.registered(addr, size)))
        real_close(reg)
        if addr:
            seen.append(("after", mapped(name), realcuda.registered(addr, size)))

    monkeypatch.setattr(cuda.HostRegistration, "close", close)
    b = HostBuffer(size, name)
    assert b.pin() and b.pinned and b.pin()
    (reg,) = b._pins
    addr = reg._addr
    del reg
    assert realcuda.registered(addr, size)
    fd = b.fileno()
    b.view[:5] = b"hello"
    view = b.view if release == "collect-keep-view" else None
    if release == "close":
        b.close()
    del b
    gc.collect()
    assert fd not in memfds(name)
    if view is not None:
        # A tensor over the view may still copy from it, so it stays pinned and mapped.
        assert seen == [] and mapped(name) and realcuda.registered(addr, size)
        with cuda.GpuAllocation(size) as a:
            a.copy_from_host(0, view)
            assert contents(a, 0, 5) == b"hello"
        del view
        gc.collect()
    # Unpinned while still mapped, then unmapped.
    assert seen == [("before", True, True), ("after", True, False)]
    assert not mapped(name) and memfds(name) == []


# Reads into GPU memory


@pytest.mark.parametrize("mode", [GPU, HOST, STREAM])
@pytest.mark.parametrize(
    "dest_offset,offset,length",
    [
        (0, 0, None),  # the whole object
        (1, PAGE - 1, PAGE + 2),  # odd offsets, a page edge on each side
        ("granule", 3, 3 * PAGE),  # the destination crosses a granule boundary
        (12345, len(DATA) - 1, 1),  # the last byte
    ],
)
def test_read_into_gpu(gpu, server, mode, dest_offset, offset, length):
    g = cuda.granularity()
    if dest_offset == "granule":
        dest_offset = g - PAGE - 1
    req = add(server)
    deliver(server, mode)
    want = DATA[offset:] if length is None else DATA[offset : offset + length]
    pages = []

    def on_page(o, n):
        pages.append((o, n))

    with cuda.GpuAllocation(2 * g) as a, client(server) as c:
        a.copy_from_host(0, bytes([FILL]) * a.size)
        r = c.read_into_gpu(req, a, dest_offset, offset=offset, length=length, on_page=on_page)
        assert r.delivery == mode and (r.first, r.end) == (offset, offset + len(want))
        lo = max(0, dest_offset - 64)
        got = contents(a, lo, dest_offset - lo + len(want) + 64)
        assert got == bytes([FILL]) * (dest_offset - lo) + want + bytes([FILL]) * 64
        if mode == GPU:
            (rpc,) = [s for s in server.seen if s.target.endswith("/gpu-read")]
            assert rpc.headers["racer-allocation-size"] == str(a.size)
            assert rpc.headers["racer-gpu-uuid"] == a.gpu_uuid()
    # Every byte was reported once, in order.
    pos = offset
    for o, n in pages:
        assert o == pos
        pos += n
    assert pos == offset + len(want)


@pytest.mark.parametrize("mode", [GPU, HOST])
def test_read_into_second_gpu(gpu, server, mode):
    if realcuda.device_count() < 2:
        pytest.skip("needs two GPUs")
    req = add(server)
    deliver(server, mode, device=1)
    with cuda.GpuAllocation(PAGE, 0) as a0, cuda.GpuAllocation(len(DATA), 1) as a1, client(server) as c:
        assert a1.gpu_uuid() != a0.gpu_uuid()
        a0.copy_from_host(0, b"zero")
        with cuda.bound_context(0):
            # Copies and reads bind the allocation's context and restore this one.
            r = c.read_into_gpu(req, a1, 0)
            assert realcuda.current_context() == cuda._primary_context(0).value
        assert r.delivery == mode
        assert contents(a1, 0, len(DATA)) == DATA
        assert contents(a0, 0, 4) == b"zero"


# torch


def test_as_tensor_shares_memory_and_keeps_the_allocation(cuda_torch):
    torch = cuda_torch
    a = cuda.GpuAllocation(10_000)
    a.copy_from_host(0, bytes(range(256)) * 4)
    ref = weakref.ref(a)
    t = tensors.as_tensor(a)
    assert (t.device, t.dtype, t.numel(), t.data_ptr()) == (torch.device("cuda", 0), torch.uint8, a.size, a.ptr)
    view = t[:1024]
    del a, t
    gc.collect()
    # The view keeps the allocation alive and mapped.
    assert ref() is not None
    assert view.cpu().tolist() == list(range(256)) * 4
    view[:4] = torch.tensor([9, 8, 7, 6], dtype=torch.uint8, device="cuda")
    torch.cuda.synchronize()
    assert contents(ref(), 0, 4) == bytes([9, 8, 7, 6])
    del view
    gc.collect()
    assert ref() is None


@pytest.mark.parametrize("mode", [HOST, STREAM])
def test_tensor_target(cuda_torch, server, mode):
    torch = cuda_torch
    req = add(server)
    deliver(server, mode)
    t = torch.full((len(DATA) + 10,), FILL, dtype=torch.uint8, device="cuda")
    target = tensors.TensorTarget(t)
    assert target.device == 0 and target.size == t.numel()
    target.copy_from_host(len(DATA) + 7, b"xyz")
    target.copy_from_host(0, b"")
    with client(server) as c:
        r = c.read_into_gpu(req, target, 5, offset=1, length=len(DATA) - 1)
    assert r.delivery == mode
    got = bytes(t.cpu().tolist())
    assert got == bytes([FILL]) * 5 + DATA[1:] + bytes([FILL]) * 3 + b"xyz"
