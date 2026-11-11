# Racer: out-of-order fetch, in-order delivery

## Summary

Large reads should move every page of the requested range onto the requesting
node as fast as the cluster allows, in whatever order pages arrive, while the
client still receives pages strictly in order.

Today the dataplane exposes two ordering modes to clients through the
`Racer-Ordered` request header:

- Ordered mode (what the Go SDK always requests) fetches a small window of
  pages (`range_window_pages`, default 2) one page per acquisition and
  delivers them in order. One slow page blocks the pages behind it.
- Unordered mode lets a peer choose which page of the range to send, one page
  per round trip, and only one such selection per object version can run on a
  node at a time. Nothing in the SDK uses it.

Warming, which this document originally also changed, has since been removed
and replaced by prefetch tickets (`designs/racer-prefetch.md`). Prefetch never
gates or delays reads, so this document no longer has warm rules.

This change removes the ordering choice from the client contract. Ordering
terminates at the dataplane: the dataplane always fetches out of order and
always delivers in order.

## Goals

- One read path. Fetch out of order, deliver in order.
- Pages already in local memory or on local disk are served without waiting
  for earlier pages to be fetched.
- Peers may send pages of the requested range in any order, with more than one
  page in flight per peer.
- Pages that arrive ahead of the delivery cursor are buffered in memory and
  spill to disk through the existing hold mechanism. No new caching mechanism.

## Non-goals

- Changing page size, page verification, or zero-copy delivery.
- Changing eviction policy.
- Changing how the client grants and releases credits.

## Design

### 1. (Removed) Warm progress

This section described warm job progress. Warming is removed; see
`designs/racer-prefetch.md`.

### 2. Holds release individual pages

Holds (`store/holds.rs`, `store/catalog.rs`) are per version per worker and
already hold an arbitrary page set (`HoldPages`, `reserve_hold_pages`).
Releasing today frees only unused capacity; written pages stay held until
expiry.

Holds gain per-page holder counts. `reserve_hold_pages` increments the count
for each member page and `release_hold_pages(version, pages)` decrements it.
A page whose count reaches zero leaves the hold and becomes evictable like any
other cached page. A read's spill pages can be released as soon as they are
delivered without disturbing another read that holds the same page.

### 3. Per-read fetch job

`read/range_stream.rs` replaces the `Scheduler`/`Demand` machinery and the
ordered/unordered split with one job per read stream.

State: a delivery cursor `next`, and a fetch window of pages starting at
`next`. Each window page is one of `Probing` (memory lookup on its owner),
`Missing`, `Fetching` (direct fill), `Selecting` (covered by an outstanding
peer selection) or `Ready` (page or error).

Window size: at most `range_window_pages` (default 2, maximum 64). Each page
takes its exact client credit (`Window::reserve`) when it enters the window and
is marked issued on delivery. So the window is also limited by unreleased
client credit, and window pages plus delivered, unreleased pages never exceed
the client's credits. This keeps the existing plaintext floor
(`(range_window_pages + 1) * PAGE_BYTES`) valid.

Each poll:

1. Refill the window. A page entering it reserves credit and starts a memory
   probe (`cached_page`). Hits become `Ready`. A miss or a failed probe becomes
   `Missing`.
2. Start a direct fill (`Work::Fetch`, normal Fill)
   for the head page, and for up to `DIRECT_AHEAD = 1` further `Missing`
   page. The head is always filled directly; pages ahead are skipped when
   another read of the version is already selecting or filling them.
3. Send the remaining `Missing` pages to peers as selection demands, at most
   `SELECTIONS = 4` per read. A read always may start one selection; it
   starts more only while some provider contract of the version has an unused
   granted flight (`Scheduler::spare`). Each selection demands a disjoint
   share of the selectable holes: the holes divided evenly over the selections
   the read can start now, so a single-flight read still demands every hole
   and lets the provider choose. A node keeps one contract per (version,
   membership, provider). `Scheduler::contract` issues the next sequence and
   refuses with `Overloaded` once the contract has its granted number of
   selections in flight; dropping a `Selection` frees its flight. Each request
   asks for `REQUESTED_FLIGHTS = 4` (section 4). The grant starts at 1, takes
   the provider's `Selected` grant (never above the request), and drops back
   to 1 when the provider refuses a multi-flight request.
