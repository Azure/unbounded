# Racer RDMA verbs library (`rdma-verbs`)

## Summary

`rdma-verbs` lets Racer move bytes between nodes with one-sided RDMA writes.
A receiver opens a memory window over one of its buffers and hands the peer an
address and key. The sender writes into that window. There are no RDMA sends,
receives, reads, or atomics.

The crate has three parts:

- `src/lib.rs`: the public API, the slot pool, and the native service.
- `src/ffi.rs`: safe Rust wrappers around a small C adapter, plus the rules for
  freeing (or leaking) NIC objects.
- `native/verbs.c`: the C adapter that calls libibverbs.

A simulated fabric (`src/simulation.rs`) replaces the C adapter in tests.

Paths below are relative to `cmd/racer-dataplane/verbs/`.

## Goals and non-goals

Goals:

- Never let the CPU reuse or free memory that the NIC can still write.
- Keep libibverbs off the I/O thread, so a slow NIC call cannot stall it.
- Fixed, bounded resources that are allocated up front.
- Fail closed. When cleanup fails, leak the resource and keep it counted.
- Test every lifetime and failure path without RDMA hardware.

Non-goals:

- Authentication, peer admission, topology, retries, and timeouts. The caller
  owns all of these.
- A general verbs binding. The crate exposes only what Racer needs.

## Threads and slots

There are two threads: the I/O thread and the native thread
(`src/lib.rs:1-62`). All libibverbs calls run on the native thread. The I/O
thread never touches RDMA objects or registered memory. The crate spawns no
threads and runs no executor. Time and randomness come from the
`uring-runtime` environment, so the same code runs in simulation.

The threads share a fixed pool of 1 to 256 slots (`src/lib.rs:905-950`). A
slot is one queue pair (QP) and one registered buffer. Each slot has a mailbox
behind a mutex (`src/lib.rs:791-819`) and holds at most one pending command:
connect, bind, write, or invalidate (`src/lib.rs:776-789`). Neither side waits
on a mailbox lock; both use `try_lock` and retry on the next poll
(`src/lib.rs:1796-1806`). Callers must therefore also poll on a timer.

`NativeService::poll_budgeted` (`src/lib.rs:1317-1413`) does one step per
budget unit: discover devices, build one slot, or service one slot. Budgets
count steps, not time.

Why this shape: libibverbs calls can block, and an I/O thread that stalls
stops every connection. A fixed pool keeps memory and NIC usage bounded and
lets the native side check every resource on shutdown.

## Lifecycle

1. `pair(slots)` returns an `IoPort` and a `NativePort`. The native side
   creates a `NativeService`, which is not `Send` (`src/lib.rs:234-256`).
2. `IoPort::activate` sends a `Configuration` (`src/lib.rs:206-221`): a port
   selector, one admission guard per slot, and the buffer size per slot. The
   selector runs on the native thread against discovered ports.
3. The native side validates the plan (`src/lib.rs:1200-1254`), then builds
   one slot per step (`src/lib.rs:1256-1315`). It spreads slots over the
   selected ports, registers each buffer, creates each QP, and checks that the
   device can create a memory window. No slot becomes ready until all are
   built.
4. A caller leases a ready slot on a port with `QueuePairHandle::poll_new`
   (`src/lib.rs:531-579`). If none is free it gets `Overloaded`.
5. Peers swap a 32-byte `Endpoint` over the caller's own channel
   (`src/lib.rs:329-363`) and connect.
6. The receiver binds a window over its region and sends the address and key
   to the sender. The sender fills its region and writes.
7. The caller stops the QP and waits for `poll_stopped` before it reads
   received bytes. Dropping the handle returns the slot. The native side stops
   the QP if needed and rebuilds the slot with a fresh QP.

Discovery only accepts devices that support type 2B memory windows, active
ports, and GID index 0 (`native/verbs.c:53-88`).

## Memory safety rules

**Only a stopped QP is a fence.** Invalidating a window or timing out does not
stop writes the NIC has already accepted (`src/lib.rs:31-38`,
`src/lib.rs:654-666`). Stopping moves the QP to the error state and destroys
it (`native/verbs.c:173-184`). Only after that succeeds does the crate treat
the buffer as quiet. A bound region stays busy until then
(`src/ffi.rs:722-760`, `src/ffi.rs:846-865`).

**The I/O thread only sees staging copies.** Writes from the I/O thread go to
staging bytes in the mailbox. The native thread copies them into registered
memory when it runs the write (`src/lib.rs:1568-1573`). Received bytes are
copied out only after the QP stops (`src/lib.rs:1459-1468`), and
`Region::poll_copy_to` fails before that (`src/lib.rs:450-458`). So a late
remote write can never race with a CPU read.

