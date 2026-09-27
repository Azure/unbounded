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
This covers idle waiting and the entire head; partial/trickled bytes never renew
the budget. Application assembly supplies `Config.request_timeout`; the constructor
default is 30 seconds. Expiry closes the connection, retaining I/O resources and
admission charges until completion is fenced.

After the head, authenticated requests retain the existing signed request deadline
policy, bounded by the listener deadline, for dispatch and response transfer. The
header cap does not bound the whole response or reset the signed request budget.
Connection handshakes have a five-second cap within the listener scope. The listener uses
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
or per-request capability negotiation remains. The outer wire version is 2 and
the exchange target is `/racer/peer/v2/exchange`; mixed v1/v2 paths fail closed.

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
Accept-loop tests exercise the production selection helper across pending and ready
accepts, concurrent connection completions, cancellation, and abandonment.
They are under `peer::tests`, `peer::server::tests`, and
`peer::wire::tests`; run `cargo test --lib peer::` and the all-feature variant.
`app::peer_tests` additionally exercises the assembled requester/server I/O over
real sockets: all three maximum client fields, credential encryption/decryption,
session authentication, eight-link forwarding and signed reverse responses,
oversized heads, worker budget boundaries, and staging-pressure rejection.
