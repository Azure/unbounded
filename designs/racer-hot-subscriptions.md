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
`pkg/racersdk/subscription.go:80-151,653-703`).

## Local demand and ownership

The receiving node retains compact per-version range demand with independent
page and byte credits for each subscriber. Ordered delivery does not serialize
acquisition: it reserves each next page's exact selected slice before dispatching
fixed-page work through the stable page owner and existing singleflight. Different
pages may complete concurrently or out of order, but only the ordered head is
delivered. Pending, ready/reordered, and delivered-unreleased pages together fit
`min(RACER_RANGE_WINDOW_PAGES, page credits)`; byte credits independently bound
their selected slice bytes. Whole-page acquisition still uses ordinary payload
admission, so slice-byte credits are not a physical allocation estimate
(`cmd/racer-dataplane/src/read/subscription.rs:230-271,590-641`,
`cmd/racer-dataplane/src/read/range_stream.rs:259-268,297-355,628-658,699-722`,
`cmd/racer-dataplane/src/read/fill.rs:366-390`).

Unordered readers instead union capacity-bearing demand, exclude already assigned
holes, and serialize provider selection per immutable version. They alternate
oldest-page progress with broader unordered demand. Canonical wire demand is
limited to 64 intervals; excessive aggregate fragmentation returns Overloaded,
not a silently shortened prefix. Ordered readers do not enter that union
(`cmd/racer-dataplane/src/read/subscription.rs:272-342`).

Fixed-page acquisitions may overlap one another, but must not overlap an unknown
provider selection for the same version, which bypasses fixed-page singleflight.
Each capacity-bearing polling demand gets one admission ticket, retained across
polls. Ordered work may batch across earlier ordered tickets, but a waiting
unordered ticket blocks later ordered admissions. Earlier ordered tickets may
each admit one page; selection waits for accepted fixed work to finish, then
takes its turn. Selection also yields to earlier tickets, preventing repeated
selections from starving ordered work. Cancellation removes the waiting ticket
and wakes successors, but accepted workers retain exclusion through actual
completion, not merely client detachment. This is bounded turn arbitration, not
a wall-clock latency guarantee
(`cmd/racer-dataplane/src/read/subscription.rs:234-294,483-535,561-587`).

Unordered selected results are fanned out only to eligible unordered subscribers
with capacity. Each receives its own credit reservation and immutable plaintext
ownership. A slow
reader with no credits is not assigned more work. RangeStream retains plaintext
after delivery until the exact page/length release, independently of the delivery
pipe. Subscriber close detaches demand; accepted worker commands and submitted I/O
retain their own completion owners
(`cmd/racer-dataplane/src/read/subscription.rs:177-228,508-535`,
`cmd/racer-dataplane/src/read/range_stream.rs:270-276,618-694`). Ordered readers
share fixed-page work through singleflight rather than selected-result fanout.

Prefetch is real acquisition progress, not just polling a previously full window.
While the current frame or payload is blocked on the client socket,
`poll_prefetch` polls completions and can admit new work when credits, the window,
budgets, and arbitration permit. Acquisition does not hold a delivery pipe;
ordered delivery admits its pipe only once the head is ready. Backpressure does
not expand either window or credits
(`cmd/racer-dataplane/src/client/subscription.rs:262-280`,
`cmd/racer-dataplane/src/read/range_stream.rs:278-390,628-658`).

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
restart (`pkg/racersdk/subscription.go:358-555,597-651`,
`cmd/racer-dataplane/src/client/subscription.rs:65-120,286-293`).

Get adds one receive goroutine and at most
`min(2, page credits, floor(byte credits / PageSize))` request-local payload
buffers. This includes the consuming lease, queued leases, and in-flight receive,
not two buffers in addition to the current page. Only release makes storage
reusable. OpenPages (even with Ordered) and DownloadTo remain synchronous lease
consumers without this reuse/read-ahead layer. Get keeps its connection admission
slot through consumption, not just wire Complete; terminal reads and Close join
receiver cleanup and drop held, queued, and reusable buffers before returning
capacity. WriteTo's separately bounded 32 KiB scratch prevents a blocked caller
Write from retaining the page buffers during cleanup
(`pkg/racersdk/ordered.go:11-98,100-133`,
`pkg/racersdk/subscription.go:35-45,497-517`,
`pkg/racersdk/value.go:57-103,179-207,217-335`).

