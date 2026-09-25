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
let transfers = Rc::new(Transfers::new(http.clone(), peer_io.clone(), rdma.clone())
    .with_wire(admission.clone(), codec.clone()));
let handshake = Rc::new(Handshake::new(signatures.clone(), sessions)
    .with_http(network.clone(), transfers.clone())
    .with_discovery(keys.clone(), certificates.clone(), replay.clone()));
let requester = Rc::new(Requester::new(paths.clone(), rails, forwarding.clone(),
    handshake.clone(), transfers).with_network(network.clone()));
let relay = Rc::new(Relay::new(paths, forwarding.clone(), requester.clone(), admission.clone())
    .with_network(network.clone()).with_handshake(handshake.clone()));
let server = PeerServer::new(peer_io, forwarding, admission, local_service, relay)
    .with_network(network).with_wire(codec).with_handshake(handshake).with_reactor(reactor);
```

The worker must poll `server.listen(address, scope)` and drive its reactor. HTTP
connections and ciphertext retain quota through I/O completion. The listener scope
bounds connection lifetime; decoded request deadlines can only shorten it. The
listener uses bounded concurrent connection tasks. Each task serves sequential
pooled exchanges. Errors close that connection and do not stop other connections.

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

Page, metadata, and copy-only requests use actual pooled HTTP. Page bodies preserve
ciphertext; AEAD verification belongs to the receiving read coordinator. Relay
verification preserves the original and every hop signature in both directions.

Native RDMA scoped operations are exposed through `prepare_session`,
`finish_session`, `send_scoped`, `prepare_receive`, and `finish_receive`. They require
verified signed setup, grant, and completion messages. Capability-only handshakes
currently advertise HTTP and no RDMA grants. Automatic RDMA page/control exchange
is not integrated: it needs a security-approved control schema carrying the scoped
setup/grant/completion and original page request binding. These APIs do not claim a
native transfer occurred. Standalone legacy `send`/`receive` reject unbound work.

## Checks

Peer tests cover bounded/versioned envelope framing, original signature and opaque
credential preservation across a relay, replay and attempt substitution rejection,
deadline/cancellation preservation, authenticated copy-only dispatch and signed
failure replies, capability tampering, and real TCP partial ciphertext bodies with
pool reuse and truncated-body rejection. They are under `peer::tests` and
`peer::wire::tests`; run `cargo test --lib peer::` and the all-feature variant.
