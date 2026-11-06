# Racer persistent RDMA links

Status: implemented. This file describes Racer's link setup, page protocol,
memory ownership, and configuration. [racer-verbs.md](racer-verbs.md) covers
the verbs crate. Paths below are relative to `cmd/racer/`.

## Design

Each node keeps one bidirectional RC queue pair per peer and rail. A signed
HTTP exchange sets up the link independently of page requests. A page request
names a receive grant, and the response reports either the RDMA completion or
an HTTP body. There is no per-page QP setup or extra HTTP control round trip.

RDMA sends read immutable ciphertext in place. On window-mode devices (mlx5)
they go through an implicit on-demand paging (ODP) region, and ODP is
required for activation. On registration-mode devices (MANA, no memory
windows and no ODP) each write registers its source while it is in flight,
and no ODP is needed; see [racer-verbs.md](racer-verbs.md), "Device modes".
There is no staged RDMA send path or switch to enable one. Ordinary HTTP remains the transport
for unavailable links and ineligible hops, not a second RDMA implementation.

The main source files are:

- `src/app/native.rs`: node ownership, activation, and rail publication.
- `src/rdma/links.rs`: worker-facing link and page operations.
- `src/rdma/link_setup.rs`: setup encoding, binding, link id, and tie-break.
- `src/rdma/setup.rs`: the per-key setup state machine (`SetupTable`).
- `src/rdma/discovery.rs`: rail journal, publication matching, and NUMA-aware
  selection over `RailMapping` (`rdma_verbs` only enumerates ports).
- `peer-wire/src/link.rs`: signed link request and response.
- `src/peer/transport.rs`: background setup and per-page transport.
- `src/memory.rs`, `src/read/fill.rs`, and `src/store.rs`: page retention.

## Threading

`rdma_verbs::node` creates one `Client` per I/O worker and one `OwnerPort`.
The owner runs on its own thread, `racer-rdma-owner`, pinned to the crypto CPU
paired with the first I/O worker so registered memory is first touched on that
NUMA node. The thread counts against `max_threads`. It runs
`rdma_verbs::OwnerRole`, which parks on an eventfd (commands, closes, returned
buffers, stop) and on `Owner::poll_fds` (completion and connection manager
channels) with a timeout from `Owner::next_wake`; an idle owner has no periodic
tick. Page and device pool registrations run on short-lived helper threads
so the owner keeps serving. Otherwise the verbs adapter spawns no threads: in
registration mode the RDMA
connection manager is an event-driven state machine the owner advances on each
poll. The owner thread starts before the worker group and stops after it
joins; stopping waits until every page buffer has returned, printing a
diagnostic every `DRAIN_REPORT`, and never frees early. Only this owner posts
verbs operations.

Each worker has a `Links` facade and a bounded command queue. Results return
through one-shot `Op` cells. The owner publishes link snapshots by swapping
an `Arc` with a generation counter; a worker read never reports `Idle`
because of a concurrent publish, though it may lag the owner. A sender
releases a peer's grant by link id, which the owner resolves, so a lagging
snapshot never skips a `Release`. `LinkNode` holds the shared rail
publication and the setup state machine.

