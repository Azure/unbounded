# Racer persistent bidirectional RDMA sessions

Status: implemented (stages 1 to 5 done). The verbs crate side is in
[racer-verbs.md](racer-verbs.md). This file covers threading, the link
handshake, the per-page protocol, Racer types, zero-copy, configuration,
deletions, tests, and the implementation plan. The code is authoritative;
see "Deviations in the implementation". Hardware validation is still open
(see "Contradictions with the plan and open risks").

Paths are relative to `cmd/racer-dataplane/` unless noted.

## Why

Today every 16 MiB page (`object-wire/src/model.rs:150`) that goes over RDMA:

- leases a slot, creates a new QP, and runs a signed SETUP/SETUP_BINDING
  exchange (`src/rdma.rs:192-333`, `src/peer/transport.rs:279-407`);
- adds Offer, Setup, Ready, Grant, Complete, Done, and Finish signed HTTP
  heads on top of the request and response (`src/peer/transport.rs:279-475`,
  `654-925`);
- destroys the QP as the only fence (`verbs/native/verbs.c:173-184`);
- copies the page four times: page to staging, staging to registered,
  registered to staging, staging to `Vec` (`src/rdma.rs:748-766`,
  `src/rdma.rs:690-695`);
- funds roughly two slots per I/O worker, because each slot charges a
  registered buffer and a staging buffer (`src/rdma.rs:787-791`,
  `src/app/native.rs:25-27`).

The target removes per-page QP setup and teardown, all extra HTTP round
trips, and all staging copies, and replaces per-worker slots with a node-wide
buffer pool.

## Overview

- One RC QP per (peer, rail) per node, used in both directions. Its state
  lives in a node-wide link table owned by one native thread.
- The link is set up once with a signed HTTP exchange, then reused until
  error, idle timeout, or shutdown.
- Per page, the receiver grants a window in-band and names it in the signed
  HTTP request. The sender writes with immediate (or releases) and then sends
  the signed HTTP response head, which is the authoritative outcome. There is
  no HTTP round trip beyond the existing request and response.
- Fallback to an HTTP body is decided by the sender and carried in the same
  response, as today.

## Threading

Decision: one `Owner` per node, hosted by `WithNative` on the crypto thread
paired with I/O worker 0. Every I/O worker gets a `Client`. No new threads.

Why:

- A per-node QP needs one place that posts to it and tracks its credits,
  tags, and grants. Posting from several workers would need locks around the
  QP and shared credit counters. A single owner keeps all link state
  single-threaded.
- `WithNative` already composes a native service with a crypto engine on the
  crypto thread (`src/rdma.rs:62`, `src/app/native.rs:84-99`), and the
  service loop, budgeting, and drain rules carry over unchanged.
- Owner work per page is a few posts and completions. The one heavy step,
  the sender's copy into registered memory, moves to the sending worker's
  paired crypto thread in stage 4 (see "Zero-copy").
- Crypto threads are shared by several I/O workers (`src/worker.rs:181-187`),
  so the owner shares its thread with crypto for some workers. That is
  acceptable for a few posts per page. If profiling shows contention, the API
  allows one owner per device later: `LinkKey` includes the rail, and
  `node()` can return one owner port per device without changing `Client`.

Hand-off:

- I/O to owner: one bounded command queue per worker (capacity 64,
  `Mutex<VecDeque<Command>>` taken with `try_lock`, retried via
  `NativeRetry`, `src/rdma/retry.rs`), plus the owner's `AtomicWaker`.
  A full queue returns `Overloaded`, which Racer treats as "use HTTP".
- Owner to I/O: each command carries an `Op<T>` one-shot cell
  (`Arc<Mutex<Option<Result<T>>>>` + `AtomicWaker`). The worker polls only
  the ops it owns.
- Link table snapshot for fast checks: `Arc<RwLock<HashMap<LinkKey,
  LinkState>>>` written by the owner, read with `try_read` by workers. A
  failed `try_read` reads as "not ready", which means HTTP for that page.
- Buffers cross threads as `RegisteredPage` and `SendBuffer`, which are
  `Send` and hold `Arc<PoolShared>`.

## Link table and state machine

Owner-held, keyed by `LinkKey { peer: SHA-256(NodeId), rail }`:

```
            open (we initiate)              connect ok + hello sent
  Idle ───────────────────────▶ Connecting(e) ─────────────────────▶ Ready(e)
   ▲ ▲  accept (peer initiates): QP to RTS, wait for peer hello ─────▶ │
   │ │                                                                 │
   │ └──── peer Close / idle close (after destroy) ◀── Breaking(e) ◀──┤ error, cancel,
   │                                                     │             │ deadline, bad CQE
   └──── backoff expired ◀── Backoff(until) ◀────────────┘ (on error)  │
                          Connecting: 5 s timeout or error ─▶ Breaking ┘
```

