# page-alloc

`page-alloc` is the dataplane's worker-local page allocator for generic bytes:
direct-I/O geometry, zeroized aligned buffers, lease-fenced segment allocation,
bounded reclamation, and one locked sparse slab file. The name does not imply
an application page format or a fixed OS page size. This is a scoped internal
crate, not a universal storage library.

The allocator is independent of the application's data model. It owns no cache
keys, record format, encryption, integrity checks, versions, admission classes,
or persistence protocol. The caller supplies accounting guards, a runtime
`Reactor`, scope/cancellation policy, and index-removal callbacks. Types using
`Rc` and interior mutability are worker-local, not a cross-thread allocator.

## Startup and binding

Construct `Slab::<C>::new(full_file_path, capacity_bytes, segment_bytes,
max_record_bytes)`, create `Segments::new(segment_bytes)`, and call
`slab.open_configured(&segments)` during startup. Opening is **blocking**:
directory traversal, file creation, locking, metadata probing, and sizing run
synchronously. Do not put this on a latency-sensitive worker path. There is no
async `open()` API.

`open_configured` discovers the filesystem's direct-I/O alignment, configures an
unconfigured table to the full slab geometry, and permanently binds the slab to
that table's identity. An already configured table may expose fewer segments,
but must have exactly the same slab capacity, segment size, and alignment.
Binding the same table again is allowed; rebinding to a different table is not.
`slab.geometry()` reports the full physical geometry, not a partial table's
count. `SegmentGeometry` validates dimensions; `Segments::configure` bounds the
retained table to `MAX_SEGMENTS` (1,000,000), not the physical slab. Its
`matches_segments` checks dimensions only;
alignment compatibility is a separate check.

`open_now()` opens for geometry discovery and buffer allocation without binding.
Read/write return `Unavailable` until `configure_segments(&segments)` succeeds.
Prefer `open_configured` for normal use. Opening and binding are separate steps:
a binding error can leave the file open and locked, but does not authorize I/O;
fix the table or drop the slab.

`max_record_bytes` validates at open that the padded maximum fits in a segment.
It is not an allocation quota or a per-submission record-size limit. Capacity
must be a positive integral number of segments, fit in a signed file offset,
and agree with any nonempty existing file's size. A size mismatch is rejected,
not truncated away.

### Filesystem and security contract

The Linux open path resolves directories relative to already-open directory
descriptors. It rejects `..` and follows neither intermediate nor final
symlinks. Missing directories are created with mode 0700 and files with mode
0600 (subject to the process umask). An accepted file must be regular, owned by
the effective user, have exactly mode 0600 without special permission bits, and
have one hard link. Existing permissions are not silently repaired. Opening a
FIFO cannot block startup before its type is rejected. The file is exclusively
locked with nonblocking `flock`, opened close-on-exec, and enabled for
`O_DIRECT`; geometry comes from `statx(STATX_DIOALIGN)`, not an assumed page size.

Parent directories must still be trusted against hostile rename/unlink. Safe
descriptor traversal does not make an attacker-writable namespace private, and
the file lock does not protect against noncooperating processes modifying it.

An empty file is extended to capacity with sparse sizing. **Capacity is a
logical address bound, not a reservation of physical disk space.** Later writes
can fail with ENOSPC even after successful startup. Recycling does not truncate,
zero, or hole-punch disk bytes: it must remain a lease-safe metadata operation,
not an asynchronous disk mutation that could race reuse. Physical blocks can
therefore remain allocated after logical eviction. Buffer zeroization is not
secure erasure of the slab file.

## Alignment, buffers, and accounting

`Alignment::new(memory, offset, length)` accepts arbitrary positive offset and
length units; only memory alignment must be a supported power of two.
`alignment.extent(offset, logical_length)` checks the offset and rounds length
up to `lcm(offset_unit, length_unit)`, so consecutive appends remain aligned.
For example, offset unit 768 and length unit 512 require padding to 1536, not
merely to the larger unit. Arithmetic is checked. Empty or overflowing extents
are rejected. `Extent::new` checks the range and transfer cap, but does not by
itself establish alignment or authorize access to a segment.

`Alignment::MAX_TRANSFER_LENGTH` is a conservative **1 GiB** single-transfer
limit, enforced before backing allocation. It fits the runtime's `u32` length
and stays below Linux `MAX_RW_COUNT` on supported base-page sizes without
querying the host. Padding must also fit this cap. Larger logical records must
be split by the caller; this crate does not do chunked record I/O.

