# Racer replacement subscriptions

## Scope

The implemented client API is a breaking replacement, not an optional fast path.
Body reads use one duplex `POST /v2/objects/<key>`; client GET bootstrap and
continuation windows are no longer accepted. HEAD remains metadata-only on v1
and v2 paths. Origin v1 HEAD, bootstrap GET, and pinned whole-page GET are unchanged
(`cmd/racer-dataplane/src/client/request.rs:217-237`). See the normative
[client/origin contract](../cmd/racer-dataplane/CLIENT_ORIGIN_API.md) and
[SDK design](racer-sdk.md) for framing and public APIs.

One immutable version and selected byte range define the page membership.
Delivery may be out of order without changing that membership. OpenPages exposes
page-numbered slices and explicit leases; Get forces ordered delivery as an
io.Reader adapter. DownloadTo writes at absolute offsets through io.WriterAt and
releases each lease after the destination returns. Neither adapter opens a second
client request (`pkg/racersdk/client.go:241-255`,
`pkg/racersdk/subscription.go:72-105,298-318`).

## Local demand and ownership

The receiving node retains compact per-version range demand with independent
page and byte credits for each subscriber. The production poll path unions
capacity-bearing demand, excludes already assigned holes, and serializes the next
receiving selection per immutable version. It alternates oldest-page progress
with broader unordered demand; ordered readers offer only their next page.
Canonical wire demand is limited to 64 intervals. Excessive aggregate fragmentation
returns Overloaded, not a silently shortened prefix
(`cmd/racer-dataplane/src/read/subscription.rs:223-289,431-446`).

Verified results are fanned out only to eligible subscribers with capacity. Each
receives its own credit reservation and immutable plaintext ownership. A slow
reader with no credits is not assigned more work. RangeStream retains plaintext
after delivery until the exact page/length release, independently of the delivery
pipe. Subscriber close detaches demand; accepted worker commands and submitted I/O
retain their own completion owners
(`cmd/racer-dataplane/src/read/subscription.rs:173-220,456-525`,
`cmd/racer-dataplane/src/read/range_stream.rs:263-269,502-565`,
`cmd/racer-dataplane/src/read/dispatch.rs:198-205`).

Selected ciphertext is dispatched to its stable page owner before plaintext
allocation and authentication. That owner admits a local ciphertext charge when
the receive charge belongs to another worker, then reserves plaintext, decrypts,
and publishes. Exclusive ciphertext can move without copying; shared transport
owners require an admitted copy and retain their original bytes/charge until
completion. This lets the caching owner reclaim its own payload budget rather
than pinning a foreign worker's receive budget indefinitely
(`cmd/racer-dataplane/src/read/dispatch.rs:243-269`,
`cmd/racer-dataplane/src/read/fill.rs:136-190`,
`cmd/racer-dataplane/src/memory/pool.rs:184-206`).

Client credits are 1..64 pages and 16 MiB..1 GiB of slice bytes, defaulting to two
pages and 32 MiB. They bound outstanding assignments and delivered leases, not
the object's page count. The SDK reads a full slice before exposing it and bounds
duplicate tracking to 4096 merged intervals. Release is idempotent in the SDK,
but each wire release must match an outstanding issued slice exactly once.
Complete validates total pages/bytes and does not wait for final releases. Close,
malformed frames, missing Complete, and cancellation never trigger a transparent
restart (`pkg/racersdk/subscription.go:180-295`,
`cmd/racer-dataplane/src/client/subscription.rs:65-120,266-272`).

## Remote selection, not a persistent whole-object push stream

The node shares one provider Subscriptions scheduler across worker PeerServers
(`cmd/racer-dataplane/src/app.rs:119,158,811`). The production receiving path calls
CandidatePolicy::subscribe through Requester and the authenticated peer v5
transport. It routes to the primary of the oldest demanded page. The provider
chooses an eligible sweep endpoint from the compact demand, and Acquire permits
only a page for which that provider is also primary under the same membership.
The receiver recomputes placement for the actual selected page. CopyOnly does not
grant origin authority; the existing verified Page service and Fill remain the
authority boundary (`cmd/racer-dataplane/src/read/candidates.rs:55-138`,
`cmd/racer-dataplane/src/peer/server.rs:567-674`).