- `Connecting` covers both roles. The initiator becomes `Ready` after
  `connect` succeeds and its hello (`Credit` message with full credits) is
  posted. The responder becomes `Ready` on the first receive completion from
  the initiator. Neither side sends data until `Ready`.
- `Breaking`: QP moved to ERR, destroy pending. Quarantined buffers wait here.
- `Backoff`: 1 s, doubling to 60 s, plus or minus 20 % jitter using
  `uring_runtime::environment` randomness. It resets after a link has been
  `Ready` for 60 s. Backoff throttles only our own `open`; an authenticated
  incoming setup is accepted in any state.
- Idle close: a `Ready` link with no grant, write, or release for 120 s sends
  `Close`, then breaks after 1 s. A peer that receives `Close` breaks its side
  and returns to `Idle`, not `Backoff`.
- New offer while `Ready`: the peer lost the link (restart or break we have
  not seen yet). Accept it and break the old link.

Tie-break on simultaneous setup: the lower NodeId's offer wins (byte order of
the NodeId string). When a node in `Connecting` as initiator receives an
offer from the same peer for the same rail:

- peer NodeId lower than ours: break our attempt, accept the peer's;
- otherwise: reject with `409 Conflict`. The peer will accept our offer when
  it arrives by the same rule.

Who initiates: any worker that needs a link and sees `Idle`. `Client::open`
returns `Busy` if another worker already started, so at most one setup runs
per key. The initiator holds its per-key reservation in drop guards from
`open` through the HTTP exchange and `connect`, so a cancelled or failed setup
clears only its own reservation and breaks only its own epoch. If `open` is
dropped while the owner is still opening, the owner sees the abandoned op and
breaks the link it opened; if the owner already answered but the result was
never read, the reservation guard takes the result and breaks that epoch. The
link then backs off instead of sitting in `Connecting` until its setup
deadline, and a later attempt for the same peer and rail proceeds after the
backoff. `LinkNode::reset` does not touch reservations: every entry is owned by
a live guard that clears it. Pages never wait for a link: while it is not `Ready`, pages use HTTP.

## Link setup over HTTP

A new signed peer request on the existing peer HTTP server, independent of
page requests:

- `peer-wire`: new request kind `RdmaLink` (`POST /racer/v1/rdma/link`),
  no body. Request and response heads carry `racer-rdma-setup` and, in the
  response, `racer-rdma-setup-binding`. Both must be explicitly signed; the
  existing check is reused (`src/rdma.rs:143-164`).
- Setup value (base64 of):

  ```
  "racer-rdma-setup-v2\0" | rail u16 | nonce [16] | endpoint [32]
                          | recv_credits u32 | grants_per_link u32
  ```

- Binding value: `SHA-256(initiator setup bytes)`, as today
  (`src/rdma.rs:127-129`).
- Link id: first 8 bytes of
  `SHA-256("racer-rdma-session-v2\0" || min(setups) || max(setups))`.
  Every control message carries it (`racer-verbs.md`, "Control messages").

Flow:

1. Initiator worker: `Client::open(key)` returns `Opened { epoch, setup }`.
   Send the signed `RdmaLink` request with that setup. Deadline 5 s.
2. Responder: the peer server verifies the signed head, checks the peer is a
   same-Site member with the rail (`select_hop` membership rules,
   `src/rdma.rs:1156-1222`), applies the tie-break, then
   `Client::accept(key, remote, initiator = false)`: creates the QP, posts
   receives, and moves it to RTS. Respond signed with its setup and binding.
3. Initiator: verify binding, reject if the responder's setup equals ours
   (`Replay`, as `src/rdma.rs:302-304`), then `Client::connect(key, epoch,
   remote)`. On any failure the link goes to `Backoff`.

Rate limit: the responder accepts at most one setup per (peer, rail) per
second. Excess requests get `429` and do not touch the owner.

## Per-page protocol

Roles follow data flow: the HTTP requester B receives, the HTTP responder A
sends.

1. B builds the page request (`accept_native`, `src/peer/transport.rs:546`).
   If the plan is `Rdma { rail }` and the link is `Ready(e)`, B calls
   `Client::grant(key, e, transfer, MAX_CIPHERTEXT, deadline)` where
   `deadline = native_scope(scope)` (`src/peer/transport.rs:198-205`, kept
   at 5 s). The owner binds a window and posts `Grant`. B adds a signed
   `racer-rdma-grant` extension to the `Accept` control:

   ```
   "racer-rdma-grant-v2\0" | link_id u64 | tag u32 | length u32 | rail u16
   ```

   If grant fails or the link is not ready, B sends a plain `Accept` (HTTP).
