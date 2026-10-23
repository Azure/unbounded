# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0
"""Minimal ctypes bindings to the CUDA driver API (libcuda.so.1).

Only what Racer needs: exportable device allocations through the virtual
memory management (VMM) API, host-to-device copies, and host registration.

Struct layouts and enum values come from cuda.h in the CUDA toolkit
(https://docs.nvidia.com/cuda/cuda-driver-api/group__CUDA__TYPES.html and
https://docs.nvidia.com/cuda/cuda-driver-api/group__CUDA__VA.html):

    typedef struct CUmemLocation_st { CUmemLocationType type; int id; } CUmemLocation;
    typedef struct CUmemAllocationProp_st {
        CUmemAllocationType type;
        CUmemAllocationHandleType requestedHandleTypes;
        CUmemLocation location;
        void *win32HandleMetaData;
        struct {
            unsigned char compressionType;
            unsigned char gpuDirectRDMACapable;
            unsigned short usage;
            unsigned char reserved[4];
        } allocFlags;
    } CUmemAllocationProp;
    typedef struct CUmemAccessDesc_st { CUmemLocation location; CUmemAccess_flags flags; } CUmemAccessDesc;

    CU_MEM_ALLOCATION_TYPE_PINNED            = 0x1
    CU_MEM_HANDLE_TYPE_POSIX_FILE_DESCRIPTOR = 0x1
    CU_MEM_LOCATION_TYPE_DEVICE              = 0x1
    CU_MEM_ACCESS_FLAGS_PROT_READWRITE       = 0x3
    CU_MEM_ALLOC_GRANULARITY_MINIMUM         = 0x0

Versioned symbols follow the cuda.h #defines: cuMemcpyHtoD -> cuMemcpyHtoD_v2,
cuMemHostRegister -> cuMemHostRegister_v2, cuDevicePrimaryCtxRelease ->
cuDevicePrimaryCtxRelease_v2.
"""

from __future__ import annotations

import contextlib
import ctypes
import logging
import os
import threading

from .errors import RacerError

log = logging.getLogger(__name__)

CU_MEM_ALLOCATION_TYPE_PINNED = 0x1
CU_MEM_HANDLE_TYPE_POSIX_FILE_DESCRIPTOR = 0x1
CU_MEM_LOCATION_TYPE_DEVICE = 0x1
CU_MEM_ACCESS_FLAGS_PROT_READWRITE = 0x3
CU_MEM_ALLOC_GRANULARITY_MINIMUM = 0x0
CU_MEMHOSTREGISTER_PORTABLE = 0x1

CUdeviceptr = ctypes.c_ulonglong
CUmemGenericAllocationHandle = ctypes.c_ulonglong


class CudaUnavailable(RacerError):
    """libcuda is missing or the driver cannot be initialized."""


class CudaError(RacerError):
    def __init__(self, func: str, code: int) -> None:
        super().__init__(f"{func} failed with CUresult {code}")
        self.func = func
        self.code = code


class CUmemLocation(ctypes.Structure):
    _fields_ = [("type", ctypes.c_int), ("id", ctypes.c_int)]


class _AllocFlags(ctypes.Structure):
    _fields_ = [
        ("compressionType", ctypes.c_ubyte),
        ("gpuDirectRDMACapable", ctypes.c_ubyte),
        ("usage", ctypes.c_ushort),
        ("reserved", ctypes.c_ubyte * 4),
    ]


class CUmemAllocationProp(ctypes.Structure):
    _fields_ = [
        ("type", ctypes.c_int),
        ("requestedHandleTypes", ctypes.c_int),
        ("location", CUmemLocation),
        ("win32HandleMetaData", ctypes.c_void_p),
        ("allocFlags", _AllocFlags),
    ]


class CUmemAccessDesc(ctypes.Structure):
    _fields_ = [("location", CUmemLocation), ("flags", ctypes.c_int)]


_lock = threading.Lock()
_lib = None
_contexts: dict[int, ctypes.c_void_p] = {}


