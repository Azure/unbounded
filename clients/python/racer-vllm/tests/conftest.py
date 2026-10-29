# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0
import importlib.util
import json
import math
import os
import struct
import sys
import types

import pytest

HERE = os.path.dirname(__file__)
sys.path.insert(0, os.path.join(HERE, "..", "..", "racer", "tests"))

# Reuse the racer client's fake server fixtures without a module name clash.
_spec = importlib.util.spec_from_file_location(
    "racer_test_fixtures", os.path.join(HERE, "..", "..", "racer", "tests", "conftest.py")
)
_fixtures = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(_fixtures)
server = _fixtures.server
small_pages = _fixtures.small_pages


def make_safetensors(tensors, metadata=None, pad=0):
    """tensors: list of (name, dtype, shape, bytes). Returns file bytes."""
    header = {}
    data = b""
    for name, dtype, shape, raw in tensors:
        header[name] = {"dtype": dtype, "shape": list(shape), "data_offsets": [len(data), len(data) + len(raw)]}
        data += raw
    if metadata:
        header["__metadata__"] = metadata
    h = json.dumps(header).encode() + b" " * pad
    return struct.pack("<Q", len(h)) + h + data


class FakeTensor:
    """Just enough of a torch tensor: bytes in a shared bytearray, a dtype and a shape."""

    def __init__(self, buf, off=0, n=None, dtype="uint8", shape=None):
        self.buf = buf
        self.off = off
        self.n = len(buf) - off if n is None else n
        self.dtype = dtype
        self.shape = (self.n,) if shape is None else tuple(shape)
        self.device = types.SimpleNamespace(index=0)

    def bytes(self):
        return bytes(self.buf[self.off : self.off + self.n])

    def __getitem__(self, s):
        return FakeTensor(self.buf, self.off + s.start, s.stop - s.start)

    def clone(self):
        return FakeTensor(bytearray(self.bytes()), 0, self.n, self.dtype, self.shape)

    def view(self, arg):
        if isinstance(arg, str):
            return FakeTensor(self.buf, self.off, self.n, arg, self.shape)
        return FakeTensor(self.buf, self.off, self.n, self.dtype, arg)

    def copy_(self, src):
        if tuple(src.shape) != self.shape:
            raise RuntimeError("shape mismatch")
        self.buf[self.off : self.off + self.n] = src.bytes()

    @property
    def data(self):
        return self

    def narrow(self, dim, start, size):  # 1-D uint8 only
        return FakeTensor(self.buf, self.off + start, size, self.dtype)

    def dim(self):
        return len(self.shape)

    def is_contiguous(self):
        return True

    def numel(self):
        return self.n


class FakeTorch(types.ModuleType):
    """A torch module with fake tensors and CUDA events that record their use."""

    def __init__(self):
        super().__init__("torch")
        for name in ("bool", "uint8", "int8", "int16", "uint16", "int32", "uint32", "int64", "uint64"):
            setattr(self, name, name)
        for name in ("float16", "bfloat16", "float32", "float64", "float8_e4m3fn", "float8_e5m2", "float8_e8m0fnu"):
            setattr(self, name, name)
        self.events = []
        events = self.events

        class Event:
            def __init__(self):
                self.recorded = self.synced = False
                events.append(self)

            def record(self, stream=None):
                self.recorded = True

            def synchronize(self):
                self.synced = True

        class Device:
            def __init__(self, dev):
                pass

            def __enter__(self):
                return self

            def __exit__(self, *a):
                return False

        self.cuda = types.SimpleNamespace(
            current_device=lambda: 0, device=Device, Event=Event, current_stream=lambda dev=None: None
        )

    def empty(self, size, dtype="uint8", device=None):
        if isinstance(size, int):
            return FakeTensor(bytearray(size), 0, size, dtype)
        return FakeTensor(bytearray(), 0, 0, dtype, size if math.prod(size) == 0 else (0,))

    def frombuffer(self, mv, dtype="uint8"):
        return FakeTensor(bytearray(mv))


@pytest.fixture
def torch(monkeypatch):
    t = FakeTorch()
    monkeypatch.setitem(sys.modules, "torch", t)
    return t
