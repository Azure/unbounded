# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0
"""Staging buffers, zero-copy tensor views, and delivery selection.

Lifetime rule for zero-copy views: every read gets a fresh *owner* object
over its staging buffer. torch wraps the owner (``__cuda_array_interface__``
for GPU memory, the buffer protocol for host memory), so the owner lives
exactly as long as some tensor shares that storage, including narrowed
views a model keeps. A ``weakref.finalize`` on the owner marks the buffer
released. A buffer is reused only when (a) that finalizer has fired and
(b) the CUDA event recorded when the consumer finished the read has
completed. Live views make the pool allocate another buffer, up to
``max_bytes``; past the cap the pool clones the remaining tensors (clone
"auto") or fails (clone "never").
"""

from __future__ import annotations

import collections
import gc
import logging
import os
import threading
import weakref
from concurrent.futures import ThreadPoolExecutor

from .manifest import DTYPES

log = logging.getLogger("vllm.racer_vllm.reader")

GPU = "gpu"
HOST = "host"
STREAM = "stream"
MODES = (GPU, HOST, STREAM)
FALLBACK_STATUSES = frozenset({400, 404, 405, 501})


def _torch():
    try:
        import torch
    except ImportError as exc:  # pragma: no cover - torch comes from the host image
        raise ImportError("racer-vllm needs torch from the vLLM image") from exc
    return torch


# Owners -----------------------------------------------------------------------


class GpuOwner:
    """A flat uint8 window of device memory. Keeps the allocation alive."""

    def __init__(self, allocation, length: int) -> None:
        self.allocation = allocation
        self.length = length
        self.device = getattr(allocation, "device", 0)

    @property
    def __cuda_array_interface__(self) -> dict:
        return {"shape": (self.length,), "typestr": "|u1", "data": (self.allocation.ptr, False), "version": 3}


class HostOwner:
    """A flat uint8 window of host memory.

    ``anchor`` is a ctypes array over the window. torch.frombuffer keeps a
    reference to the object it wraps, so the anchor lives exactly as long as
    some tensor shares the window; the staging pool watches the anchor, not
    this object. A plain memoryview cannot serve: it does not support weak
    references.
    """

    def __init__(self, view: memoryview, length: int) -> None:
        import ctypes

        self.length = length
        self.anchor = (ctypes.c_ubyte * length).from_buffer(view[:length]) if length else (ctypes.c_ubyte * 0)()


# Allocators -------------------------------------------------------------------


class HostAllocator:
    """Pinned memfd buffers (host-read capable), else bytearrays (stream only)."""

    kind = HOST

    def __init__(self, device: int = 0, pin: bool = True) -> None:
        self.device = device
        self.pin = pin

    def allocate(self, size: int):
        import racer

        try:
            buf = racer.HostBuffer(size, "racer-vllm-staging")
        except (OSError, AttributeError):
            return bytearray(size)
        if self.pin:
            try:
                buf.pin(self.device)
            except Exception as exc:  # pinning is an optimization only
                log.debug("racer: pinning staging failed: %s", exc)
        return buf

    @staticmethod
    def view(raw) -> memoryview:
        return memoryview(raw.view if hasattr(raw, "view") else raw)

    def owner(self, raw, length: int):
        return HostOwner(self.view(raw), length)

    def as_tensor(self, owner):
        torch = _torch()
        # torch keeps a reference to the anchor for as long as the storage lives.
        return torch.frombuffer(owner.anchor, dtype=torch.uint8)

    def free(self, raw) -> None:
        close = getattr(raw, "close", None)
        if close is not None:
            try:
                close()
            except BufferError:  # a view escaped despite the finalizer; leak, never free early
                log.warning("racer: staging buffer still exported; leaking it")


class GpuAllocator:
    """Exportable device memory from racer.cuda.GpuAllocation (gpu-read capable)."""

    kind = GPU

    def __init__(self, device: int = 0) -> None:
        import racer.cuda as rcuda

        if not rcuda.available():
            raise rcuda.CudaUnavailable("libcuda is not available")
        self.device = device
        self._rcuda = rcuda

    def allocate(self, size: int):
        return self._rcuda.GpuAllocation(size, self.device)

    def owner(self, raw, length: int):
        return GpuOwner(raw, length)

    def as_tensor(self, owner):
        torch = _torch()
        return torch.as_tensor(owner, device=torch.device("cuda", owner.device))

    def free(self, raw) -> None:
        raw.close()


class DoneEvent:
    """Stand-in event when CUDA is not in use: always complete."""

    def record(self, stream=None) -> None:
        pass

    def query(self) -> bool:
        return True

    def synchronize(self) -> None:
        pass


