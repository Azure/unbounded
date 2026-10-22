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