2. A verifies the request (`admit_native`, `src/peer/transport.rs:579`). If
   A has a ciphertext response, the plan matches, and its link for (B, rail)
   is `Ready` with the same link id, A calls `Client::write(key, e, tag,
   source, deadline)` with `native_scope`. The owner waits for the in-band
   `Grant` with that tag (it may arrive after the HTTP request), checks
   `length <= granted`, posts `WRITE_WITH_IMM`, and resolves on its CQE.
3. A responds with one signed head:
   - success: `Phase::Complete` with
     `racer-rdma-complete = "racer-rdma-complete-v2\0" | link_id | tag |
     ciphertext_length u32 | transfer [16]`, body length 0;
   - otherwise: `Client::release(tag)` if the link is still up, then the
     existing fallback head and body (`send_fallback`,
     `src/peer/transport.rs:511-545`, `Phase::Finish` with length).
   A always does exactly one of write or release for a granted tag, except
   when the link breaks, which fences everything.
4. B reads the response head:
   - `Complete`: await `Arrival::Page` (bounded by the same 5 s deadline),
     check `ciphertext_length <= page length` and the transfer id, and build
     the `CiphertextPage` (see "Zero-copy"). AEAD verification happens where
     it does today, after transport returns.
   - fallback: drop the arrival op. The owner keeps the tag until `Release`
     or a write with immediate arrives, then frees the buffer; if neither
     arrives by the grant deadline, it breaks the link.
   - `Complete` but the arrival fails (link broke after A's CQE): the exchange
     fails with `Error::Io`. The HTTP body was not sent, so this request
     cannot fall back in place; the caller's normal peer error handling
     applies.

Old flow: request, response head plus five more signed heads and one HTTP
round trip per head pair. New flow: request and response only, plus two
in-band messages (Grant one way, write with immediate the other).

Security, as accepted in the plan: in-band messages are protected only by
RC transport (random PSN and QPN, link id bound to the signed handshake).
A forged or replayed control message can cause a failure, a fallback, or a
link break, never acceptance: B accepts bytes only when the signed HTTP head
names the same link id, tag, transfer id, and length, and AEAD verifies them.

## Timeouts and errors

- Grant deadline: `native_scope` (5 s, or less if the request deadline is
  sooner). Expiry of an `Exposed` grant breaks the link.
- Pending write waiting for its `Grant`: same deadline; expiry fails the
  write op, and A falls back after `release`.
- Ambiguous write: if cancellation or the request deadline wins after
  `Links::send` submitted the write, the owner may already have consumed the
  tag, and a `Release` for it would wait forever for a `Grant` that never
  comes. `Links::send` breaks that link epoch with `break_link` instead; the
  later `release` from `send_native` then lands on a broken epoch.
- Unadmitted release: `Client::release` fails at once when the worker queue
  is full or the owner is gone, and then sends nothing. `Links::release`
  checks that result and falls back to `break_link`, which never drops (a
  full queue spills to the node's shared break list).
- Setup: 5 s.
- Any error CQE, unexpected opcode or wr id, decode failure, unknown or
  duplicate tag, credit or grant-cap violation, RNR, or `Close` breaks the
  link. All ops on it fail with `Unavailable`. Racer maps `Unavailable`,
  `Overloaded`, `Io`, and `DeadlineExceeded` to HTTP fallback where the
  protocol still allows it (`recoverable` and `native_failure`,
  `src/peer/transport.rs:192-208`, kept).
- Shutdown: `Links::drain` closes the client; the owner breaks every link and
  drains as in `racer-verbs.md`. `WithNative` drain and fence close the owner,
  then drive it and the crypto engine's drain or fence together until both
  finish (`verbs/src/lib.rs`, `close_alongside`). A queued crypto job (a Stage
  or a registered-page decrypt) can own a pool buffer the owner waits for, so
  draining the owner first would deadlock.

## Racer types (`src/rdma.rs`)

Replaces `Sessions`, `PreparedSession`, `SessionLease`, `SetupParameters`,
`Grant`, `RemoteDescriptor`, `AuthenticatedDescriptor`, `SendCompletion`,
`RegisteredLease`, and the `send_to`, `prepare_receive`, `finish_receive`
methods.

```rust
pub const SETUP_HEADER: &str = "racer-rdma-setup";
pub const SETUP_BINDING_HEADER: &str = "racer-rdma-setup-binding";
pub const GRANT_HEADER: &str = "racer-rdma-grant";
pub const COMPLETION_HEADER: &str = "racer-rdma-complete";

/// Per-worker facade over the node owner.
pub struct Links {
    client: Rc<rdma_verbs::Client>,
    devices: Rc<Devices>,            // rail mapping and publication, kept
    local: NodeId,
    retry: Rc<NativeRetry>,
}
impl Links {
    pub fn ready(&self, peer: &NodeId, rail: RailId) -> Option<LinkId>;
    /// Start setup in the background if Idle and not backing off.
    pub fn ensure(&self, peer: &VerifiedPeer, rail: RailId, http: &Transfers);
    pub fn grant<'a>(&'a self, peer: &NodeId, rail: RailId, transfer: TransferId,
                     scope: &'a RequestScope) -> Operation<'a, PendingReceive>;
    pub fn send<'a>(&'a self, peer: &NodeId, grant: &AuthenticatedGrant,
                    page: &CiphertextPage, scope: &'a RequestScope)
                    -> Operation<'a, NativeSent>;
    pub fn release(&self, peer: &NodeId, grant: &AuthenticatedGrant);
    pub fn accept_setup(&self, head: &VerifiedHead, rail: RailId) -> Result<LinkSetup>;
    pub fn drain(&self) -> Operation<'_, ()>;
}
pub struct LinkSetup { /* encoded setup bytes, binding */ }
pub struct PendingReceive { link: LinkId, tag: u32, transfer: TransferId,
                            arrival: rdma_verbs::Op<Arrival> }
impl PendingReceive {
    pub fn header_value(&self) -> Vec<u8>;
    /// Requires the signed COMPLETION_HEADER to match link, tag, transfer.
    pub fn finish<'a>(self, head: &'a VerifiedHead, envelope: PageEnvelope,
                      pool: &BufferPool, scope: &'a RequestScope)
                      -> Operation<'a, CiphertextPage>;
}
pub struct AuthenticatedGrant { link: LinkId, tag: u32, length: u32, transfer: TransferId }
impl AuthenticatedGrant { pub fn from_verified(head: &VerifiedHead, peer: &NodeId,
                                               transfer: TransferId) -> Result<Self>; }
pub struct NativeSent { /* COMPLETION_HEADER value */ }
```

`Devices`, `Inventory`, `select`, `select_hop`, `select_worker`,
`match_publication`, and `signed_value` stay. `select_worker` becomes
`select_node` (no worker index) since devices are now node-wide.

`src/peer/transport.rs`: `send_native` and `receive_native` shrink to the
flow above. Delete `send_native_page`, `receive_native_page`,
`failed_then_fallback`, and `fence`. `receive_fallback` and `send_fallback`
stay. `peer-wire/src/control.rs` drops `Phase::Offer`, `Setup`, `Ready`,
`Grant`, `Done`, `Failed` once unused (stage 3d); `Accept`, `Complete`,
`Fallback`, `Finish` stay.

## Zero-copy

Facts that shape this (see "Contradictions"): ciphertext is produced once and
reused verbatim for peers and disk (`src/security.rs:348`). A sender's page
is a cached or disk-read `CiphertextPage` (`src/memory.rs:77-96`); no
per-hop encryption exists to redirect into registered memory.

Receive (zero copies):

- New `CiphertextStorage::Registered { page: rdma_verbs::RegisteredPage,
  len: usize }` in `src/memory.rs`, and `BufferPool::registered_ciphertext(
  reservation, envelope, page)`. The ciphertext admission reservation is
  still taken (`ResourceClass::Ciphertext`), so cache accounting is
  unchanged; it double-counts pool memory, which is conservative.
- Decrypt reads directly from it on the crypto thread (`PageCrypto::decrypt`,
  `src/security.rs:316`); `CiphertextPage` is already `Arc` and `Send`.
- Retention must not pin pool buffers. Every path that retains ciphertext
  past the request (memory cache publish, `src/memory.rs:645`; store writes;
  rehome, `src/memory.rs:277-301`) copies `Registered` into an owned or slab
  buffer. `rehome` never moves `Registered` in place.
- A relay forwarding the page uses it as the next hop's `Source` directly.

Send (one copy, off the I/O thread):

- Stages 2 and 3: `Source::Bytes(page)`; the owner copies into a free page
  buffer on its thread. This already removes the staging copy.
- Stage 4: new `CryptoInput::Stage { ciphertext, into: SendBuffer }` on the
  sending worker's paired crypto thread copies (and can verify the stored
  checksum on the way). `write` then takes `Source::Staged`. The owner thread
  then does no bulk copies.
  Implemented in stage 4b: `Links::send` takes a free buffer without waiting
  (`Client::send_buffer` with a no-op waker), copies on the crypto thread
  (`CryptoInput::Stage { ciphertext, buffer }`, `PageCryptoEngine::stage` in
  `src/security.rs`), and writes `Source::Staged`. See deviation 8.
- Full zero-copy send would need cache slabs allocated from registered
  memory (`alloc` crate). Out of scope; noted as follow-up.

Deleted copies: `Region::poll_copy_from` and `poll_copy_to` staging
(`verbs/src/lib.rs` `Mailbox::bytes`), `RegisteredLease::copy_from` and
`to_vec` (`src/rdma.rs:748-766`), and the `Vec` path in `finish_receive`
(`src/rdma.rs:690-695`).

## Configuration

- `RACER_REGISTERED_BYTES`: always node-wide. Default stays
  `96 MiB * final I/O workers` so totals do not change, but it is no longer
  divided per worker (`src/app.rs:2590`). It is split evenly across selected
  devices, then into page buffers of `registered_charge(MAX_CIPHERTEXT)`
  (16 MiB + 4 KiB). Staging is gone, so the same budget funds twice as many
  buffers. Validation (`src/config.rs:541`): with RDMA enabled, at least two
  buffers per device (one send, one receive), else RDMA is disabled for that
  device with a warning.
- `connections_per_neighbor`: no longer admits sessions
  (`src/rdma.rs:221-226` goes away). It sets `grants_per_link`, because each
  concurrent page to a peer already needs one HTTP connection.
- New internal constants, not env vars: setup timeout 5 s, idle 120 s,
  backoff 1 s to 60 s, command queue 64.
- `src/app/native.rs`: `slot_count` becomes `buffer_count`; `NativePair`
  per worker becomes one owner port plus one `Client` per worker.

Docs updated in stage 5: `docs/content/reference/racer.md:104` (the
`RACER_REGISTERED_BYTES` row) and the RDMA section (`racer.md:803-857`):
persistent links, node-wide pinned pool at activation, fallback behavior,
and that links reconnect with backoff.

## What is deleted

- verbs: `Slot`, `Mailbox`, `Lease`, `QueuePairHandle`, `Region`, `Window`,
  `Ticket`, `Transfer`, `Receive`, `QpSet`, `IoPort`, `NativePort`,
  `DeviceHandle`, `pair`, `NativeService`, `Configuration`, the `testing`
  hooks, slot rebuild, per-slot admission guards, and staging bytes.
  `rdma_verbs_write` is not deleted (deviation 13).
- racer: `Sessions`, `PreparedSession`, `SessionLease` and `claim()`,
  `SetupParameters` (v1), `Grant`, `RemoteDescriptor`,
  `AuthenticatedDescriptor`, `SendCompletion`, `RegisteredLease`,
  `native_slot_charge`, `DESCRIPTOR_HEADER`, the 30 s setup expiry
  (`src/rdma.rs:244`), per-transfer `expire_at`, `fence_cut`, and the
  transport helpers listed above.
- peer-wire: phases `Offer`, `Setup`, `Ready`, `Grant`, `Done`, `Failed`.

Tests that cover deleted behavior are replaced by tests of the new behavior
in the same commit. Removing a test without a replacement needs approval
(AGENTS.md).

## Test plan

All in simulation unless noted. Each failure test asserts: the op error, the
link state, that no `RegisteredPage` was produced, the buffer state
(`Free`, `Quarantined`, or `Leaked`), and guard counts.

verbs `ffi.rs` and simulator:

- Layout and opcode test for ABI 2; load refuses ABI 1.
- `post_recv`, `send`, `write_imm`, `bind` with offset: bounds, `EINVAL`,
  leak on failed free (existing pattern).
- RNR with `rnr_retry = 0` errors both QPs.
- Write after `LOCAL_INV` gets `REM_ACCESS_ERR`; write beyond window fails.
- MR without `REMOTE_WRITE`: both simulator provider modes.

verbs `control.rs`: round trip of every kind; rejection of every malformed
field (version, kind, reserved, length, link id, zero or oversized grant).
Property test: random 64-byte inputs never decode to an accepted `Grant`
with bad fields.

verbs `pool.rs`: every state transition; no slice API outside `Staging` and
`Owned`; `RegisteredPage` drop on another thread returns the buffer; region
outlives pages; leak on failed destroy keeps guard.

verbs owner and links:

- Happy path both directions on one link, concurrently.
- Grant before write; write before grant arrives (waits); grant never
  arrives (write deadline).
- Release path frees buffer without CPU access.
- Fallback after grant: release arrives late; never arrives (break at
  deadline).
- Cancel exposed grant breaks the link and quarantines.
- Error CQE on each WR kind; flush handling; unknown wr id; duplicate imm;
  imm for unknown tag; wrong link id; credit exhaustion waits; credit
  violation breaks; grant cap exceeded breaks; `Close` handling.
- Credits: long run of transfers never hits RNR; standalone `Credit` sent at
  half depth; reserved credit used only for `Credit`.
- State machine: open, accept, tie-break both orders, offer while `Ready`,
  setup timeout, backoff doubling and reset, idle close, reconnect.
- Command queue full returns `Overloaded`; contention on `try_lock`.
- Shutdown with pages still held by Racer; drained only after they drop.
- Destroy failure leaks link, MWs, and buffers; reopen stays blocked.

racer:

- Setup codec v2, link id derivation symmetric, signed-header requirement,
  replay rejection, tie-break function, rate limit.
- Transport (existing simulated native harness,
  `src/peer/transport.rs:1971-2330`): grant-in-request round trip both
  directions on one link; sender fallback with release; sender link down at
  response time; receiver link not ready (plain HTTP); completion head with
  wrong link, tag, transfer, or length rejected; `Complete` then arrival
  failure returns `Io`; 5 s native deadline; cancellation.
- Zero-copy: decrypt from `Registered` storage; cache publish and rehome copy
  out and free the pool buffer; relay forwards `Registered` pages.
- Config: node-wide budget, buffer count, two-buffer minimum,
  `connections_per_neighbor` to `grants_per_link`.

Hardware (ignored in CI, run on target): MR without `REMOTE_WRITE` bind;
end-to-end throughput; forced link break mid-transfer; peer restart.

## Implementation plan

Every commit builds and passes `make fmt`, `make lint`, and the racer and
verbs tests. Old and new paths coexist until stage 3 switches the transport.

Stage 1, verbs ABI 2 (worktree `racer-verbs-abi`):

1. `feat(racer-verbs): add send, recv, and write-with-imm to native adapter`
   (verbs.c, ABI 2, wc v2, depth args, MR access arg, bind offset,
   `rnr_retry = 0`; ffi.rs wrappers and layout test; slot code adapted to
   the new signatures).
2. `feat(racer-verbs): simulate send, recv, immediates, and RNR` (simulator
   parity and fault knobs, ffi-level tests).

Stage 2, verbs links (after stage 1). New files only plus `mod` lines, so
2a, 2b, and 2c can run in parallel worktrees and merge trivially:

3. 2a `feat(racer-verbs): add in-band control message codec` (`control.rs`).
4. 2b `feat(racer-verbs): add registered page buffer pool` (`pool.rs`).
5. 2c `feat(racer-verbs): add worker command channel and one-shot ops`
   (`channel.rs`).
6. 2d `feat(racer-verbs): add link state, credits, and grants` (`link.rs`,
   owner-internal, driven by tests).
7. 2e `feat(racer-verbs): add node owner and client API` (`Owner`, `Client`,
   `node`, `WithNative` over `Owner`; public API tests). Old slot API still
   present.

Stage S, racer pure pieces (parallel with stages 1 and 2, own worktree;
touches only `src/rdma/link_setup.rs` and `peer-wire`):

8. `feat(racer): add v2 link setup codec, link id, and tie-break`.
9. `feat(racer-peer-wire): add RdmaLink request kind` (wire only).

Stage 3, racer integration (after 2 and S, one worktree, serial):

10. 3a `feat(racer): run one node RDMA owner and per-worker clients`
    (`src/app.rs`, `src/app/native.rs`, config semantics, `Links` facade,
    `buffer_count`; old `Sessions` still used by transport through a shim
    or left unused).
11. 3b `feat(racer): handle RdmaLink setup and background connect`
    (`src/peer/server.rs`, `Links::ensure`, `accept_setup`).
12. 3c `feat(racer): grant in request and write with immediate per page`
    (`src/peer/transport.rs` rewrite, `Source::Bytes`).
13. 3d `refactor(racer): remove per-transfer RDMA sessions` (delete old
    racer types, phases, and tests replaced in 3c).

Stage 4, zero-copy (after 3; 4a and 4b in parallel worktrees, they touch
`memory.rs` and `security.rs` respectively):

14. 4a `perf(racer): keep received ciphertext in registered memory`.
15. 4b `perf(racer): stage outgoing ciphertext on the crypto thread`.

Stage 5, cleanup (after 4):

16. `refactor(racer-verbs): remove slot API and plain RDMA write`.
17. `docs(racer): document persistent RDMA links` (`racer.md`, status lines
    in both design files).

## Contradictions with the plan and open risks

1. Crypto does not run per hop. The plan says crypto encrypts into and
   decrypts from registered buffers. The code encrypts once and reuses the
   ciphertext for peers and disk (`src/security.rs:348`), and senders hold
   cached or slab ciphertext (`src/memory.rs:90-96`). This design keeps one
   send-side copy, moved to the crypto thread, and makes receive zero-copy.
2. The plan puts the transfer description in the HTTP response head with
   the rest in-band. This design moves the grant into the signed request so
   no extra round trip is needed, and keeps the response head as the single
   authoritative outcome. Otherwise the receiver could not tell "RDMA
   succeeded" from "sender fell back" without another message.
3. `Done` and `Abort` are dropped, `Release` is added. Invalidation alone is
   not a fence for in-flight multi-packet writes, so an abandoned exposed
   grant needs either the peer's ordered `Release` or a link break.
4. Transfer ids are 16 bytes (`object-wire/src/model.rs:116`) and immediates
   are 32 bits, so the immediate carries a per-link tag; the signed headers
   bind tag to transfer id.
5. MR without `REMOTE_WRITE`: depends on the provider. Needs a hardware check;
   the `mr_remote_write` escape hatch exists for that.
6. `RACER_REGISTERED_BYTES` semantics change: today the default is per
   worker and an explicit value is node-wide
   (`docs/content/reference/racer.md:104`, `src/app.rs:2590`).
7. `rnr_retry` changes from 3 to 0 (`verbs/native/verbs.c:186`). A credit bug
   then breaks links instead of stalling them; tests must cover credits well.
8. A sender CQE success followed by a receiver-side link break before the
   receive CQE is processed fails that request with `Io` and no in-place
   fallback. That relies on the caller's existing peer error handling (an
   inference, not verified).