def default_event_factory():
    try:
        torch = _torch()
        if torch.cuda.is_available():
            return torch.cuda.Event
    except Exception:  # pragma: no cover - no torch: no CUDA work to fence
        pass
    return DoneEvent


# Buffers ----------------------------------------------------------------------


class Buffer:
    def __init__(self, pool: "StagingPool", raw, size: int, owned: bool) -> None:
        self.pool = pool
        self.raw = raw
        self.size = size
        self.owned = owned
        self.released = True  # no live owner
        self.event = None
        self.retired = False

    def make_owner(self, length: int):
        owner = self.pool.allocator.owner(self.raw, length)
        self.released = False
        weakref.finalize(getattr(owner, "anchor", owner), self._finalized)
        return owner

    def _finalized(self) -> None:
        self.released = True

    def ready(self) -> bool:
        return self.released and (self.event is None or self.event.query())

    def tensor_views(self, read, clone: bool):
        """Yield (name, tensor) for every tensor of read, sharing this buffer unless clone."""
        if read.length == 0 or all(t.nbytes == 0 for t in read.tensors):
            for t in read.tensors:
                yield t.name, empty_tensor(t)
            return
        owner = self.make_owner(read.length)
        base = self.pool.allocator.as_tensor(owner)
        del owner
        for meta in read.tensors:
            t = tensor_view(base, meta)
            if clone:
                t = t.clone()
            yield meta.name, t
            del t
        del base


def empty_tensor(meta):
    torch = _torch()
    return torch.empty(tuple(meta.shape), dtype=getattr(torch, DTYPES[meta.dtype][1]))


def tensor_view(base, meta):
    """A dtype/shape view of base[rva:rva+nbytes] sharing storage (clone only if misaligned)."""
    torch = _torch()
    dtype = getattr(torch, DTYPES[meta.dtype][1])
    if meta.nbytes == 0:
        return torch.empty(tuple(meta.shape), dtype=dtype, device=getattr(base, "device", None))
    raw = base[meta.rva : meta.rva + meta.nbytes]
    if meta.rva % DTYPES[meta.dtype][0]:
        raw = raw.clone()  # views need element-aligned storage offsets
    return raw.view(dtype).reshape(tuple(meta.shape))


class StagingPool:
    """Buffers of staging_bytes (and dedicated larger ones) under max_bytes."""

    def __init__(
        self,
        staging_bytes: int,
        max_bytes: int,
        concurrency: int,
        allocator,
        clone: str = "auto",
        event_factory=None,
    ) -> None:
        if concurrency < 1:
            raise ValueError("concurrency must be at least 1")
        if clone not in ("auto", "always", "never"):
            raise ValueError("clone must be auto, always or never")
        self.staging_bytes = staging_bytes
        self.max_bytes = max_bytes
        self.concurrency = concurrency
        self.allocator = allocator
        self.clone_mode = clone
        self.cloning = clone == "always"
        self.event_factory = event_factory or default_event_factory()
        self.buffers: list = []
        self.idle: list = []
        self.allocated = 0
        self.allocations = 0
        self._logged = False
        self._lock = threading.Lock()

    def set_allocator(self, allocator) -> None:
        """Switch memory kind (after a delivery fallback). Idle buffers are freed."""
        self._sweep()
        for b in list(self.idle):
            if b.ready():
                self._drop(b)
        self.allocator = allocator

    def _drop(self, b: Buffer) -> None:
        if b in self.idle:
            self.idle.remove(b)
        if b in self.buffers:
            self.buffers.remove(b)
            self.allocated -= b.size
        self.allocator_for(b).free(b.raw)

    def allocator_for(self, b: Buffer):
        return getattr(b, "alloc", self.allocator)

    def _new(self, size: int, owned: bool) -> Buffer:
        b = Buffer(self, self.allocator.allocate(size), size, owned)
        b.alloc = self.allocator
        self.buffers.append(b)
        self.allocated += size
        self.allocations += 1
        return b

    def _sweep(self) -> None:
        for b in [b for b in self.buffers if b.retired and b.ready()]:
            self._drop(b)

    def _switch_to_clone(self) -> None:
        if self.clone_mode == "never":
            raise MemoryError(
                f"racer loader: staging buffers ({self.allocated} bytes) are all held by tensor views and "
                f"max_staging_bytes is {self.max_bytes}; raise max_staging_bytes or set clone to auto"
            )
        self.cloning = True
        if not self._logged:
            self._logged = True
            log.warning(
                "racer: staging reached max_staging_bytes=%d with views still alive; cloning remaining tensors",
                self.max_bytes,
            )

    def acquire(self, size: int, owned: bool = False) -> Buffer:
        with self._lock:
            self._sweep()
            if owned or size > self.staging_bytes:
                if self.allocated + size > self.max_bytes and not self.cloning:
                    gc.collect()
                    self._sweep()
                    if self.allocated + size > self.max_bytes:
                        self._switch_to_clone()
                b = self._new(size, True)
                return b
            for b in self.idle:
                if b.ready():
                    self.idle.remove(b)
                    return b
            if self.allocated + self.staging_bytes <= self.max_bytes:
                return self._new(self.staging_bytes, False)
            # At the cap: wait on a buffer whose views are gone but whose GPU work is pending.
            for attempt in range(2):
                for b in self.idle:
                    if b.released:
                        if b.event is not None:
                            b.event.synchronize()
                        self.idle.remove(b)
                        return b
                if attempt == 0:
                    gc.collect()
            self._switch_to_clone()
            # Clone mode returns buffers at once, so this overshoot stays bounded by concurrency.
            return self._new(self.staging_bytes, False)

    def release(self, b: Buffer) -> None:
        """The consumer is done with b: fence its GPU work and return it."""
        ev = self.event_factory()
        ev.record()
        with self._lock:
            b.event = ev
            if b.owned:
                b.retired = True
            else:
                self.idle.append(b)

    def abandon(self, b: Buffer) -> None:
        """Return a buffer that was never consumed (canceled or failed read)."""
        with self._lock:
            if b.owned:
                b.retired = True
            else:
                self.idle.append(b)

    def close(self) -> None:
        """Free every buffer whose views are gone, after its GPU work; leak the rest."""
        with self._lock:
            for b in list(self.buffers):
                if b.released:
                    if b.event is not None:
                        b.event.synchronize()
                    self._drop(b)
                else:
                    log.debug("racer: leaving a staging buffer to its live views")


