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

Live allocation and I/O authority is worker-local. `Segments` and `Slab` use
`Rc`-owned state and are neither `Send` nor `Sync`
(see `Segments` in `alloc/src/segments.rs` and `Slab` in `alloc/src/slab.rs`).
Buffers, leases, freeze guards, and the reclamation clock have the same restriction
(see `AlignedBuffer` in `alloc/src/lib.rs` and `SegmentLease`, `FreezeGuard`, and
`SegmentClock` in `alloc/src/segments.rs`).
Value types such as `Alignment` and `SegmentId`, and startup inputs such as
`DevicePlacement`, are `Send + Sync` (see their declarations in `alloc/src/lib.rs`,
`alloc/src/segments.rs`, and `alloc/src/slab.rs`, respectively). Each worker owns its
storage ranges, segment table, and buffer pool; live allocation and I/O authority
cannot move to another worker.

## Buffers

`AlignedBuffer` is a heap allocation from `alloc_zeroed` with the alignment that
the file needs (see `Alignment::allocate` in `alloc/src/lib.rs`). Lengths are
padded to the least common multiple of the offset and length units, so the next
record also starts aligned.
A single buffer is at most 1 GiB (see `Alignment::extent` and
`Alignment::MAX_TRANSFER_LENGTH` in `alloc/src/lib.rs`).

Each buffer holds a caller-supplied `Charge`, so the caller can account for
memory against its own budget. The slab keeps at most one idle buffer.
After checking charge coverage, allocation reuses it only for an exact length
match; otherwise it frees the idle buffer and allocates new storage
(see `Slab::allocate` in `alloc/src/slab.rs`). On drop, a buffer fills the idle slot
only if the pool still exists and the slot is empty and can be mutably borrowed; otherwise
its storage is freed (see the `Drop` implementations for `AlignedBuffer` and
`Allocation` in `alloc/src/lib.rs`).
The retained size depends on return order, not necessarily the last size used.
This is not a general size-class pool.

Each buffer tracks whether it is still all zeros. Any mutable access, including
handing it to the kernel for a read, marks it dirty. On drop, a dirty buffer is
wiped in full, including padding, with `explicit_bzero` (or `zeroize` where that
is not available) before it is pooled or freed (see `Allocation::as_mut_slice`,
`Allocation::wipe`, and the `IoBuffer` implementation for `AlignedBuffer` in
`alloc/src/lib.rs`).
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
  with an old generation fails with `Stale`. This rejects stale mappings, not
  stale bytes read through a lease for the current generation.

Append reserves space without writing or initializing disk bytes. Neither a
`SegmentLease` nor a successful read proves that bytes were initialized in the
current generation. Recycled storage may still hold bytes from an earlier
generation or a different cache. Before exposing bytes as a valid record, the
caller must check record integrity, authentication, and cache identity (see
`SegmentLease` and `Segments::append` in `alloc/src/segments.rs`, and `Slab::read`
in `alloc/src/slab.rs`).

`freeze`, `snapshot`, and `restore` support restart. Restore checks the whole
image before it changes anything, requires that no leases are live, and seals
any segment that was open before the restart.

## Reclaim

`SegmentClock::reclaim` in `alloc/src/segments.rs` is a bounded clock
(second-chance) sweep over sealed segments. The caller calls `mark_read` when it
serves a read from a segment. The sweep clears that mark once before it picks the
segment. Each call has limits on the number of segments it visits and the number
of index entries it removes.
For a nonzero reserve, `reclaim` and `reclaim_scored` succeed only when the reserve
(capped at the slot count) is met and no evictions remain. They intentionally
return `Busy` while evictions remain, even if enough slots are already free.
Callers should use `Segments::free_count` to check capacity and keep retrying
bounded reclamation later to drain pending evictions. A zero reserve is a no-op,
not a drain request.

Evicting a segment happens in this order:

1. Ask the caller `can_evict`. The caller says no while an unpublished write
   still needs the segment. This check happens only before eviction starts.