9. One owner thread for the node, shared with crypto work, until stage 4b
   moves bulk copies off it. Owner per device is the escape hatch.
10. The old `racer-verbs.md` cited line ranges that no longer matched
    `verbs/src/lib.rs` (for example the slot pool range). The rewrite drops
    line citations for code that this plan replaces.

## Deviations in the implementation

Recorded while building stage 3; the code is authoritative.

1. Link id. `Client::accept` connects the responder's QP and takes the link
   id before the responder's endpoint exists, so the id cannot hash the
   responder's full setup. The link id is the first 8 bytes of
   `SHA-256("racer-rdma-session-v2\0" || initiator setup || responder nonce)`
   (`LinkId::for_setup`, `src/rdma/link_setup.rs`). Both inputs are in the
   signed exchange. `LinkId::derive` (symmetric) is kept but unused.
2. No plain `Accept`. When the receiver cannot grant, it sends no payload
   control at all and the page is ordinary HTTP. `Phase::Accept` always
   carries `racer-rdma-grant`. `Complete` and the fallback `Finish` both name
   the `Accept` digest as their previous head.
3. Commit split. Stages 3a and 3b add `Links`, the setup handler, and the
   background connect without switching the application; stage 3c switches
   the application (node owner, per-worker clients, node-wide
   `RACER_REGISTERED_BYTES`) together with the transport, so every commit
   keeps native DST coverage green.