Activation selects rails once for the node, using the owner's NUMA location.
The registered-memory budget is split across the funded devices and rounded
down to whole receive buffers on each device. Each device needs at least one
receive buffer. Sends do not consume these buffers. Every selected
window-mode device must support implicit ODP; registration-mode devices need
none, but need a 2 MiB multiple buffer length (Racer's is). Failed activation
does not enable a copied-send mode. Discovery or activation failure leaves
ordinary HTTP available.

Non-owner workers read the shared active rails rather than opening their own
devices. Only the owner initiates activation or closes native resources.

## Link setup over HTTP

Links are keyed by `LinkKey { peer: SHA-256(NodeId), rail }`. A local `Epoch`
identifies one incarnation; the signed handshake establishes a separate
64-bit link id shared by the two nodes.

Only RDMA edges get links. An edge is eligible when both ends share a
nonempty Site and at least one rail. Each node ranks its eligible mesh edges
(see [racer-mesh-v2.md](racer-mesh-v2.md)) and takes the best `RDMA_PEERS`
(128). An edge is an RDMA edge only when both ends took it. Each membership
caches these peer sets, computed on first use per node. A hop that qualifies
by Site and rail but is not an RDMA edge sends its page over HTTP. The node
that receives the page body on that hop, the requester or a relay, counts
`racer_rdma_fallback_overflow_total` once the body arrives in full. Misses,
signed overloads, and failed exchanges deliver no body and do not count.

When a page needs an idle link, `Transfers::ensure_link` starts setup in the
background. That page uses HTTP instead of waiting for setup.

The setup request is `POST /racer/v1/rdma/link`, kind `rdma-link-v1`, with an
empty body. Both signed heads carry `racer-rdma-setup`. A successful response
also carries `racer-rdma-setup-binding`, the SHA-256 of the initiator's setup.
The binary setup is 78 bytes:

```text
"racer-rdma-setup-v2\0" | rail u16 | nonce [16] | endpoint [32]
                      | recv_credits u32 | grants_per_link u32
```

Integers use network byte order. Decoding checks the exact version and length,
the expected rail, a nonzero nonce and credits, and a grant limit in 1..4096.
The link id is the first eight bytes of:

```text
SHA-256("racer-rdma-session-v2\0" || initiator setup || responder nonce)
```

The responder chooses its nonce before accepting the link. Both inputs are
covered by the signed exchange, and the id is available before the responder
posts link traffic. The response must bind our exact setup and must not echo
it back as the responder's setup.

The server checks the signature and session peer, a shared nonempty Site, and
the rail in both nodes' published NIC lists and the rail domain. An offer
on an edge that is not an RDMA edge in the responder's membership gets `403`,
and the initiator backs off. Simultaneous
setup uses byte ordering of NodeIds: the lower NodeId's offer wins. The other
offer gets `409`. Setup is limited to one accepted attempt per peer and rail
per second; excess attempts get `429`. Unavailable native service gets `503`.

One `SetupTable` per node holds each key's setup state: idle, `Opening`
(a token names the attempt while the owner opens the link), or `Exchanging`
(the owner's epoch, during the signed HTTP exchange). Only an idle key may
begin, so two workers never open the same link. The responder's admission
applies the tie-break against an attempt of ours, then the rate limit, and
an accepted offer supersedes our attempt. Every transition names its token
or epoch, so dropping an open or setup attempt clears only its own state and
breaks only its own epoch, including when the owner completed an open that
the caller never read. An old guard cannot remove a newer attempt. The
responder accepts through the owner, which creates the QP for the link id
before the HTTP reply goes out.

## Peer wire v2: certificate chains per session

Link setup and every per-page control head ride the peer wire profile
`racer-peer-v2` (envelope `racer-peer-version: 2`). The profile has no
compatibility with v1.

- Every signed head, both proofs and outer session frames, names its
  signer's chain by a signed 32-byte fingerprint in `racer-certificate`.
  The fingerprint is a SHA-256 of the length-prefixed chain bytes.
- The session handshake heads carry the full chain in
  `racer-certificates`. Each side seeds the new session with the peer's
  chain.
- Each session keeps a chain cache, keyed by fingerprint, of every chain the
  peer has sent. It never evicts entries while the session lives, and it
  holds at most 512 chains. A repeated chain or a cache overflow is a
  protocol error. Decoding a message resolves each proof's fingerprint from
  the carrying session while the connection is held. A miss is a protocol
  error. A retained request binding keeps its resolved chain so it can be
  verified again later.
- The sender keeps a sent-set of fingerprints per session. A chain the peer
  has not yet seen, for the outer signer or any proof signer (original or
  hop), rides once in a signed `racer-chains` field on the outer head. A
  relay therefore forwards the chains of signers that are new to its
  outbound session.
- A chain counts as sent once the head is signed for sending, as the
  sequence number does. A send that fails after signing leaves the
  connection unusable, so it is never reused. A session that could overflow
  either chain set on its next message fails its reuse check and the sender
  reconnects.
- Limits:
  - A chain is at most 16 KiB.
  - A proof head is at most 32 KiB. The largest request proof, with 8 KiB
    of metadata, 8 KiB plus a tag of sealed authorization, and 256 prefetch
    pages, is over 22 KiB.
  - `MAX_ENVELOPE_HEAD` covers nine proofs, the ten chains one message can
    attach, and 16 KiB of outer fields. That is about 615 KiB, down from
    1.125 MiB in v1.
  - `MIN_REQUEST_CONTEXT_BYTES` is eight times `MAX_ENVELOPE_HEAD`.
- The crypto layer still caches chain validation, keyed by fingerprint. That
  cache is only an optimization. The session cache is the authority for
  which chain a fingerprint names.

## Link table and state machine

The owner reports `Idle`, `Connecting`, `Ready`, `Breaking`, and `Backoff`.
The initiator becomes ready after connecting and queuing its credit hello.
The responder becomes ready on its first receive completion. Receives are
posted before either side can send.

- Setup times out after 5 seconds.
- Error and timeout breaks back off from 1 second to 5 seconds, doubling
  with plus or minus 20 percent jitter. A link ready for 60 seconds resets
  the backoff. A setup the peer declines with `409`, `429`, or `503` retries
  after 1 second without escalating; `403` backs off as an error. Incoming authenticated setup can replace an old link.
- An idle link closes after 120 seconds. A ready link sends `Close` and breaks
  when that completes or the 1-second close grace expires.
- Idle and peer-requested closes return to `Idle`, without error backoff.
- Protocol violations and bad completions break the link. Buffer reuse waits
  for successful QP destruction on these paths.

## Per-page protocol

The HTTP requester receives the page; the HTTP responder sends it.

1. The requester checks the membership lease, RDMA plan, and ready link. It
   grants a receive window for `MAX_CIPHERTEXT`, with a native deadline capped
   at 5 seconds and bounded by the request deadline. After the in-band `Grant`
   is posted, the request carries a signed `Accept` control with:

   ```text
   racer-rdma-grant = "racer-rdma-grant-v2\0" | link_id u64
                     | tag u32 | length u32 | rail u16
   ```

   The signed request already binds the transfer id. If no grant is available,
   the request has no RDMA control and uses HTTP. There is no empty `Accept`.

2. The responder verifies the request and grant, then rechecks the response's
   membership and transport plan. It writes the immutable page with
   `WRITE_WITH_IMM`, using the grant tag as the immediate. A write may wait for
   the in-band grant if the HTTP request arrives first.

3. After a successful send completion, the responder sends a signed,
   header-only `Complete` response:

   ```text
   racer-rdma-complete = "racer-rdma-complete-v2\0" | link_id u64
                        | tag u32 | ciphertext_length u32 | transfer [16]
   ```

   A recoverable RDMA failure can instead produce a signed `Finish` with the
   HTTP body, while the parent request is still live. Both controls chain to
   the `Accept` digest. The active phases are `Accept`, `Complete`, and
   `Finish`; the former multi-round control dialogue is not supported.

4. The requester verifies the signed outcome, membership, rail, link id, tag,
   transfer id, and length. `Finish` discards the pending RDMA receive and
   reads the HTTP body. `Complete` waits for a page whose receive and window
   invalidation both succeeded, then constructs the ciphertext response.

The transport does not treat in-band control as authentication. Signed HTTP
heads bind the transfer metadata. Page AEAD tag checks remain at
their existing read and crypto boundaries; transport does not grant access
to plaintext or encryption keys.

## Failure and cancellation

The sender must write or release each grant unless the link breaks. A
non-page response or changed transport plan releases the grant before taking
the normal HTTP path. Signature, membership, or expired-request errors remain
errors; they are not silently converted to successful HTTP responses.

Before a grant is exposed, cancellation can invalidate it locally. Once it
is exposed, dropping the arrival does not make its memory reusable. The owner
waits for the peer's ordered write or release, followed by invalidation. An
exposed grant that is still exposed 10 seconds past its deadline closes the
link gracefully, without error backoff.

If cancellation or a deadline wins after a write was submitted, `Links::send`
breaks that epoch: the remote tag might already be consumed, so a later
release cannot safely be assumed to complete. If a release cannot enter the
worker queue, `Links::release` also breaks the epoch. Break requests have a
separate shared path when the command queue is full.

A signed `Complete` followed by a failed or lost arrival fails the request
with `Io`. No HTTP body was sent, so that exchange cannot fall back in place.
The caller's normal peer-error handling applies.

Shutdown closes links and drives the native owner and crypto drain together.
Crypto work can still hold received pages that the owner needs returned.
Draining the owner first would block that work. Native teardown is not
abandoned when a request deadline expires.

## Racer types

Direct sends hold a stable, immutable source until the NIC completes the
write or the QP is successfully destroyed. Failed destruction keeps the source
and registration alive. This also lets a relay send from a received page
without an intermediate RDMA staging buffer.

`CiphertextStorage::Registered` holds the completed receive buffer and its
ciphertext admission charge. Decryption reads that storage directly, but
still produces a separate plaintext buffer. The registered receive pool and
the request's ciphertext reservation are both accounted for.

Long-lived memory-cache entries and queued disk writes must not pin receive
buffers. The read path tries to detach registered ciphertext into the
requesting worker's aligned pool after authentication. Cache publication and
store enqueue enforce the same rule if that early copy cannot be admitted.
A retained copy is remembered and shared, so caching and persistence do not
each make a second copy. Admission failure can reject retention without
invalidating the request-owned page.

Aligned ciphertext can be encrypted or received over HTTP directly into the
worker pool. Disk format 2 puts a padded header before an aligned ciphertext
body, allowing one `writev` of header and shared body. An unaligned body still
needs ordinary disk staging. That is a storage alignment requirement, not a
legacy RDMA path. Older disk-format records are cache misses, not converted.

These boundaries are not a claim of end-to-end zero-copy: decryption,
registered-page retention, and client delivery can still copy data. See
[racer-page-alloc.md](racer-page-alloc.md) for pool ownership and wiping.

## Resource limits

- `RACER_REGISTERED_BYTES` is one node-wide receive-pool budget. When unset,
  the total is 96 MiB times the final I/O worker count. An explicit value is
  the node total, not divided into separate worker pools.
- Each receive buffer is `MAX_CIPHERTEXT` rounded up to 2 MiB, currently
  18 MiB. Rail funding requires at least one such buffer per device.
  Unfunded rails are not activated.
- `RACER_NEIGHBOR_CONNECTIONS_PER_WORKER` also sets the per-link grant limit, clamped
  to 1..4096. The two nodes use the smaller advertised limit for issued grants.
- The worker command queue holds 64 entries. The application allows up to
  `2 * MAX_DEGREE` links per device.
- `RACER_ENABLE_RDMA` still controls RDMA eligibility. There is no
  `RACER_RDMA_ZERO_COPY` setting or broad remote-write MR escape hatch.

These are admission and resource limits, not a process RSS limit. Implicit
ODP source mappings and registration-mode source registrations do not
replace receive-pool or ciphertext accounting.

## Validation

The setup codec and peer-wire tests cover exact encodings, signed fields,
binding, replay, and malformed input. Link and transport tests cover both
directions, setup races, stale guards and epochs, HTTP outcomes, bad completion
metadata, cancellation, timeouts, and drain. Verbs tests cover required ODP,
registration mode and its connection manager (both device profiles run the
link contract tests), direct-source lifetimes, credits, revocation, faults,
and failed destruction.
Memory, read, and store tests cover copy-out sharing and admission failure.

Simulation and the native fault adapter do not model hardware ODP page faults
or prove throughput. Validate bidirectional transfers, peer restart, forced
link breaks, HTTP recovery, provider memory-window behavior, and
registration-mode connection manager behavior on target hardware. Mixed protocol versions are not supported; coordinate upgrades.