Each exchange returns **one** Selected page or an outcome. The receiving scheduler
retains a bounded contract keyed by version, membership, and provider; the provider
keys retained entries by membership, receiving node, and subscription ID. The
receiver increments sequence and conservatively spends one page and maximum
ciphertext bytes before each exchange. The contract's deadline can only shorten.
These cumulative ceilings neither allocate payload memory nor authorize parallel
transfers (`cmd/racer-dataplane/src/read/subscription.rs:43-89`).

At the provider, only one live ID is allowed per receiving node/version/membership.
Updates require a strictly greater sequence, the same version, no later deadline,
and no larger remaining transfer budgets. Completed pages remain excluded from
selection. Expired entries retain replay tombstones for the 60-second signature
freshness window. Default node-wide limits are 1024 entries, 4096 completed
intervals, 64 acquisition owners, and 256 pending response slots; unconsumed
completions/errors count toward pending capacity
(`cmd/racer-dataplane/src/peer/subscriptions.rs:86-104,239-344,365-386,449-525`).

The provider ranks compact demand endpoints by distinct interested receiving
nodes and shares an in-flight page acquisition across those nodes. Successful
responses share one ciphertext allocation, with separately bound grants and
charged transfer ceilings. Credential rejection can elect one retained follower
to retry with its own verified request and original route budget; it does not
copy one supplier's credential failure into every follower's result or refund
authority (`cmd/racer-dataplane/src/peer/subscriptions.rs:389-437,449-525`,
`cmd/racer-dataplane/src/peer/server.rs:611-672`).

This is not a claim that every receiving node advertises its complete object to
every provider, that a provider keeps pushing until EOF, or that subscriptions
survive disconnect/restart or membership changes. Placement filters sweep endpoints,
not an exhaustive expansion of every page in each interval. Local demand may be
narrowed to ensure ordered/head progress, contracts have absolute deadlines and
finite transfer ceilings, and each selection needs another bounded exchange.
Failed remote selection can fall back through the existing fixed-page candidate
path with remaining acquisition credits; it does not issue speculative backup
Acquire subscriptions (`cmd/racer-dataplane/src/read/candidates.rs:55-138`,
`cmd/racer-dataplane/src/read/fill.rs:88-162`). Before remote selection, the stable
owner checks for an already verified local head page, allowing a late or formerly
credit-starved reader to reuse it without another node transfer.

The subscription exchange receives a local per-attempt time share that reserves
time for fixed-page fallback. Its signed contract retains the original bounded
deadline instead of replacing it with that shorter attempt deadline; a later
exchange cannot thereby renew provider authority. Attempt timeout cancels only
the attempt, continues polling until accepted transport work completes, and maps
to Unavailable while the parent budget remains live. Fallback uses the remaining
original attempts/links, not refunded credits
(`cmd/racer-dataplane/src/read/candidates.rs:86-108,460-587`).

The signed grant binds selected version/page, membership, receiver, subscription
ID, subscription sequence, absolute deadline, and remaining page/byte ceilings.
Socket session sequence is a separate field. Request MAC, certificate/session
admission, response binding, AEAD verification, and native completion fences are
not replaced by subscription scheduling. See [peer security](racer-peer-security.md).

## Evidence and validation scope

The following are inspected test assertions, not a new test-run report:

- `pkg/racersdk/subscription_test.go:148-311` checks out-of-order slices, credit
  waits, idempotent release, malformed/missing Complete, DownloadTo absolute offsets
  and short/error writes, cancellation, duplicates, and ordered-mode rejection.
- `cmd/racer-dataplane/src/read/subscription.rs:545-638` checks compact exclusive
  aggregate selection, stable contract IDs, increasing sequences, nonrenewing
  deadlines/budgets, and exact issued-credit release.
- `cmd/racer-dataplane/src/peer/subscriptions.rs:767-1001` checks shared provider
  work, finite capacity, replay/deadline constraints, credential-supplier election,
  shared ciphertext allocation, and per-receiver transfer accounting.
- `cmd/racer-dataplane/src/read/hot_read_tests.rs:175-546` builds real Coordinator,
  Requester, TCP/session, provider, and Fill paths under one membership. Assertions
  check page order 0, 2, 1, 3; page-2 work shared across two receiving nodes; local
  fanout without an extra receiving transfer; warm local-head reuse without another
  peer request; and four provider origin calls with none on receiving nodes. It is
  a component integration test, not deployed Go SDK
  acceptance or a general exactly-once origin guarantee.
