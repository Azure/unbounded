# page-alloc

Worker-local direct-I/O geometry, zeroized aligned buffers, lease-fenced segment
allocation, and one locked sparse slab file. It contains no cache identity,
admission classes, encryption, record format, or eviction policy.

Create `Slab::<Charge>::new(full_file_path, capacity, segment_bytes,
max_record_bytes)` and call `open_now()` (or await `open()`). Configure
`Segments::new(segment_bytes)` with the returned alignment. Round logical sizes
with `Alignment::extent`, append that padded length to obtain `(lease, extent)`,
allocate a buffer with a caller-owned charge, then call `slab.read/write` with
the caller's `Reactor`, extent, buffer, lease, and scope.

`Charge::covers` validates the primary charge; `()` disables accounting. Extra
`Rc<Charge>` guards can be attached with `buffer.retain`. An idle buffer retains
its primary charge and is zeroed before reuse; `reclaim_idle` releases it.
The caller must pair each slab with the correct segment allocator and perform
generation validation with `Segments::validate` before issuing stored reads.
`max_record_bytes` validates file geometry at open, not an allocation quota.

Accepted I/O retains buffers, segment leases, and write counters through runtime
completion, even if its waiting future is dropped. Drive the reactor while
awaiting `fence_writes`; dropping a future does not fence kernel access.

The opt-in `simulation` feature forwards `uring-runtime/simulation` and exposes
descriptor replacement hooks for fault tests. Run focused tests with
`cargo test -p page-alloc --all-features` from the enclosing workspace.
