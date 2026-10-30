# Racer page allocator (`page-alloc`)

## Summary

`page-alloc` is the storage crate under the Racer dataplane. It lives in
`cmd/racer/alloc/`. It gives one worker thread:

1. Memory buffers aligned for direct I/O, with caller-selected wiping rules.
2. A slab: a sparse cache file, or caller-opened files or devices, split into
   fixed-size segments, with async direct read, write, and sync through the
   `uring-runtime` reactor.
3. The extent volume (`alloc/src/volume/`), which stores pages in the slab.
   See [racer-extent-volume.md](racer-extent-volume.md).
4. Small retention filters (`SecondSight`, `Heat` in `alloc/src/retention.rs`)
   that the dataplane uses to decide what to keep.

The buffer and slab layers store bytes only. They do not know about page keys,
record headers, encryption, or checksums.

An older segment table, reclaim clock, and in-memory page index used to live
here. The extent volume replaced them in the dataplane. The segment table and
reclaim clock (`alloc/src/segments.rs`, `alloc/src/index.rs`) remain only
behind the `simulation` feature, as the payload-free disk model of
`racer-sim`.

## Threading model

Disk I/O authority is worker-local. `Slab` and `AlignedBuffer` use `Rc`-owned
state and are neither `Send` nor `Sync`. Each worker owns its slab and buffer
pool.

`WorkerBufferPool` is a separate, mutex-protected pool. Its `DetachedBuffer`
can move to a crypto thread for filling. Freezing produces a `FrozenBuffer`
with immutable bytes; it can be shared across threads when its charge allows
it. Dropping either buffer returns storage to its original pool, if that pool
still exists. Freezing an `AlignedBuffer` instead detaches it from the
worker-local slab pool and rejects retained `Rc` guards. This does not transfer
disk I/O authority.

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

## Slab storage and I/O

`Slab::new` describes one cache file. Opening is blocking and runs at startup.
In file mode it:

- Walks the path without following symlinks or `..`.
- Requires a regular file owned by the current user, mode 0600, one hard link.
- Takes an exclusive non-blocking `flock` and enables `O_DIRECT`.
- Reads the alignment from `statx(STATX_DIOALIGN)`.
- Sizes an empty file sparsely to capacity and fsyncs it and its parent
  directories. A file with the wrong size is rejected, not truncated.

`Slab::from_devices` instead takes one caller-opened placement per segment,
without creating, sizing, or locking files. Files must be read/write,
`O_DIRECT`, and not `O_APPEND`. Overlap checks only compare the same inode or
device within one slab. The caller must keep physical ranges disjoint.

`Slab::open_volume(slot_count)` opens the backing and binds the slab for volume
use. `slot_count` must be at most the physical segment count and
`volume::MAX_SLOTS`. Until it succeeds, I/O returns `Unavailable`.

`read_at` and `write_at` take an absolute slab offset, a buffer, and a guard.
They check alignment and that the transfer stays inside one bound segment,
then pass the buffer and guard to the reactor. The reactor holds both until the
kernel reports completion, even if the caller drops the future. Short
transfers are errors. `fence_writes` waits until no write is in flight; it
does not flush. The distinct backing descriptors are found once at open.
`sync` fsyncs all of them in parallel. `sync_dirty` fsyncs, in parallel, only
the descriptors that completed a write since their last successful sync, and
marks them dirty again if the fsync fails. `sync_at` fsyncs the one descriptor
behind an offset.

`write_vectored_at` writes an exclusive aligned header followed by a shared,
immutable body in one positioned `writev`, with the same offset checks. Both
addresses and lengths must meet direct-I/O alignment. The reactor holds both
buffers, the guard, and the write fence until completion, including after
cancellation. A short write is an error. Racer uses this, through
`Volume::write_vectored`, to write an aligned ciphertext body without copying
it into the header buffer.

## Simulation

With the `simulation` feature, file open, lock, stat, and sizing go through the
`uring-runtime` simulated filesystem, so the same tests run in both modes.

## Testing

- Unit tests in `alloc/src/lib.rs` and `alloc/src/slab.rs` cover padding math,
  pool bounds, wipe and reuse rules, file security checks, and device placement
  checks.
- Shared-pool tests cover cross-thread return, charge coverage, idle limits,
  stale non-secret bytes, zero padding, and secure wiping. Demand-controller
  tests cover growth, decay, and pressure constraints.
- `alloc/tests/workflows.rs` covers short I/O, dropped reads and writes,
  vectored body lifetimes, startup failures, and real io_uring I/O on files and
  placements.
- `alloc/tests/volume*.rs` and `alloc/src/volume/` tests cover the volume.
- CI runs the production and simulation builds as separate commands. The
  production run sets `PAGE_ALLOC_REQUIRE_REAL_IO=1`, so a host without
  io_uring or direct I/O fails instead of skipping.