2. Mark the segment `Evicting`. New leases are refused.
3. Call `remove_bounded` to drop the caller's index entries, a few per call.
4. When the index is empty and all leases are gone, bump the generation and
   mark the segment `Free`.

The key rule is: remove the index entries first, then wait for in-flight I/O,
then reuse. `reclaim_scored` visits at most `min(slot count, max_visits, 64)`
slots, including skipped slots. It ranks eligible candidates in that sample by a
caller-provided score instead of recent reads. The limit is on visited slots,
not eligible candidates. `reclaim_index` drops index entries one at a time until a
caller check (for example, an index size limit) passes. It does not free segments
and does not ask `can_evict`.

## Storage and I/O

`Slab::new` describes one cache file. Opening is blocking and is meant to run at
startup (see `Slab::open_configured` and `Slab::open_file` in `alloc/src/slab.rs`).
For this file-backed mode, it:

- Walks the path without following symlinks or `..`.
- Requires a regular file owned by the current user, mode 0600, one hard link.
- Takes an exclusive non-blocking `flock` and enables `O_DIRECT`.
- Reads the required alignment from `statx(STATX_DIOALIGN)`.
- Sizes an empty file sparsely to capacity. A file with the wrong size is
  rejected, not truncated.

See `Slab::open_file`, `Slab::validate_layout`, `open_private_file`,
`validate_file`, and `probe_fd` in `alloc/src/slab.rs` for these checks.

`Slab::from_devices` instead owns caller-opened file or block-device placements,
one per logical segment, without creating, sizing, or locking them. Files must
be read/write with `O_DIRECT` and without `O_APPEND`. The constructor checks
geometry, offset alignment, regular-file length, and block-device capacity from
`BLKGETSIZE64`. Overlap checks only compare the same inode or device identity
within the slab; they cannot detect whole-disk, partition, or device-mapper
aliases. The caller must guarantee disjoint physical storage across aliases and
slabs, keep exclusive ownership, supply suitable alignment, and keep file flags
and sizes unchanged while in use (see `Slab::from_devices` in `alloc/src/slab.rs`).

For device placements, `open_configured` duplicates the owned files into
worker-local descriptors and releases the original placement references
(see `Slab::open_file` in `alloc/src/slab.rs`). In both modes, it then binds the
slab to one segment table. I/O is refused until binding succeeds, and a slab cannot be
rebound to a different table (see `Slab::bind` and `Slab::submission` in
`alloc/src/slab.rs`).

`read` and `write` check the extent, alignment, and lease, then pass the buffer
and lease to the reactor (see `Slab::submission`, `Submission::read`, and
`Submission::write` in `alloc/src/slab.rs`). The reactor holds both
until the kernel reports completion, even if the caller drops the future or
cancels. So the memory and the segment both stay reserved until the kernel is
done with them. Short reads and writes are returned as errors.

Any lease, including one from `Segments::lease`, allows reads and writes within
its captured used prefix, not just the latest append. It does not grant exclusive
record ownership. The trusted caller must write only reserved extents it owns,
never overwrite published or readable records, and publish a mapping only after
the write succeeds (see `Slab::write` in `alloc/src/slab.rs`).

`fence_writes` waits until no write is in flight. It is a count, not a snapshot,
and it does not flush to disk.

## Simulation

For file-backed slabs, the `simulation` feature routes open, lock, stat, and sizing to the
`uring-runtime` simulated filesystem. Buffers, leases, and segment rules do not
change, so the same workflow tests run in both modes.

## Testing

- Unit tests cover padding math, wipe and reuse rules, lease and generation
  checks, reclaim limits, the eviction veto, restore, and file security checks.
- `alloc/tests/workflows.rs` covers restart, short I/O, dropped and canceled
  reads and writes, startup failures, and confirms that reuse does not erase
  disk bytes.
- CI checks and tests the production and simulation builds as separate
  commands, so feature unification cannot hide one from the other. The
  production run sets `PAGE_ALLOC_REQUIRE_REAL_IO=1`, so a host without
  io_uring or direct I/O fails instead of skipping. Miri runs only on
  `uring-runtime`, not on this crate.
