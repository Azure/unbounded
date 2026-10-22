# Racer RDMA verbs library (`rdma-verbs`)

Status: implemented. This file covers the verbs crate. Racer's signed setup
and HTTP page protocol are in [racer-rdma-sessions.md](racer-rdma-sessions.md).
Paths below are relative to `cmd/racer-dataplane/verbs/`.

## Contract

The crate moves ciphertext over persistent, bidirectional RC queue pairs,
one per peer and rail. It keeps libibverbs off I/O threads and never exposes
a receive buffer to the CPU while the NIC may still write it.

A transfer has four steps:

1. Bind a type 2 memory window over a free receive buffer and send a `Grant`.
2. The peer writes with immediate, or sends an ordered `Release`.
3. Observe the receive completion and invalidate the window.
4. After successful invalidation, return `Arrival::Page` or free a released
   buffer. Abnormal paths require successful QP destruction before reuse.

Send sources are immutable and registered through implicit ODP. There is no
copied-send mode, send-buffer pool, per-transfer QP API, or plain-write API.
Peer authentication and the decision to use HTTP belong to Racer, not this
crate.

## Source map

| File | Responsibility |
|---|---|
| `src/node.rs` | Owner, clients, activation, link table, backoff, shutdown |
| `src/link.rs` | Per-QP grants, writes, credits, timers, and completion handling |
| `src/pool.rs` | Registered receive buffers and ownership states |
| `src/channel.rs` | Bounded worker command queues and one-shot results |
| `src/discovery.rs` | Stable port identities, port journal, and NUMA-aware worker selection |
| `src/control.rs` | Fixed-size in-band control codec |
| `src/ffi.rs` | Native resource ownership, bounds checks, and completion validation |
| `native/verbs.c` | libibverbs adapter |
| `src/simulation.rs` | Deterministic fabric and fault injection |
| `src/lib.rs` | Public exports and `WithNative` service wrapper |

## Public API

`node(workers)` returns one `Client` per worker and an `OwnerPort`.
`Owner::new` runs on the native thread and captures its environment.
`WithNative::with_host` combines it with another service through `NativeHost`.
Clients are `Send`; they contain shared handles, not native resources.

Each worker has a FIFO command queue with capacity 64. The owner drains them
round-robin. `try_send` returns the command with `Overloaded` when full or
`Unavailable` when the owner is gone; `poll_ready` waits for room. Dropping a
worker keeps its queued commands. Dropping the owner drops queued commands,
whose one-shot results become `Cancelled`.

`Op<T>` yields its result once. A second poll after completion returns
`InvalidRequest`. If the caller abandons an operation, `Completer::complete`
returns an unread value to the owner so it can release any acquired resources.

`Client::activate` returns selected rails asynchronously. Link operations are
`open`, `accept`, `connect`, `grant`, `write`, and `release`. Local epochs
prevent commands from acting on replacement links. `break_link` cannot be
lost to a full command queue; it also has a shared break list. Link snapshots
use a try-read lock, and a contended read reports `Idle`.

The owner polls commands, shared breaks, every link's completions and timers,
pending destruction, and shutdown, then publishes snapshots. It needs regular
polls even when no worker command is pending.

## Page buffer pool

Each selected device owns one page region, split into fixed-size receive
buffers. Registration grants `LOCAL_WRITE | MW_BIND`, not broad remote-write
access. A bound memory window grants remote write only to its selected range.
Control regions use `LOCAL_WRITE` only.

Each device also needs an implicit ODP region for local write sources. It is
not a remote-write region and cannot be used for receives or memory windows.
Activation fails when ODP is unsupported; it does not silently copy sources
into the receive pool.

`DirectBytes` is an unsafe trait: its slice must stay at the same address and
length, with unchanged contents, for the entire shared owner's lifetime.
The write captures that address and length once. The owner holds the source
through completion or successful QP destruction, and leaks it if destruction
cannot make DMA safe. Implementations include owned vectors, boxed slices,
and static slices; arbitrary `AsRef<[u8]>` is not sufficient.

The receive-buffer states are:

| State | Owner | CPU access |
|---|---|---|
| `Free` | Pool | None |
| `Binding` | Link posting `BIND_MW` | None |
| `Exposed` | Link after posting `Grant` | None |
| `Invalidating` | Link waiting for `LOCAL_INV` | None |
| `Owned` | `RegisteredPage` | Immutable bytes |
| `Quarantined` | Link awaiting QP destruction | None |
| `Leaked` | Retained permanently | None |

The normal receive path is:

```text
Free -> Binding -> Exposed -> Invalidating -> Owned -> Free
```

A released or abandoned page goes from `Invalidating` to `Free` without CPU
access. An unexposed grant can be canceled after its bind completes. An
exposed grant cannot be reclaimed merely by dropping its arrival operation;
it needs an ordered receive and invalidate, or a link break.

Only `RegisteredPage` exposes receive memory. Its drop can run on another
thread and returns the buffer to the pool. Dropping a pool with held,
quarantined, or leaked buffers keeps the region and charge alive rather than
deregistering memory prematurely.

## Links

For a configured local grant limit `g`:

```text
recv_depth = 2 * g + 4
send_depth = recv_depth + 2 * g + 4
control_bytes = 64 * 2 * recv_depth
```

Each link owns its QP and CQ, its control region, and `g` memory windows.
They are not preallocated as device-wide window or control pools. Receives
are posted before connect. All send-queue work is signaled.

On connect, the issued-grant cap becomes `min(local, peer)`. Incoming grants
must fit the local advertised limit. A receiver also needs a free pool buffer;
exhaustion returns `Overloaded`. Writes do not need local receive-pool space.

