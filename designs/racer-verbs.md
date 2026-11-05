# Racer RDMA verbs library (`rdma-verbs`)

Status: implemented. This file covers the verbs crate. Racer's signed setup
and HTTP page protocol are in [racer-rdma-sessions.md](racer-rdma-sessions.md).
Paths below are relative to `cmd/racer/verbs/`.

## Contract

The crate moves ciphertext over persistent, bidirectional RC queue pairs,
one per peer and rail. It keeps libibverbs off I/O threads and never exposes
a receive buffer to the CPU while the NIC may still write it.

A transfer has four steps:

1. Grant remote write to a free receive buffer and send a `Grant`.
2. The peer writes with immediate, or sends an ordered `Release`.
3. Observe the receive completion and revoke the grant.
4. After successful revocation, return `Arrival::Page` or free a released
   buffer. Abnormal paths require successful QP destruction before reuse.

Send sources are immutable and read in place by the NIC. There is no
copied-send mode, send-buffer pool, per-transfer QP API, or plain-write API.
Peer authentication and the decision to use HTTP belong to Racer, not this
crate.

## Device modes

Discovery reports facts per port (memory window support, whether an RC QP can
be created, and the IPv4-mapped RoCE GID); `src/ffi.rs` picks the mode:

| | Window mode (mlx5) | Registration mode (MANA) |
|---|---|---|
| Chosen when | the device has type 2B memory windows | no windows, RC QPs work, and the port has an IPv4-mapped GID |
| Grant | bind a type 2 window over the buffer | register the buffer for remote write (`ibv_reg_mr`) |
| Revoke | signaled `LOCAL_INV` | deregister the grant's MR |
| Write source | one implicit ODP MR over the address space | register the source for the write's lifetime, post in 8 MiB pieces |
| Connect | plain verbs (`RTR`/`RTS` from the setup exchange) | RDMA connection manager, event driven |
| Requires | implicit ODP for RC writes | max message size of at least 8 MiB, `buffer_len % 2 MiB == 0` |

Registration mode needs no ODP. Its logic lives in `src/registration.rs`:

- A grant registers the granted buffer, rounded up to whole 2 MiB pages when
  it starts on one but never past the end of the granted buffer, and hands
  the peer that MR's rkey. Revocation deregisters it. Both complete at once:
  the QP's ledger queues a synthetic `BIND_MW` or `LOCAL_INV` completion,
  with no allocation that could drop it.
- A write registers its source (a 2 MiB aligned head and a tail, so the huge
  page part registers fast), posts unsignaled pieces of at most 8 MiB and a
  final signaled write with immediate, and deregisters on completion. The
  immediate's top byte counts the pieces before the last, so the receiver
  reports the whole length; tags use only the low 24 bits. Every check
  (QP established, tag width, at most four pieces) runs before anything is
  registered. A write before the connection is established fails with
  `Unavailable` and registers nothing.
- The send queue and CQ hold four entries per counted send, so a full error
  flush, including unsignaled pieces, fits.
- A failed deregistration keeps the MR until the QP stops, when it is retried
  once; if that fails too, the MR leaks for good and quarantines the node
  guard, so the leak counts against the caller's limits.
- `buffer_len` must be a multiple of 2 MiB, or activation fails with
  `InvalidConfiguration`.

### Connection manager

Each registration-mode device has a nonblocking connection manager event
channel and a listener on its IPv4 address (port `RACER_RDMA_CM_PORT`, else
18515). The endpoint's `qpn` is a random 24-bit token that names the QP in the
setup exchange and the connection request. The adapter spawns no threads;
`NativeDevice::progress` reads events without blocking, and the owner calls it
on every poll (a QP still connecting also calls it from its own progress).

- Initiator: `connect` creates an id and starts address resolution. Then
  `ADDR_RESOLVED` resolves the route, `ROUTE_RESOLVED` creates the QP on the
  link's CQ, replays queued receives, and connects with both tokens as private
  data, and `ESTABLISHED` makes the QP ready. Until then the link stays
  `Ready` but posts nothing, and breaks at the setup timeout if the
  connection never establishes.
- Responder: `connect` records the initiator's token before the HTTP reply
  (Racer accepts before replying). A `CONNECT_REQUEST` whose tokens match a
  waiting QP creates its QP, replays receives, and accepts; any other request
  is rejected and its id destroyed.
