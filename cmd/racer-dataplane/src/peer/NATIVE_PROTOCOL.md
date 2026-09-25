# Native payload protocol v1

This peer-owned extension uses `Signatures::sign/verify` without a separate signing
algorithm or signature-base encoder. `protocol::agrees` enforces exact application
fields for each state. Security review has not been independently completed; the
available session tools provide no agent messaging API. This document records the
implemented schema for that review rather than claiming external approval.

Every control has `content-length`, `racer-kind: payload-v1-<state>`, receiver, and:

- `racer-payload-request`: SHA-256 of domain `racer-peer-v1/payload-envelope\0`,
  u64-BE hop count, and the security-owned `signed_digest` of original then each hop.
- `racer-payload-response`: the same digest for the immutable response envelope;
  zero only in the initial accept.
- `racer-payload-transfer`: fresh random 16-byte transport identity.
- `racer-payload-previous`: security-owned digest of the immediately preceding
  signed control, zero only in accept.
- `racer-payload-membership`, `racer-payload-deadline`, `racer-payload-rail`:
  minimal decimal version, absolute security-profile deadline, and rail ID.

Binary fields use the security profile's canonical padded base64. All these fields
are RFC 9421 signed components. Unknown/missing/duplicate fields, wrong signer,
receiver, state, request, response, predecessor, rail, or deadline fail closed.
Page bytes are never signed or hashed. Their existing AEAD descriptor and tag remain
authoritative. Controls neither grant origin access nor change logical routing.

## Connection state machine

1. `accept` accompanies the original request in outer `racer-payload-control`.
2. `offer` accompanies the unchanged signed response envelope, with outer length
   zero, and `racer-rdma-setup`. The ordinary logical response still signs its exact
   ciphertext length. The offer digest binds that envelope, including every hop.
3. Receiver sends `setup` with its setup and `racer-rdma-setup-binding` acknowledging
   the offer. Sender finishes its session and replies `ready` with its original
   setup and the reciprocal setup binding. Receiver checks the original setup is
   unchanged, finishes its session, and awaits asynchronous readiness.
4. Receiver allocates a scoped window, waits for bind CQE, and sends `grant` with
   `racer-rdma-descriptor`. Sender verifies descriptor/session/transfer/length,
   performs the write, fences, and sends `complete` with `racer-rdma-completion`.
5. Receiver verifies completion, invalidates/fences, copies ciphertext, and sends
   `done`. Sender responds `finish`, length zero. Logical verification and AEAD
   authentication follow their existing boundaries.

Requests use POST `/racer/peer/v1/payload`; responses use status 200. Each complete
control round resets HTTP framing before the next request. Intermediate controls
are framed as single signed-head envelopes; original and hop signatures are never
replaced. Each exclusive socket owns all exchange state; there is no token lookup
table, reconnect resume, detached grant, or independently rerouted response.

## Fallback

Before offer, unavailable native resources use ordinary HTTP. After offer, receiver
may send `fallback` instead of setup/grant/done. Sender-native failure returns
`failed` instead of ready/complete; receiver fences and requests `fallback`.
Sender fences before replying with `finish` attached to the original response
envelope and real HTTP ciphertext. Finish signs the exact HTTP body length and
fallback predecessor. The native transfer ID remains the correlation ID for its
terminal fallback, but no native grant is reused and no new native write is issued.
All future native attempts allocate a fresh ID, QP, and window. Authentication,
deadline, and cancellation failures close the attempt without fallback.

## Review points

- The payload-envelope digest is domain-separated and contains only security-owned
  canonical signed-head digests. Security remains the signing-bytes authority.
- Allocation/decode is bounded; grants are exported only after bind completion.
- Prepared sessions have no exported grant; dropping one requests quarantine.
  Connected sessions use terminal fence completion before HTTP fallback.
- The complete signed response path must select the offered rail. Local hardware
  activation can veto membership, never invent a mapping.
- Failed control sockets close the attempt; logical acquisition policy owns any
  later retry and retains original budget/deadline constraints.
