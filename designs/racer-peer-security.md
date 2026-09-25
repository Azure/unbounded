# Racer peer security v1

This document is owned by the security implementer. It defines the peer protocol,
not the control HTTPS or SDK Unix-socket protocol. Implemented exported encoders
under `security` are the sole encoding authority for peer transport. Peer code must
preserve the original signed head, all hop heads, and ciphertext verbatim.

## Cryptography and identities

Pages and ephemeral origin credentials use XChaCha20-Poly1305 with independent
cache/purpose keys, random 24-byte nonces, 16-byte tags, and versioned,
length-delimited AAD. Node HTTP signatures use Ed25519 and a certificate validated
against projected peer roots, with the exact URI SAN
`spiffe://<cluster UUID>/node/<Node UID>`. Deployment TLS trust is separate.

## Canonical message profile

Use RFC 9421 signature-base construction and structured `Signature-Input` and
`Signature` fields. Requests cover `@method` and `@request-target`; responses cover
`@status`. Cover every security/operation header in deterministic order. Reject
duplicate fields, malformed structured fields, unsupported signature algorithms,
unknown profile versions, and logical fields that disagree with the signed head.
The label is `racer`, algorithm is `ed25519`, and tag is `racer-peer-v1`.
No body digest is used: the signed page envelope and AEAD tag protect page bytes.

All binary header values use canonical padded standard base64. UUIDs use canonical
lowercase text. Object keys are lowercase hex; strong ETags include exact quotes;
expiration is Unix milliseconds. Opaque metadata and credentials remain byte values
and use binary encoding in signed headers, never UTF-8 normalization.

The exported logical request/response encoders bind every field, including cache,
object, operation/mode, pin, page, request/attempt, effective route, membership,
visited nodes, deadline, metadata presence, encrypted Authorization, lengths,
expiration, key ID and nonce. Response outcomes, including misses/errors, bind the
SHA-256 digest of the exact original request signature base and signature.

Forwarding preserves the original and appends an Ed25519 signed hop that binds the
original, previous chain digest, intended next receiver, and effective route. A hop
may consume but never extend link budget or deadline. Reverse hops bind the exact
request and use the recorded reverse path. Authentication never grants origin-fill
authority; the read candidate policy remains authoritative.

## Freshness and admission

Replay protection is node-wide, atomic across reactors, and bounded. Freshness uses
a random 24-byte nonce, Unix-millisecond timestamp, and a 32-byte receiver restart
challenge. The acceptance window is 60 seconds old and 5 seconds into the future.
Live entries are never evicted for capacity. Store a successfully verified envelope
only once at its intended receiver; forwarding retains verifiable originals but
does not readmit an original addressed to another hop. Restart changes challenges.
Session challenges must be authenticated before use; a peer transport must not
substitute an unauthenticated challenge. Signature verification precedes replay
insertion so forged packets cannot consume another signer's nonce.

## Ownership and integration

- Security owns `security/*`, this document, and typed verified wrappers.
- Control owns enrollment persistence/TLS and uses security identity construction,
  recovery, validation, key publication and certificate adapters. Private key bytes
  are available only through explicitly named persistence/TLS adapters.
- Runtime owns crypto queues and completion fences. The page engine consumes owned
  jobs and returns every accepted job, including canceled/failed jobs, with buffers,
  key lease and reserved completion capacity retained until consumption.
- Memory owns charged buffer construction. Crypto must never allocate publishable
  buffers by bypassing reservations or expose unauthenticated plaintext.
- Peer transport uses public canonical security encoders and signed envelopes;
  only security verification can mint `VerifiedRequest`/`VerifiedResponse`.
- Key retirement stops new lease acquisition, waits for registered storage,
  checkpoint and transport barriers, then waits for all existing key leases before
  erasing material. Missing barrier integration must fail closed, not report success.

Implementation-specific public APIs and vectors are documented alongside their
exported functions. No caller should duplicate canonicalization in peer code.

### Integration requests (security owner)

Control: the security-owned identity API is
`security::identity::SigningIdentity::from_pkcs8(cluster, node, pkcs8, chain, roots)`
and `Keyring::install_signing_identity(Arc<SigningIdentity>)`. Use this validated
identity for TLS and peer signing; adapt `LocalSigningIdentity` explicitly rather
than make its fields public. Enrollment persistence remains control-owned.

