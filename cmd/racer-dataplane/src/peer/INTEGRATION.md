# Peer integration

All constructors remain side-effect-free;
missing required operational inputs fail with `InvalidConfiguration`.

```rust,ignore
let network = Rc::new(PeerNetwork::new(local_node, published_state.clone())?);
let codec = Rc::new(SecurityCodec::new(admission.clone(), buffers.clone()));
let peer_io = Rc::new(HttpIo::with_admission(
    reactor.clone(),
    Codec::new(wire::MAX_ENVELOPE_HEAD, PAGE_BYTES + 16),
    admission.clone(),
));
let mut transfers = Transfers::new(http.clone(), peer_io.clone(), rdma.clone())
    .with_wire(admission.clone(), codec.clone());
if let Some(sessions) = &sessions {
    transfers = transfers.with_native(signatures.clone(), sessions.clone());
}
let transfers = Rc::new(transfers);
let handshake = Rc::new(Handshake::new(signatures.clone(), sessions));
let requester = Rc::new(Requester::new(paths.clone(), rails, forwarding.clone(),
    handshake.clone(), transfers.clone()).with_network(network.clone()));
let relay = Rc::new(Relay::new(paths, forwarding.clone(), requester.clone(), admission.clone())
    .with_network(network.clone()));
let server = PeerServer::new(peer_io, forwarding, admission, local_service, relay)
    .with_opaque_relay(config.opaque_relay)
    .with_request_timeout(config.request_timeout)
    .with_network(network).with_wire(codec).with_handshake(handshake).with_reactor(reactor)
    .with_transfers(transfers);
```

Application assembly supplies this peer I/O to both `Transfers` and `PeerServer`.
Origins get a separate capped view and clients use their own 32 KiB profile.
`MAX_SIGNED_HEAD` is 64 KiB; `MAX_ENVELOPE_HEAD` is 1,179,648 bytes, including
the immediate-hop signature. The envelope budget allows the original plus eight
bounded base64 proofs and authentication/native-control overhead. Metadata and
encrypted Authorization are base64 encoded inside the original and again in the
outer envelope; accepted 8,192-byte client fields must not be limited to a 32 KiB
peer head. The forwarding verifier permits at most eight links (seven relays).

Worker sizing requires `8 * MAX_ENVELOPE_HEAD` request-context bytes plus the
existing `4 * max(configured header cap, 8192)` allowance. This funds retained
inbound/outbound heads, decoded context, signing/encoding scratch, and peer I/O
staging during a relay. It reduces worker count to fit the node budget, failing
startup if even one worker cannot fit. The default node request-context budget
is 64 MiB. This is a progress floor, not a concurrency guarantee: live requests
still share bounded admission and saturation returns `Overloaded`.

The worker must poll `server.listen(address, scope)` and drive its reactor. HTTP
connections and ciphertext retain quota through I/O completion. The listener scope
bounds connection lifetime. At the start of every exchange, including the first
and each reused keepalive exchange, header reception gets a fixed deadline of
`min(listener deadline, now + request_timeout)` with the listener's cancellation.
This covers idle waiting, the complete connection handshake (reads, verification,
signing, and both response writes), and the entire application head. Partial reads,
writes, and handshake rounds never renew the budget.
Application assembly supplies `Config.request_timeout`; the constructor
default is 30 seconds. Expiry closes the connection, retaining I/O resources and
admission charges until completion is fenced.

After the head, authenticated requests retain the existing signed request deadline
policy, bounded by the listener deadline, for dispatch and response transfer. The
header cap does not bound the whole response or reset the signed request budget.
Connection handshakes also have a fixed five-second cap within that initial header
scope. Their control admission travels with the connection through I/O fences,
including when the waiting future is dropped, and is released after successful
handshake completion. There is no unauthenticated public challenge response in v2;
the first response requires a verified signed hello. The listener uses
bounded concurrent connection tasks. Each task serves sequential pooled exchanges.
Errors close that connection and do not stop other connections.
The listener retains one outstanding accept across connection task completions,
including when its accepted socket is ready but has not yet been consumed.

Share the node's `PublishedState` with every worker network. Publication admission
creates one immutable `Arc<Membership>` per version and cache-only publications
reuse it. It bounds distinct live generations to `retained_snapshots + 1`, counting
the current generation; cache-only history consumes no additional membership slots.
The single incoming-version registry holds weak references and prunes dead entries
at publication. Workers do not install or retire membership tables.

Reads pass their lease to `PeerClient::request`; relays pass it to
`PeerTransport::exchange`. Routing and endpoint lookup receive
that lease directly. Authenticated ingress resolves its wire version once, then
passes the lease through local worker dispatch or relay and retains it through
HTTP/native response completion. Delayed workers and cancellation keep actual
operation leases alive until completion; no worker reference-count bookkeeping is
required. Neighbor validation still uses the leased bounded graph.

`SecurityCodec` decodes the security owner's canonical headers and checks exact
agreement by re-encoding through `security::protocol`. It does not authenticate.
`HttpIo` first admits the signed immediate-hop head using the session owned by its
`ConnectionLease`. `Forwarding` then validates historical signatures, routing, and
exact request/response provenance before dispatch. `Signatures::verify_proof` alone
is not replay admission. `security::connection::{connect,accept}` establishes the
session once per socket using reciprocal signed fresh challenges. No challenge map
or per-request capability negotiation remains. The outer wire version is 5 and
the exchange target is `/racer/peer/v5/exchange`; older profiles fail closed.
V5 retains the rotating request MAC and certificate/session proofs, and adds
compact subscriptions with signed selected-page grants. See
[replacement subscriptions](../../../../designs/racer-hot-subscriptions.md)
for node-wide assembly, receiving aggregation, and the one-page-per-exchange limit.
The explicit Bootstrap operation returns metadata with optional ciphertext page zero. Empty
objects carry no page. HEAD remains Metadata. Bootstrap uses HTTP until its
version is known; subsequent pinned pages retain native transport eligibility.