def _load():
    global _lib
    with _lock:
        if _lib is not None:
            return _lib
        name = os.environ.get("RACER_LIBCUDA", "libcuda.so.1")
        try:
            lib = ctypes.CDLL(name)
        except OSError as exc:
            raise CudaUnavailable(f"cannot load {name}: {exc}") from exc
        sig = {
            "cuInit": [ctypes.c_uint],
            "cuDeviceGet": [ctypes.POINTER(ctypes.c_int), ctypes.c_int],
            "cuCtxGetCurrent": [ctypes.POINTER(ctypes.c_void_p)],
            "cuCtxSetCurrent": [ctypes.c_void_p],
            "cuDevicePrimaryCtxRetain": [ctypes.POINTER(ctypes.c_void_p), ctypes.c_int],
            "cuMemGetAllocationGranularity": [
                ctypes.POINTER(ctypes.c_size_t),
                ctypes.POINTER(CUmemAllocationProp),
                ctypes.c_int,
            ],
            "cuMemCreate": [
                ctypes.POINTER(CUmemGenericAllocationHandle),
                ctypes.c_size_t,
                ctypes.POINTER(CUmemAllocationProp),
                ctypes.c_ulonglong,
            ],
            "cuMemRelease": [CUmemGenericAllocationHandle],
            "cuMemAddressReserve": [
                ctypes.POINTER(CUdeviceptr),
                ctypes.c_size_t,
                ctypes.c_size_t,
                CUdeviceptr,
                ctypes.c_ulonglong,
            ],
            "cuMemAddressFree": [CUdeviceptr, ctypes.c_size_t],
            "cuMemMap": [CUdeviceptr, ctypes.c_size_t, ctypes.c_size_t, CUmemGenericAllocationHandle, ctypes.c_ulonglong],
            "cuMemUnmap": [CUdeviceptr, ctypes.c_size_t],
            "cuMemSetAccess": [CUdeviceptr, ctypes.c_size_t, ctypes.POINTER(CUmemAccessDesc), ctypes.c_size_t],
            "cuMemExportToShareableHandle": [
                ctypes.c_void_p,
                CUmemGenericAllocationHandle,
                ctypes.c_int,
                ctypes.c_ulonglong,
            ],
            "cuMemcpyHtoD_v2": [CUdeviceptr, ctypes.c_void_p, ctypes.c_size_t],
            "cuMemcpyDtoH_v2": [ctypes.c_void_p, CUdeviceptr, ctypes.c_size_t],
            "cuMemHostRegister_v2": [ctypes.c_void_p, ctypes.c_size_t, ctypes.c_uint],
            "cuMemHostUnregister": [ctypes.c_void_p],
        }
        for fn, args in sig.items():
            f = getattr(lib, fn, None)
            if f is None:
                raise CudaUnavailable(f"libcuda has no {fn}")
            f.argtypes = args
            f.restype = ctypes.c_int
        rc = lib.cuInit(0)
        if rc != 0:
            raise CudaUnavailable(f"cuInit failed with CUresult {rc}")
        _lib = lib
        return lib


def _check(func: str, rc: int) -> None:
    if rc != 0:
        raise CudaError(func, rc)


def available() -> bool:
    try:
        _load()
    except CudaUnavailable:
        return False
    return True


def ensure_context(device: int = 0) -> None:
    """Make sure a context is current; retain the primary context if not.

    torch uses the primary context too, so both share one context.
    """
    lib = _load()
    cur = ctypes.c_void_p()
    _check("cuCtxGetCurrent", lib.cuCtxGetCurrent(ctypes.byref(cur)))
    if cur.value:
        return
    _check("cuCtxSetCurrent", lib.cuCtxSetCurrent(_primary_context(device)))


def _primary_context(device: int) -> ctypes.c_void_p:
    """Return the device's primary context, retained once per process.

    The retain is never released: torch shares the same primary context and
    the cached handle stays valid for the life of the process.
    """
    lib = _load()
    with _lock:
        ctx = _contexts.get(device)
        if ctx is None:
            dev = ctypes.c_int()
            _check("cuDeviceGet", lib.cuDeviceGet(ctypes.byref(dev), device))
            ctx = ctypes.c_void_p()
            _check("cuDevicePrimaryCtxRetain", lib.cuDevicePrimaryCtxRetain(ctypes.byref(ctx), dev.value))
            _contexts[device] = ctx
    return ctx


