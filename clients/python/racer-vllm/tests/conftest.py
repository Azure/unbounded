# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0
import json
import os
import struct
import sys

HERE = os.path.dirname(__file__)
sys.path.insert(0, os.path.join(HERE, "..", "..", "racer", "tests"))

import importlib.util  # noqa: E402

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
