# Racer page allocator (`page-alloc`)

## Summary

`page-alloc` is the storage layer under the Racer dataplane. It lives in
`cmd/racer-dataplane/alloc/`. It gives one worker thread three things:

1. Memory buffers aligned for direct I/O, with caller-selected wiping rules.
2. A table of fixed-size segments in a sparse cache file or caller-opened files
   or devices, with leases that stop reuse while I/O still points at a segment.
3. Async read and write of that storage through the `uring-runtime` reactor.

The crate stores bytes only. It does not know about page keys, record headers,
encryption, checksums, or versions. The caller owns the page index and
tells the crate how to remove entries when a segment is evicted.

## Goals and non-goals

Goals:

- Never reuse disk space or memory while the kernel may still read or write it.
- Keep disk allocation and I/O authority on one worker.
- Let immutable buffers cross threads without moving their storage.
- Bound the work done by each reclaim call, so the hot path never stalls.
- Fail with an error instead of wrapping counters or truncating files.

Non-goals:

- Durability. Writes are not followed by `fsync`, and there is no log.
- Secure erasure of the file. Eviction changes metadata only. Old bytes stay on
  disk until they are overwritten.
- Compaction, record integrity, or deciding when a page is published.

## Threading model

Disk allocation and I/O authority is worker-local. `Segments`, `Slab`,
`SegmentLease`, `FreezeGuard`, `SegmentClock`, and `AlignedBuffer` use
worker-local state and cannot move to another worker. See `alloc/src/segments.rs`,
`alloc/src/slab.rs`, and `alloc/src/lib.rs`.

`WorkerBufferPool` is a separate, mutex-protected pool. Its `DetachedBuffer`
can move to a crypto thread for filling. Freezing produces a `FrozenBuffer`
with immutable bytes; it can be shared across threads when its charge allows
it. Dropping either buffer returns storage to its original pool, if that pool
still exists. Freezing an `AlignedBuffer` instead detaches it from the
worker-local slab pool and rejects retained `Rc` guards. This does not transfer
disk allocation or I/O authority.

## Buffers

Buffers have the alignment that the file needs (see `Alignment::allocate` and
`allocate_zeroed` in `alloc/src/lib.rs`). On Linux, allocations of at least
2 MiB use anonymous mappings aligned to at least 2 MiB, with a huge-page hint.
Smaller allocations and other targets use `alloc_zeroed`. Lengths are
padded to the least common multiple of the offset and length units, so the next
record also starts aligned.
A single buffer is at most 1 GiB (see `Alignment::extent` and
`Alignment::MAX_TRANSFER_LENGTH` in `alloc/src/lib.rs`).

Each buffer holds a caller-supplied `Charge`, so the caller can account for
memory against its own budget. The charge must cover the backing capacity,
not just the requested bytes.

### Worker-local slab pool

The slab keeps a small list of idle buffers,
oldest first, grouped in 64 KiB size classes and bounded by 128 MiB. After
checking charge coverage, allocation reuses an idle buffer of the same size
class and leaves mismatches pooled (see `Slab::allocate` in
`alloc/src/slab.rs`). On drop, a buffer joins the pool only if the pool still
exists, can be mutably borrowed, and admits it; otherwise its storage is freed
(see the `Drop` implementations for `AlignedBuffer` and `Allocation` in
`alloc/src/lib.rs`).

The pool depth is not fixed. A `PoolDemand` controller (`alloc/src/demand.rs`)
holds a target depth between 0 and 8, starting at 2. Returns beyond the target
are refused. `Slab::maintain_idle` runs one observation window (1 s) at a time:
when allocations missed and returns were refused for depth, the target grows by
half the misses (at least 1); entries that stayed unused for the whole window
are halved, oldest first; after 10 miss-free windows the target drops by 1.
`Slab::constrain_idle` refuses returns and trims to the minimum depth, which
defaults to zero. The flow
recycler (`Quotas::maintain_buffers` in `flow/src/lib.rs`) uses the same
controller and also retains at most half of each class limit. Workers drive
both from `poll_services`. Idle slab buffers keep their admission charges.

### Cross-thread buffer pool

`WorkerBufferPool` groups storage into 64 KiB capacity classes. It keeps at
most eight idle buffers per class and a caller-set byte budget, 128 MiB by
default. It reuses the most recently returned buffer in a matching class.
This pool does not use `PoolDemand`; `clear` frees its idle storage. Wiping
and charge destructors run outside the mutex.

Racer uses this pool for aligned ciphertext. Its `PoolCharge` releases live
ciphertext admission before parking idle storage, so Racer sets a separate
idle budget of four maximum-page capacity classes (`src/memory.rs`). A new
checkout must bring a charge covering the full capacity. Encryption and HTTP
receive can fill this storage, and a frozen page can be sent or written to
disk without another body allocation.

### Wiping and padding

Mutable access marks an allocation dirty. If `Charge::requires_wipe` is true,
drop securely wipes the full dirty allocation, including padding, before
pooling or freeing it. Clean allocations skip the wipe. See
`Allocation::wipe` and the buffer `Drop` implementations in `alloc/src/lib.rs`.

The worker-local slab pool also clears non-secret storage before reuse, using
`MADV_DONTNEED` for mappings when possible and zero filling otherwise. The
cross-thread pool deliberately does not clear non-secret storage. Its producers
must overwrite every requested byte. `DetachedBuffer::freeze` clears only the
padding up to the direct-I/O length unit. This rule allows ciphertext reuse
without weakening plaintext wiping.

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
any segment that was open before the restart. It also checks generations: an
image with any slot generation below the live table returns `Stale`. An image
that marks a slot Free at the generation of an occupied live slot publishes it
at the next generation. Otherwise the next append would reissue the same
extents under a generation that old mappings still match. When that increment
would wrap, restore returns `Unavailable`. Restoring into a freshly configured
table keeps the image generations, so recovered mappings stay valid.

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

`write_vectored` writes an exclusive aligned header followed by a shared,
immutable body in one positioned `writev`. Both addresses and lengths must
meet direct-I/O alignment, and their lengths must sum to the extent length.
The reactor holds both buffers, the lease, and the write fence until completion,
including after cancellation. A short write is an error. Racer uses this to
write an aligned ciphertext body without copying it into the header buffer.

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
- Shared-pool tests cover cross-thread return, charge coverage, idle limits,
  stale non-secret bytes, zero padding, and secure wiping. Demand-controller
  tests cover growth, decay, and pressure constraints.
- `alloc/tests/dst.rs` is a deterministic simulation test suite. One generator
  drives appends, simulated I/O with injected faults, leases, freezes,
  snapshots, historical and edited restores, reclaim, and restart. After every
  step it checks the allocator against an independent record model. The checks
  cover aliasing, generation monotonicity, mapping fencing, atomic restore
  failure, reclaim bounds, and charge accounting. Fixed regression traces, a
  restore cross-product, and seeded sequences all run through the same runner.
- `alloc/tests/workflows.rs` covers short I/O, dropped and canceled reads and
  writes, vectored body lifetimes, startup failures, and confirms that reuse
  does not erase disk bytes.
- CI checks and tests the production and simulation builds as separate
  commands, so feature unification cannot hide one from the other. The
  production run sets `PAGE_ALLOC_REQUIRE_REAL_IO=1`, so a host without
  io_uring or direct I/O fails instead of skipping. Miri runs only on
  `uring-runtime`, not on this crate.