`Session` moves with the lease into reactor operations and idle pooling. Each
direction has an exact-next u64 counter, never reset by `finish_exchange`.
`next_round` resets intermediate native framing without permitting pool return.
Only successful final completion permits reuse. Failed or abandoned exchanges close
after their fences; reconnect authenticates fresh challenges. There is no payload
TLS layer. Session storage is bounded by connection and handshake admissions.

## Current transport boundary

Security budget schema now requires `racer-route-attempts` (canonical u32) on every
original/request hop; decoder transfers it to `RouteBudget.remaining_attempts`.
Relays preserve/decrease, never refill; CopyOnly is zero. Application signed
`origin-forbidden`/403 and `origin-rejected`/401 remain distinct within the outer
200 envelope. See `designs/racer-peer-security.md` for exact credit rules and the
security review requirements for native setup/grant/completion bindings and fences.

Page, metadata, and copy-only requests use actual pooled HTTP. Page bodies preserve
ciphertext; AEAD verification belongs to the receiving read coordinator. Relay
verification preserves the original and every hop signature in both directions.

HTTP receive admission charges ciphertext to the request's cache before allocation.
Application assembly installs bounded idle-memory and unsubmitted-write reclamation
for that admission; active readers and submitted writes retain their ownership.
`response_reserved` transfers the completed receive charge into the decoded page
without charging the same allocation twice. The decoder checks the admission owner,
cache, resource class, capacity, and descriptor before accepting that charge.
Fill reserves separate encryption output only after selecting local origin.

### Opaque HTTP relay bodies

Only with explicit `RACER_OPAQUE_RELAY=true`, the HTTP server uses
`PeerTransport::exchange_relay` for transit requests without
an admitted native offer. `Transfers` returns the unfinished downstream HTTP
connection after its session-authenticated head. `Forwarding::forward_opaque`
decodes/re-encodes canonical metadata and verifies the original signature, request
digest, authority, length and every reverse proof before appending its signed hop.
The response head can then precede body completion. AEAD remains requester-owned.
Native offers/completions and ordinary endpoint receives retain their existing path.

The body loop consumes HTTP read-ahead first, then uses nonblocking TCP-to-pipe and
pipe-to-TCP splice, draining each chunk before receiving another. It holds one
admitted pipe (at most 64 KiB) and, only when needed, one zeroizing 64 KiB read-ahead
or fallback buffer. It never creates a transit `CiphertextPage`. Unsupported splice
(`EINVAL`, `ENOSYS`, `EOPNOTSUPP`) switches to bounded copy, including draining any
already-buffered pipe suffix. No bytes beyond Content-Length are forwarded.

The application shares its existing worker PipePool between client delivery and
relay bodies. A relay retains its Relay reservation through head/body completion;
outbound connection establishment also carries that reservation. Every readiness
wait retains both connections, the pipe, staging and reservations through original
and cancellation CQEs. Partial body failure closes both dirty connections without
an appended error or pool return. Before success headers, pipe pressure returns a
signed Overloaded response. Keepalive requires exact completion on both sides.

The default is off: absent or `false` retains the previous materialized relay.
Opt-in requires no wire version or dependency change. Existing `pipes` is shared
by clients and opted-in transit, so the concurrent
relay-body ceiling can be lower than `relay_transfers`. Each live pipe uses two
descriptors; pipe capacity is charged in existing pipe units. Growth to 64 KiB is
best-effort and remains bounded if kernel/user pipe limits reject it. The request
deadline never renews; a bounded readiness tick also observes TCP FIN during a
silent downstream read. No sysctl or elevated container capability is required.

`Requester::exchange` selects the route rail and automatically attempts native
transfer when the local session provider is ready. The server validates the entire
signed response path's rail mapping, then exchanges setup, grant, and completion
controls on the same HTTP connection. Setup unavailability chooses ordinary HTTP.
Post-offer native failures use a signed fallback handshake and retained ciphertext,
without repeating acquisition, resetting the deadline, or decrypting credentials.
Both terminal native fences precede fallback data delivery. Cancellation and
authentication failures terminate the exchange. Standalone legacy `send`/`receive`
reject unbound work. The native lifecycle service must be activated and driven on
the existing paired worker as described by the RDMA owner's `native/README.md`.

See `NATIVE_PROTOCOL.md` for the exact closed control schema and review points.

## Checks

Peer tests cover bounded/versioned envelope framing, original signature and opaque
credential preservation across a relay, replay and attempt substitution rejection,
deadline/cancellation preservation, authenticated copy-only dispatch and signed
failure replies, capability tampering, and real TCP partial ciphertext bodies with
pool reuse and truncated-body rejection. Header timeout tests cover silent/partial
peers, idle keepalive, healthy reuse, dispatch deadline preservation, shorter listener
deadlines, cancellation, and admission retention through completion fences.
Real socket backpressure tests pre-fill the server send buffer and cover both
signed handshake responses, fixed deadlines across rounds, cancellation,
abandonment, and admission recovery after CQEs.
Accept-loop tests exercise the production selection helper across pending and ready
accepts, concurrent connection completions, cancellation, and abandonment.
They are under `peer::tests`, `peer::server::tests`, and
`peer::wire::tests`; run `cargo test --lib peer::` and the all-feature variant.
`app::peer_tests` additionally exercises the assembled requester/server I/O over
real sockets: all three maximum client fields, credential encryption/decryption,
session authentication, eight-link forwarding and signed reverse responses,
oversized heads, worker budget boundaries, and staging-pressure rejection.