# Delivery ---------------------------------------------------------------------

_mode_cache: "weakref.WeakKeyDictionary" = weakref.WeakKeyDictionary()
_mode_lock = threading.Lock()


def cached_mode(client):
    with _mode_lock:
        got = _mode_cache.get(client)
    if got and got[0] == os.getpid():
        return got[1]
    return None


def remember_mode(client, mode: str) -> None:
    with _mode_lock:
        _mode_cache[client] = (os.getpid(), mode)


def forget(client=None) -> None:
    with _mode_lock:
        if client is None:
            _mode_cache.clear()
        else:
            _mode_cache.pop(client, None)


def advertised_modes(client):
    """The delivery modes GET /v1/capabilities lists, or None when unknown.

    An older dataplane without the endpoint, or any error asking, leaves the
    choice to real reads, which fall back on refusal.
    """
    caps = getattr(client, "capabilities", None)
    if not callable(caps):
        return None
    try:
        doc = caps()
    except Exception as exc:
        log.debug("racer: capabilities unavailable: %s", exc)
        return None
    delivery = doc.get("delivery") if isinstance(doc, dict) else None
    if not isinstance(delivery, list):
        return None
    return {m for m in delivery if isinstance(m, str)}


def candidates(client, delivery: str = "auto") -> list:
    """Modes to try, in order, for a client and a delivery setting."""
    if delivery == "gpu":
        modes = [GPU]
    elif delivery == "host":
        modes = [HOST, STREAM]
    else:
        modes = [GPU, HOST, STREAM]
    have = {GPU: "gpu_read", HOST: "host_read", STREAM: "read"}
    modes = [m for m in modes if callable(getattr(client, have[m], None))]
    allowed = advertised_modes(client)
    if allowed is not None:
        modes = [m for m in modes if m in allowed]
    if not modes:
        raise RuntimeError(f"racer loader: no usable delivery mode for delivery={delivery!r}")
    return modes


def probe(client, delivery: str = "auto", make_allocator=None) -> list:
    """Ordered usable modes. A mode a previous read settled on is used alone.

    make_allocator(mode) must succeed for a mode to stay; a GPU mode whose
    allocator fails (no CUDA) is dropped. Real reads confirm the rest.
    """
    got = cached_mode(client)
    modes = candidates(client, delivery)
    if got in modes:
        return [got]
    if make_allocator is not None:
        usable = []
        for m in modes:
            try:
                make_allocator(m)
            except Exception as exc:
                log.info("racer: %s delivery unavailable: %s", m, exc)
                continue
            usable.append(m)
        if not usable:
            raise RuntimeError(f"racer loader: no delivery mode could allocate staging (tried {modes})")
        modes = usable
    return modes


def is_fallback(exc: BaseException) -> bool:
    status = getattr(exc, "status", None)
    if status in FALLBACK_STATUSES:
        return True
    try:
        import racer.cuda as rcuda

        return isinstance(exc, (rcuda.CudaUnavailable, rcuda.CudaError, NotImplementedError))
    except ImportError:  # pragma: no cover
        return isinstance(exc, NotImplementedError)


