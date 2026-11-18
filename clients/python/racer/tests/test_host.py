# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0
import ctypes
import gc
import os

import pytest

from racer import HostBuffer, RacerError, cuda


def memfds(name):
    """This process's open fds for memfds called name."""
    out = []
    for fd in os.listdir("/proc/self/fd"):
        try:
            if os.readlink(f"/proc/self/fd/{fd}").startswith(f"/memfd:{name} "):
                out.append(int(fd))
        except OSError:
            # The fd closed between listdir and readlink.
            pass
    return out


def mapped(name):
    with open("/proc/self/maps") as f:
        return f"/memfd:{name} " in f.read()


def test_unclosed_buffer_is_released_when_collected():
    b = HostBuffer(4096, "racer-test-gc")
    assert memfds("racer-test-gc")
    del b
    gc.collect()
    assert memfds("racer-test-gc") == []
    assert not mapped("racer-test-gc")


def test_retained_view_outlives_collection():
    # torch.frombuffer keeps a reference to the view without a buffer export.
    b = HostBuffer(4096, "racer-test-keep")
    fd = b.fileno()
    view = b.view
    view[:3] = b"abc"
    del b
    gc.collect()
    assert fd not in memfds("racer-test-keep")
    assert bytes(view[:3]) == b"abc" and mapped("racer-test-keep")
    del view
    gc.collect()
    assert not mapped("racer-test-keep")


def test_torch_tensor_outlives_collection():
    torch = pytest.importorskip("torch")
    b = HostBuffer(4096, "racer-test-torch")
    fd = b.fileno()
    t = torch.frombuffer(b.view, dtype=torch.uint8)
    t[:3] = torch.tensor([1, 2, 3], dtype=torch.uint8)
    del b
    gc.collect()
    assert fd not in memfds("racer-test-torch")
    assert t[:3].tolist() == [1, 2, 3] and mapped("racer-test-torch")
    del t
    gc.collect()
    assert not mapped("racer-test-torch")


def test_close_twice():
    b = HostBuffer(4096, "racer-test-close")
    b.close()
    b.close()
    assert memfds("racer-test-close") == []
    assert b._map.closed
    with pytest.raises(RacerError, match="closed"):
        b.fileno()
    with HostBuffer(4096, "racer-test-close") as b:
        pass
    b.close()
    assert memfds("racer-test-close") == []


@pytest.mark.parametrize("collect", [False, True])
def test_exported_view_does_not_keep_the_fd(collect):
    # torch.frombuffer holds an export of the view, as this ctypes array does.
    b = HostBuffer(4096, "racer-test-export")
    fd = b.fileno()
    b.view[:3] = b"abc"
    export = (ctypes.c_char * 4096).from_buffer(b.view)
    if collect:
        del b
        gc.collect()
    else:
        b.close()
        del b
    assert fd not in memfds("racer-test-export")
    # The mapping (and the fd mmap keeps for it) lives as long as the export.
    assert export.raw[:3] == b"abc" and mapped("racer-test-export")
    del export
    gc.collect()
    assert memfds("racer-test-export") == []
    assert not mapped("racer-test-export")


@pytest.mark.parametrize("keep", [False, True])
def test_unclosed_pinned_buffer_is_unpinned(monkeypatch, keep):
    unpinned = []

    class Registration:
        def close(self):
            # Unpinning must come before unmapping.
            assert mapped("racer-test-pin")
            unpinned.append(True)

    monkeypatch.setattr(cuda, "host_register", lambda view, device: Registration())
    b = HostBuffer(4096, "racer-test-pin")
    assert b.pin() and b.pinned and b.pin()
    view = b.view if keep else None
    del b
    gc.collect()
    if keep:
        # A tensor over the view may still copy from the pinned mapping.
        assert unpinned == [] and mapped("racer-test-pin")
        del view
        gc.collect()
    assert unpinned == [True]
    assert memfds("racer-test-pin") == []
    assert not mapped("racer-test-pin")
