# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0
"""Read planning: turn a file's kept tensors into ordered byte-range reads.

Rules:

* Kept tensors that are adjacent, or separated by less than a whole Racer
  page of skipped bytes, merge into one run.
* A run never exceeds ``staging_bytes``. A tensor is never split across two
  reads: when it does not fit the remaining run, the run closes and a new one
  starts with that tensor.
* A tensor larger than ``staging_bytes`` gets a dedicated read with
  ``owns_buffer=True``.
* A read edge moves out to a page boundary when that boundary lies in
  padding (bytes of no other read and no kept tensor) and the read still fits
  its buffer, so neighboring reads ask Racer for whole pages.
"""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import Callable, Iterable

from .manifest import Tensor

PAGE_SIZE = 16 * 1024 * 1024

Keep = Callable[[str, str, tuple], bool]


@dataclass(frozen=True)
class PlannedTensor:
    name: str
    dtype: str
    shape: tuple
    rva: int  # offset within the read
    nbytes: int


@dataclass
class Read:
    file: str
    offset: int
    length: int
    owns_buffer: bool
    tensors: list = field(default_factory=list)

    @property
    def end(self) -> int:
        return self.offset + self.length


def _down(x: int, page: int) -> int:
    return x - x % page


def _up(x: int, page: int) -> int:
    return _down(x + page - 1, page)


def keep_all(name: str, dtype: str, shape: tuple) -> bool:
    return True


def plan(
    file: str,
    tensors: Iterable[Tensor],
    keep: Keep | None,
    staging_bytes: int,
    page_size: int = PAGE_SIZE,
    file_size: int | None = None,
) -> list:
    """Return the ordered Reads for one file. See the module docstring."""
    if staging_bytes <= 0:
        raise ValueError("staging_bytes must be positive")
    if page_size <= 0:
        raise ValueError("page_size must be positive")
    keep = keep or keep_all
    kept = sorted((t for t in tensors if keep(t.name, t.dtype, tuple(t.shape))), key=lambda t: (t.start, t.end))
    prev_end = 0
    for t in kept:
        if t.start < prev_end:
            raise ValueError(f"overlapping tensor {t.name!r} in {file}")
        if file_size is not None and t.end > file_size:
            raise ValueError(f"tensor {t.name!r} is past the end of {file}")
        prev_end = max(prev_end, t.end)

    groups: list = []  # [start, end, tensors, owned]
    cur = None
    for t in kept:
        if t.nbytes > staging_bytes:
            if cur is not None:
                groups.append(cur)
                cur = None
            groups.append([t.start, t.end, [t], True])
            continue
        if cur is not None:
            # Merge over a gap only when no whole skipped page would be read.
            gap_ok = _up(cur[1], page_size) >= _down(t.start, page_size)
            if not gap_ok or t.end - cur[0] > staging_bytes:
                groups.append(cur)
                cur = None
        if cur is None:
            cur = [t.start, t.end, [t], False]
        else:
            cur[1] = max(cur[1], t.end)
            cur[2].append(t)
    if cur is not None:
        groups.append(cur)

    reads = []
    lower = 0
    for i, (start, end, ts, owned) in enumerate(groups):
        limit = (end - start) if owned else staging_bytes
        upper = groups[i + 1][0] if i + 1 < len(groups) else file_size
        s = _down(start, page_size)
        if s < start and s >= lower and end - s <= limit:
            start = s
        if upper is not None:
            e = _up(end, page_size)
            if end < e <= upper and e - start <= limit:
                end = e
        lower = end
        r = Read(file, start, end - start, owned)
        r.tensors = [PlannedTensor(t.name, t.dtype, tuple(t.shape), t.start - start, t.nbytes) for t in ts]
        reads.append(r)
    return reads