def do_read(client, mode: str, request, read, buf: Buffer, if_match):
    """Fill buf with read's bytes using one mode. Returns bytes transferred."""
    n = read.length
    if n == 0:
        return 0
    raw = buf.raw
    if mode == GPU:
        fd = raw.export_fd()
        try:
            uuid = None
            get = getattr(raw, "gpu_uuid", None)
            if callable(get):
                try:
                    uuid = get()
                except Exception:
                    uuid = None
            kw = {"gpu_uuid": uuid} if uuid else {}
            client.gpu_read(request, fd, raw.size, 0, n, offset=read.offset, length=n, if_match=if_match, **kw)
        finally:
            os.close(fd)
    elif mode == HOST:
        if not hasattr(raw, "fileno"):
            raise NotImplementedError("host-read needs a memfd staging buffer")
        client.host_read(request, raw.fileno(), 0, n, offset=read.offset, length=n, if_match=if_match)
    else:
        data = client.read(request, read.offset, n, if_match=if_match)
        if len(data) != n:
            raise IOError(f"short read: wanted {n} bytes, got {len(data)}")
        if hasattr(raw, "copy_from_host"):
            raw.copy_from_host(0, data)
        else:
            HostAllocator.view(raw)[:n] = data
    return n


def new_stats() -> dict:
    return {"files": 0, "reads": dict.fromkeys(MODES, 0), "bytes": dict.fromkeys(MODES, 0)}


class WeightReader:
    """Reads planned Reads through one client and yields (name, tensor)."""

    def __init__(self, client, pool: StagingPool, modes: list, allocators: dict, stats: dict | None = None) -> None:
        self.client = client
        self.pool = pool
        self.modes = list(modes)
        self.allocators = allocators  # mode -> allocator
        self.stats = stats if stats is not None else new_stats()
        self.confirmed = len(self.modes) == 1 and cached_mode(client) == self.modes[0]
        if self.allocators.get(self.modes[0]) is not None:
            pool.allocator = self.allocators[self.modes[0]]

    @property
    def mode(self) -> str:
        return self.modes[0]

    def _first(self, request, read, if_match):
        """Run the first real read, falling back through modes until one works."""
        while True:
            mode = self.modes[0]
            buf = self.pool.acquire(read.length, read.owns_buffer)
            try:
                do_read(self.client, mode, request, read, buf, if_match)
            except Exception as exc:
                self.pool.abandon(buf)
                if len(self.modes) > 1 and is_fallback(exc):
                    log.info("racer: %s delivery refused (%s); trying %s", mode, exc, self.modes[1])
                    self.modes.pop(0)
                    nxt = self.allocators.get(self.modes[0])
                    if nxt is not None and nxt is not self.pool.allocator:
                        self.pool.set_allocator(nxt)
                    continue
                raise
            self.confirmed = True
            remember_mode(self.client, mode)
            return buf

    def iter(self, jobs, on_data=None):
        """jobs: iterable of (request, Read, if_match). Yields (name, tensor) in order.

        on_data(read, memoryview) runs for host buffers before tensors are made
        (used for sha256 checks).
        """
        jobs = collections.deque(jobs)
        inflight: collections.deque = collections.deque()
        pool = self.pool
        executor = None
        current = None
        try:
            while jobs and not self.confirmed:
                req, read, im = jobs.popleft()
                if read.length == 0:
                    inflight.append((None, read, None))
                    continue
                buf = self._first(req, read, im)
                inflight.append((None, read, buf))
                self._count(read)
                break
            executor = ThreadPoolExecutor(max_workers=pool.concurrency, thread_name_prefix="racer-read")
            while jobs or inflight:
                while jobs and len(inflight) < pool.concurrency:
                    req, read, im = jobs.popleft()
                    buf = pool.acquire(read.length, read.owns_buffer) if read.length else None
                    fut = executor.submit(do_read, self.client, self.mode, req, read, buf, im) if buf else None
                    inflight.append((fut, read, buf))
                fut, read, buf = inflight.popleft()
                if fut is not None:
                    fut.result()
                    self._count(read)
                if buf is None:
                    for t in read.tensors:
                        yield t.name, empty_tensor(t)
                    continue
                current = buf
                if on_data is not None and pool.allocator.kind == HOST:
                    on_data(read, HostAllocator.view(buf.raw)[: read.length])
                clone = pool.cloning
                yield from buf.tensor_views(read, clone)
                current = None
                pool.release(buf)
        finally:
            for fut, _, buf in inflight:
                if fut is not None:
                    fut.cancel()
            if executor is not None:
                executor.shutdown(wait=True)
            for _, _, buf in inflight:
                if buf is not None:
                    pool.abandon(buf)
            if current is not None:
                pool.release(current)

    def _count(self, read) -> None:
        self.stats["reads"][self.mode] += 1
        self.stats["bytes"][self.mode] += read.length