4. Non-owner workers do not wait for activation at startup; they read the
   node's active rails from the shared `LinkNode` when it settles.
5. Owner maps are ordered (`BTreeMap`) so simulated runs replay exactly.
6. The grant extension carries the rail; the transfer id is already bound by
   the signed `Accept`. The grant length is `MAX_CIPHERTEXT`.
7. `Devices` and the slot `WithNative` alias are deleted from `src/rdma.rs`
   rather than kept: they wrapped the slot `IoPort`, and `Links::activate`
   now does rail selection and activation for the node. `Inventory`,
   `select`, `select_hop`, `select_worker`, `match_publication`, and
   `signed_value` stay. `NativeRetry::fence` went with the per-transfer
   sessions.

Recorded while building stage 4a (receive-side zero-copy, done):

8. `CiphertextStorage::Registered` holds `{ page: RegisteredPage,
   reservation: Charge }` rather than `{ page, len }`; the length is
   `RegisteredPage::len`, and the ciphertext charge lives with the page.
   `PendingReceive::finish` calls `BufferPool::registered_ciphertext` and
   no longer copies (`src/rdma/links.rs`).
9. Copy-out goes through `BufferPool::retainable` (`src/memory.rs`), which
   takes a fresh `Ciphertext` charge and copies through `rehome`. It runs in
   `MemoryCache::publish` (skipped for a duplicate page) and
   `MemoryCache::publish_ciphertext`. Memory caching is optional, so it uses
   ordinary admission and stops when admission stops.
   `Writer::enqueue_reclaiming` (`src/store.rs`, after the dedup checks)
   instead reserves the copy through the caller's staging callback, the
   same one used for disk staging. The callbacks in `src/read/fill.rs`
   reclaim cached pages before failing and use completion admission, and
   `Writer::enqueue` passes `Writer::reserve_staging`. A fill that already holds an
   accepted dirty charge still persists its page during drain. `rehome` always copies a
   registered page, even when it holds the only reference. The charge
   reserved for the copy is separate from the request's, so the page is
   counted twice until the request drops it. If the copy cannot be admitted,
   the publish or enqueue fails with the admission error and the request
   still releases the buffer when it ends.
