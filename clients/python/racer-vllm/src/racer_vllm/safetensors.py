# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0
"""Torch-free safetensors header parsing and read planning.

Format: an 8-byte little-endian header length N, N bytes of JSON, then the
tensor data. Each JSON entry has dtype, shape and data_offsets [begin, end)
relative to the end of the header. "__metadata__" holds string metadata.
"""

from __future__ import annotations

import json
import math
import struct
from dataclasses import dataclass, field

MAX_HEADER_BYTES = 100 * 1024 * 1024

# Bytes per element for each safetensors dtype.
DTYPE_SIZES = {
    "BOOL": 1,
    "U8": 1,
    "I8": 1,
    "F8_E4M3": 1,
    "F8_E5M2": 1,
    "F8_E8M0": 1,
    "I16": 2,
    "U16": 2,
    "F16": 2,
    "BF16": 2,
    "I32": 4,
    "U32": 4,
    "F32": 4,
    "I64": 8,
    "U64": 8,
    "F64": 8,
}


class SafetensorsError(ValueError):
    pass


@dataclass(frozen=True)
class TensorInfo:
    name: str
    dtype: str
    shape: tuple
    start: int  # absolute file offset
    end: int

    @property
    def nbytes(self) -> int:
        return self.end - self.start

    @property
    def itemsize(self) -> int:
        return DTYPE_SIZES[self.dtype]


@dataclass
class Header:
    data_start: int
    tensors: list
    metadata: dict = field(default_factory=dict)


def header_length(prefix: bytes) -> int:
    if len(prefix) != 8:
        raise SafetensorsError("short safetensors prefix")
    (n,) = struct.unpack("<Q", prefix)
    if n < 2 or n > MAX_HEADER_BYTES:
        raise SafetensorsError(f"bad safetensors header length {n}")
    return n


def parse_header(raw: bytes, file_size: int | None = None) -> Header:
    """Parse header JSON (without the 8-byte prefix) into sorted TensorInfo."""
    try:
        doc = json.loads(raw.decode("utf-8").rstrip(" "))
    except (UnicodeDecodeError, ValueError) as exc:
        raise SafetensorsError("bad safetensors header JSON") from exc
    if not isinstance(doc, dict):
        raise SafetensorsError("safetensors header is not an object")
    data_start = 8 + len(raw)
    metadata = doc.pop("__metadata__", None) or {}
    tensors = []
    for name, spec in doc.items():
        try:
            dtype = spec["dtype"]
            shape = tuple(int(d) for d in spec["shape"])
            begin, end = (int(x) for x in spec["data_offsets"])
        except (KeyError, TypeError, ValueError) as exc:
            raise SafetensorsError(f"bad entry for {name!r}") from exc
        if dtype not in DTYPE_SIZES:
            raise SafetensorsError(f"unsupported dtype {dtype} for {name!r}")
        if any(d < 0 for d in shape) or begin < 0 or end < begin:
            raise SafetensorsError(f"bad shape or offsets for {name!r}")
        if end - begin != math.prod(shape) * DTYPE_SIZES[dtype]:
            raise SafetensorsError(f"size mismatch for {name!r}")
        tensors.append(TensorInfo(name, dtype, shape, data_start + begin, data_start + end))
    tensors.sort(key=lambda t: (t.start, t.end))
    prev = data_start
    for t in tensors:
        if t.start < prev:
            raise SafetensorsError(f"overlapping tensor {t.name!r}")
        prev = t.end
    if file_size is not None and prev > file_size:
        raise SafetensorsError("tensor data past end of file")
    return Header(data_start, tensors, metadata)


@dataclass
class Chunk:
    """A contiguous file range read with one request."""

    start: int
    end: int
    tensors: list
    dedicated: bool = False  # larger than the staging buffer

    @property
    def length(self) -> int:
        return self.end - self.start


def plan_chunks(tensors: list, staging_bytes: int) -> list:
    """Group sorted tensors into ranges of at most staging_bytes.

    A tensor bigger than staging_bytes gets its own dedicated chunk. Zero-size
    tensors join the current chunk and need no bytes.
    """
    chunks: list = []
    cur: Chunk | None = None
    for t in tensors:
        if t.nbytes > staging_bytes:
            if cur is not None:
                chunks.append(cur)
                cur = None
            chunks.append(Chunk(t.start, t.end, [t], dedicated=True))
            continue
        if cur is not None and t.end - cur.start <= staging_bytes:
            cur.tensors.append(t)
            cur.end = max(cur.end, t.end)
            continue
        if cur is not None:
            chunks.append(cur)
        cur = Chunk(t.start, t.end, [t])
    if cur is not None:
        chunks.append(cur)
    return chunks


def tensor_bytes(buf, chunk: Chunk, t: TensorInfo) -> memoryview:
    """Return the bytes of t inside a buffer holding chunk (torch-free)."""
    rel = t.start - chunk.start
    return memoryview(buf)[rel : rel + t.nbytes]
