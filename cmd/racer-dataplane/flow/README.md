# flow-control

Policy-driven quotas, charged payload recycling, exact-once credit windows, and
bounded Linux kernel pipes. It contains no application resource names, cache
identity, telemetry implementation, deadlines, or cancellation policy.

Implement `Class` with dense, stable indices in `0..COUNT`, then implement `Policy`
with resource limits, per-key floors, a key-record limit, release-wake and payload
coverage rules, and a rejection callback. Create `Quotas::new(policy)` and call
`reserve(Some(&key), class, amount)` for keyed fair-share admission, or pass `None`
for aggregate-only admission. Amounts must be nonzero. `reserve_completion` is for
already-admitted work during drain: it bypasses stop and fair-share checks, but
still enforces aggregate and key-record limits. `stop()` rejects ordinary new
work unless the policy allows that class after stop.

Keep the returned `Charge` alive with the resource. Dropping it releases usage;
`split` divides its amount without changing aggregate usage, and `shrink` releases
excess. The caller must retain enough charge for every live backing allocation.
Charges implement `page_alloc::Charge` using the policy's coverage rule.
`Quotas::shared()` requires a cloneable policy and provides unkeyed admission,
usage queries, and waker registration; it is thread-safe when the policy is
`Send + Sync`. Keyed admission remains local, while charges can be released from
other threads.

`Charge::buffer(length)` returns a zeroed payload buffer. Only the final exclusive
payload owner may pass its allocation to `Charge::recycle`; recycling wipes the
full allocation before deciding whether to retain it. At most two buffers of at
least 1 MiB retain their live charges. `reclaim_buffers()` releases idle buffers,
and quota pressure retries admission after applicable reclamation. `stop()` also
reclaims buffers; shared handles do not keep them alive.

Create `Window::<K>::new(slots, bytes, max_item)` for ordered item keys. Call
`reserve(key, length)`, then `issued(key)`, then `release(key, length)` with the
exact admitted length. Pending and issued items consume the same slot and byte
budgets; issuance does not return credit. Duplicate reservations, repeated
issuance, early release, and wrong-length or repeated release fail.

Create `pipe::PipePool::new(Rc<Quotas<P>>, pipe_class, waiter_class, waiter_limit)`.
`acquire()` immediately leases a nonblocking pipe charged by one pipe unit, with
capacity at most `pipe::MAX_PIPE_BYTES` (64 KiB). `acquire_wait(check, subscribe)`
adds bounded FIFO waiting with byte-charged queue entries. The check runs before
acquisition and on queued polls; subscription is lazy and returns a closure that
registers each poll's waker. Callers arrange wakeups for deadlines, cancellation,
quota stop, and charges held outside the pool; no timer or polling loop is added.

`PipeLease` exposes nonblocking read, write, and socket-splice operations. Writes
copy into kernel pipe pages, not borrowed userspace pages. Socket splice requires
a nonblocking stream socket; callers must not clear `O_NONBLOCK` concurrently.
Empty returned pipes retain their charges in the local pool; partially drained
pipes close instead of being reused. Socket-retained bytes are outside the pipe
capacity budget.

Admission and credit operations use `Error::{InvalidInput, Overloaded,
Unavailable, Io}`; pipe data operations return `std::io::Result`. The opt-in
`simulation` feature forwards `uring-runtime/simulation` and uses simulated pipe
descriptors when a simulation is active. Run focused tests from the enclosing
workspace with `timeout --signal=TERM --kill-after=10s 300s cargo test -p
flow-control --all-features`.