10. The cache keeps a copy, not the request's `Arc`, so
    `MemoryCache::invalidate_ciphertext` matches a registered argument by
    envelope and bytes as well as by pointer.
11. Relays: send-side code is unchanged in 4a. With stage 4b, a relayed
    registered page is copied once into a send buffer on the crypto thread
    by `Links::stage` (it reads `CiphertextPage::bytes`, whatever the
    storage); it is not sent from its receive buffer in place. Without a
    stager or free send buffer it still takes the owner `Source::Bytes` copy.

Recorded while building stage 4b (staged sends, done):

12. Stage 4b staging (`Links::stage`, `src/rdma/links.rs`). The input field is
    `buffer`, not `into`. The copy does not verify the stored checksum; the
    ciphertext is AEAD-authenticated by the receiver anyway. When no buffer is
    free at once, the stager fails for any reason other than the request's
    own cancellation or deadline, or the worker has no stager, the send falls
    back to `Source::Bytes` (owner copies, owner queues for a buffer) rather
    than waiting or falling back to HTTP. A failed or cancelled stage job
    drops the `SendBuffer`, which returns it to `Free`; on cancellation
    `Links::send` returns the error and `send_native` releases the grant. The
    stage job runs without a key lease and records no crypto metrics
    (`checksum_only`). The owner still copies under pool exhaustion, so it is
    not entirely free of bulk copies.

