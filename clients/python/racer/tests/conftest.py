# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0
import os
import sys

import pytest

sys.path.insert(0, os.path.dirname(__file__))

from fakeserver import FakeRacer  # noqa: E402

import racer  # noqa: E402
import racer._wire  # noqa: E402

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

    def __init__(self, size, exportable=True):
        self.size = size
        self.device = 0
        self.fd = os.memfd_create("fake-gpu")
        os.ftruncate(self.fd, size)
        self.copies = 0
        if not exportable:
            self.export_fd = self._no_export

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
    def write(fd, offset, data):
        os.pwrite(fd, data, offset)

    return write