Buffers have an initialized, stable, exclusively owned address. They start
zeroed, expose slices of their entire padded length, and cannot be resized.
`Alignment::allocate` requires a nonzero, length-aligned size and a primary
`Charge` whose `covers(bytes)` accepts the allocation. `()` opts out of
accounting; production admission policy belongs in the caller's guard. This
contract trusts the guard to account truthfully. Extra `Rc<C>` guards attached
with `buffer.retain` keep additional caller accounting alive with the buffer.

`Slab::allocate` adds a **single-slot, exact-size** idle pool:

- Drop zeroizes bytes before pooling or deallocation. The primary charge stays
  with idle memory; retained extra guards are released rather than pooled.
- An exact-size reuse replaces the old primary charge with the newly admitted
  one. An undercharge is rejected before consuming the idle slot.
- A size mismatch releases the old idle allocation and its charge, even if
  allocating the replacement fails. There are no size classes or unbounded pool.
- If the slot is occupied or the slab has been dropped, returned storage is
  freed. Outstanding buffers do not keep the slab's pool alive.
- `idle_bytes` reports retained idle memory; `reclaim_idle` releases that memory
  and returns its size. Neither reaches buffers still owned by in-flight I/O.

## I/O and completion ownership

For a write, round the logical length, pass the padded length to
`segments.append`, allocate a matching buffer, copy the record into its zeroed
bytes, and submit `slab.write(&reactor, extent, buffer, lease, &scope)`. Publish
the application mapping only after handling the result. Append reserves bytes;
it does not roll back occupancy when a write fails.

For a stored read, validate `(segment_id, generation, extent)` with
`segments.validate`, obtain `segments.lease(id, generation)`, allocate a matching
buffer, and call `slab.read`. A bound slab rejects foreign-table leases and
extents outside the lease's captured used prefix, even if more bytes have since
been appended. A lease is not an exclusive record capability: it authorizes the
segment's used prefix at acquisition, not only its most recent append. Bound
submission also checks buffer alignment, exact transfer length, segment
boundaries, and capacity. Both reads and writes reject short completions.

Accepted runtime I/O retains the buffer, its charges, and the segment lease
through the runtime completion fence, even when the waiting future is dropped.
Writes also retain their in-flight counter. Dropping a future is not permission
to recycle the segment or reuse kernel-accessible memory. An unpolled operation
has not submitted I/O. Continue driving the reactor to complete or cancel
accepted work and release these resources.

`fence_writes()` waits for accepted write completion ownership to be released;
it does not wait for reads or prevent new writes. Stop admitting writes first
if quiescence is required, and drive the reactor concurrently with the fence.
**This is a completion fence, not a durability barrier:** neither it nor normal
write completion calls `fsync`/`fdatasync` or promises crash persistence.

## Segment state, freezing, and recovery

The normal state machine is `Free -> Open -> Sealed -> Evicting -> Free`:

- Append chooses the current open segment if it fits, otherwise the lowest free
  slot. Rotation seals an unused tail; filling a segment seals it. A valid
  request that cannot fit seals the current tail even if no free slot remains,
  enabling reclamation and retry. It reserves no bytes on failure. Malformed
  requests and lease-count overflow leave the table unchanged.
- `begin_evict` accepts Sealed or already Evicting segments. New leases and
  ordinary generation validation reject Evicting slots, but existing leases
  remain usable within their captured range.
- `recycle` requires Evicting state and zero outstanding leases. It increments
  generation without wrapping and frees metadata for reuse. Old generations
  cannot acquire new leases. Generation exhaustion returns `Unavailable`.

`snapshot()` returns an owned `Vec<SegmentSnapshot>`, not a `Result`, and is
available during a freeze. `freeze()` returns a must-use `FreezeGuard`. Retain
the guard for the whole protected interval; dropping it thaws the table.
Only one guard can exist. It blocks configuration, append, eviction, recycling,
and restore, but not reads, new read leases, snapshots, or already accepted I/O.
It can safely outlive the table. It is not a data-I/O fence.

`validate_restore(&images)` and `restore(images)` require an unfrozen table
with no outstanding leases. Restore validates the whole image before changing
state: slot count and ordered IDs, nonzero generations, aligned bounded used
lengths, empty Free slots, nonempty used slots, and at most one partial Open
slot. Restore seals the Open tail instead of appending into it, rebuilds the
free set, and advances an epoch that resets clock cursor/recent-read state.
Invalid images do not partially publish; busy tables return `Busy` unchanged.

