# Racer RDMA verbs library (`rdma-verbs`)

Status: implemented. Stages 1 to 5 of
[racer-rdma-sessions.md](racer-rdma-sessions.md) moved the code from the
per-transfer slot model to this design; stage 5 removed the slot API. The
code is authoritative where it differs.
The session protocol, threading, HTTP integration, and the implementation
plan live in `racer-rdma-sessions.md`. This file covers only the crate.

Paths below are relative to `cmd/racer-dataplane/verbs/` unless noted.

## Summary

`rdma-verbs` moves ciphertext pages between Racer nodes over persistent,
bidirectional RC queue pairs (QPs). There is one QP per (peer, rail) per
node, used in both directions. Each transfer:

1. The receiver binds a type 2 memory window (MW) with a fresh rkey over one
   page buffer and sends a `Grant` control message to the peer.
2. The sender posts one `RDMA_WRITE_WITH_IMM` with `imm = tag`, or sends a
   `Release(tag)` control message if it will not write.
3. The receiver gets exactly one receive completion for the tag (write with
   immediate or `Release`). RC ordering guarantees that every write the peer
   posted before it has been placed. That completion is the fence.
4. The receiver posts a signaled, fenced `LOCAL_INV` of the rkey. After its
   completion the CPU may read the buffer. AEAD verification still happens
   later, in Racer.

QPs are destroyed only on error, timeout, idle close, or shutdown. Destroying
a QP stays the fallback fence for every abnormal path.

The crate has these parts:

- `src/lib.rs`: public API: `node`, `Client`, `Owner`, `WithNative`, errors.
- `src/link.rs`: per-link (QP) state, credits, tags, grants.
- `src/pool.rs`: registered page buffers and their ownership states.
- `src/control.rs`: the fixed-size in-band control message codec.
- `src/channel.rs`: bounded per-worker command and completion queues.
- `src/ffi.rs`: safe wrappers around the C adapter and the leak rules.
- `native/verbs.c`: the C adapter (ABI version 2).
- `src/simulation.rs`: deterministic fabric implementing the adapter ABI.
- `src/discovery.rs`: unchanged.

## Goals and non-goals

Goals:

- Never let the CPU read, reuse, or free memory that the NIC may still write.
- Keep libibverbs off I/O threads.
- Bounded resources sized up front: page buffers, links, queue depths.
- Fail closed: an unexpected completion breaks the link, never accepts data.
- No RNR: receive credits make it impossible to send without a posted receive.
- Test every lifetime and failure path in simulation.

Non-goals:

- Authentication, peer identity, tie-break policy, HTTP handshake, and
  fallback. Racer (`src/rdma.rs`) owns these. The crate takes an opaque
  `LinkKey` and an `initiator` flag from the caller.
- A general verbs binding.

## Threads

There is one `Owner` per node. It runs as a `uring-runtime` service on one
crypto thread through `WithNative` (`src/lib.rs`). All libibverbs calls run on that thread. Each I/O
worker holds one `Client`. A `Client` and the `Owner` share one `Channel`
per worker (`src/channel.rs`): one bounded command queue (I/O to owner) and
two `AtomicWaker`s (one per side). Results come back
through per-operation one-shot cells (`Op<T>`), not a queue, so a worker
never has to drain unrelated results.

Channel details (as built): `channels(workers, capacity)` returns one
`Worker` per I/O worker and one `Owner`. `Worker::try_send` returns the
command with `Overloaded` when the queue is full (wait with
`Worker::poll_ready`) or `Unavailable` once the owner is gone.
`Owner::poll_drain(cx, budget, f)` hands out up to `budget` commands,
round-robin across workers, in send order per worker. Dropping the `Owner`
drops queued commands; each command owns the `Completer` for its `Op`, and a
dropped `Completer` resolves the `Op` to `Cancelled`. A dropped `Worker`
keeps its queued commands; `Owner::is_closed` turns true once they drain.
`Completer::complete` hands the value back if the `Op` was dropped, so the
owner can clean it up. Polling an `Op` after it returned `Ready` gives
`InvalidRequest`.

Rationale is in `racer-rdma-sessions.md` ("Threading").

## Public API