## Remote selection, not a persistent whole-object push stream

The node shares one provider Subscriptions scheduler across worker PeerServers
(`cmd/racer-dataplane/src/app.rs:119,158,811`). The unordered receiving path calls
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
narrowed to ensure head progress, contracts have absolute deadlines and
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
- `cmd/racer-dataplane/src/read/subscription.rs:663-836` checks mixed-mode ticket
  fairness under sustained ordered demand and immediate selection retries,
  canceled-waiter wakeups, exact slice credits, and completion-fenced exclusion.
- `cmd/racer-dataplane/src/read/subscription.rs:839-932` checks compact exclusive
  aggregate selection, stable contract IDs, increasing sequences, nonrenewing
  deadlines/budgets, and exact issued-credit release.
- `cmd/racer-dataplane/src/peer/subscriptions.rs:767-1001` checks shared provider
  work, finite capacity, replay/deadline constraints, credential-supplier election,
  shared ciphertext allocation, and per-receiver transfer accounting.
- `cmd/racer-dataplane/src/read/hot_read_tests.rs:175-535` checks ordered same-page
  sharing, byte-credit limits, later-page completion before the head, acquisition
  during blocked socket delivery, fair mixed-mode refill, and cancellation/drop
  exclusion through actual completion. Failure must not restart or admit the tail.
- `pkg/racersdk/ordered_test.go:62-458` checks read-ahead overlap, buffer identity
  reuse only after release, one-buffer credit limits, late-failure prefix delivery,
  withheld invalid final slices, cleanup, and isolation from blocked writers and
  subsequent requests.
- `cmd/racer-dataplane/src/read/hot_read_tests.rs:537-908` builds real Coordinator,
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

The harness defines 15 subtests: four ordered Get sizes; a large read-ahead Get;
partial-range Get with one and two page credits;
partial-range fragmented releases; empty OpenPages; byte-credit exhaustion with
a held final lease; partial/empty DownloadTo; a large DownloadTo; context and
stream-close cancellation while credit is held; and destination failure
(`pkg/racersdk/rust_subscription_interop_test.go:105-417`). Assertions check socket
silence without released page/byte credit, exact payload geometry/content, valid
Complete before final-slice exposure, idempotent release, and recovered admission
after cancellation. SDK accounting checks held bytes, page credits, and bounded
interval state (`pkg/racersdk/rust_subscription_interop_test.go:431-443`).

Both large Get and DownloadTo require exactly `32*PageSize+13` bytes (512 MiB + 13)
with two SDK page credits. DownloadTo checks 33 page offsets; both destinations
validate bytes without an object-sized backing buffer. Get also asserts the
two-buffer limit and clean ordered storage/credit/admission teardown
(`pkg/racersdk/rust_subscription_interop_test.go:137-173,338-347,460-470`,
`pkg/racersdk/ordered_test.go:45-60`).
Rust admits at most four plaintext pages (64 MiB) and asserts the observed charged
plaintext peak stays within that limit. After draining and reclaiming reusable
buffers it asserts zero plaintext, flight, and waiter charges
(`cmd/racer-dataplane/src/subscription_interop.rs:157-162,307-354`). These are
payload-admission/accounting bounds, not a measurement of process RSS or Go heap
high-water usage, a persistence test, or a distributed throughput result.

The [2026-09-29 validation record](racer-ordered-validation-20260929.md) records
the independent live race rerun after the fairness and SDK cleanup fixes, plus
the separate SDK fixture benchmark caveats. No historical deployed results or
performance numbers are transferred to this implementation.
