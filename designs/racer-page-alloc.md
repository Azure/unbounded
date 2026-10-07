# Racer page allocator (`page-alloc`)

## Summary

`page-alloc` is the storage layer under the new Racer dataplane. It lives in
`cmd/racer-dataplane/alloc/`. It gives one worker thread three things:

1. Memory buffers that are aligned for direct I/O and wiped before reuse.
2. A table of fixed-size segments in a sparse cache file or caller-opened files
   or devices, with leases that stop reuse while I/O still points at a segment.
3. Async read and write of that storage through the `uring-runtime` reactor.

The crate stores bytes only. It does not know about page keys, record headers,
encryption, checksums, or versions. The caller owns the page index and
tells the crate how to remove entries when a segment is evicted. No Racer
binary uses the crate yet. The dataplane executable is still a placeholder.

## Goals and non-goals

Goals:

- Never reuse disk space or memory while the kernel may still read or write it.
- Keep every allocation and reuse decision on one worker, with no locks.
- Bound the work done by each reclaim call, so the hot path never stalls.
- Fail with an error instead of wrapping counters or truncating files.

Non-goals:

- Durability. Writes are not followed by `fsync`, and there is no log.
- Secure erasure of the file. Eviction changes metadata only. Old bytes stay on
  disk until they are overwritten.
- Compaction, record integrity, or deciding when a page is published.

## Threading model

All state is worker-local. Types use `Rc`, `Cell`, and `RefCell`, so none of them
are `Send` or `Sync` (`alloc/src/segments.rs:153-179`, `alloc/src/slab.rs:35-50`).
Each worker owns its storage ranges, segment table, and buffer pool. This removes lock
contention and makes ownership easy to reason about. The cost is that one worker
cannot hand its storage to another.

## Buffers

`AlignedBuffer` is a heap allocation from `alloc_zeroed` with the alignment that
the file needs (`alloc/src/lib.rs:415-442`). Lengths are padded to the least common
multiple of the offset and length units, so the next record also starts aligned.
A single buffer is at most 1 GiB.

Each buffer holds a caller-supplied `Charge`, so the caller can account for
memory against its own budget. The slab keeps at most one idle buffer of the
last size used (`alloc/src/slab.rs:398-415`). It is not a general size-class pool.

Each buffer tracks whether it is still all zeros. Any mutable access, including
handing it to the kernel for a read, marks it dirty. On drop, a dirty buffer is
wiped in full, including padding, with `explicit_bzero` (or `zeroize` where that
is not available) before it is pooled or freed (`alloc/src/lib.rs:483-520`).
Clean buffers skip the wipe. This keeps old page data from leaking into the
next request without paying for a wipe on every allocation.

## Segments

Storage is split into fixed-size logical segments. Segment `n` starts at
`n * segment_bytes`; device placements map it to a caller-supplied physical range.
Each segment has a state and a generation number:

```
Free -> Open -> Sealed -> Evicting -> Free (generation + 1)
```

- `append` reserves space at the end of the one open segment. When it is full,
  the segment is sealed and the lowest free segment is opened. If none is free,
  the call returns `Busy` after sealing, so reclaim can make room.
- `append` and `lease` return a `SegmentLease`. A lease records the segment ID,
  generation, and how much of the segment was in use when it was taken. While
  any lease exists, the segment cannot go back to `Free`.
- The caller stores `(segment, generation, extent)` in its own index. A lookup
  with an old generation fails with `Stale`, so a reused segment can never be
  read as if it still held the old page.

`freeze`, `snapshot`, and `restore` support restart. Restore checks the whole
image before it changes anything, requires that no leases are live, and seals
any segment that was open before the restart.

## Reclaim

`SegmentClock` is a bounded clock (second-chance) sweep over sealed segments
(`alloc/src/segments.rs:785-842`). The caller calls `mark_read` when it serves
a read from a segment. The sweep clears that mark once before it picks the
segment. Each call has limits on
the number of segments it visits and the number of index entries it removes. If it
cannot free enough space within those limits, it returns `Busy` and the caller
tries again later.

Evicting a segment happens in this order:

1. Ask the caller `can_evict`. The caller says no while an unpublished write
   still needs the segment. This check happens only before eviction starts.
2. Mark the segment `Evicting`. New leases are refused.
3. Call `remove_bounded` to drop the caller's index entries, a few per call.
4. When the index is empty and all leases are gone, bump the generation and
   mark the segment `Free`.

The key rule is: remove the index entries first, then wait for in-flight I/O,
then reuse. `reclaim_scored` is a variant that ranks a small sample by a
caller-provided score instead of recent reads. `reclaim_index` drops index entries one at a time until a caller check
(for example, an index size limit) passes. It does not free segments and does not
ask `can_evict`.

## Storage and I/O

`Slab::new` describes one cache file. Opening is blocking and is meant to run at
startup (`alloc/src/slab.rs:368-409`). For this file-backed mode, it:

- Walks the path without following symlinks or `..`.
- Requires a regular file owned by the current user, mode 0600, one hard link.
- Takes an exclusive non-blocking `flock` and enables `O_DIRECT`.
- Reads the required alignment from `statx(STATX_DIOALIGN)`.
- Sizes an empty file sparsely to capacity. A file with the wrong size is
  rejected, not truncated.

`Slab::from_devices` instead owns caller-opened file or block-device placements,
one per logical segment, without creating, sizing, or locking them. Files must
be read/write with `O_DIRECT` and without `O_APPEND`. The constructor checks
geometry, offset alignment, regular-file bounds, and overlapping ranges within
the slab (`alloc/src/slab.rs:99-176`). The caller must open real devices
exclusively, verify device capacity, supply alignment that meets every device's
requirements, and keep ranges in different slabs disjoint
(`alloc/src/slab.rs:34-42`, `alloc/src/slab.rs:93-98`).

For device placements, `open_configured` duplicates the owned files into
worker-local descriptors and releases the original placement references
(`alloc/src/slab.rs:295-325`). In both modes, it then binds the slab to one
segment table. I/O is refused until binding succeeds, and a slab cannot be
rebound to a different table (`alloc/src/slab.rs:241-276`,
`alloc/src/slab.rs:447-449`).

`read` and `write` check the extent, alignment, and lease, then pass the buffer
and lease to the reactor (`alloc/src/slab.rs:461-515`). The reactor holds both
until the kernel reports completion, even if the caller drops the future or
cancels. So the memory and the segment both stay reserved until the kernel is
done with them. Short reads and writes are returned as errors.

`fence_writes` waits until no write is in flight. It is a count, not a snapshot,
and it does not flush to disk.

## Simulation

For file-backed slabs, the `simulation` feature routes open, lock, stat, and sizing to the
`uring-runtime` simulated filesystem. Buffers, leases, and segment rules do not
change, so the same workflow tests run in both modes.

## Testing

- Unit tests cover padding math, wipe and reuse rules, lease and generation
  checks, reclaim limits, the eviction veto, restore, and file security checks.
- `alloc/tests/workflows.rs` covers restart, short I/O, dropped and cancelled
  reads and writes, startup failures, and confirms that reuse does not erase
  disk bytes.
- CI checks and tests the production and simulation builds as separate
  commands, so feature unification cannot hide one from the other. The
  production run sets `PAGE_ALLOC_REQUIRE_REAL_IO=1`, so a host without
  io_uring or direct I/O fails instead of skipping. Miri runs only on
  `uring-runtime`, not on this crate.
