# Racer page allocator (`page-alloc`)

## Summary

`page-alloc` is the storage crate under the Racer dataplane. It lives in
`cmd/racer-dataplane/alloc/`. It gives one worker thread:

1. Memory buffers aligned for direct I/O and wiped before reuse.
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
here. The extent volume replaced them, and they were removed.

## Threading model

`Slab` and `AlignedBuffer` use `Rc`-owned state and are neither `Send` nor
`Sync`. Each worker owns its slab and buffer pool. `FrozenBuffer` is the one
exception: it is an immutable copy-free view of completed I/O that can move to
another thread (see `AlignedBuffer::freeze` in `alloc/src/lib.rs`).

## Buffers

`Alignment::allocate` returns zeroed memory with the alignment the file needs.
Lengths are padded to the least common multiple of the offset and length
units. A single transfer is at most 1 GiB (`Alignment::MAX_TRANSFER_LENGTH`).
Allocations of 2 MiB or more use an anonymous `mmap` with a huge-page hint.

Each buffer holds a caller-supplied `Charge`, so the caller can account for
memory against its own budget. `Slab::allocate` keeps a small idle pool
(`BufferPool` in `alloc/src/lib.rs`): at most 8 buffers and 128 MiB in total.
Reuse matches by 64 KiB size class (`buffer_capacity`), and requires that the
new charge covers the full capacity. On drop, a buffer goes back to the pool
only if the pool still exists, is not borrowed, and has room; otherwise it is
freed. `Slab::reclaim_idle` frees all idle buffers.

Each buffer tracks whether it is still all zeros. Any mutable access, including
handing it to the kernel for a read, marks it dirty. A dirty buffer is wiped in
full with `explicit_bzero` (or `zeroize`) before it is pooled or freed. A
charge can opt out of the wipe (`Charge::requires_wipe`) for public data; a
mapped buffer is then cleared with `MADV_DONTNEED` instead.

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
does not flush. `sync` fsyncs every backing descriptor.

## Simulation

With the `simulation` feature, file open, lock, stat, and sizing go through the
`uring-runtime` simulated filesystem, so the same tests run in both modes.

## Testing

- Unit tests in `alloc/src/lib.rs` and `alloc/src/slab.rs` cover padding math,
  pool bounds, wipe rules, file security checks, and device placement checks.
- `alloc/tests/workflows.rs` covers short I/O, dropped reads and writes,
  startup failures, and real io_uring I/O on files and placements.
- `alloc/tests/volume*.rs` and `alloc/src/volume/` tests cover the volume.
- CI runs the production and simulation builds as separate commands. The
  production run sets `PAGE_ALLOC_REQUIRE_REAL_IO=1`, so a host without
  io_uring or direct I/O fails instead of skipping.
