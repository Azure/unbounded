# Peer integration

All constructors remain side-effect-free. Existing constructors are retained;
missing required operational inputs fail with `InvalidConfiguration`.

```rust,ignore
let network = Rc::new(PeerNetwork::new(local_node, retained_snapshot_limit)?);
network.install(membership.clone())?;
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
let handshake = Rc::new(Handshake::new(signatures.clone(), sessions)
    .with_http(network.clone(), transfers.clone())
    .with_discovery(keys.clone(), certificates.clone(), replay.clone()));
let requester = Rc::new(Requester::new(paths.clone(), rails, forwarding.clone(),
    handshake.clone(), transfers.clone()).with_network(network.clone()));
let relay = Rc::new(Relay::new(paths, forwarding.clone(), requester.clone(), admission.clone())
    .with_network(network.clone()).with_handshake(handshake.clone()));
let server = PeerServer::new(peer_io, forwarding, admission, local_service, relay)
    .with_request_timeout(config.request_timeout)
    .with_network(network).with_wire(codec).with_handshake(handshake).with_reactor(reactor)
    .with_transfers(transfers);
```

The worker must poll `server.listen(address, scope)` and drive its reactor. HTTP
connections and ciphertext retain quota through I/O completion. The listener scope
bounds connection lifetime. At the start of every exchange, including the first
and each reused keepalive exchange, header reception gets a fixed deadline of
`min(listener deadline, now + request_timeout)` with exchange-local cancellation.
Listener cancellation wakes and cancels each exchange, while acquisition cleanup
can cancel only its own exchange. It cannot cancel the listener or another request.
This covers idle waiting and the entire head; partial/trickled bytes never renew
the budget. Application assembly supplies `Config.request_timeout`; the constructor
default is 30 seconds. Expiry closes the connection, retaining I/O resources and
admission charges until completion is fenced.

After a completed exchange, global outbound connection pressure may close an
idle accepted peer before that deadline. An empty-socket peek is the reclamation
decision point: queued bytes protect the next head, while bytes arriving after
the peek can race with closure. The idle wait is non-consuming and cancellation
is fenced before releasing its connection charge. First exchanges, partial heads,
and active responses do not enter this idle registry. Deadlines remain upper
bounds, not promises that keepalives remain open.

Incoming keepalive retention also targets at most half the worker connection
quota (at least one). Each new idle registration cancels excess quiet idle waits
through the same peek and completion fences. Queued heads remain protected, so
the target is not a hard active-connection ceiling. This prevents completed
accepted sockets from occupying all outbound forwarding capacity under sustained
arrivals; transit still uses nonwaiting admission and the unchanged total quota.

After the head, authenticated requests retain the existing signed request deadline
policy, bounded by the listener deadline, for dispatch and response transfer. The
header cap does not bound the whole response or reset the signed request budget.
Challenge and handshake responses retain the listener deadline and the same
one-way cancellation propagation. The listener uses
bounded concurrent connection tasks. Each task serves sequential pooled exchanges.
Errors close that connection and do not stop other connections.
The listener retains one outstanding accept across connection task completions,
including when its accepted socket is ready but has not yet been consumed.

Install exact immutable membership snapshots on every worker and retire old entries
only after acquisition policy stops issuing requests for those versions. Replacing
a version with a different allocation is rejected. Retiring a map entry does not
invalidate an already acquired membership lease.

`SecurityCodec` decodes the security owner's canonical headers and checks exact
agreement by re-encoding through `security::protocol`. It does not authenticate.
Only `Forwarding` admits operations or responses. Challenge discovery uses
`security::session::{ChallengeProbe, ChallengeReply, respond}` and installs the
typed authenticated challenge. No unsigned capability or challenge grants service.

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

Production HTTP relay requests use cut-through forwarding. Each admitted exchange
owns one 16 KiB ciphertext chunk and a relay permit through body I/O completion.
Relay, chunk, and outbound connection admission are nonwaiting, including challenge
discovery and capability negotiation. Exhaustion returns a bound failure before
the upstream page head; it never queues while holding another transit resource.
The buffered `Relay::forward` interface retains its bounded FIFO for standalone
callers, but production `PeerServer` with Transfers selects the stream path.

The worker protects a derived chunk allowance inside the configured ciphertext
ceiling. Ordinary fills, storage staging, and native output cannot consume it.
Partition validation preserves the existing window/storage floor plus this reserve.
Descriptors and complete signature chains are verified before reverse forwarding;
the requester still collects and authenticates the whole page before plaintext
publication. A body failure after the upstream head closes the exchange.

After overload, the acquisition owner may back off 200-250 ms before the next
existing candidate attempt. It drops its ciphertext output during that wait, uses
the original deadline/credits, and reacquires output before submitting again.
There is no relay retry, fresh credit, or deadline extension. Worker driver ticks
and cancellation subscriptions drive this bounded wait.

The worker must continue calling `server.poll_admission_deadlines()` for local
peer-pool waiters and buffered-interface waits. Production already does so.

`Requester::exchange` selects the route rail and automatically attempts native
transfer when the local session provider is ready. The server validates the entire
signed response path's rail mapping, then exchanges setup, grant, and completion
controls on the same HTTP connection. Setup unavailability chooses ordinary HTTP.
Post-offer native failures use a signed fallback handshake and retained ciphertext,
without repeating acquisition, resetting the deadline, or decrypting credentials.
Cut-through relay hops do not advertise native acceptance downstream and answer
upstream native acceptance with the existing ordinary HTTP response. Direct native
requester/destination exchanges retain their existing setup and fallback fences.
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