A caller-owned checkpoint flow should stop mutation admission, retain a freeze
guard, drive accepted writes through their completion fence, and capture the
allocator image together with compatible application metadata. Drop the guard
before restore, and drain **all** leases (including reads and canceled I/O).
Application admission must stay coordinated around that thaw/restore interval.
Restoring allocator metadata alone is not a transaction or a record validator.
For cache recovery, validate record identity, lengths, integrity/authentication,
and any application versions before serving bytes. Missing, stale, torn, or
invalid records must become cache misses and be refetched from the authoritative
source; the allocator snapshot and completion fence do not prove durability.

## Bounded reclamation

`SegmentClock` shares a cursor and second-chance recent-read set across index
and segment reclamation. `mark_read` records live Open/Sealed segments and
ignores late reads of other valid states. It rejects invalid IDs.

`reclaim_index(entries, max_visits, ready)` forgets at most one mapping per
candidate until caller admission succeeds, without changing bytes or segment
generations. `reclaim(entries, free_reserve, max_visits, max_entries)` considers
only Sealed/Evicting candidates, removes mappings within the total entry budget,
and recycles only empty segments whose leases have drained. Both visit at most
two rotations, further capped by `max_visits`; insufficient progress returns
`Busy` rather than spinning. Busy leases leave Evicting slots for a later sweep.
A zero reserve is a no-op; a zero entry budget may recycle empty candidates but
does not start evicting populated ones. There is no compaction.

`SegmentEntries` is application-owned. Removal must compare current mappings
before forgetting them, report no more removals than the supplied budget, and
keep `is_empty` accurate. Version ownership and side effects remain the caller's
responsibility. Over-reporting returns `InvalidConfiguration`, but cannot undo
callback side effects. Freeze does not block caller index mutation, including
`reclaim_index`; coordinate that separately when checkpointing.

## Errors and tests

`Error` is non-exhaustive. Treat categories distinctly:

| Error | Meaning |
| --- | --- |
| `Stale` | Wrong table, outdated generation, or state no longer available for a new access. |
| `Corrupt` | Malformed extent/image, invalid ID, or range outside authorized used bytes. |
| `InvalidConfiguration` | Invalid caller geometry, binding, buffer/charge, or callback contract. |
| `Busy` | Frozen/leased state, admission/allocation exhaustion, or bounded sweep unable to progress. |
| `Unavailable` | Not opened/configured, lock held elsewhere, or exhausted generation/restore epoch. |
| `Unsupported` | Required direct-I/O geometry/capability cannot be provided. |
| `Io` | Short completion or storage failure without OS detail. |
| `SystemIo { operation, errno }` | Synchronous OS failure with diagnostic operation and optional errno. |

`read`/`write` return the scope's error type, preserving its conversions from
allocator and runtime errors. In particular, asynchronous runtime I/O errors
are not automatically enriched into allocator `SystemIo` errors.

From `cmd/racer-dataplane`, run both feature configurations and the targeted
workflow suite with bounded commands:

```sh
timeout --signal=TERM --kill-after=10s 300s cargo test -p page-alloc --no-default-features
timeout --signal=TERM --kill-after=10s 300s cargo test -p page-alloc --all-features
timeout --signal=TERM --kill-after=10s 300s cargo test -p page-alloc --features simulation --test workflows
```

The opt-in `simulation` feature forwards `uring-runtime/simulation` and enables
deterministic fault tests. Hidden descriptor-replacement hooks are fallible:
they require an open slab and no live writes, and do not revalidate replacement
geometry. They are test tools, not a production reopen protocol. Simulation is
not proof of real filesystem security or kernel support.

Real-file tests exercise direct I/O, sparse sizing, file validation, and locking.
The real io_uring roundtrip smoke test probes kernel support and emits explicit
skip reasons for known capability/permission denials; unexpected failures must
still fail. Use `-- --nocapture` to see those reasons. A capability skip is not
evidence that the real I/O path passed. Run on a suitable Linux filesystem and
kernel to validate that path; do not replace kernel coverage with simulation.
Set `PAGE_ALLOC_REQUIRE_REAL_IO=1` to turn capability skips into failures. CI
requires this mode for an isolated default-feature allocator run as well as
the all-feature workspace suite.

Miri can check the pure allocation/geometry/state-machine paths when a compatible
nightly toolchain is installed, for example:

```sh
timeout --signal=TERM --kill-after=10s 300s cargo +nightly miri test -p page-alloc --lib buffer::tests
timeout --signal=TERM --kill-after=10s 300s cargo +nightly miri test -p page-alloc --lib segments::tests
```

Do not interpret Miri as a direct-I/O or io_uring test: real syscalls and kernel
completion ordering are outside its coverage, and running the entire real-file
suite under Miri is not supported. The simulation workflows complement pure
tests with restart, accounting, bound I/O, and freeze/completion lifetime checks.