- Address, route, connect, reject, unreachable, and disconnect events fail
  the QP (moving it to the error state if it exists), which breaks the link.

Every event is copied, private data included, and acknowledged before it is
handled, because `rdma_destroy_id` waits for an id's events to be
acknowledged. Stopping a QP destroys the QP, then its id.

### Event-driven progress

Every device has one completion channel with a nonblocking fd; every CQ
reports to it. `Owner::poll_fds` returns the completion channel fds and, in
registration mode, the connection manager fds, so a dedicated owner thread
can wait on them. A QP re-arms its CQ before polling it (arm, then poll), so a
completion that lands after the poll raises an event. Polling the owner
without waiting on the fds still works.

## Source map

| File | Responsibility |
|---|---|
| `src/node.rs` | Owner, clients, activation, link table, backoff, shutdown |
| `src/link.rs` | Per-QP grants, writes, credits, timers, and completion handling |
| `src/pool.rs` | Registered receive buffers and ownership states |
| `src/channel.rs` | Bounded worker command queues and one-shot results |
| `src/discovery.rs` | Stable port identities, port journal, and NUMA-aware worker selection |
| `src/control.rs` | Fixed-size in-band control codec |
| `src/ffi.rs` | Native resource ownership, mode choice, the work request ledger, bounds checks, and completion validation |
| `src/registration.rs` | Registration mode: per-grant and per-write registrations, connection manager state machine |
| `native/verbs.c` | Thin libibverbs and librdmacm wrappers |
| `src/simulation.rs` | Deterministic fabric and fault injection |
| `src/lib.rs` | Public exports |
| `src/role.rs` | `OwnerRole`: the owner as its own thread's role, parked on its wake sources |

## Public API

`node(workers)` returns one `Client` per worker and an `OwnerPort`.
`Owner::new` runs on the native thread and captures its environment.
`OwnerRole::new` wraps it as a `racer_io::exec::Role` for a dedicated thread;
`OwnerStop::stop` closes it, and the role finishes once the owner drained.
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
buffers. In window mode the region is registered `LOCAL_WRITE | MW_BIND`, not
with broad remote-write access, and a bound memory window grants remote write
only to its selected range. In registration mode it is registered
`LOCAL_WRITE`, and each grant registers its buffer for remote write. Control
regions use `LOCAL_WRITE` only.

In window mode each device also needs an implicit ODP region for local write
sources. It is not a remote-write region and cannot be used for receives or
memory windows. Activation fails when ODP is unsupported; it does not
silently copy sources into the receive pool. Registration mode registers each
write's source instead.

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

## Device page pools

A rail can also hold one device pool: caller memory that the NIC reaches
through a Linux dma-buf, such as GPU memory. The caller passes an owner that
implements `DmabufMemory` (descriptor, offset, length, and the NIC address of
the first byte) to `Client::attach_device`, once per rail per round, after
activation, in window mode only. The owner thread registers it with `ibv_reg_dmabuf_mr`
(`LOCAL_WRITE | MW_BIND`) and splits it into buffers of the node's buffer
length. The crate does not depend on any GPU library.

`Client::grant_device` grants a device buffer. It uses the same states, the
same window bind, and the same fence as a host grant: the arrival resolves
only after the peer's write completion and a successful `LOCAL_INV`, so the
peer can no longer write the buffer. The arrival is `Arrival::Device` with a
`DevicePage`, which names the memory owner, the offset, and the length, and
holds the buffer until it is dropped. A device grant fails with `Unavailable`
when the rail has no device pool and `Overloaded` when no device buffer is
free; it never falls back to host memory. A window stays tied to the region
it first binds, so a link makes device windows on first use and keeps them
apart from host windows, at most one per concurrent grant.

The CPU never maps or copies device memory. `DevicePage` has no byte API, the
region reports a null pointer, and sends, writes, and receives that name a
device region are rejected in Rust, in the C adapter, and in the simulation.
If deregistration fails, the region leaks with its memory owner and the node
guard, so the memory is never freed while the NIC might write it. The
simulation maps a memfd in place of the dma-buf (`MemfdMemory`), and traces
and faults the registration as `Operation::RegisterDmabuf`.

