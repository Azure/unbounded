# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0
"""Driver calls the real-GPU tests need beyond racer.cuda.

Signatures from cuda.h (https://docs.nvidia.com/cuda/cuda-driver-api/):

    CUresult cuDeviceGetCount(int *count);
    CUresult cuCtxGetCurrent(CUcontext *pctx);
    CUresult cuCtxSynchronize(void);
    CUresult cuMemGetInfo_v2(size_t *free, size_t *total);
    CUresult cuMemImportFromShareableHandle(CUmemGenericAllocationHandle *handle, void *osHandle,
                                            CUmemAllocationHandleType shHandleType);

    CUDA_ERROR_HOST_MEMORY_ALREADY_REGISTERED = 712
"""

from __future__ import annotations

import contextlib
import ctypes

from racer import cuda

CUDA_ERROR_HOST_MEMORY_ALREADY_REGISTERED = 712


def _fn(name, *argtypes):
    # Indexing makes a new function object, so racer.cuda's argtypes stay as they are.
    f = cuda._load()[name]
    f.argtypes = list(argtypes)
    f.restype = ctypes.c_int
    return f


def _check(name, rc):
    if rc != 0:
        raise cuda.CudaError(name, rc)


def device_count() -> int:
    n = ctypes.c_int()
    _check("cuDeviceGetCount", _fn("cuDeviceGetCount", ctypes.POINTER(ctypes.c_int))(ctypes.byref(n)))
    return n.value


def current_context() -> int | None:
    ctx = ctypes.c_void_p()
    _check("cuCtxGetCurrent", cuda._load().cuCtxGetCurrent(ctypes.byref(ctx)))
    return ctx.value


def mem_get_info(device=0) -> tuple[int, int]:
    """(free, total) bytes of device memory."""
    free, total = ctypes.c_size_t(), ctypes.c_size_t()
    f = _fn("cuMemGetInfo_v2", ctypes.POINTER(ctypes.c_size_t), ctypes.POINTER(ctypes.c_size_t))
    with cuda.bound_context(device):
        _check("cuMemGetInfo_v2", f(ctypes.byref(free), ctypes.byref(total)))
    return free.value, total.value


def registered(addr, size, device=0) -> bool:
    """Whether [addr, addr + size) is page-locked with cuMemHostRegister.

    Registering a registered range again fails with
    CUDA_ERROR_HOST_MEMORY_ALREADY_REGISTERED; a probe that succeeds is undone.
    """
    lib = cuda._load()
    with cuda.bound_context(device):
        rc = lib.cuMemHostRegister_v2(addr, size, 0)
        if rc == CUDA_ERROR_HOST_MEMORY_ALREADY_REGISTERED:
            return True
        _check("cuMemHostRegister_v2", rc)
        _check("cuMemHostUnregister", lib.cuMemHostUnregister(addr))
    return False


@contextlib.contextmanager
def imported(fd, size, device=0):
    """Map an exported allocation at a second address, as the dataplane does for gpu-read."""
    lib = cuda._load()
    imp = _fn(
        "cuMemImportFromShareableHandle",
        ctypes.POINTER(cuda.CUmemGenericAllocationHandle),
        ctypes.c_void_p,
        ctypes.c_int,
    )
    handle = cuda.CUmemGenericAllocationHandle()
    ptr = cuda.CUdeviceptr()
    with cuda.bound_context(device):
        # A POSIX file descriptor is passed as the pointer value itself.
        _check(
            "cuMemImportFromShareableHandle",
            imp(ctypes.byref(handle), fd, cuda.CU_MEM_HANDLE_TYPE_POSIX_FILE_DESCRIPTOR),
        )
        try:
            g = cuda.granularity(device)
            _check("cuMemAddressReserve", lib.cuMemAddressReserve(ctypes.byref(ptr), size, g, 0, 0))
            try:
                _check("cuMemMap", lib.cuMemMap(ptr.value, size, 0, handle.value, 0))
                try:
                    desc = cuda.CUmemAccessDesc()
                    desc.location.type = cuda.CU_MEM_LOCATION_TYPE_DEVICE
                    desc.location.id = device
                    desc.flags = cuda.CU_MEM_ACCESS_FLAGS_PROT_READWRITE
                    _check("cuMemSetAccess", lib.cuMemSetAccess(ptr.value, size, ctypes.byref(desc), 1))
                    yield ptr.value
                    _check("cuCtxSynchronize", _fn("cuCtxSynchronize")())
                finally:
                    _check("cuMemUnmap", lib.cuMemUnmap(ptr.value, size))
            finally:
                _check("cuMemAddressFree", lib.cuMemAddressFree(ptr.value, size))
        finally:
            _check("cuMemRelease", lib.cuMemRelease(handle.value))


def write(fd, size, offset, data, device=0) -> None:
    """Write data at offset into the exported allocation fd of size bytes."""
    src = ctypes.create_string_buffer(bytes(data), len(data))
    with imported(fd, size, device) as ptr:
        _check("cuMemcpyHtoD_v2", cuda._load().cuMemcpyHtoD_v2(ptr + offset, src, len(data)))


def read(fd, size, offset, n, device=0) -> bytes:
    """Read n bytes at offset from the exported allocation fd of size bytes."""
    out = ctypes.create_string_buffer(n)
    with imported(fd, size, device) as ptr:
        _check("cuMemcpyDtoH_v2", cuda._load().cuMemcpyDtoH_v2(out, ptr + offset, n))
    return out.raw


def gpu_writer(device=0):
    """A FakeRacer.gpu_write that writes pages into the real allocation, so gpu-read delivers."""

    def gpu_write(fd, offset, data, allocation_size):
        write(fd, allocation_size, offset, data, device)

    return gpu_write
