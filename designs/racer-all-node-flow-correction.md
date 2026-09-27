# All-node flow: cut-through admission candidate

Status: implemented and locally verified correction; fleet recovery requires deployment.
Base: `8ea0c92c24dbb68324ff76ecadbbb97141927c0b`.
Worktree: `tmp/racer-fix-all-node-flow`, branch `racer-fix-all-node-flow`.
All abbreviated source paths below are under `cmd/racer-dataplane`.

## Implemented result

The sections below preserve the investigation and approved design rationale.
The final production path is `PeerServer::serve_exchange` -> `Relay::serve_stream`
-> non-cloneable `TransitBody` in `src/peer/stream.rs`. Its descriptor verification
uses `Forwarding::verify_response_head` and the same canonical response encoder as
whole-page verification. No fake ciphertext or seeded page reference is used by
the production path or final cut-through progress tests.

One 16 KiB chunk and its Relay permit move together into every body reactor
operation. Nonwaiting outbound checkout and nonwaiting handshake/probe checkout
avoid local resource hold-and-wait. Both successful and failed terminal operations
retain existing FD completion fences. Metadata outcomes use the same zero-body
stream path. Direct native exchange is unchanged; relay hops choose the existing
ordinary HTTP response instead of negotiating native transfer.

Admission protects `min(relay slots, queue entries, connections/2, available
chunks, one-page/chunk)` chunks within the existing Ciphertext ceiling. A worker
must fit its existing window/storage progress floor plus the derived reserve.
Tiny invalid worker budgets fail partition validation; standalone local-only
fixtures cannot enable transit below one full page plus a chunk. Separate atomic
ordinary/transit counters avoid cross-thread release snapshots borrowing another
role's headroom. Aggregate admission still enforces the configured total.

The existing candidate sequence initially failed the SDK transient-saturation
case under nonwaiting relay admission. CandidatePolicy now waits 200-250 ms after
overload before advancing to the next already-budgeted candidate, dropping its
ciphertext output during backoff and reacquiring before submission. Cancellation
and original budget deadline terminate the wait. No attempt, link credit, response
refund, or deadline is added. Unit assertions cover release, restore, cancellation,
deadline and abandoned-wait accounting; the existing warm/cold SDK saturation
fixture demonstrates successful full-image acquisition after finite pressure.

The new path initially used `RouteBudget::forwarded`, whose validation rejects a
visited-plus-links sum accepted by the existing signed failure-route profile.
The final code uses the exact effective-budget transition of buffered Relay,
then `Forwarding::append_request` validates the signed transition. It preserves
eight-link candidate fallback behavior rather than changing routing/security caps.

### Final verification

- Release library: **575 passed, 21 ignored**. Includes native fallback, security,
  partial-header/FD fences, new descriptor validation, tiny quota and backoff tests.
- Production dataplane integration: **11 passed**.
- Existing actual Go SDK variants: **7 passed**, each warm and cold, manifest,
  config and eight layer lengths/digests, four concurrent layers. These are the
  existing fixture settings, not 1500 active clients or stock concurrency 64.
- Actual Fill crossing: **2 passed**: 4 KiB/16 MiB success cases and corrupted
  streamed ciphertext rejected by requester's AEAD. Success fixture starts seven
  full-output owners at each ingress under the live 128 MiB ciphertext ceiling.
- Intersecting-route suite: **5 passed**, including small/full-page production
  stream progress, signed overload unwind, and retained diagnostic counterexamples.
- New stream lifetime test: success, short body, cancel/drop during pending receive
  and backpressured send, exact output bytes and zero final charges.
- Descriptor test: wrong body length, wrong binding, signature corruption and
  replay rejected without constructing a page body. Oversized body lengths fail.
- Release binary build passed. Crate rustfmt and scoped `make fmt` passed.
  Clippy warning-mode completed; existing repository warnings mean this is not a
  warning-free Clippy claim.

The identical final crossing fixture was temporarily forced through the existing
buffered relay using its test-only selector. It failed on signed Overloaded;
restoring production cut-through passes. This is a controlled old-path/new-path
comparison, not a claim that the entire working tree was reset to the base.
The earlier exact-base regression is described in ignored checkpoints. All added
tests remain; the unused prototype adapter was removed after real integration.

Ignored logs are `tmp/final-{lib,production,sdk,crossing,build}.log`,
`tmp/old-fail.log`, `tmp/{cut-through,cut-pressure,intersecting,reverse-ready}.log`
and `tmp/make-fmt.log`. Earlier failed experiment logs are not final acceptance.

### Remaining limits

