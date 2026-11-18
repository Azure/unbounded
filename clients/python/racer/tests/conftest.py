# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0
import gc
import os
import sys

import pytest

sys.path.insert(0, os.path.dirname(__file__))

from fakeserver import FakeRacer  # noqa: E402

import racer  # noqa: E402
import racer._wire  # noqa: E402
from racer import cuda  # noqa: E402

SMALL_PAGE = 4096


@pytest.fixture
def small_pages(monkeypatch):
    monkeypatch.setattr(racer._wire, "PAGE_SIZE", SMALL_PAGE)
    return SMALL_PAGE


@pytest.fixture
def server(tmp_path, small_pages):
    s = FakeRacer(str(tmp_path / "s"))
    racer.forget_modes()
    yield s
    s.close()
    racer.forget_modes()


class FakeGpu:
    """A GPU destination backed by a memfd, so the fake server can write it."""

    def __init__(self, size, exportable=True, uuid=None):
        self.size = size
        self.device = 0
        self.fd = os.memfd_create("fake-gpu")
        os.ftruncate(self.fd, size)
        self.copies = 0
        if not exportable:
            self.export_fd = self._no_export
        if uuid is not None:
            self.gpu_uuid = uuid if callable(uuid) else (lambda: uuid)

    def _no_export(self):
        raise NotImplementedError

    def export_fd(self):
        return os.dup(self.fd)

    def copy_from_host(self, offset, data):
        self.copies += 1
        os.pwrite(self.fd, bytes(data), offset)

    def read(self, offset, n):
        return os.pread(self.fd, n, offset)

    def close(self):
        os.close(self.fd)


@pytest.fixture
def gpu_writer():
    def write(fd, offset, data, allocation_size):
        os.pwrite(fd, data, offset)

    return write


# Real-GPU tests (marker gpu). They skip without a GPU, or without torch for
# the tensor tests; RACER_GPU_TESTS=require and RACER_GPU_TORCH=require make
# those cases fail instead, so a GPU runner cannot pass by skipping.
GPU_REQUIRED = os.environ.get("RACER_GPU_TESTS") == "require"
TORCH_REQUIRED = os.environ.get("RACER_GPU_TORCH") == "require"


def _unavailable(required, var, why):
    if required:
        pytest.fail(f"{var}=require but {why}", pytrace=False)
    pytest.skip(why)


@pytest.fixture
def gpu():
    """racer.cuda backed by libcuda and at least one GPU."""
    why = None
    try:
        cuda._load()
    except cuda.CudaUnavailable as exc:
        why = f"no usable GPU: {exc}"
    if why:
        _unavailable(GPU_REQUIRED, "RACER_GPU_TESTS", why)
    yield cuda
    # Release dropped allocations and pins while the real driver is loaded;
    # other tests swap racer.cuda._lib for a fake.
    gc.collect()


@pytest.fixture
def real_torch():
    """The torch package, CPU-only or with CUDA."""
    try:
        import torch
    except ImportError as exc:
        torch, why = None, f"torch is not importable: {exc}"
    if torch is None:
        _unavailable(TORCH_REQUIRED, "RACER_GPU_TORCH", why)
    return torch


@pytest.fixture
def cuda_torch(gpu, real_torch):
    """torch with CUDA, on a real GPU."""
    if not real_torch.cuda.is_available():
        _unavailable(TORCH_REQUIRED, "RACER_GPU_TORCH", f"torch {real_torch.__version__} has no usable CUDA")
    return real_torch