```rust
/// Opaque caller identity for a link. Racer uses SHA-256(NodeId) and RailId.
#[derive(Clone, Copy, Eq, PartialEq, Hash, Debug)]
pub struct LinkKey { pub peer: [u8; 32], pub rail: u16 }

/// One QP incarnation. Never reused within a process.
#[derive(Clone, Copy, Eq, PartialEq, Hash, Debug)]
pub struct Epoch(pub u64);

pub struct NodeConfig {
    pub selector: Box<dyn Fn(&[PortInfo]) -> Vec<Selection> + Send>,
    /// Registered bytes per selected device; split into page buffers.
    pub pool_bytes: usize,
    /// Page buffer length, already rounded to 4 KiB (MAX_CIPHERTEXT rounded).
    pub buffer_len: usize,
    /// Max links (QPs) per device. Racer uses topology::MAX_DEGREE.
    pub max_links: usize,
    /// Max concurrent grants a peer may hold against us, per link.
    pub grants_per_link: u32,
    /// Idle time after which a link with no transfers is closed.
    pub idle: Duration,
    /// Admission guard charged for the pool; leaks with leaked resources.
    pub guard: Guard,
}

/// Create one owner and `workers` clients.
pub fn node(workers: usize) -> Result<(Vec<Client>, OwnerPort)>;

impl Client {                                  // !Send, lives on one I/O worker
    pub fn activate(&self, config: NodeConfig) -> Result<()>;
    pub fn link(&self, key: LinkKey) -> LinkState;      // lock-free snapshot
    /// Start a link as initiator. Returns the local setup to sign and send.
    pub fn open(&self, key: LinkKey) -> Op<Opened>;
    /// Responder side: create a QP and connect it to `remote` in one step.
    pub fn accept(&self, key: LinkKey, remote: Setup, initiator: bool) -> Op<Opened>;
    /// Initiator side: finish with the responder's setup.
    pub fn connect(&self, key: LinkKey, epoch: Epoch, remote: Setup) -> Op<()>;
    /// Receiver: bind a window over a free buffer and send Grant.
    pub fn grant(&self, key: LinkKey, epoch: Epoch, transfer: [u8; 16],
                 length: u32, deadline: Instant) -> Op<Granted>;
    /// Sender: copy (or move) a staged page into the peer's granted window.
    pub fn write(&self, key: LinkKey, epoch: Epoch, tag: u32, source: Source,
                 deadline: Instant) -> Op<()>;
    /// Sender: decline a grant. Always safe; consumes one credit.
    pub fn release(&self, key: LinkKey, epoch: Epoch, tag: u32) -> Op<()>;
    /// Break the link now (ERR + destroy). Outstanding ops fail.
    pub fn break_link(&self, key: LinkKey, epoch: Epoch);
    pub fn send_buffer(&self, len: usize) -> Poll<Result<SendBuffer>>;
    pub fn close(&self);
    pub fn register_driver(&self, waker: &Waker);
}

pub enum LinkState { Idle, Connecting(Epoch), Ready(Epoch), Backoff(Instant) }

pub struct Setup {                       // carried in the signed HTTP setup
    pub endpoint: Endpoint,              // existing 32-byte encoding
    pub recv_credits: u32,               // receives the peer may consume
}
pub struct Opened { pub epoch: Epoch, pub setup: Setup }

pub struct Granted { pub tag: u32, pub arrival: Op<Arrival> }
pub enum Arrival {
    /// Write with immediate observed, invalidate done. Exclusive CPU access.
    Page(RegisteredPage),
    /// Peer sent Release(tag). Buffer already returned to the pool.
    Released,
}

pub enum Source { Bytes(Arc<dyn AsRef<[u8]> + Send + Sync>), Staged(SendBuffer) }

/// A page buffer with no valid MW and no posted WR. The only way Rust code
/// can see registered page memory.
pub struct RegisteredPage { /* Arc<PoolShared>, index, len */ }
impl RegisteredPage { pub fn bytes(&self) -> &[u8]; }      // Send + Sync
pub struct SendBuffer { /* same, writable */ }
impl SendBuffer { pub fn bytes_mut(&mut self) -> &mut [u8]; pub fn truncate(&mut self, len: usize); }

pub struct Op<T>(Arc<OpCell<T>>);
impl<T> Op<T> { pub fn poll(&self, cx: &mut Context<'_>) -> Poll<Result<T>>; }
```