The 1500-member graph test runs six actual peers. Low Relay/Connection limits
prove prompt signed failure/unwind, not universal success under sustained overload.
The SDK fixture verifies recovery from finite saturation; no fleet throughput or
fairness guarantee follows. Whole-page acquisition policy, bounded candidate
credits and reader deadlines can still terminate requests under sustained load.
No live rollout occurred in this worktree. Parent must build racer-dataplane and
verify the 1500-client c1/layer4 run, then stock concurrency, before claiming the
all-node incident resolved. No other image is affected by production changes.

## Investigation and design rationale (historical)

## Established mechanism

Local Fill reserves a full output before requesting peers (`src/read/fill.rs:468-483`).
Neighbor admission can park while that output is held (`src/http/pool.rs:328-400`).
A relay instead allocates its response after the downstream head arrives, from
the same ciphertext quota (`src/peer/transfer.rs`, `exchange_reserved`). These
are different resource owners; moving the same charge through receive/decode
does not solve competing local acquisitions versus transit.

`crossing_fills_leave_transit_progress` uses actual Fill owners and fails on the
base's signed Overloaded response. Earlier live evidence and its limitations
are indexed in ignored `tmp/checkpoint.md`.

The actual 1500-member radix-18 topology permits opposite relay order:

```
X: 3  -> 0 -> 1 -> 18
Y: 19 -> 1 -> 0 -> 2
```

The experiment verifies the middle routes using production `Paths::shortest`.
Only six nodes have running peer endpoints; this is not 1500 running dataplanes.

Two superficially plausible corrections fail:

1. Output before forwarding: relay 0 holds X's output while X waits at relay 1;
   relay 1 holds Y's output while Y waits at relay 0. Demonstrated with one and
   two protected slots, small and full destination pages, and no local Fill.
2. Output only after downstream readiness plus bounded receive waiting: relay 1
   receives X from destination 18 and holds X while sending toward relay 0;
   relay 0 receives Y from destination 2 and holds Y while sending toward relay 1.
   Each needs receive space occupied by its reverse-direction send. The new
   `response_ready_transit_cycles_with_saturated_reverse_buffers` observes this
   cycle with actual full-page bodies and production send/receive operations.

Deferring or releasing local Fill output before a peer-slot wait cannot remove
the second cycle: neither relay has a local Flight owner in that experiment.
Consequently this candidate does not rely on changing Fill's output handoff.

## Tested feasible ownership rule

Each transit exchange owns one fixed, small chunk, independent of page size.
It must obtain all local forwarding capacity by nonwaiting try-admission before
submitting downstream work. Once accepted, it requests no additional byte, relay
or connection capacity while waiting for downstream or upstream I/O. The chunk
is reused only after each receive/send completion, never while in flight.

The test-only `cut_through_exchange` applies this rule:

- Production request decoding, signature verification and route selection.
- Try-admit a Relay permit and a 16 KiB Ciphertext chunk, then use the existing
  nonwaiting pool checkout. Failed checkout drops the preceding owners.
- Forward the signed request using production HTTP and reactor operations.
- Verify the complete response signature chain before forwarding the head.
- Move exact ciphertext bytes through the chunk; hold the same chunk and permit
  across partial receive and upstream send completion.
- The requester verifies the response binding/chain and compares every received
  ciphertext byte with the seeded destination copy.

On the same reverse-capacity-limited graph, the prototype completes all four
small/full-page, two/four-concurrent-request cases with zero errors. Source:
`src/peer/intersecting_flow_tests.rs:11-119`, `:215-221`, `:645-684`.
Log: ignored `tmp/cut-through.log`.

It also exercises real low Relay and Connection limits, with no injected quota
holders. For each limit, two of four requests complete and two fail promptly;
the test asserts the failures, all requests terminate without deadline expiry,
and final resource accounting is zero. This demonstrates unwind, not successful
retry or all-node availability. Source: `:242-246`, `:378-387`, `:645-684`.
Log: ignored `tmp/cut-pressure.log`.

### Scope of the acyclicity argument

- Forward direction: no admitted request waits for another request to release a
  local Relay/Connection/chunk slot. Downstream resource failure returns an error
  and unwinds. Existing request visited sets forbid loops in one route.
- Reverse direction: every admitted transit request already owns its receiving
  chunk. A reverse send cannot wait for the next hop to acquire a shared full-page
  buffer. Each chunk follows its own request's finite reverse path to the sink.
- Sinks must retain admitted Fill receive/decrypt output through the request;
  otherwise sink admission can recreate a dependency. This favors keeping Fill's
  existing output handoff while protecting a small transit reserve from it.
- Metadata/handshake/control paths must obey nonwaiting forwarding admission too.
  A hidden call to waiting `checkout_peer` during handshake would invalidate the
  argument. The prototype uses preconfigured identities and no live discovery.