## Links

For a configured local grant limit `g`:

```text
recv_depth = 2 * g + 4
send_depth = recv_depth + 2 * g + 4
control_bytes = 64 * 2 * recv_depth
```

Each link owns its QP and CQ, its control region, and `g` memory windows
(in registration mode, `g` grant holders). They are not preallocated as
device-wide window or control pools. Receives are posted before connect (in
registration mode, queued until the connection manager creates the QP).
All counted send-queue work is signaled; registration-mode write pieces
before the last are not.

On connect, the issued-grant cap becomes `min(local, peer)`. Incoming grants
must fit the local advertised limit. A receiver also needs a free pool buffer;
exhaustion returns `Overloaded`. Writes do not need local receive-pool space.

Grant tags are nonzero 24-bit values and skip outstanding tags. Window rkeys
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

The adapter ABI is version 7. Version 7 moved registration mode, the
connection manager state machine, and the work request ledger from C to Rust;
the adapter is now a set of thin wrappers: discovery facts, open and close,
`query_caps`, completion channels (`comp_channel`, `req_notify`,
`get_cq_event`, `ack_cq_events`), `cq_create` (separate from QP creation),
QP creation, connect, `qp_error`, and `qp_free`, region registration
(`register`, `register_implicit`, `register_dmabuf`, `reg_mr`, `dereg_mr`),
windows, bind, invalidate, scatter-gather posts (`post_write` with up to four
entries and immediate and signaled flags, `post_send`, `post_recv`), `poll`
on a CQ, and connection manager primitives (`cm_channel`, `cm_listen`,
`cm_id`, `cm_resolve_addr`, `cm_resolve_route`, `cm_create_qp`, `cm_connect`,
`cm_accept`, `cm_reject`, `cm_get_event`, `cm_ack`, `cm_destroy_id`), plus
`alloc_buffer` (2 MiB aligned, huge-page advised). Rust requires that version
and every symbol; there is no legacy ABI loader. The completion record is 32
bytes:

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
Static C assertions and Rust layout tests pin the record, the port,
capability, scatter-gather, and connection manager event records, and the
opcode and event type values.

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

The QP's ledger is the only record of posted work: each request holds its
region, window, and registration-mode source registrations, and the link
attaches its own record (control slot, write source and completer, grant
tag), which comes back with the completion or after a stop. The ledger
counts send and receive work separately. Completions must match an
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
the pools. Pages held on other threads (crypto jobs) return on their own; the
pool wakes the owner role when they do, and the role waits for them however
long that takes, reporting every `DRAIN_REPORT`. Native teardown ignores
request deadlines because abandoning it could permit DMA into freed memory.

## Simulation and tests

The simulator models posted receive queues, send and write-with-immediate
delivery, range and rkey checks, revocation, queue errors, flushes, and resource
cleanup. It has two device profiles, window (mlx5-like) and registration (no
windows, no ODP), and the contract tests in `src/link/tests.rs` and
`tests/links.rs` run against both. Its connection manager resolves addresses
and routes, delivers requests to listeners, accepts, rejects, and reports
established and disconnected connections through real eventfds, as do its
completion channels. Faults can fail registration and deregistration,
resolve an address or route with an error, or make a connect never answer. Write data can land before an RNR failure, keeping failure tests
conservative about DMA. Work normally completes in post order; injected
faults can reject calls, delay or fail completions, drop or reorder delivery,
duplicate immediates, or alter reported lengths.

An implicit simulated region reads the direct source address, so tests check
source ownership through completion, cancellation, and failed destruction.
They also check unavailable ODP, both directions on one link, credit progress,
grant limits, stale epochs, abandoned operations, cross-thread page return,
shutdown, reopen, and leak accounting. The native fault adapter
(`native/discovery_faults.c`, over mocked libibverbs and librdmacm calls)
covers bind and invalidate flags, discovery and open cleanup failures, and
the thin wrappers: scatter-gather bounds, write flags and immediates,
completion translation, completion channels, and connection manager event
copies.

CI builds the crate with no features, simulation, native, and both features.
Simulation does not model provider ODP page faults or prove real-hardware
ordering and throughput. Native transfer tests on target hardware remain
necessary in addition to these gates.
