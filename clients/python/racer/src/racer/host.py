# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0
"""Host memory backed by a memfd, the destination for host-read."""

from __future__ import annotations

import fcntl
import mmap
import os
import weakref

from .errors import RacerError


# fcntl constants from <linux/fcntl.h>; the fcntl module has them only on
# Python 3.8+ builds against recent headers.
_F_ADD_SEALS = getattr(fcntl, "F_ADD_SEALS", 1033)
_F_SEAL_SHRINK = getattr(fcntl, "F_SEAL_SHRINK", 0x0002)


def _seal_shrink(fd: int) -> None:
    """Seal ``fd`` against shrinking. Best effort: an unsealed memfd still works."""
    try:
        fcntl.fcntl(fd, _F_ADD_SEALS, _F_SEAL_SHRINK)
    except OSError:
        # Older kernels or filesystems may refuse the seal. The dataplane then
        # falls back to positional writes, so a failed seal is not an error.
        pass


class HostBuffer:
    """A memfd of a fixed size, mapped read-write into this process.

    The memfd is sealed against shrinking (``F_SEAL_SHRINK``), so the
    dataplane may map it and decrypt pages straight into it instead of using
    positional writes. Writes are not sealed. ``view`` is a writable memoryview over the mapping, so the
    buffer also works as a stream-mode destination. Call ``pin()`` to register
    the mapping with CUDA for faster host-to-device copies. A buffer dropped
    without ``close()`` is released when it is garbage collected.
    """

    def __init__(self, size: int, name: str = "racer-host") -> None:
        if size <= 0:
            raise RacerError("size must be positive")
        self.size = size
        self._fd = os.memfd_create(name, os.MFD_CLOEXEC | os.MFD_ALLOW_SEALING)
        try:
            os.ftruncate(self._fd, size)
            _seal_shrink(self._fd)
            self._map = mmap.mmap(self._fd, size, mmap.MAP_SHARED, mmap.PROT_READ | mmap.PROT_WRITE)
        except BaseException:
            os.close(self._fd)
            raise
        self.view = memoryview(self._map)
        self._pins = []
        self._finalizer = weakref.finalize(self, _release, self._fd, self._pins)

    def fileno(self) -> int:
        if self._fd < 0:
            raise RacerError("buffer closed")
        return self._fd

    @property
    def pinned(self) -> bool:
        return bool(self._pins)

    def pin(self, device: int = 0) -> bool:
        """Register the mapping with CUDA in ``device``'s context. Returns False when CUDA is missing."""
        if self._pins:
            return True
        from . import cuda

        try:
            self._pins.append(cuda.host_register(self.view, device))
        except cuda.CudaUnavailable:
            return False
        return True

    def close(self) -> None:
        """Release the buffer. Safe to call more than once."""
        if not self._finalizer.alive:
            return
        self._fd = -1
        self._finalizer()
        try:
            self.view.release()
            self._map.close()
        except BufferError:
            # A view of the mapping (such as a ctypes array) still holds an
            # export, so the mapping stays until it goes.
            pass

    def __enter__(self) -> "HostBuffer":
        return self

    def __exit__(self, *exc: object) -> None:
        self.close()


def _release(fd: int, pins: list) -> None:
    """Unpin and close a HostBuffer's memfd. Runs once, from close() or garbage collection.

    It does not unmap: torch.frombuffer keeps the memoryview without holding a
    buffer export, so after garbage collection the mapping must live until the
    last reference to the view goes.
    """
    try:
        while pins:
            pins.pop().close()
    finally:
        os.close(fd)
