# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0
import ctypes

import pytest

from racer import cuda


def test_struct_layouts():
    # Sizes and offsets of the cuda.h structs on LP64 Linux.
    assert ctypes.sizeof(cuda.CUmemLocation) == 8
    assert ctypes.sizeof(cuda.CUmemAllocationProp) == 32
    assert cuda.CUmemAllocationProp.location.offset == 8
    assert cuda.CUmemAllocationProp.win32HandleMetaData.offset == 16
    assert cuda.CUmemAllocationProp.allocFlags.offset == 24
    assert ctypes.sizeof(cuda.CUmemAccessDesc) == 12


def test_missing_libcuda(monkeypatch):
    monkeypatch.setenv("RACER_LIBCUDA", "libdoes-not-exist.so")
    monkeypatch.setattr(cuda, "_lib", None)
    assert not cuda.available()
    with pytest.raises(cuda.CudaUnavailable):
        cuda.GpuAllocation(1)


def test_host_buffer_pin_without_cuda(monkeypatch):
    from racer import HostBuffer

    monkeypatch.setenv("RACER_LIBCUDA", "libdoes-not-exist.so")
    monkeypatch.setattr(cuda, "_lib", None)
    with HostBuffer(4096) as b:
        assert b.pin() is False and not b.pinned
        b.view[:3] = b"abc"
        assert bytes(b.view[:3]) == b"abc"


@pytest.mark.skipif(not cuda.available(), reason="needs a GPU")
def test_gpu_allocation_roundtrip():
    with cuda.GpuAllocation(1000) as g:
        assert g.size >= 1000
        g.copy_from_host(0, b"hello")
        out = bytearray(5)
        g.copy_to_host(0, out)
        assert out == b"hello"
        import os

        fd = g.export_fd()
        assert fd >= 0
        os.close(fd)


class _FakeLib:
    """Records driver calls; contexts are ints, primary context of device d is 100 + d."""

    def __init__(self, current: int = 0, fail: str | None = None) -> None:
        self.current = current
        self.fail = fail
        self.calls: list[tuple] = []

    def _rc(self, name: str) -> int:
        return 1 if name == self.fail else 0

    def cuDeviceGet(self, out, ordinal):
        out._obj.value = ordinal
        return 0

    def cuDevicePrimaryCtxRetain(self, out, dev):
        out._obj.value = 100 + dev
        self.calls.append(("retain", dev))
        return 0

    def cuCtxGetCurrent(self, out):
        out._obj.value = self.current or None
        return 0

    def cuCtxSetCurrent(self, ctx):
        value = ctx.value if isinstance(ctx, ctypes.c_void_p) else ctx
        self.current = value or 0
        self.calls.append(("set", self.current))
        return 0

    def cuMemcpyHtoD_v2(self, dst, src, n):
        self.calls.append(("htod", self.current))
        return self._rc("cuMemcpyHtoD_v2")

    def cuMemcpyDtoH_v2(self, dst, src, n):
        self.calls.append(("dtoh", self.current))
        return self._rc("cuMemcpyDtoH_v2")

    def cuMemHostRegister_v2(self, addr, n, flags):
        self.calls.append(("register", self.current))
        return self._rc("cuMemHostRegister_v2")

    def cuMemHostUnregister(self, addr):
        return 0


@pytest.fixture
def fake(monkeypatch):
    def make(**kw):
        lib = _FakeLib(**kw)
        monkeypatch.setattr(cuda, "_lib", lib)
        monkeypatch.setattr(cuda, "_contexts", {})
        return lib

    return make


def _alloc(device: int) -> cuda.GpuAllocation:
    # Skip __init__ (VMM calls); only the fields the copy paths use.
    g = cuda.GpuAllocation.__new__(cuda.GpuAllocation)
    g.device, g.size, g.ptr = device, 4096, 0x1000
    g._handle, g._reserved, g._mapped = None, False, False
    return g


@pytest.mark.parametrize("current", [0, 100, 101])
def test_copy_from_host_binds_allocation_context(fake, current):
    lib = fake(current=current)
    _alloc(2).copy_from_host(0, b"abc")
    assert ("htod", 102) in lib.calls
    assert lib.current == current
    if current:
        assert lib.calls[-1] == ("set", current)


def test_copy_from_host_restores_context_on_error(fake):
    lib = fake(current=100, fail="cuMemcpyHtoD_v2")
    with pytest.raises(cuda.CudaError):
        _alloc(1).copy_from_host(0, b"abc")
    assert lib.calls[-2:] == [("htod", 101), ("set", 100)]
    assert lib.current == 100


def test_copy_from_host_without_prior_context(fake):
    lib = fake(current=0)
    _alloc(1).copy_from_host(0, b"abc")
    assert lib.calls == [("retain", 1), ("set", 101), ("htod", 101), ("set", 0)]


def test_copy_to_host_binds_allocation_context(fake):
    lib = fake(current=100)
    _alloc(3).copy_to_host(0, bytearray(3))
    assert ("dtoh", 103) in lib.calls and lib.current == 100


def test_already_current_context_is_not_switched(fake):
    lib = fake(current=101)
    _alloc(1).copy_from_host(0, b"abc")
    assert [c for c in lib.calls if c[0] == "set"] == []


def test_host_register_uses_requested_device(fake):
    lib = fake(current=0)
    cuda.host_register(bytearray(64), 2).close()
    assert ("register", 102) in lib.calls and lib.current == 0


def test_host_register_restores_context_on_error(fake):
    lib = fake(current=100, fail="cuMemHostRegister_v2")
    with pytest.raises(cuda.CudaError):
        cuda.host_register(bytearray(64), 1)
    assert lib.current == 100


def test_staging_pinned_for_destination_device(fake, tmp_path):
    from racer.client import Client

    lib = fake(current=0)
    c = Client.__new__(Client)
    c._pool, c.staging_bytes, c.pin_staging = [], 4096, True
    import threading

    c._pool_lock = threading.Lock()
    buf = c._take_staging(3)
    try:
        assert ("register", 103) in lib.calls
    finally:
        buf.close()


def _stub_torch(monkeypatch):
    import sys
    import types

    torch = types.ModuleType("torch")
    torch.uint8 = "uint8"
    monkeypatch.setitem(sys.modules, "torch", torch)


class _StubTensor:
    def __init__(self, index):
        self.dtype = "uint8"
        self.device = type("Dev", (), {"type": "cuda", "index": index})()

    def dim(self):
        return 1

    def is_contiguous(self):
        return True

    def numel(self):
        return 16


def test_tensor_target_exposes_device(monkeypatch):
    from racer.client import _dest_device
    from racer.tensors import TensorTarget

    _stub_torch(monkeypatch)
    assert TensorTarget(_StubTensor(3)).device == 3
    assert _dest_device(TensorTarget(_StubTensor(3))) == 3
    assert _dest_device(TensorTarget(_StubTensor(None))) is None


def test_tensor_target_staging_on_tensor_device(fake, monkeypatch):
    import threading

    from racer.client import Client, _dest_device
    from racer.tensors import TensorTarget

    _stub_torch(monkeypatch)
    lib = fake(current=0)
    c = Client.__new__(Client)
    c._pool, c.staging_bytes, c.pin_staging = [], 4096, True
    c._pool_lock = threading.Lock()
    buf = c._take_staging(_dest_device(TensorTarget(_StubTensor(3))))
    try:
        assert ("register", 103) in lib.calls
        assert ("register", 100) not in lib.calls
    finally:
        buf.close()
