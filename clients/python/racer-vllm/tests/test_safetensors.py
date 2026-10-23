# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0
import struct

import pytest
from conftest import make_safetensors

from racer_vllm.safetensors import (
    SafetensorsError,
    header_length,
    parse_header,
    plan_chunks,
    tensor_bytes,
)


def sample():
    return make_safetensors(
        [
            ("a", "F32", (2, 3), struct.pack("<6f", *range(6))),
            ("b", "BF16", (4,), b"\1\2" * 4),
            ("c", "U8", (0,), b""),
            ("d", "I64", (), struct.pack("<q", -7)),
        ],
        metadata={"format": "pt"},
        pad=3,
    )


def test_parse_and_slice():
    f = sample()
    n = header_length(f[:8])
    h = parse_header(f[8 : 8 + n], len(f))
    assert h.metadata == {"format": "pt"}
    assert h.data_start == 8 + n
    names = [t.name for t in h.tensors]
    assert names == ["a", "b", "c", "d"]
    a = h.tensors[0]
    assert (a.dtype, a.shape, a.nbytes, a.itemsize) == ("F32", (2, 3), 24, 4)
    chunks = plan_chunks(h.tensors, 1 << 20)
    assert len(chunks) == 1
    c = chunks[0]
    buf = f[c.start : c.end]
    assert struct.unpack("<6f", tensor_bytes(buf, c, a)) == tuple(float(x) for x in range(6))
    assert bytes(tensor_bytes(buf, c, h.tensors[3])) == struct.pack("<q", -7)
    assert bytes(tensor_bytes(buf, c, h.tensors[2])) == b""


def test_plan_chunks_split_and_dedicated():
    f = make_safetensors([(f"t{i}", "U8", (100,), bytes([i]) * 100) for i in range(5)] + [("big", "U8", (500,), b"x" * 500)])
    n = header_length(f[:8])
    h = parse_header(f[8 : 8 + n])
    chunks = plan_chunks(h.tensors, 250)
    assert [[t.name for t in c.tensors] for c in chunks] == [["t0", "t1"], ["t2", "t3"], ["t4"], ["big"]]
    assert [c.dedicated for c in chunks] == [False, False, False, True]
    assert all(c.length <= 250 for c in chunks if not c.dedicated)


@pytest.mark.parametrize(
    "header",
    [
        b"[]",
        b"{not json",
        b'{"a":{"dtype":"F4","shape":[1],"data_offsets":[0,1]}}',
        b'{"a":{"dtype":"F32","shape":[2],"data_offsets":[0,4]}}',
        b'{"a":{"dtype":"U8","shape":[2],"data_offsets":[0,2]},"b":{"dtype":"U8","shape":[2],"data_offsets":[1,3]}}',
        b'{"a":{"dtype":"U8","shape":[-1],"data_offsets":[0,0]}}',
        b'{"a":{"dtype":"U8"}}',
        b'{"a":{"dtype":"U8","shape":[1.9],"data_offsets":[0.9,1.9]}}',
        b'{"a":{"dtype":"U8","shape":[1],"data_offsets":[0.0,1.0]}}',
        b'{"a":{"dtype":"U8","shape":[-0.5],"data_offsets":[0,0]}}',
        b'{"a":{"dtype":"U8","shape":["1"],"data_offsets":[0,1]}}',
        b'{"a":{"dtype":"U8","shape":[1],"data_offsets":["0","1"]}}',
        b'{"a":{"dtype":"U8","shape":[true],"data_offsets":[0,1]}}',
        b'{"a":{"dtype":"U8","shape":[1],"data_offsets":[false,true]}}',
        b'{"a":{"dtype":"U8","shape":"1","data_offsets":[0,1]}}',
        b'{"a":{"dtype":"U8","shape":[1],"data_offsets":[0,1,2]}}',
        b'{"a":{"dtype":"U8","shape":[1],"data_offsets":{"0":0}}}',
        b'{"a":[]}',
    ],
)
def test_bad_headers(header):
    with pytest.raises(SafetensorsError):
        parse_header(header)


def test_bad_length():
    with pytest.raises(SafetensorsError):
        header_length(struct.pack("<Q", 1 << 40))
    with pytest.raises(SafetensorsError):
        header_length(b"abc")


def test_past_end():
    f = sample()
    n = header_length(f[:8])
    with pytest.raises(SafetensorsError):
        parse_header(f[8 : 8 + n], len(f) - 1)