Grant tags are nonzero 32-bit values and skip outstanding tags. Window rkeys
advance their low-byte generation after a successful bind post. Generation
values can wrap; they are not permanent identities. Racer's signed metadata
also binds the link, tag, length, and transfer id.

## Credits and queues

Every `SEND` or `WRITE_WITH_IMM` consumes one peer receive credit. Data work
(`Grant`, `Release`, and write) needs at least two credits, keeping one reserved
for `Credit` or `Close`. Bind and invalidate need no receive credit.

Each receive is reposted immediately and increases the credits owed to the
peer. Control messages return owed credits. A standalone `Credit` is queued
at half receive depth when no other control can return them, including when
data is blocked on credits. `Credit`, `Close`, binds, and invalidates can pass
blocked data in the outbox; data work keeps its order.

A write or release whose grant has not arrived waits separately, with at most
`2 * g` waiters. A release replaces a waiting write for that tag and cancels
the write. A write larger than its grant fails without consuming the grant.
Unposted work can expire and return its unused grant. An exposed receive
grant that expires breaks the link instead.

Credit overruns, excess or duplicate grants, invalid releases, unknown or
duplicate immediates, bad lengths, malformed controls, and failed posts or
completions break the link. QPs use `rnr_retry = 0`: a missing posted receive
fails instead of hiding a credit bug behind retries.

## Control messages

Control messages are exactly 64 bytes in network byte order:

| Offset | Bytes | Field |
|---|---|---|
| 0 | 1 | Version, currently 1 |
| 1 | 1 | Kind: `Grant` = 1, `Release` = 2, `Credit` = 3, `Close` = 4 |
| 2 | 2 | Reserved, zero |
| 4 | 4 | Credits returned |
| 8 | 8 | Signed-handshake link id, not the local epoch |
| 16 | 4 | Tag |
| 20 | 4 | Rkey, `Grant` only |
| 24 | 8 | Remote address, `Grant` only |
| 32 | 4 | Granted length, `Grant` only |
| 36 | 4 | Reserved, zero |
| 40 | 16 | Transfer id, `Grant` and `Release` |
| 56 | 8 | Reserved, zero |

The codec rejects wrong lengths, versions, kinds, reserved bytes, link ids,
and fields that do not belong to a kind. Grants need a nonzero tag and a
length within the pool's buffer size. Releases need a nonzero tag. Link state
checks catch unknown and duplicate tags and incorrect transfer ids.

The immediate contains only the grant tag. The signed HTTP response, outside
this crate, remains the authoritative page outcome. There is no in-band
`Complete`, `Done`, or receiver-driven `Abort` protocol.

## Native ABI and completion checks

The adapter ABI is version 4. Rust requires that version and the current
symbols; there is no legacy ABI loader. The completion record is 32 bytes:

```c
struct rdma_verbs_wc {
    uint64_t id;
    uint32_t status, opcode;
    uint32_t byte_len, imm;
    uint32_t flags, vendor_err;
};
```

On failure, `opcode` is `UINT32_MAX` and `byte_len`, `imm`, and `flags` are
zero. The id, status, and vendor error remain available. Successful immediate
values are converted between network and host byte order by the adapter.
Static C assertions and Rust layout tests pin the record and opcode values.

The adapter posts bind with `IBV_SEND_SIGNALED | IBV_SEND_FENCE` and local
invalidate with `IBV_SEND_SIGNALED`. The normal ownership fence is the ordered
receive followed by successful window invalidation, not a claim that
`LOCAL_INV` carries the send-fence flag. The native fault test checks both
sets of flags; it does not model ODP page faults or hardware ordering.

Ranged operations validate nonempty ranges, arithmetic overflow, device and
region ownership, access kind, queue capacity, and duplicate work ids before
posting. Implicit regions are accepted only as write sources. Memory windows
retain their bound region, and advance their rkey only after a successful
post. The native adapter also checks ranges.

The QP tracks send and receive work separately. Completions must match an
outstanding id, opcode, and expected immediate and length rules. Racer uses
the high byte of a work id for its kind and the low bits for its index.
Unknown ids, failed statuses, or mismatches record the first failure and
stop the QP; they never produce a successful page.

## Memory safety rules

Normal page transfers do not destroy QPs. Error, timeout, idle close, and
shutdown do. A broken link quarantines buffers and retains direct sources
until QP destruction succeeds. Destruction is retried while the owner runs.
If the owner must drop resources that cannot be made safe, it leaks those
resources and their admission guard. A leak prevents a later reopen.

Ready links send `Close` during shutdown; other links break immediately.
Draining waits for links, pending destruction, and held pages, then withdraws
the pools. `WithNative` drives the inner service's drain or fence alongside
the owner, holding the result until both finish. Native teardown ignores
request deadlines because abandoning it could permit DMA into freed memory.

## Simulation and tests

The simulator models posted receive queues, send and write-with-immediate
delivery, range and rkey checks, revocation, queue errors, flushes, and resource
cleanup. Write data can land before an RNR failure, keeping failure tests
conservative about DMA. Work normally completes in post order; injected
faults can reject calls, delay or fail completions, drop or reorder delivery,
duplicate immediates, or alter reported lengths.

An implicit simulated region reads the direct source address, so tests check
source ownership through completion, cancellation, and failed destruction.
They also check unavailable ODP, both directions on one link, credit progress,
grant limits, stale epochs, abandoned operations, cross-thread page return,
shutdown, reopen, and leak accounting. The native fault adapter covers bind
and invalidate flags and discovery cleanup failures.

CI builds the crate with no features, simulation, native, and both features.
Simulation does not model provider ODP page faults or prove real-hardware
ordering and throughput. Native transfer tests on target hardware remain
necessary in addition to these gates.
