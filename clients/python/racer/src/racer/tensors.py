# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0
"""torch helpers. torch is imported only when these functions run.

Lifetime rule: a tensor made by ``as_tensor`` holds a reference to the
GpuAllocation it views, so the allocation lives at least as long as the
tensor (and any view of it). Closing the allocation by hand while tensors
still use it is a bug.
"""

from __future__ import annotations


def _torch():
    import torch  # noqa: PLC0415 - lazy on purpose

    return torch


def as_tensor(allocation, device: int | None = None):
    """Wrap a GpuAllocation as a flat uint8 CUDA tensor without copying.

    This uses ``torch.as_tensor`` on the object's ``__cuda_array_interface__``.
    torch keeps a reference to the exporting object for as long as the
    tensor's storage lives, which gives the lifetime rule above.
    """
    torch = _torch()
    dev = torch.device("cuda", allocation.device if device is None else device)
    return torch.as_tensor(allocation, device=dev)


class TensorTarget:
    """A GPU destination backed by an existing torch CUDA uint8 tensor.

    It cannot be exported, so reads use host or stream mode and copy with
    ``Tensor.copy_``.
    """

    def __init__(self, tensor) -> None:
        torch = _torch()
        if tensor.dtype != torch.uint8 or tensor.dim() != 1 or not tensor.is_contiguous():
            raise ValueError("tensor must be a contiguous 1-D uint8 tensor")
        self.tensor = tensor
        self.size = tensor.numel()

    def export_fd(self) -> int:
        raise NotImplementedError("torch memory cannot be exported")

    def copy_from_host(self, offset: int, data) -> None:
        torch = _torch()
        mv = memoryview(data).cast("B")
        if not mv:
            return
        src = torch.frombuffer(mv, dtype=torch.uint8)
        self.tensor[offset : offset + len(mv)].copy_(src)
