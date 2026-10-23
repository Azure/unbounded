# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0
"""Stream safetensors weights from Racer into GPU staging buffers.

The pipeline is torch-free; a Backend supplies memory and tensor views.
"""

from __future__ import annotations

import collections
import logging
from concurrent.futures import ThreadPoolExecutor
from typing import Iterator

import racer

from .model import ModelURI, weight_files
from .safetensors import Chunk, TensorInfo, header_length, parse_header, plan_chunks

log = logging.getLogger(__name__)

DEFAULT_STAGING_BYTES = 256 * 1024 * 1024
DEFAULT_DEPTH = 2


class Backend:
    """Memory and tensor views for the pipeline."""

    def new_staging(self, size: int):
        raise NotImplementedError

    def new_dedicated(self, size: int):
        raise NotImplementedError

    def tensor(self, target, rel: int, info: TensorInfo, staged: bool):
        raise NotImplementedError

    def new_joined(self, info: TensorInfo):
        """Memory for a tensor that is split across chunks."""
        raise NotImplementedError

    def copy_part(self, joined, offset: int, target, rel: int, length: int) -> None:
        """Copy length bytes at rel in target to offset in joined."""
        raise NotImplementedError

    def joined_tensor(self, joined, info: TensorInfo):
        """Return the tensor once every part has been copied into joined."""
        raise NotImplementedError

    def done(self, target) -> None:
        """The consumer is finished with every tensor from target."""

    def wait(self, target) -> None:
        """Block until work queued by the consumer on target has finished."""


def _file_chunks(client: racer.Client, uri: ModelURI, staging_bytes: int, page_bytes: int):
    for name in weight_files(client, uri):
        req = uri.request(name)
        meta = client.stat(req)
        prefix = client.read(req, 0, 8, if_match=meta.etag)
        n = header_length(prefix)
        header = parse_header(client.read(req, 8, n, if_match=meta.etag), meta.size)
        log.debug("racer: %s has %d tensors", name, len(header.tensors))
        for chunk in plan_chunks(header.tensors, staging_bytes, page_bytes):
            yield req, meta.etag, chunk


def iter_weights(
    client: racer.Client,
    uri: ModelURI,
    backend: Backend,
    staging_bytes: int = DEFAULT_STAGING_BYTES,
    depth: int = DEFAULT_DEPTH,
    page_bytes: int = racer.PAGE_SIZE,
) -> Iterator[tuple]:
    """Yield (name, tensor) for every tensor in the model.

    Up to ``depth`` chunks are read ahead, each into its own staging buffer,
    so reads overlap with the consumer. A staging buffer is reused only after
    the consumer has moved past all of its tensors and backend.wait() says
    queued GPU work that reads it has finished. Every staging and dedicated
    target is fenced with done() and wait() before the pipeline drops its
    reference, including on early close or error, so external allocations
    are never released while queued GPU work may still read them. Dedicated
    targets are fenced and dropped as soon as the consumer resumes past
    them, so oversized tensors do not accumulate in GPU memory.

    Chunks end on Racer page boundaries (page_bytes) where they can, so
    neighboring chunks do not both read the boundary page. A tensor that
    crosses a boundary is copied out of each chunk into memory from
    backend.new_joined() and yielded with the chunk that holds its last byte.
    """
    if depth < 1:
        raise ValueError("depth must be at least 1")
    free = [backend.new_staging(staging_bytes) for _ in range(depth)]
    chunks = _file_chunks(client, uri, staging_bytes, page_bytes)
    joined: dict = {}
    inflight: collections.deque = collections.deque()
    pool = ThreadPoolExecutor(max_workers=depth, thread_name_prefix="racer-read")

    def read(req, etag, chunk: Chunk, target):
        backend.wait(target)
        if chunk.length:
            client.read_into_gpu(req, target, 0, offset=chunk.start, length=chunk.length, if_match=etag)
        return target

    def fill() -> bool:
        while len(inflight) < depth:
            nxt = next(chunks, None)
            if nxt is None:
                return bool(inflight)
            req, etag, chunk = nxt
            if chunk.dedicated:
                target = backend.new_dedicated(chunk.length)
            else:
                target = free.pop()
            inflight.append((pool.submit(read, req, etag, chunk, target), chunk))
        return True

    active = None
    try:
        while fill():
            fut, chunk = inflight.popleft()
            target = fut.result()
            active = target
            for t in chunk.tensors:
                yield t.name, backend.tensor(target, t.start - chunk.start, t, not chunk.dedicated)
            for part in chunk.parts:
                t = part.tensor
                if part.offset == 0:
                    joined[t.name] = backend.new_joined(t)
                rel = t.start + part.offset - chunk.start
                backend.copy_part(joined[t.name], part.offset, target, rel, part.length)
                if part.last:
                    yield t.name, backend.joined_tensor(joined.pop(t.name), t)
            active = None
            backend.done(target)
            if chunk.dedicated:
                # Fence and drop now so oversized targets do not accumulate;
                # tensors the consumer kept hold their own storage reference.
                backend.wait(target)
                del target
            else:
                free.append(target)
    finally:
        for fut, _ in inflight:
            fut.cancel()
        pool.shutdown(wait=True)
        if active is not None:
            backend.done(active)
            backend.wait(active)
        # Reads have stopped; fence consumer GPU work before release. Targets
        # still in flight were fenced by read() and the consumer never saw them.
        for target in free:
            backend.wait(target)