Recorded while building stage 5 (cleanup, done):

13. `rdma_verbs_write` stays in the adapter, and the native ABI stays at 2.
    Links never post a plain write, but about 15 adapter and simulator tests
    (`src/ffi.rs`, `src/simulation.rs`) use the whole-region bind, plain
    write, and invalidate ticket path to check window revocation, delayed
    DMA, stop fences, and quarantine. That path is now `cfg(test)` in
    `src/ffi.rs`, so production builds carry only the `Api::write` symbol
    pointer. Removing the symbol needs those tests ported to write with
    immediate first.
14. The C discovery fault tests (`src/discovery_fault_tests.rs`) now activate
    through a one-worker node `Owner` instead of `NativeService`. They still
    check that a leaked discovery or open keeps its guard charged and blocks
    reopen (`Overloaded`).
15. The worker test `shared_native_wrappers_are_drained_and_destroyed_on_owner_thread`
    (`src/worker.rs`) hosts a node `Owner` through `WithNative::with_host`.
16. The slot crate tests `tests/public_api.rs`, `tests/startup.rs`, and
    `tests/device_discovery.rs`, the slot unit tests in `src/lib.rs`
    (including two ignored hardware tests), and the slot simulation test
    `native_services_activate_discovered_nodes_and_copy_through_io_proxies`
    are deleted with the API. Their live concerns are covered by
    `tests/links.rs`, the node and link unit tests, and the discovery fault
    tests. The ignored loopback test
    `ffi::tests::native_available_provider_write_bind_invalidate_and_fence`
    remains the only real-hardware DMA test, and it uses plain writes.
17. `grants_per_link` is negotiated (resolves the earlier open issue that the
    remote value was decoded but ignored). Both sides pass the peer's
    advertised value through `rdma_verbs::Setup::grants_per_link`, and a link
    holds at most `min(local, peer)` outstanding grants
    (`verbs/src/link.rs`, `Link::connect` and `Link::grant`). Incoming grants
    are still checked against the local limit, which is never below the
    negotiated one, so a side configured for more grants can no longer break
    a smaller peer. Receive and send depths stay sized by the local limit
    (`2 * grants_per_link + 4` receives), which bounds the peer's traffic
    because the peer issues at most the minimum. Setup decode rejects a
    `grants_per_link` of 0 or above `MAX_GRANTS_PER_LINK` (4096,
    `src/rdma/link_setup.rs`), and Racer clamps its own value to that range.
