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


def _pages(start, end, page):
    return set(range(start // page, (end - 1) // page + 1)) if end > start else set()


def test_plan_chunks_on_page_boundaries():
    f = make_safetensors([(f"t{i}", "U8", (90,), bytes([i]) * 90) for i in range(8)] + [("z", "U8", (0,), b"")])
    n = header_length(f[:8])
    h = parse_header(f[8 : 8 + n])
    chunks = plan_chunks(h.tensors, 250, 64)
    # Every chunk but the last ends on a page boundary, and no two chunks share a page.
    assert all(c.end % 64 == 0 for c in chunks[:-1])
    assert all(c.length <= 250 for c in chunks)
    seen = set()
    for c in chunks:
        pages = _pages(c.start, c.end, 64)
        assert not pages & seen
        seen |= pages
    # Each tensor is either whole in one chunk or covered exactly once by its parts.
    covered = {t.name: [] for t in h.tensors}
    for c in chunks:
        for t in c.tensors:
            assert c.start <= t.start and t.end <= c.end
            covered[t.name].append((0, t.nbytes))
        for p in c.parts:
            assert c.start <= p.tensor.start + p.offset and p.tensor.start + p.offset + p.length <= c.end
            covered[p.tensor.name].append((p.offset, p.offset + p.length))
    for t in h.tensors:
        spans = sorted(covered[t.name])
        assert spans[0][0] == 0 and spans[-1][1] == t.nbytes, t.name
        assert all(a[1] == b[0] for a, b in zip(spans, spans[1:])), t.name
    assert any(c.parts for c in chunks)
    assert [p.last for c in chunks for p in c.parts].count(True) == len({p.tensor.name for c in chunks for p in c.parts})


def test_plan_chunks_splits_across_several_chunks():
    # With staging of just under two pages, each chunk after the first holds
    # one page, so a tensor that starts mid-chunk spans three chunks.
    f = make_safetensors([("a", "U8", (100,), b"a" * 100), ("b", "U8", (127,), b"b" * 127)])
    n = header_length(f[:8])
    h = parse_header(f[8 : 8 + n])
    chunks = plan_chunks(h.tensors, 127, 64)
    parts = [p for c in chunks for p in c.parts if p.tensor.name == "b"]
    assert len(parts) >= 3
    assert [(p.offset, p.length) for p in parts][0][0] == 0
    assert all(a.offset + a.length == b.offset for a, b in zip(parts, parts[1:]))
    assert sum(p.length for p in parts) == 127
    assert [p.last for p in parts] == [False] * (len(parts) - 1) + [True]
    assert all(c.length <= 127 for c in chunks)


def test_plan_chunks_ignores_pages_larger_than_staging():
    f = make_safetensors([(f"t{i}", "U8", (100,), bytes([i]) * 100) for i in range(5)])
    n = header_length(f[:8])
    h = parse_header(f[8 : 8 + n])
    assert [[t.name for t in c.tensors] for c in plan_chunks(h.tensors, 250, 1024)] == [
        [t.name for t in c.tensors] for c in plan_chunks(h.tensors, 250)
    ]
    assert not any(c.parts for c in plan_chunks(h.tensors, 250, 1024))


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