- `cmd/racer-dataplane/src/read/fill_tests.rs:381-482` drives eight full pages
  through distinct source/target admissions under a two-page plaintext limit.
  It asserts target ownership of both payload charges, correct bytes, no target
  origin fill, cancellation before acceptance, and reclaimable source capacity
  while the target retains its cached page.
- `cmd/racer-dataplane/src/read/dispatch.rs:1047-1102` asserts that cancellation
  leaves the selected-owner receipt pending and its mailbox slot occupied until
  the actual completion arrives.
- `cmd/racer-dataplane/src/read/candidate_timeout_tests.rs:210-310` checks the
  shorter local attempt deadline, unchanged signed deadline, cancellation fencing,
  successful fixed-page fallback authority, and a subsequent contract update with
  the same ID, increasing sequence, and decreased transfer ceilings.

Older GET-based process, conformance, E2E, and throughput reports are historical.
The process, conformance, and E2E client fixtures now use subscriptions, not dual
GET support. Their inspected contracts are:

- Process clients send v2 POST with one page credit
  (`cmd/racer-dataplane/tests/process_restart.rs:382-404`,
  `cmd/racer-dataplane/tests/process/throughput.rs:50-67`). The completion oracle
  validates metadata, ordered page geometry, every payload byte, exact releases
  between pages, Complete counts, and transport EOF
  (`cmd/racer-dataplane/tests/process/measurement.rs:79-157`).
- The scripted Go/Rust conformance driver asserts one ordered Subscription with
  one page credit and preserved opaque context, then verifies release page/length
  before sending each subsequent page and the terminal frame
  (`cmd/racer-dataplane/tests/conformance/sdk.rs:101-159,325-432`). This remains
  scripted HTTP/request-parser coverage, distinct from the live read graph below.
- Deployed peer/lifecycle probes send v2 POST and validate 200, selected metadata,
  and payload (`e2e/racer/peers_test.go:321-353`,
  `e2e/racer/lifecycle_test.go:149-202`). Their shared decoder checks Page/Complete
  fields and body EOF (`e2e/racer/subscription_test.go:22-70`). These probes request
  at most one page, so Complete permits termination without a final release;
  they do not exercise multipage duplex credit return.

### Live Go SDK/Rust interoperability

`TestRustSubscriptionInterop` starts a Rust subprocess serving production
ClientListeners, Coordinator, RangeStream, Fill, and crypto over a real Unix
socket. The fixture generates origin content and supplies initial publication;
it does not script subscription framing. It is single-node, with no peer traffic,
and discards unsubmitted persistence work
(`pkg/racersdk/rust_subscription_interop_test.go:21-103`,
`cmd/racer-dataplane/src/subscription_interop.rs:42-123,265-320`).

The harness defines 12 subtests, not a test-run total: four ordered Get sizes;
partial-range fragmented releases; empty OpenPages; byte-credit exhaustion with
a held final lease; partial/empty DownloadTo; a large DownloadTo; context and
stream-close cancellation while credit is held; and destination failure
(`pkg/racersdk/rust_subscription_interop_test.go:105-316`). Assertions check socket
silence without released page/byte credit, exact payload geometry/content, valid
Complete before final-slice exposure, idempotent release, and recovered admission
after cancellation. SDK accounting checks held bytes, page credits, and bounded
interval state (`pkg/racersdk/rust_subscription_interop_test.go:329-339`).

The large case requires exactly `32*PageSize+13` bytes (512 MiB + 13) and 33 page
offsets with two SDK page credits. Its WriterAt validates bytes without an
object-sized backing buffer (`pkg/racersdk/rust_subscription_interop_test.go:252-259,354-362`).
Rust admits at most four plaintext pages (64 MiB) and asserts the observed charged
plaintext peak stays within that limit. After draining and reclaiming reusable
buffers it asserts zero plaintext, flight, and waiter charges
(`cmd/racer-dataplane/src/subscription_interop.rs:157-162,307-354`). These are
payload-admission/accounting bounds, not a measurement of process RSS or Go heap
high-water usage, a persistence test, or a distributed throughput result.

This documentation review records code assertions rather than new execution
results. No previous test totals, deployed results, or performance numbers are
transferred to the replacement implementation.