Memory/runtime: provide `PlaintextBuffer::into_parts() -> (Box<[u8]>, Reservation)`
and reservation `amount()` / `class()` accessors. Retain reservation when moving
staging bytes into security-produced authenticated pages. Runtime crypto job and
completion fields already permit crate-visible access; preserve that handoff.

Model: provide `StrongEtag::as_bytes() -> &[u8]` returning the exact quoted strong
ETag. This must not normalize or reinterpret the tag. Opaque context accessors
likewise preserve bytes and presence.

Replay API: `ReplayWindow::challenge()` returns the local receiver challenge;
`Freshness::generate(challenge)` produces a fresh nonce and millisecond timestamp;
`admit_verified(signer, verified_certificate_epoch, freshness)` is atomic and checks
the local challenge. The challenge is not a sender-selected session identifier.

Cross-owner transient verification blocker observed: `src/topology/graph.rs:90`
had an extra closing brace during a security test build. Security does not edit it.

### Plain-HTTP authenticated challenge discovery

`security::session::ChallengeProbe::new(local, remote)` creates a one-use random
outstanding probe valid for five monotonic seconds. Send `request_bytes()` unchanged
(or standard-base64 framed). Receiver calls `session::respond(keys, replay, bytes)`
after bounded handshake admission and returns `ChallengeReply` fields. Requester
consumes the probe with `verify(certificates, reply)`, obtaining an
`AuthenticatedChallenge`. Install its peer/challenge with the signing facade.
Verification binds both Node UUIDs, the fresh requester nonce, receiver challenge,
and responder certificate. This avoids any circular dependency on established HTTP
message-signature sessions or an unavailable peer TLS channel. Discover both
directions before bidirectional signed traffic. Probe responses grant no application
or RDMA permission and allocate no receiver-side session state.

`ChallengeReply::encode/decode` provides the bounded canonical binary reply framing
(64 KiB maximum, eight DER certificates, exactly one Ed25519 signature). Use these
methods rather than duplicating a challenge-reply codec in peer transport.

### Security integration review requirements

The identity and protocol submodules are now registered by the security parent.
All production cluster and Node identifiers must be canonical lowercase UUID text,
not just arbitrary alphanumeric path components. Tests must use canonical IDs.
Certificate checks must require Ed25519 digital-signature usage and client-auth EKU
and reject absent/mismatched usage; bounds apply to the total chain as well as each
certificate. The certificate leaf must not be a CA.

Signature challenge discovery is implemented in `security/session.rs` by the
parent. Add a typed `Signatures::install_peer_challenge(AuthenticatedChallenge)`
adapter when convenient; the existing explicitly authenticated install method is
the lower-level integration entry point. Peer owner must use fresh probe verification
before installing challenges.

The signature profile maximum must agree with `peer::wire::MAX_SIGNED_HEAD` (64 KiB).
Logical encoders validate bounded fields before allocation. Historical original
signatures must also enforce freshness/deadline validity, while nonce insertion is
only for the final receiver-addressed envelope. Immutable response descriptors must
match the requested object/version/page, not only a self-consistent signed response.
The canonical application target is `/racer/peer/v1`; the transport envelope uses
`/racer/peer/v1/exchange`, with the original signed application head carried intact.

Current compiler integration requests:

- Application: replace unit `KeyEpochs` construction with `KeyEpochs::default()`.
- Security/peer immutable signed heads use `Arc<SignedHead>` rather than `Rc` so
  verified requests can cross owned worker mailboxes. Service graphs remain `Rc`.
  Peer `WireCodec` must construct `Arc` for its preserved original.
- Read origin servicing must retain `ChargedOriginContext` for the entire async
  origin operation. It dereferences to `OriginContext`; never detach uncharged raw
  credentials from it. Security will not make `OriginContext` fields public beyond
  the model's existing interface or bypass reservation ownership.

Keyring validation review: equal-generation identical content is idempotent; lower
generation or changed equal-generation content fails. Generation zero fails. Exactly
one active key per represented cache/purpose is required, while a removed scope can
be absent. A removed key must stop new leases immediately, then await registered
retirement fences and existing leases. Reappearing retired IDs cannot resurrect old
material. Retirement is retryable and must retain material if barriers are absent.
