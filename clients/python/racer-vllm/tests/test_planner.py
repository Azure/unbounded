# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0
"""Read planning."""

import pytest

from racer_vllm.manifest import Tensor
from racer_vllm.planner import plan

PAGE = 1000


def tensors(*spans):
    """Tensors t0, t1, ... at the given (start, end) offsets."""
    return [Tensor(f"t{i}", "U8", (e - s,), s, e) for i, (s, e) in enumerate(spans)]


def shape(reads):
    return [(r.offset, r.end, [t.name for t in r.tensors], r.owns_buffer) for r in reads]


def check_cover(reads, ts, keep=None):
    """Every kept tensor lies in exactly one read, wholly, at its rva."""
    by_name = {t.name: t for t in ts}
    seen = []
    for r in reads:
        for pt in r.tensors:
            t = by_name[pt.name]
            assert r.offset + pt.rva == t.start
            assert pt.rva + pt.nbytes <= r.length
            seen.append(pt.name)
    want = [t.name for t in ts if keep is None or keep(t.name, t.dtype, t.shape)]
    assert sorted(seen) == sorted(want)


def test_adjacent_tensors_merge_and_runs_respect_staging():
    ts = tensors((100, 400), (400, 700), (700, 1000), (1000, 1300))
    reads = plan("f", ts, None, staging_bytes=700, page_size=PAGE)
    assert shape(reads) == [(0, 700, ["t0", "t1"], False), (700, 1300, ["t2", "t3"], False)]
    check_cover(reads, ts)


def test_tensor_never_splits_across_reads():
    ts = tensors((0, 300), (300, 900), (900, 1000))
    reads = plan("f", ts, None, staging_bytes=800, page_size=PAGE)
    # t1 does not fit after t0, so the run closes and t1 starts a new one.
    assert [[t.name for t in r.tensors] for r in reads] == [["t0"], ["t1", "t2"]]
    assert all(r.length <= 800 for r in reads)
    check_cover(reads, ts)


def test_large_tensor_gets_a_dedicated_read():
    ts = tensors((0, 100), (100, 5100), (5100, 5200))
    reads = plan("f", ts, None, staging_bytes=1000, page_size=PAGE)
    assert shape(reads) == [(0, 100, ["t0"], False), (100, 5100, ["t1"], True), (5100, 5200, ["t2"], False)]
    check_cover(reads, ts)


def test_gap_below_a_page_merges_and_a_whole_skipped_page_splits():
    ts = tensors((0, 100), (300, 400), (5000, 5100))
    keep = lambda n, d, s: True  # noqa: E731
    reads = plan("f", ts, keep, staging_bytes=10_000, page_size=PAGE, file_size=6000)
    assert [[t.name for t in r.tensors] for r in reads] == [["t0", "t1"], ["t2"]]
    check_cover(reads, ts)


def test_skipped_tensors_are_not_read():
    ts = tensors((0, 100), (100, 3000), (3000, 3100))
    keep = lambda n, d, s: n != "t1"  # noqa: E731
    reads = plan("f", ts, keep, staging_bytes=10_000, page_size=PAGE)
    assert [[t.name for t in r.tensors] for r in reads] == [["t0"], ["t2"]]
    assert all(not (r.offset < 3000 and r.end > 1000) for r in reads)
    check_cover(reads, ts, keep)


def test_edges_extend_to_pages_only_into_padding():
    ts = tensors((1100, 1500), (2600, 2900))
    reads = plan("f", ts, None, staging_bytes=1000, page_size=PAGE, file_size=3500)
    # Both reads grow to whole pages because the bytes they add are padding.
    assert shape(reads) == [(1000, 2000, ["t0"], False), (2000, 3000, ["t1"], False)]
    # Past the end of the file an edge stays put.
    reads = plan("f", ts, None, staging_bytes=1000, page_size=PAGE, file_size=2950)
    assert reads[1].end == 2900
    check_cover(reads, ts)


def test_edges_do_not_grow_past_staging():
    ts = tensors((1100, 1950),)
    reads = plan("f", ts, None, staging_bytes=860, page_size=PAGE, file_size=4000)
    assert (reads[0].offset, reads[0].end) == (1100, 1950)


def test_empty_and_errors():
    assert plan("f", [], None, 10) == []
    with pytest.raises(ValueError, match="staging_bytes"):
        plan("f", [], None, 0)
    with pytest.raises(ValueError, match="page_size"):
        plan("f", [], None, 10, page_size=0)
    with pytest.raises(ValueError, match="overlapping"):
        plan("f", tensors((0, 10), (5, 15)), None, 100)
    with pytest.raises(ValueError, match="past the end"):
        plan("f", tensors((0, 10)), None, 100, file_size=5)