4. Every selected page, and every directly filled page, is offered to all open
   reads of the version whose window still wants it (fan-out).
5. Deliver `next` when it is `Ready`, attaching it to the client as before
   (`Delivery::attach`).

Errors: a failed direct fill stays in its slot and ends the stream when it
reaches the head, as before. Pages after it are not admitted, and no new
selections start, but earlier holes are still filled directly so the error can
reach the head. A failed selection,
including its own child deadline, returns its pages to direct fills only. The
stream's own cancellation and deadline are always fatal.

#### Spill (optional)

`RACER_RANGE_SPILL_PAGES` (default 0, off; at most 1024) lets a read park
pages beyond its window on local disk. For pages in
`[window end, window end + range_spill_pages)`, with at most two in flight:

- Reserve one holder on the page's owning worker only (`Work::Hold`, deadline
  `min(60s, remaining request time)`).
- Fill it through the spill path (`Work::SpillFill`), which persists it held.
- When the window reaches the page, read it back from memory or the held disk
  copy (`Work::WarmLocal`, the local held-copy read). If that fails, fetch it
  normally.
- Release the holder (`Work::Release`) once after delivery, and every remaining
  holder on cancel or drop. A hold still pending at that point is released
  once its completion shows it applied. Hold expiry is the backstop.
- If a hold is refused or a spill fill fails, the read stops spilling and keeps
  only its memory window; the failed page is fetched normally.

### 4. Peer protocol: several pages in flight per subscription

On the serving peer, `peer/subscriptions.rs` allows one pending page per
subscription (`Entry.pending: Option<u64>`). It becomes a bounded set.

- Subscribe carries a new optional field, the number of selections the reader
  wants in flight. The server grants `min(requested, server cap)` where the cap
  is a new `SubscriptionLimits` field bounded by the existing `max_inflight`
  and `max_pending` limits.
- Each transfer grant is still exactly one page, so verification and zero-copy
  delivery do not change.
- Compatibility: `peer/protocol.rs` rejects unknown fields (`agrees()`), so an
  older peer refuses a Subscribe that carries the new field. The reader then
  marks that provider as single-flight and retries without the field. The peer
  wire `VERSION` does not change.

Implemented as: the wire header `racer-subscription-flights`
(`Subscription.flights`, `TransferGrant.flights`, both `Option<u16>` at most
`MAX_SUBSCRIPTION_FLIGHTS = 64`); `SubscriptionLimits.max_flights_per_subscription`
(default 8) caps the grant at `min(requested, cap)`. The requester side
(`Candidates::subscribe_provider`) returns `ProviderSelection::{Local, Skipped,
Refused, Selected}`. An unanswered multi-flight request (connection drop, I/O
error or `Unavailable`) is a refusal: the provider is remembered as
single-flight for 5 minutes (at most 1024 peers) and the contract's grant drops
to 1.

### 5. `Racer-Ordered` is removed

- Rust: `ReadKind::Subscription.ordered` and the `racer-ordered` header are
  removed from `racer_wire::object`. The server ignores unknown headers, so an older
  SDK that still sends `Racer-Ordered` keeps working.
- Go: `SubscriptionOptions.Ordered`, the header, its allowlist entries and the
  `ordered` flag on `Sequence` are removed. `Sequence` always enforces order.

Rollout: an older dataplane treats a missing header as unordered, and the new
SDK rejects out-of-order pages. Roll the dataplane before the SDK.

## Testing

- Holds: refcounted reserve/release, release to zero makes the page
  evictable, one reader's release leaves another's holder, expiry still
  applies.
- Read job: memory hits in the window are ready without fetching earlier
  pages first; an out-of-order completion is buffered and delivered in order;
  selection fan-out to multiple reads; a failed selection falls back to direct
  fills; spill holds are released once each after delivery or cancel; a
  refused hold falls back to the memory window.
- Peer: multiple pending selections per subscription up to the granted
  limit; old-peer refusal falls back to one in flight; budgets and replay
  checks unchanged.
- Go SDK: `Sequence` rejects out-of-order frames; no `Racer-Ordered` header is
  sent.
