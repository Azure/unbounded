# RDMA lifecycle integration (audit follow-up)

The serving-worker native calls have moved to a bounded `NativeService` on the
existing paired crypto OS thread. There is no hidden executor or thread pool.
The C adapter now requires private ABI version 2; rebuild `make -C native`.
Cargo dependencies and build.rs do not change.

Application factory wiring:

1. Before creating the pair's services, call `rdma::lifecycle::pair(slot_count)`.
   Store its two Send endpoints by WorkerId in the shared factory, take each once.
2. On I/O: `devices.attach(io_port)?` before constructing Sessions/RegisteredPool.
3. On crypto: return `WithNative::new(page_crypto_engine, native_port)` from
   `build_crypto`. This implements the existing `CryptoService` interface and
   forwards wake registration, polling, drain and shutdown. Do not spawn a thread.
4. On I/O startup, after the authenticated publication arrives, await
   `devices.activate(local_member.rails.clone(), associations, &admission,
   MAX_CIPHERTEXT, scope)`. Retain the returned `Vec<RailMapping>` and use it with
   `Rails::local_compatible` / `select_with_local`. If local alignment is disabled,
   pass an empty publication and keep HTTP. An activation error means RDMA is
   unavailable, not a node startup error. The old synchronous `configure` cannot
   activate native resources and must not be used for this path.
5. Associations are trusted node-local `FabricPort { fabric, device, port, gid }`
   values. Publication has opaque fabric labels and a NUMA hint, not a device/GID
   identity. Discovery cannot derive `fabric-a` from a GID, NUMA ID, or sorted NIC
   order. Missing/ambiguous associations select HTTP. Omit gid only when exactly
   one discovered active port matches. Matching verifies published NUMA too.
6. Each slot preprovisions a native registered buffer, staging buffer and INIT QP.
   Admission charges **2 * round_up_4KiB(bytes_per_slot) * slot_count** registered
   bytes before the native command is accepted. Choose slot_count to fit the
   worker partition; exhaustion returns Overloaded and permits HTTP fallback.
   No MR allocation/registration occurs in a transfer. The service replenishes
   single-use QPs after a terminal fence and final lease release.
7. `PreparedSession::finish` now submits connection transitions to the service.
   Await `session.wait_ready(scope)` before receiver `prepare_receive`.
   `send_to` awaits it internally. Signed offer/descriptor/completion encoding
   is unchanged. All grant readback awaits the asynchronous terminal fence.
8. Continue `Sessions::progress` on I/O turns for deadline handling. It deliberately
   absorbs per-session errors, leaving the error on the attempt's ticket/session.
   It must not shut down a worker for CQ errors/timeouts. Async native completions
   wake the I/O driver; native CQ progress is driven by crypto's bounded turns.
   Call `sessions.register_driver(worker_waker)` or
   `rdma_transfer.register_driver(worker_waker)` before each driver turn. Task
   waiters and worker wake registration are independent.
9. Drain sessions before releasing I/O service owners, then close Devices and let
   WithNative drain native resources on crypto. Failed terminal fences retain
   native ownership, memory and quota. Dropping a waiter only cancels the slot;
   it never runs a native destructor on I/O or permits early pool reuse.

The lifecycle handoff uses fixed per-slot mailboxes; I/O uses try_lock only.
The crypto role can block inside provider resource syscalls, but the I/O role
continues socket/reactor/HTTP work. This is genuine cross-thread completion,
not an async fn executing a syscall on its caller's thread.

The runtime's bounded crypto/lifecycle timer tick drives pending CQ work and
retries failed fences. The service does not spin or spawn timer threads. Failed
native fence retries have a 10 ms backoff on the crypto role. Slot reuse waits
for the actual fence and the final I/O lease, including receive readback owners.

On each later publication call `devices.revalidate(&local_member.rails,
local_member.alignment_enabled)`. False requests revocation and removes readiness;
replace the closed lifecycle pair before reactivation. Never continue advertising
a fabric mapping after publication changes.

For live cache/key retirement, pause the affected read/peer producers, capture
`rdma_transfer.fence_cut()` and await it before acknowledging the retirement.
This snapshots current sessions, requests cancellation on all of them before
awaiting any fence, and leaves admission for future unrelated sessions open.
`drain()` is terminal and is reserved for worker shutdown.
