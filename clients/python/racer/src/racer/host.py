# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0
"""Host memory backed by a memfd, the destination for host-read."""

from __future__ import annotations

import mmap
import os

from .errors import RacerError


class HostBuffer:
    """A memfd of a fixed size, mapped read-write into this process.

    The dataplane writes into the file with positional writes, so the memfd is
    not sealed. ``view`` is a writable memoryview over the mapping, so the
    buffer also works as a stream-mode destination. Call ``pin()`` to register
    the mapping with CUDA for faster host-to-device copies.
    """

    def __init__(self, size: int, name: str = "racer-host") -> None:
        if size <= 0:
            raise RacerError("size must be positive")
        self.size = size
        self._fd = os.memfd_create(name, os.MFD_CLOEXEC)
        try:
            os.ftruncate(self._fd, size)
            self._map = mmap.mmap(self._fd, size, mmap.MAP_SHARED, mmap.PROT_READ | mmap.PROT_WRITE)
        except BaseException:
            os.close(self._fd)
            raise
        self.view = memoryview(self._map)
        self._pinned = None

    def fileno(self) -> int:
        if self._fd < 0:
            raise RacerError("buffer closed")
        return self._fd

    @property
    def pinned(self) -> bool:
        return self._pinned is not None

    def pin(self) -> bool:
        """Register the mapping with CUDA. Returns False when CUDA is missing."""
        if self._pinned is not None:
            return True
        from . import cuda

        try:
            self._pinned = cuda.host_register(self.view)
        except cuda.CudaUnavailable:
            return False
        return True

    def close(self) -> None:
        if self._pinned is not None:
            self._pinned.close()
            self._pinned = None
        if self._fd >= 0:
            self.view.release()
            self._map.close()
            os.close(self._fd)
            self._fd = -1

    def __enter__(self) -> "HostBuffer":
        return self

    def __exit__(self, *exc: object) -> None:
        self.close()