@contextlib.contextmanager
def bound_context(device: int):
    """Make ``device``'s primary context current, restoring the previous one after.

    Worker threads do not inherit a context from the thread that created an
    allocation, and an unrelated current context (for example another
    device's) is wrong for driver calls on this device's memory, so callers
    bind explicitly instead of relying on thread history. Uses
    cuCtxGetCurrent/cuCtxSetCurrent
    (https://docs.nvidia.com/cuda/cuda-driver-api/group__CUDA__CTX.html).
    """
    lib = _load()
    ctx = _primary_context(device)
    prev = ctypes.c_void_p()
    _check("cuCtxGetCurrent", lib.cuCtxGetCurrent(ctypes.byref(prev)))
    if prev.value == ctx.value:
        yield
        return
    _check("cuCtxSetCurrent", lib.cuCtxSetCurrent(ctx))
    try:
        yield
    finally:
        rc = lib.cuCtxSetCurrent(prev)
        if rc != 0:
            log.warning("racer: restoring the CUDA context failed with CUresult %d", rc)


def _address(buf) -> tuple[int, object]:
    """Return the address of a buffer and an object that keeps it valid."""
    mv = memoryview(buf).cast("B")
    if mv.readonly:
        holder = ctypes.create_string_buffer(mv.tobytes(), len(mv))
        return ctypes.addressof(holder), holder
    arr = (ctypes.c_char * len(mv)).from_buffer(mv)
    return ctypes.addressof(arr), arr


def _allocation_prop(device: int) -> CUmemAllocationProp:
    prop = CUmemAllocationProp()
    prop.type = CU_MEM_ALLOCATION_TYPE_PINNED
    prop.requestedHandleTypes = CU_MEM_HANDLE_TYPE_POSIX_FILE_DESCRIPTOR
    prop.location.type = CU_MEM_LOCATION_TYPE_DEVICE
    prop.location.id = device
    return prop


def granularity(device: int = 0) -> int:
    lib = _load()
    ensure_context(device)
    g = ctypes.c_size_t()
    prop = _allocation_prop(device)
    _check(
        "cuMemGetAllocationGranularity",
        lib.cuMemGetAllocationGranularity(ctypes.byref(g), ctypes.byref(prop), CU_MEM_ALLOC_GRANULARITY_MINIMUM),
    )
    return g.value