`Op` drop semantics: dropping an `Op<Arrival>` before it resolves abandons
the grant. The owner keeps the tag until the peer's write with immediate or
`Release` arrives, then frees the buffer without exposing it. If neither
arrives by the grant deadline, the owner breaks the link, because
invalidation alone cannot fence an exposed window (see "Memory safety
rules"). Dropping `Op<Granted>` before the bind completes frees the buffer
after the bind CQE and breaks nothing if the `Grant` was not yet posted. Dropping `Op<()>` for a write does not
cancel the posted WR; the owner keeps the source until its completion.

As built (`src/node.rs`, stage 2e), the API differs from the sketch above:

- `node(workers) -> Result<(Vec<Client>, OwnerPort)>`; `Owner::new(port)` on
  the native thread captures its environment (and simulation). Host it with
  `WithNative::with_host(inner, owner)`: `WithNative<T, N>` is generic over
  the `NativeHost` trait (`register_driver`, `poll_budgeted`, `close`,
  `drained`). There is no `WithNative::new`. `Owner::poll_budgeted`
  handles up to `budget` commands, then progresses every link, runs timers,
  and advances shutdown; completions and timers need periodic polls.
- `Client` is `Send` (it holds only `Arc`s), not `!Send`.
- `activate(config) -> Op<Vec<Selection>>` instead of `Result<()>`, so the
  caller learns the selected rails. `NodeConfig::selector` is a `FnOnce`
  returning `(rail: u16, index)` pairs (the old slot selector's shape), and
  `NodeConfig` carries `mr_remote_write`.
- The link id is the caller's: `accept(key, remote, link_id, initiator)` and
  `connect(key, epoch, remote, link_id)` take it, because Racer derives it
  with SHA-256 from the signed setups. `LinkState::Ready { epoch, link_id }`
  reports it, and `LinkState::Breaking` covers closing and destroying links.
- "Busy" is `Error::Overloaded`: Racer's error mapping is exhaustive over
  `rdma_verbs::Error`, and `Overloaded` already means "use HTTP". `open`
  returns it when a link or setup for the key exists, while backing off, or
  when the rail has `max_links` links. A full command queue also returns it;
  `Client::poll_ready` waits for room.
- `send_buffer(rail, len, cx)` takes the rail (pools are per device) and a
  `Context` to wait on. `buffers(rail, state)` reports pool counts.
- `close`, `closed`, `drained`, and `reopen` follow the old slot API's rules;
  there is no `register_driver` on `Client` (each `Op` wakes its poller).
- `break_link` never loses the request: if the queue is full it goes to a
  shared list the owner drains.
- Shutdown sends `Close` on ready links and breaks each one once the `Close`
  completes or after `CLOSE_GRACE` (1 s); other links break at once. The
  node is drained when every link is destroyed and no page or send buffer is
  held; a leaked object keeps it from ever draining.
- Dropping the `Owner` fails every op (`Unavailable` for link ops,
  `Cancelled` for queued commands), destroys links (leaking what cannot be
  destroyed), and leaks pools whose pages are still held.

Removed types: `Slot`, `Lease`, `Mailbox`, `QueuePairHandle`, `Region`,
`Window`, `Ticket`, `Transfer`, `Receive`, `QpSet`, `IoPort`, `NativePort`,
`DeviceHandle`, `pair(slots)`, `Configuration`, and the `testing` hooks.
`NativeService` is replaced by `Owner`. `Endpoint`, `PortInfo`, `Selection`,
`Guard`, `Error`, `inventory`, `discovery`, and `WithNative` stay.

## Page buffer pool (`src/pool.rs`)

Per selected device, the owner registers one region of
`pool_bytes / buffer_len * buffer_len` bytes, 4 KiB aligned, with
`LOCAL_WRITE | MW_BIND` and no remote access (see "Least access" for the
fallback). It is split into `n` page buffers of `buffer_len` each. A second,
small region (`LOCAL_WRITE` only) holds control slots: `64 B * (send_depth +
recv_depth)` per link.

Each page buffer has one state, kept in an `AtomicU8` array in `PoolShared`
plus owner-only bookkeeping:

| State | Holder | CPU access | NIC access |
|---|---|---|---|
| `Free` | pool free list | none | none |
| `Staging` | `SendBuffer` (I/O or crypto) | `&mut [u8]` | none |
| `Sending` | owner, posted `WRITE_WITH_IMM` | none | local read |
| `Binding` | owner, posted `BIND_MW` | none | none yet |
| `Exposed` | owner, rkey sent in `Grant` | none | remote write |
| `Invalidating` | owner, posted `LOCAL_INV` | none | remote write until CQE |
| `Owned` | `RegisteredPage` | `&[u8]` | none |
| `Quarantined` | owner, link breaking | none | maybe, until QP destroyed |
| `Leaked` | nobody, counted forever | none | maybe |

Transitions:

- `Free -> Staging`: `Client::send_buffer`. `SendBuffer` drop returns `Free`.
- `Staging -> Sending`: `write(Source::Staged)`. With `Source::Bytes` the owner
  takes a `Free` buffer and copies on its own thread (`Free -> Sending`).
- `Sending -> Free`: send completion, success or flush error.
- `Free -> Binding -> Exposed`: `grant`, after the bind CQE succeeds and the
  `Grant` message is posted.
- `Exposed -> Invalidating`: receive completion for the tag (write with
  immediate or `Release`).
- `Invalidating -> Owned`: `LOCAL_INV` CQE success. With `Arrival::Released`
  the owner goes straight to `Free` instead.
- `Owned -> Free`: `RegisteredPage` drop pushes the index onto the free list
  (a `Mutex<Vec<u32>>`; the critical section is one push, so `lock()` is used,
  not `try_lock`).
- `Binding | Exposed | Invalidating -> Quarantined`: any link break, cancel,
  deadline, or failed CQE on that link.
- `Quarantined -> Free`: QP destroyed successfully (`rdma_verbs_stop` returns 0)
  and the MW invalidated or deallocated.
- `Quarantined -> Leaked`: destroy failed. The guard leaks with it (existing
  rule, `src/ffi.rs:1-5`).

`RegisteredPage` and `SendBuffer` hold an `Arc<PoolShared>`. The region is
deregistered only after every `Arc` is gone and every link is destroyed, so
a `RegisteredPage` held by the cache cannot dangle.

Pool details (as built, `src/pool.rs`): owner-held states are a `Buffer`
token with no slice or pointer API; only `SendBuffer` (`Staging`) and
`RegisteredPage` (`Owned`) expose bytes. `BufferState` is public for
diagnostics. Dropping the owner's `Pool` while any page or send buffer is
alive, or while any buffer is `Quarantined` or `Leaked`, leaks the region and
quarantines its charge instead of deregistering, so pages never dangle.
Waiting clients register through `PoolShared::poll_stage`.

Windows: the owner allocates `grants_per_link * max_links` type 2 MWs per
device at activation, kept on a free list. Each bind uses
`ibv_inc_rkey(previous)`, so every grant has a fresh rkey and a stale rkey
from an earlier grant can never match.

## Links (`src/link.rs`)

Each link is one RC QP with its own CQ (kept per QP so the existing
destroy-ordering code in `src/ffi.rs` is unchanged), `send_depth` and
`recv_depth` from "Credits", and a control slot ring.

Per-link owner state:

```rust
struct Link {
    key: LinkKey, epoch: Epoch, qp: ffi::QueuePair,
    state: State,                          // Connecting | Ready | Breaking | Closed
    credits: u32,                          // peer receives we may still consume
    owed: u32,                             // receives we reposted but not reported
    next_tag: u32,                         // receiver-side tag allocator
    grants: Slab<GrantEntry>,              // tags we issued (receiver side)
    remote: HashMap<u32, RemoteGrant>,     // grants the peer issued (sender side)
    waiting: Vec<PendingWrite>,            // writes waiting for their Grant
    sends: u32, last_used: Instant,
}
```

Tags are `u32`, allocated by the receiver per link, nonzero, skipping tags
still in `grants`. `imm_data = tag` (network order on the wire per verbs).

Credits:

- Each side posts `recv_depth = 2 * grants_per_link + 4` receives at connect
  and advertises it in `Setup.recv_credits`. `credits` starts at the peer's
  value.
- Every `SEND` and `WRITE_WITH_IMM` consumes one credit. A message is never
  posted with `credits == 0`.
- One credit is reserved for `Credit` messages. Data writes, `Grant`, and
  `Release` need `credits >= 2`; if not, they wait in `waiting` until credits
  return or their deadline passes.
- On each receive completion the owner reposts that receive immediately and
  increments `owed`. Every outgoing control message carries `credits = owed`
  and resets it. When `owed >= recv_depth / 2` and nothing else is queued,
  the owner sends a standalone `Credit` message.
- A peer that exceeds its credits causes RNR. QPs connect with
  `rnr_retry = 0`, so that surfaces as an error completion and breaks the link
  instead of stalling.

Grant concurrency: `grants` is capped at `grants_per_link`. A peer whose
`Grant` would exceed our advertised `grants_per_link` (the sender-side table
`remote` is full) breaks the link.

Send queue depth: `send_depth = recv_depth + 2 * grants_per_link + 4`
(controls, writes, binds, invalidates). All send WRs are signaled.

Links as built (`src/link.rs`), where they refine or differ from the above:

- Each link allocates its own `grants_per_link` MWs and its own small
  `LOCAL_WRITE` control region (`64 B * 2 * recv_depth`) instead of drawing
  from device-wide pools. They are freed with the link, after the QP is
  destroyed, which keeps the destroy ordering local to one link.
- Work waits in one outbox. Data (`Grant`, `Release`, write) keeps its order
  and needs two credits; `Credit` and `Close` use the reserved credit and may
  pass blocked data. A standalone `Credit` is queued when `owed >=
  recv_depth / 2`, none is queued yet, and either no other control is queued
  or data is starved of credits (otherwise both sides could wait forever).
  Binds and invalidates need no credit and pass blocked data.
- The receiver also tracks credits it has not returned; a peer returning
  more than it consumed is a credit violation and breaks the link.
- `grant` fails `Overloaded` if the link already holds `grants_per_link`
  grants or no page buffer is free. Its op resolves once the `Grant` is
  posted. A grant still waiting to post its `Grant` at its deadline is
  invalidated and fails `DeadlineExceeded`, without a break.
- `write` with `Source::Bytes` fails `Overloaded` if no page buffer is free
  (it does not wait). A write or release whose `Grant` has not arrived waits
  for it; at most `2 * grants_per_link` may wait (`Overloaded` beyond). A
  release replaces a waiting write for the same tag (the write fails
  `Cancelled`).
- A write longer than the grant fails `InvalidRange` and leaves the grant in
  place so the caller can release it.

## Control messages (`src/control.rs`)

Fixed 64 bytes, network byte order, sent with `IBV_WR_SEND` from the control
region, received into control slots:

| Offset | Size | Field |
|---|---|---|
| 0 | 1 | version = 1 |
| 1 | 1 | kind: 1 `Grant`, 2 `Release`, 3 `Credit`, 4 `Close` |
| 2 | 2 | reserved, zero |
| 4 | 4 | credits returned |
| 8 | 8 | epoch (sender's view of the link epoch pair, see below) |
| 16 | 4 | tag |
| 20 | 4 | rkey (`Grant` only) |
| 24 | 8 | remote address (`Grant` only) |
| 32 | 4 | length (`Grant` only, max bytes the peer may write) |
| 36 | 4 | reserved, zero |
| 40 | 16 | transfer id (`Grant` and `Release`) |
| 56 | 8 | reserved, zero |

`epoch` on the wire is the 64-bit link id agreed in the handshake (low 64
bits of the session binding hash), not the local `Epoch` counter.

Decoding rejects: wrong length (`byte_len != 64`), unknown version or kind,
nonzero reserved bytes, wrong link id, fields not valid for the kind, a
`Grant` with zero length or length above `buffer_len`, a `Release` or write
with immediate for an unknown tag, and a duplicate tag. Every rejection
breaks the link. Fields not valid for the kind must be zero: `Release` has no
rkey, address, or length; `Credit` and `Close` also have zero tag and transfer
id. `Grant` and `Release` need a nonzero tag. The codec checks everything that
needs no link state; unknown and duplicate tags are checked by the link layer.
None of them can make the receiver accept bytes: acceptance
still needs the signed HTTP head and AEAD in Racer.

Removed from the earlier plan, with reasons:

- `Complete`: the write with immediate completion replaces it.
- `Done`: the sender frees its source on its own send CQE, and the HTTP
  response head is the authoritative outcome. Nothing waits for `Done`.
- `Abort` (receiver revokes a grant): invalidation without a preceding
  receive completion is not a fence, because a multi-packet write may still
  be in flight. A receiver that must abandon an exposed grant breaks the link
  instead. The sender's decline is `Release`, which is a fence because it is
  ordered after every write the sender posted on the same QP.

## Memory safety rules

**Fence on the normal path.** A receive completion for the tag (either opcode)
proves that every earlier RDMA write on that QP has been placed. The owner
then posts `LOCAL_INV` with `IBV_SEND_FENCE | IBV_SEND_SIGNALED`. Only its
successful CQE moves the buffer to `Owned`. A late write using the old rkey
then fails with a remote access error, which breaks the link on both sides.

**Fence on every other path.** Cancel, deadline, any error CQE, any
unexpected opcode, wr id, tag, or byte length, decode failure, credit
violation, or owner shutdown breaks the link: modify to ERR, destroy the QP,
then move its quarantined buffers to `Free`. If destroy fails, the buffers,
MWs, link, and guard are leaked and stay counted (existing rule,
`src/ffi.rs:1-5`).

**Rust sees registered memory only through `RegisteredPage` and
`SendBuffer`.** Both exist only in states with no valid MW and no posted WR.
There is no API that returns a pointer or slice for any other state.

**Least access.** The QP grants `IBV_ACCESS_REMOTE_WRITE`. The page region is
registered `LOCAL_WRITE | MW_BIND`; remote write is granted only through a
window of exactly the granted length. If a provider rejects a remote-write
bind on an MR without `REMOTE_WRITE`, the owner fails activation with
`Unsupported`, and a `NodeConfig::mr_remote_write` escape hatch re-adds the
flag. This must be checked on target hardware (see risks in
`racer-rdma-sessions.md`). Control slots use `LOCAL_WRITE` only.

**Random starting PSN and QPN.** Unchanged (`src/ffi.rs`). The link id in
every control message is bound to the signed handshake.

## Native adapter ABI version 2

`rdma_verbs_abi()` returns 2. Rust refuses any other value. Changes:

```c
struct rdma_verbs_wc {          /* 32 bytes */
    uint64_t id;
    uint32_t status, opcode;    /* opcode UINT32_MAX unless status == SUCCESS */
    uint32_t byte_len, imm;     /* imm valid only if flags & 1 */
    uint32_t flags, vendor_err;
};
_Static_assert(IBV_WC_SEND == 0 && IBV_WC_RDMA_WRITE == 1 && IBV_WC_BIND_MW == 5 &&
               IBV_WC_LOCAL_INV == 6 && IBV_WC_RECV == 128 &&
               IBV_WC_RECV_RDMA_WITH_IMM == 129, "completion opcode ABI");

enum { RDMA_VERBS_MR_PAGES = 1, RDMA_VERBS_MR_CONTROL = 2, RDMA_VERBS_MR_REMOTE_WRITE = 4 };

void *rdma_verbs_qp(struct rdma_verbs_device *, uint8_t port, uint32_t send_depth,
                    uint32_t recv_depth, uint32_t *qpn);            /* changed */
int rdma_verbs_connect(...);                    /* rnr_retry = 0, else unchanged */
void *rdma_verbs_register(struct rdma_verbs_device *, uint64_t length,
                          uint32_t access);                         /* changed */
int rdma_verbs_bind(struct rdma_verbs_qp *, struct ibv_mw *, struct rdma_verbs_mr *,
                    uint32_t key, uint64_t id, uint64_t offset, uint32_t length);
int rdma_verbs_write_imm(struct rdma_verbs_qp *, struct rdma_verbs_mr *, uint64_t offset,
                         uint32_t length, uint64_t address, uint32_t key, uint32_t imm,
                         uint64_t id);                              /* new */
int rdma_verbs_send(struct rdma_verbs_qp *, struct rdma_verbs_mr *, uint64_t offset,
                    uint32_t length, uint64_t id);                  /* new */
int rdma_verbs_post_recv(struct rdma_verbs_qp *, struct rdma_verbs_mr *, uint64_t offset,
                         uint32_t length, uint64_t id);             /* new */
```

`rdma_verbs_write` (plain write) stays in ABI 2. Links never use it, but
the adapter and simulator tests still drive whole-region bind, plain write,
and invalidate through a `cfg(test)` ticket path in `src/ffi.rs` to check
window and fence rules. Removing it needs those tests ported to write with
immediate first. `rdma_verbs_invalidate`,
`stop`, `qp_free`, `deregister`, `bytes`, `window`, `window_free`,
`discover`, `open`, `close`, and `poll` (with the new struct) keep their
contracts. Every offset/length pair is bounds-checked in C against the MR
(`EINVAL`), and again in Rust before the call.

`wr_id` encoding (Rust only): bits 63..56 kind (send ctl, recv ctl, write,
bind, inv), 55..32 link slot, 31..0 buffer, control slot, or tag. Any CQE
whose decoded kind, link, or index does not match an outstanding WR breaks
the link.

`imm` is passed and reported in host byte order; the adapter does the
`htonl`/`ntohl`. `byte_len`, `imm`, and `flags` are zero unless the status
is success.

Rust wrappers (`src/ffi.rs`): `NativeQueuePair::new_with_depth`, `send`,
`post_recv`, `write_imm`, `bind_window`, `invalidate_window`, and
`progress_with`, plus `MemoryWindow` (reusable MW that tracks its next rkey)
and `NativeRegion::with_access`. Ranged posts take the caller's `wr_id`,
which must have a nonzero kind byte (`CALLER_ID_MASK`) so it never collides
with the test harness's sequential ids. The QP still keeps a
per-id table of outstanding WRs and breaks on any CQE that does not match
its id, queue (send or receive), opcode, immediate flag, or receive length;
`NativeQueuePair::failure` reports the first such CQE (for example
`IBV_WC_RNR_RETRY_EXC_ERR`). Ranged posts do not use the test harness's
`busy` flag; the page pool owns per-buffer state.

The `_Static_assert`s and the Rust layout test cover the new struct and
opcodes. `native/discovery_faults.c` is unchanged.

## Simulation parity (`src/simulation.rs`)

The simulator implements every ABI 2 function with the same contracts:

- Per-QP receive queue. `SEND` and `WRITE_WITH_IMM` consume the peer's head
  receive; none posted means RNR, which with `rnr_retry = 0` produces
  `IBV_WC_RNR_RETRY_EXC_ERR` on the sender and moves both QPs to ERR.
- Writes and sends on one QP are delivered in order. Writes check the
  window: valid rkey, bound range, MW not invalidated; otherwise
  `REM_ACCESS_ERR` on the sender, both QPs to ERR, nothing written.
- `LOCAL_INV` with fence completes after all earlier WRs on the QP.
- ERR flushes every posted WR with `WR_FLUSH_ERR`.
- MR access flags are recorded; binding a remote-write MW on an MR without
  `MW_BIND` fails; the remote-write-without-MR-remote-write rule is a
  simulator option so both provider behaviors are tested.
- Fault injection: fail any call, any CQE status, drop or reorder a packet
  between QPs (reorder must still be refused by the fence checks), delay
  delivery, duplicate an immediate, corrupt `byte_len`.

Implementation notes (`src/simulation.rs`, module docs):

- Send work runs in post order when the posting QP polls; receive
  completions queue on the peer and are reported by its next poll.
- A `WRITE_WITH_IMM` that passes the window check places its data before it
  looks for a receive, so an RNR can leave data behind, as a multi-packet
  write can on hardware. The receiver QP is in ERR either way.
- Until a QP is destroyed, plain RDMA writes through a live grant still land,
  even if the target QP is in ERR. This keeps tests conservative about late
  DMA after a failed fence.
- Window keys are `index << 8 | generation`; a bind must use the window's
  index and a generation different from its last bind.
- Receives may be posted before connect, matching the verbs INIT state, so
  the initial `recv_depth` receives exist before the peer can send.
- The fault knobs are `Fault::Reject`, `Delay(n)`, `Completion(status)`,
  `Drop`, `Reorder`, `DuplicateImmediate`, and `ByteLength(n)`, and
  `Simulation::require_mr_remote_write` selects the provider bind rule.

The simulator remains one fabric per test thread and deterministic.

## Shutdown

`Client::close` sends `Close` to the owner. The owner sends `Close` control
messages on ready links, breaks every link, waits for destroys, deallocates
MWs, deregisters regions once no `RegisteredPage` or `SendBuffer` remains,
and only then marks the node drained. Reopen is blocked until
drained. `WithNative` keeps the rule that NIC shutdown ignores scope deadlines.

## Testing

See "Test plan" in `racer-rdma-sessions.md`. The crate-level CI matrix
(none, `simulation`, `native`, both) is unchanged.
