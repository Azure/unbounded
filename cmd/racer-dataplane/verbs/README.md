# rdma-verbs

Bounded RDMA ownership for an application with paired I/O and native threads.
The crate does not spawn a thread or drive an executor. It uses `uring-runtime`
environments for time and randomness, including deterministic simulation.

## Integration

1. Create `pair(slots)` before starting either role. `IoPort` and `NativePort`
   are Send endpoints. Capacity is fixed between 1 and 256 slots.
2. Construct `NativeService::new(native_port)` on the native thread. Its native
   owner graph is intentionally not Send. Drive `poll_budgeted` there and register
   the native driver waker. Provider calls and destruction stay on that thread.
3. Submit an owned `Configuration` through `IoPort::configure`. It contains one
   opaque `Guard` per slot, a staging/registration byte bound, and a Send selector.
   The selector executes on the native thread after discovery and returns
   `(u32 caller_tag, discovered_index)` pairs. Duplicate tags, reused indices,
   and out-of-range indices fail closed.
   Set `discover: false` only for an intentionally empty plan; this invokes the
   selector with an empty slice without loading the native adapter.
4. Poll `activation()` for the selected ports. Construct I/O-local device handles
   with `Rc<IoPort>::device(tag)`. Claim a `QueuePairHandle` with `poll_new` or
   `poll_new_admitted`, optionally attaching another opaque lifetime guard.
5. Exchange authenticated `Endpoint` values using the application's own protocol.
   Connect, acquire a `Region`, bind a `Window`, and write using the poll APIs.
   Tickets represent native completion, not submission. An application must not
   publish a window before its bind ticket succeeds.
6. Request `stop` and await `poll_stopped`. Neither a timeout nor an invalidation
   CQE is a terminal DMA fence. Readback requires the terminal native QP fence.
   Close both roles and continue native progress during shutdown.

Configuration retains guards across mailbox contention. Its future is scope-free:
callers enforce cancellation and deadlines. After configuration is submitted,
abandoning activation requires closing the I/O port and continuing native progress.
Dropping a QP requests cancellation without blocking I/O. Native ownership and
caller guards survive failed destruction, including intentional bounded quarantine
leaks. Reopening requires native drainage and release of all I/O leases. Caller
Arc clones do not affect the internal quarantine reference count.

Use `uring_runtime::poll_scoped` for caller-scoped poll APIs; the runtime owns
cancellation registration and scope checks. Never use it to truncate a required
native completion fence. `WithNative<T>` forwards the inner runtime service's
failure reporter, waker, and lifecycle hooks. Admission stop leaves native progress
available for accepted work; drain fences native resources before draining the
inner service. Close shuts native admission even if inner close fails, and fence
drives native destruction before the inner ownership fence, without scope checks.

## Features and native adapter

`native` loads `librdma_verbs.so.1` at runtime using `rdma_verbs_*` symbols and
checks ABI version 2 before creating handles. Build `native/verbs.c` against the
installed libibverbs headers; provider layouts are never reproduced in Rust.
The default build has no native library requirement and reports `Unavailable`
when no simulated fabric is active.

`simulation` exposes a deterministic connected fabric. Enter a node discovery
scope before constructing its `NativeService`; owners retain the fabric after
the scope exits. Faults cover provider rejection, delayed completion, and failed
completion. Virtual remote addresses, keys, pairing checks, and DMA side effects
are simulated behind the same private unsafe boundary. No raw ABI fixture is
part of the public API.

`Simulation::reject` keeps a provider rejection active until explicitly cleared,
including during destructor retries. The simulation-only `testing` module offers
contention closures and scalar lifecycle snapshots for integration assertions.
It does not expose mutex guards, mailbox contents, or native handles. Successful
integration transfers use the connected fabric's DMA effects, not fabricated CQEs.

Run `cargo test -p rdma-verbs --locked --offline --features simulation` from the
workspace to exercise the public API without RDMA hardware. The integration suite
covers tagged multi-port activation and bounded claims, close/reopen with stale
device rejection, and connect/bind/write/invalidate/fence readback, including
invalid inputs, delayed completion, and rejected stale keys. Native ABI and
quarantine regressions remain separate; simulated DMA does not validate a real
provider or the C adapter. Operator-selected native tests remain ignored by default.

## Limitations

This is a bounded RC/type-2B memory-window pool, not a general verbs binding.
It stages copies through mailboxes, supports one transfer region and one pending
command per leased slot, and discovers at most 64 active ports using GID index 0.
The budget bounds steps, not provider wall time: a stalled provider can stall
other work on the native thread. Mailbox contention needs periodic driver ticks
as well as completion wakes. The caller owns authentication, topology matching,
admission sizing, platform page-size accounting, retry policy, and request scopes.
There are no Racer protocol or policy dependencies.