class GpuAllocation:
    """Exportable device memory, mapped read-write in this process.

    ``size`` is the size passed to cuMemCreate, rounded up to the allocation
    granularity. It is what gpu-read sends as Racer-Allocation-Size.

    The object implements ``__cuda_array_interface__`` (a flat uint8 array),
    so torch, CuPy and Numba can wrap it without a copy. Any wrapper must keep
    this object alive; ``racer.torch.as_tensor`` does.
    """

    def __init__(self, size: int, device: int = 0) -> None:
        if size <= 0:
            raise RacerError("size must be positive")
        lib = _load()
        g = granularity(device)
        self.device = device
        self.requested = size
        self.size = (size + g - 1) // g * g
        self.ptr = 0
        self._handle = None
        self._reserved = False
        self._mapped = False
        prop = _allocation_prop(device)
        handle = CUmemGenericAllocationHandle()
        with bound_context(device):
            try:
                _check("cuMemCreate", lib.cuMemCreate(ctypes.byref(handle), self.size, ctypes.byref(prop), 0))
                self._handle = handle.value
                ptr = CUdeviceptr()
                _check("cuMemAddressReserve", lib.cuMemAddressReserve(ctypes.byref(ptr), self.size, g, 0, 0))
                self.ptr = ptr.value
                self._reserved = True
                _check("cuMemMap", lib.cuMemMap(self.ptr, self.size, 0, self._handle, 0))
                self._mapped = True
                desc = CUmemAccessDesc()
                desc.location.type = CU_MEM_LOCATION_TYPE_DEVICE
                desc.location.id = device
                desc.flags = CU_MEM_ACCESS_FLAGS_PROT_READWRITE
                _check("cuMemSetAccess", lib.cuMemSetAccess(self.ptr, self.size, ctypes.byref(desc), 1))
            except BaseException:
                self.close()
                raise

    def export_fd(self) -> int:
        """Return a new POSIX file descriptor for the allocation. The caller closes it."""
        lib = _load()
        fd = ctypes.c_int(-1)
        _check(
            "cuMemExportToShareableHandle",
            lib.cuMemExportToShareableHandle(ctypes.byref(fd), self._handle, CU_MEM_HANDLE_TYPE_POSIX_FILE_DESCRIPTOR, 0),
        )
        return fd.value

    def copy_from_host(self, offset: int, data) -> None:
        mv = memoryview(data).cast("B")
        if offset < 0 or offset + len(mv) > self.size:
            raise RacerError("copy outside the allocation")
        if not mv:
            return
        addr, keep = _address(mv)
        try:
            with bound_context(self.device):
                _check("cuMemcpyHtoD_v2", _load().cuMemcpyHtoD_v2(self.ptr + offset, addr, len(mv)))
        finally:
            del keep

    def copy_to_host(self, offset: int, out) -> None:
        mv = memoryview(out).cast("B")
        if offset < 0 or offset + len(mv) > self.size:
            raise RacerError("copy outside the allocation")
        addr, keep = _address(mv)
        try:
            with bound_context(self.device):
                _check("cuMemcpyDtoH_v2", _load().cuMemcpyDtoH_v2(addr, self.ptr + offset, len(mv)))
        finally:
            del keep

    @property
    def __cuda_array_interface__(self) -> dict:
        return {
            "shape": (self.size,),
            "typestr": "|u1",
            "data": (self.ptr, False),
            "strides": None,
            "version": 3,
        }

    def close(self) -> None:
        """Unmap and release. On failure the resource is kept (leaked), never freed early."""
        if _lib is None:
            return
        lib = _lib
        try:
            if self._mapped:
                _check("cuMemUnmap", lib.cuMemUnmap(self.ptr, self.size))
                self._mapped = False
            if self._reserved:
                _check("cuMemAddressFree", lib.cuMemAddressFree(self.ptr, self.size))
                self._reserved = False
            if self._handle is not None:
                _check("cuMemRelease", lib.cuMemRelease(self._handle))
                self._handle = None
        except CudaError as exc:
            log.warning("racer: leaking GPU allocation: %s", exc)
            self._mapped = self._reserved = False
            self._handle = None

    def __enter__(self) -> "GpuAllocation":
        return self

    def __exit__(self, *exc: object) -> None:
        self.close()

    def __del__(self) -> None:
        try:
            self.close()
        except Exception:
            # Finalizers must never raise, and during interpreter shutdown the
            # CUDA library or module globals may already be gone. Callers that
            # need to observe free errors should call close() explicitly.
            return


class HostRegistration:
    def __init__(self, addr: int, keep: object) -> None:
        self._addr = addr
        self._keep = keep

    def close(self) -> None:
        if self._addr:
            rc = _load().cuMemHostUnregister(self._addr)
            if rc != 0:
                log.warning("racer: cuMemHostUnregister failed with CUresult %d", rc)
            self._addr = 0
            self._keep = None


def host_register(buf, device: int = 0) -> HostRegistration:
    """Page-lock a writable host buffer with cuMemHostRegister.

    The registration is made in ``device``'s primary context (PORTABLE makes
    it usable from every context); pass the device the buffer will feed.
    """
    lib = _load()
    mv = memoryview(buf).cast("B")
    if mv.readonly:
        raise RacerError("buffer must be writable")
    arr = (ctypes.c_char * len(mv)).from_buffer(mv)
    addr = ctypes.addressof(arr)
    with bound_context(device):
        _check("cuMemHostRegister_v2", lib.cuMemHostRegister_v2(addr, len(mv), CU_MEMHOSTREGISTER_PORTABLE))
    # Drop the export so the owner can still close its mapping after unregistering.
    del arr
    mv.release()
    return HostRegistration(addr, None)