class MemoryBackend(Backend):
    """Host memory backend: plain buffers, bytes views. Used for tests and CPU."""

    class Target:
        def __init__(self, size: int) -> None:
            self.size = size
            self.buf = bytearray(size)

        def copy_from_host(self, offset: int, data) -> None:
            mv = memoryview(data).cast("B")
            self.buf[offset : offset + len(mv)] = mv

    def new_staging(self, size: int):
        return self.Target(size)

    def new_dedicated(self, size: int):
        return self.Target(size)

    def tensor(self, target, rel, info, staged):
        return bytes(target.buf[rel : rel + info.nbytes])

    def new_joined(self, info):
        return bytearray(info.nbytes)

    def copy_part(self, joined, offset, target, rel, length):
        joined[offset : offset + length] = target.buf[rel : rel + length]

    def joined_tensor(self, joined, info):
        return bytes(joined)


TORCH_DTYPES = {
    "BOOL": "bool",
    "U8": "uint8",
    "I8": "int8",
    "F8_E4M3": "float8_e4m3fn",
    "F8_E5M2": "float8_e5m2",
    "F8_E8M0": "float8_e8m0fnu",
    "I16": "int16",
    "U16": "uint16",
    "F16": "float16",
    "BF16": "bfloat16",
    "I32": "int32",
    "U32": "uint32",
    "F32": "float32",
    "I64": "int64",
    "U64": "uint64",
    "F64": "float64",
}


class TorchBackend(Backend):
    """CUDA backend.

    Staging buffers are exportable cuMemCreate allocations when the CUDA
    driver allows it (so gpu-read works), else plain torch tensors (host or
    stream mode plus a copy). Tensors from staging buffers are cloned unless
    clone=False; then they are views that are valid only until the consumer
    asks for the next chunk's tensors.
    """

    def __init__(self, device: int | None = None, clone: bool = True, use_vmm: bool = True) -> None:
        import torch

        self.torch = torch
        self.device = torch.cuda.current_device() if device is None else device
        self.clone = clone
        self.use_vmm = use_vmm
        self._flat: dict = {}
        self._events: dict = {}

    def _new(self, size: int):
        import racer.cuda as cuda
        import racer.tensors as tensors

        if self.use_vmm:
            try:
                alloc = cuda.GpuAllocation(size, self.device)
                self._flat[id(alloc)] = tensors.as_tensor(alloc)
                return alloc
            except racer.RacerError as exc:
                log.info("racer: exportable GPU memory unavailable (%s); using torch memory", exc)
                self.use_vmm = False
        t = self.torch.empty(size, dtype=self.torch.uint8, device=f"cuda:{self.device}")
        target = tensors.TensorTarget(t)
        self._flat[id(target)] = t
        return target

    def new_staging(self, size):
        return self._new(size)

    def new_dedicated(self, size):
        return self._new(size)

    def tensor(self, target, rel, info, staged):
        torch = self.torch
        dtype = getattr(torch, TORCH_DTYPES[info.dtype])
        if info.nbytes == 0:
            return torch.empty(info.shape, dtype=dtype, device=f"cuda:{self.device}")
        flat = self._flat[id(target)]
        if not staged:
            # Dedicated memory: the tensor keeps it alive; drop our reference.
            self._flat.pop(id(target), None)
        raw = flat[rel : rel + info.nbytes]
        if rel % info.itemsize:
            raw = raw.clone()  # realign; the clone owns its memory
        elif staged and self.clone:
            raw = raw.clone()
        return raw.view(dtype).view(info.shape)

    def new_joined(self, info):
        return self.torch.empty(info.nbytes, dtype=self.torch.uint8, device=f"cuda:{self.device}")

    def copy_part(self, joined, offset, target, rel, length):
        # Queued before done(target), so the staging buffer is not reused
        # until the copy has finished.
        joined[offset : offset + length].copy_(self._flat[id(target)][rel : rel + length])

    def joined_tensor(self, joined, info):
        dtype = getattr(self.torch, TORCH_DTYPES[info.dtype])
        return joined.view(dtype).view(info.shape)

    def done(self, target):
        # Record on the target device's stream; the calling thread's current
        # device may differ, and its stream would not cover our copies.
        cuda = self.torch.cuda
        with cuda.device(self.device):
            ev = cuda.Event()
            ev.record(cuda.current_stream(self.device))
        self._events[id(target)] = ev

    def wait(self, target):
        ev = self._events.pop(id(target), None)
        if ev is not None:
            ev.synchronize()
