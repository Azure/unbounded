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
    mapping = b._map
    assert memfds("racer-test-gc")
    del b
    gc.collect()
    assert memfds("racer-test-gc") == []
    assert mapping.closed
    assert not mapped("racer-test-gc")


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


def test_unclosed_pinned_buffer_is_unpinned(monkeypatch):
    unpinned = []

    class Registration:
        def close(self):
            unpinned.append(True)

    monkeypatch.setattr(cuda, "host_register", lambda view, device: Registration())
    b = HostBuffer(4096, "racer-test-pin")
    assert b.pin() and b.pinned and b.pin()
    del b
    gc.collect()
    assert unpinned == [True]
    assert memfds("racer-test-pin") == []
