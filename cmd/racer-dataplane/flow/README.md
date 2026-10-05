# flow-control

Policy-driven quotas, charged payload recycling, exact-once credit windows,
bounded handoffs, circuits, adaptive admission, and Linux kernel pipes.
It contains no application resource names, cache
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

`ChargedBuffer::new(charge, length)` owns fixed initialized backing and its charge,
shrinks excess charge, and recycles on drop. Mutable access is slice-only and the
buffer implements the runtime `IoBuffer` contract. `into_parts()` transfers both
backing and charge to a final owner. The application still checks class, key,
provenance, and authenticated publication rules.

`circuit::Circuits<K>` tracks bounded failures independently of active-work limits.
Supply capacity and probe timeout at construction, explicit time for operations,
and a retry-delay callback to `failure`. `success` removes failure state. `acquire`
returns an owned exclusive half-open `Probe`; its exclusivity survives timeout,
success, and retention changes until guard drop. `try_acquire` without a guard
instead relies on the probe timeout. Error classification and jitter stay with
the caller; capacity rejection does not dictate how an operation reports failure.

`adaptive::Adaptive<K, O>` provides shared total/per-key admission with `Arc`-owned
permits. `Config` supplies limits, table capacity, backoff, recovery and retirement
intervals. Supply an `Observer` for events/gauges and a clock function (including a
simulation-aware clock when needed). Call `Permit::observe` with a classified
outcome: verified success, remote/peer failure, local pressure, or neutral. Local
pressure only reduces the total limit; generation-fenced failure and exclusive
verified probes control per-key recovery. The last permit owner releases active
usage, so I/O owners can retain it through completion. Callbacks must not reenter
the admission lock. No application error taxonomy or telemetry is imported.

`handoff::Handoff<K, A, T>` scans installed admission sources in round-robin order,
reserving target capacity before returning an `Offer`. Implement `Admission` with
reservation and release-wake behavior. `Offer::deliver` builds the queued item
only if its target remains open; the item must retain its reservation. Queued and
undelivered items thus share the target's quota ceiling. `pop_batch` enforces a
caller budget and installs a wake target; `close` releases queued items outside
the lock. Admission and item-building callbacks must not reenter the handoff.

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

`pipe::splice_unsupported` distinguishes the unsupported-operation errno set from
backpressure, interruption, and disconnect; callers retain their copy-fallback
policy and buffered bytes.

Admission and credit operations use `Error::{InvalidInput, Overloaded,
Unavailable, Io}`; pipe data operations return `std::io::Result`. The opt-in
`simulation` feature forwards `uring-runtime/simulation` and uses simulated pipe
descriptors when a simulation is active. Run focused tests from the enclosing
workspace with `timeout --signal=TERM --kill-after=10s 300s env CARGO_BUILD_JOBS=2 cargo test -p
flow-control --all-features`.
