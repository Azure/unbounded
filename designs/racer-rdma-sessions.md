# Racer persistent RDMA links

Status: implemented. This file describes Racer's link setup, page protocol,
memory ownership, and configuration. [racer-verbs.md](racer-verbs.md) covers
the verbs crate. Paths below are relative to `cmd/racer/`.

## Design

Each node keeps one bidirectional RC queue pair per peer and rail. A signed
HTTP exchange sets up the link independently of page requests. A page request
names a receive grant, and the response reports either the RDMA completion or
an HTTP body. There is no per-page QP setup or extra HTTP control round trip.

RDMA sends read immutable ciphertext directly through an implicit on-demand
paging (ODP) region. ODP is required for RDMA activation. There is no staged
RDMA send path or switch to enable one. Ordinary HTTP remains the transport
for unavailable links and ineligible hops, not a second RDMA implementation.

The main source files are:

- `src/app/native.rs`: node ownership, activation, and rail publication.
- `src/rdma/links.rs`: worker-facing link and page operations.
- `src/rdma/link_setup.rs`: setup encoding, binding, link id, and tie-break.
- `peer-wire/src/link.rs`: signed link request and response.
- `src/peer/transport.rs`: background setup and per-page transport.
- `src/memory.rs`, `src/read/fill.rs`, and `src/store.rs`: page retention.

## Threading

`rdma_verbs::node` creates one `Client` per I/O worker and one `OwnerPort`.
The owner runs on the crypto thread paired with the first I/O worker, through
`WithOwner`, Racer's `WithNative<PageCryptoEngine, Owner>` alias. No separate
RDMA thread is created. Only this owner posts verbs operations.

Each worker has a `Links` facade and a bounded command queue. Results return
through one-shot `Op` cells. The owner publishes link snapshots; a worker that
cannot read a snapshot treats the link as unavailable for that request.
`LinkNode` holds the shared rail publication and setup reservations.

Activation selects rails once for the node, using the owner's NUMA location.
The registered-memory budget is split across the funded devices and rounded
down to whole receive buffers on each device. Each device needs at least one
receive buffer. Sends do not consume these buffers. Every selected device
must support implicit ODP; failed activation does not enable a copied-send
mode. Discovery or activation failure leaves ordinary HTTP available.

Non-owner workers read the shared active rails rather than opening their own
devices. Only the owner initiates activation or closes native resources.

## Link setup over HTTP

Links are keyed by `LinkKey { peer: SHA-256(NodeId), rail }`. A local `Epoch`
identifies one incarnation; the signed handshake establishes a separate
64-bit link id shared by the two nodes.

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
the rail in both nodes' published NIC lists and the rail domain. Simultaneous
setup uses byte ordering of NodeIds: the lower NodeId's offer wins. The other
offer gets `409`. Setup is limited to one accepted attempt per peer and rail
per second; excess attempts get `429`. Unavailable native service gets `503`.

Per-key reservation guards prevent two workers from opening the same link.
Dropping an open or setup attempt clears only its own reservation and breaks
only its own epoch, including when the owner completed an open that the
caller never read. An old guard cannot remove a newer setup attempt.

## Link table and state machine

The owner reports `Idle`, `Connecting`, `Ready`, `Breaking`, and `Backoff`.
The initiator becomes ready after connecting and queuing its credit hello.
The responder becomes ready on its first receive completion. Receives are
posted before either side can send.

- Setup times out after 5 seconds.
- Error and timeout breaks back off from 1 second to 60 seconds, doubling
  with plus or minus 20 percent jitter. A link ready for 60 seconds resets
  the backoff. Incoming authenticated setup can replace an old link.
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
heads bind the transfer metadata. Page checksum and AEAD checks remain at
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
exposed grant that expires breaks the link.

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
- Each receive buffer is `MAX_CIPHERTEXT` rounded up to 4 KiB, currently
  16 MiB plus 4 KiB. Rail funding requires at least one such buffer per device.
  Unfunded rails are not activated.
- `RACER_CONNECTIONS_PER_NEIGHBOR` also sets the per-link grant limit, clamped
  to 1..4096. The two nodes use the smaller advertised limit for issued grants.
- The worker command queue holds 64 entries. The application allows up to
  `2 * MAX_DEGREE` links per device.
- `RACER_ENABLE_RDMA` still controls RDMA eligibility. There is no
  `RACER_RDMA_ZERO_COPY` setting or broad remote-write MR escape hatch.

These are admission and resource limits, not a process RSS limit. Implicit
ODP source mappings do not replace receive-pool or ciphertext accounting.

## Validation

The setup codec and peer-wire tests cover exact encodings, signed fields,
binding, replay, and malformed input. Link and transport tests cover both
directions, setup races, stale guards and epochs, HTTP outcomes, bad completion
metadata, cancellation, timeouts, and drain. Verbs tests cover required ODP,
direct-source lifetimes, credits, revocation, faults, and failed destruction.
Memory, read, and store tests cover copy-out sharing and admission failure.

Simulation and the native fault adapter do not model hardware ODP page faults
or prove throughput. Validate bidirectional transfers, peer restart, forced
link breaks, HTTP recovery, and provider memory-window behavior on target
hardware. Mixed protocol versions are not supported; coordinate upgrades.