**QPs are never reused.** After a lease ends, the native side clears the full
buffer, zeroes the staging copy, and builds a new QP (`src/ffi.rs:423-432`,
`src/lib.rs:1490-1531`). A new lease never sees old data or a QP with stale
state. The buffer stays registered for the life of the pool, so there is no
registration cost per lease.

**Leak on failure.** If freeing a NIC object fails, the object is leaked on
purpose along with everything it depends on (`src/ffi.rs:1-5`). This covers
devices, regions, windows, QPs, and probe windows (`src/ffi.rs:261-272`,
`447-456`, `470-479`, `581-587`, `651-663`, `868-886`). Freeing memory the NIC
may still write is worse than leaking it.

**Leaks stay counted.** Each slot holds the caller's admission guard. When a
resource leaks, its guard leaks too (`src/lib.rs:132-135`), so the leak keeps
counting against the caller's limits. The crate wraps each guard in its own
`Arc` (`src/lib.rs:1765-1794`). Its strong count then shows leaks no matter how
many clones the caller holds. A leaked slot is never refilled
(`src/lib.rs:1493-1497`).

**Completions are checked.** Each QP allows at most 32 posted requests
(`src/ffi.rs:682-708`). Any failed status, unknown request, or wrong opcode
stops the QP and returns an error (`src/ffi.rs:799-840`). Starting PSNs are
random (`src/ffi.rs:589-607`).

**Least access.** QPs allow only remote writes (`native/verbs.c:119-172`).
Windows grant remote write over exactly the bound length
(`native/verbs.c:221-233`). Invalidation uses a fenced send
(`native/verbs.c:234-240`).

## Shutdown and reopen

`IoPort::close` cancels all slots and returns at once
(`src/lib.rs:1115-1124`). The native service then stops each QP. It marks the
pool drained only when every resource is freed, nothing is quarantined, and
the crate holds the only reference to every guard. `IoPort::reopen` returns
`Overloaded` until then and bumps a generation so old device handles go stale
(`src/lib.rs:1040-1069`).

`WithNative` (`src/lib.rs:258-269`, `src/lib.rs:1663-1744`) joins the native
service with a `uring-runtime` service. On drain it stops the NIC first and
ignores scope deadlines (`src/lib.rs:1746-1763`), because the NIC must stop
before any memory is freed.

## Native adapter

Rust does not link libibverbs. It loads `librdma_verbs.so.1` with `dlopen` and
calls a few functions that take small, fixed structs (`native/verbs.c:1-10`).
The libibverbs structs and inline helpers stay in C, built against the
installed headers. This avoids copying large, version-specific structs into
Rust.

Because the link is dynamic, the compiler cannot check the ABI. Three checks
stand in for it:

- The adapter reports ABI version 4 and Rust refuses any other
  (`native/verbs.c:51-52`, `src/ffi.rs:174-235`).
- `_Static_assert`s in C (`native/verbs.c:37-44`) and a Rust test check the
  same struct sizes, offsets, and opcode values.
- CI builds the adapter and loads every symbol.

Without the `native` feature, loading returns `Unavailable`
(`src/ffi.rs:160-165`), so the crate builds on hosts without libibverbs.

Discovery returns `INT_MIN` if closing a temporary context fails. This is
distinct from a negative errno or port count. It stops on the first failed
close, frees the device list, and leaves at most one context leaked. Rust
quarantines and retains the discovery admission owner and adapter reference.
Reopen stays blocked even when no ports were returned.

Open returns `NULL` for a clean failure and `(void *)UINTPTR_MAX` if PD
allocation fails and the context cannot close. The latter is not a handle.
Rust charges an owner before each open, retains it and the adapter on this
result, and stops opening ports. Previously opened devices close normally.

## Simulation

`src/simulation.rs` implements the same C functions as the adapter. The real
`src/ffi.rs` wrappers run unchanged on top of it. It is deterministic, has one
fabric per test thread, and can inject faults on any call. The `testing`
module exposes slot state and can hold mailbox locks to force contention.

## Testing

`src/ffi.rs` tests inject failures into each free path and check that the
object and its guard stay leaked. `tests/public_api.rs` covers the public
API: port selection, reopen, guards across failed fences, copies only after
stop, cleared buffers on reuse, contention, and empty plans.

The `racer-verbs` CI job runs four feature sets: none, `simulation`,
`native`, and both. Native runs install `libibverbs-dev`, build the adapter
with `images/racer-dataplane/build-native.sh`, and load it. Each set runs
fmt, check, clippy, and tests. Hosted runners have no RDMA hardware, so CI
does not test real DMA or fencing. Tests that need a device stay ignored.

Native CI also builds `native/discovery_faults.c`, which includes the production
C adapter with controlled libibverbs calls. `src/discovery_fault_tests.rs`
checks both internal discovery closes, failed-open cleanup, quarantine,
library retention, and blocked reopen. The Rust simulator's `Close` fault only
covers device handles returned after discovery, not these temporary contexts.