- Network failure, slow readers and quota overload remain possible. Original
  deadlines bound them. This argument removes these resource wait cycles; it is
  not a proof of throughput, fairness, or completion before every deadline.

## Minimal production design to review before implementation

This requires a substantive internal response API change. It does not require
new wire fields, changed signatures, page encryption, dependencies, or a different
read acquisition graph.

### Separate verified descriptor from owned stream

Today `Forwarding::verify_response` takes `SignedResponse` containing a complete
CiphertextPage (`src/security/forwarding.rs:312-384`). Its canonical re-encoding
checks the descriptor and body length (`src/security/protocol.rs:314-359`).
`SecurityCodec::response_reserved` constructs that complete page before it can
be passed to verification (`src/peer/decode.rs:226-295`).

Introduce a security-owned verified response-head type. Verify canonical logical
fields, signed lengths, request binding, responder authority, reverse path,
signatures, and replay exactly once. Encoding has one shared descriptor helper;
do not duplicate canonicalization in peer transport. Header verification proves
the envelope, not ciphertext integrity or completed transfer.

Pair that head with a non-cloneable, peer-owned body stream holding the downstream
ConnectionLease, exact remaining body length, and completion-owned chunk permit.
Relay appends the reverse signature to the verified head and streams opaque bytes
to its existing ingress connection. The requester collects into its pre-admitted
Fill output, validates exact length, and performs existing AEAD authentication
before plaintext becomes publishable. Local service responses can wrap existing
CiphertextPage owners. Native transfer may keep its whole-page path; native
fallback must select a compatible owner without repeating signature admission.

The prototype uses a clone of the already seeded destination ciphertext solely
to satisfy the current full-page verification API. Those bytes are not copied
through the relay, and the requester compares the actual socket result. This
test shortcut must not enter production. The new descriptor-only security API
is the required replacement.

### Small protected reserve under the unchanged total ceiling

Use a small protected chunk allowance within the configured Ciphertext ceiling,
not a page per hop or a new unaccounted buffer class. A concrete starting rule is
16 KiB per effective active transit slot. Derive effective slots from the minimum
of configured relay slots, queue entries, connection capacity and available byte
headroom after one full Fill page. Cap protected chunk bytes to one full page.
All non-transit Ciphertext paths, including storage staging and native output,
must respect the reduced non-transit ceiling; all charges still count toward the
original aggregate ceiling. Slot counts and arithmetic must be checked.

Reject a worker configuration that cannot fit one full Fill page plus one chunk
or the necessary ingress/outbound connections. Do not silently create an
impossible wait. Validate after worker partitioning and test the exact boundary,
one-byte-short, overflow and small-slot cases. Existing standalone small-buffer
unit fixtures may operate without activating the network progress policy.

### Admission failure and retry ownership

Relay/connection/chunk try-admission failure before forwarding must return a
bound signed Overloaded response where possible, without emitting a page head.
No relay retries while holding upstream/downstream resource owners. Original
request deadlines and acquisition credits remain unchanged.

The current CandidatePolicy advances through candidates after overload. A
production correction must test whether those existing attempts suffice under
distributed pressure; the prototype does not establish that. If a bounded
restart is necessary, it must live at the acquisition owner after the failed
exchange is fenced, debit existing credits, and release output/transport permits
while backing off. It must not restart a partly exposed plaintext page, replenish
credits, reset deadlines, or silently replay a signed request. This remains a
required experiment, not permission for an unbounded retry loop.

### Partial data and security failures

No error can replace a response head already sent upstream. A truncated/corrupt
body terminates the peer stream; the receiving coordinator cannot publish a page
until complete AEAD verification. Existing SDK continuation and origin-abort
tests must validate late failure semantics. Active partial heads and accepted
FD cancellation fences stay on the existing PeerServer/HttpIo/Reactor paths.

## Current verification and handoff

Feasibility tests use bounded commands, original deadlines and full cleanup
accounting. All earlier diagnostic tests remain. Final logs:

- `tmp/intersecting.log`: output-before-forward cycle, four cases confirmed.
- `tmp/reverse-ready.log`: response-ready reverse cycle confirmed.
- `tmp/cut-through.log`: four cut-through integrity cases pass.
- `tmp/cut-pressure.log`: two scarce-resource cases unwind without deadline stall.

These are not the requested final SDK/library/production acceptance checks.
No production correction, commit, image build, deployment, or fleet recovery is
claimed. Test hooks are cfg(test) only. No AKS operations occurred during this
research continuation. The next step is the internal verified-head/body-stream
API change described above, followed by production graph, security, SDK warm/cold,
library and production tests and a single reviewed fix commit.
