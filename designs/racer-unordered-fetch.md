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

Warming also completes pages strictly in order (a contiguous `frontier`), and
any read of a version that is being warmed waits behind that frontier.

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
  spill to disk through the same hold mechanism that warming uses. No new
  caching mechanism.
- Warming completes pages out of order, so a warm no longer serializes reads of
  the version it is warming.

## Non-goals

- Changing page size, page verification, or zero-copy delivery.
- Changing eviction policy.
- Changing how the client grants and releases credits.

## Design

### 1. Warm progress is a completed set

`read/warm.rs` tracks progress as a contiguous prefix and completes pages in
order with `FuturesOrdered`. It changes to:

- A completed-page set per job (bitset over the page range), filled as each
  page completes, in any order (`FuturesUnordered`, still bounded by the warm
  window).
- `wait(page)` is ready as soon as that page is in the set. Placement
  (`Local` or `Remote`) still comes from the job's `Plan`.
- A revision change clears the set, as it reset the frontier before.
- The rule that a remote-held warm page narrows a read to a single page goes
  away; readers are no longer gated by a prefix.

### 2. Holds release individual pages

Holds (`store/holds.rs`, `store/catalog.rs`) are per version per worker and
already hold an arbitrary page set (`HoldPages`, `reserve_hold_pages`).
Releasing today frees only unused capacity; written pages stay held until
expiry.

Holds gain per-page holder counts. `reserve_hold_pages` increments the count
for each member page and `release_hold_pages(version, pages)` decrements it.
A page whose count reaches zero leaves the hold and becomes evictable like any
other cached page. Warm holds keep their current behavior; a read's spill pages
can be released as soon as they are delivered without disturbing a warm (or
another read) that holds the same page.

### 3. Per-read fetch job

`read/range_stream.rs` replaces the `Scheduler`/`Demand` machinery and the
ordered/unordered split with one job per read stream.

State: a delivery cursor `next`, and a fetch window of pages
`[next, next + W)` where `W = range_window_pages`. Each window page is one of
`Missing`, `Fetching` (direct fill), `Selecting` (covered by an outstanding
peer selection demand), `Ready` (pinned plaintext page in memory) or `Spilled`
(published under this read's hold and re-read through the memory-then-disk
warm path on delivery).

Each poll:

1. Probe memory on the owning worker for every window page that has not been
   probed yet (`Work::Cached`). Hits become `Ready` immediately.
2. Ensure the page at `next` has a direct fill in flight (the normal Fill path:
   local disk, ranked peer, authorized origin). Start direct fills for up to
   `P` further `Missing` pages.
3. Send remaining `Missing` pages to peers as a selection demand. Up to `K`
   selections may be in flight per read, possibly to different providers.
   Selected pages are accepted on their owning worker as today, and every open
   read of the same version whose window wants the page takes it (fan-out).
4. Deliver `next` when it is `Ready` or `Spilled`, attaching it to the client
   as today (`Delivery::attach`). Client credits and releases are unchanged.

Out-of-order arrivals beyond the in-memory budget spill:

- The read reserves a hold for those pages on their owning workers
  (`reserve_warm`-style `Work::Hold` with a page set) and fills them through the
  warm path (`Work::Warm`, which persists held).
- On delivery a spilled page is read through `warmed_page`/`warm_local`
  (memory first, then the held disk copy).
- After delivery the read releases the page from its hold.
- If the hold is refused (hold limits, or no disk), the read keeps fetching
  ahead only within its in-memory window.

Warm interaction: a read waits on the warm job only for the page it is about
to fetch, not for a prefix. A warm page held locally is read from the warm
hold; a remote-held warm page uses the normal fill path.

Warm admission no longer needs the node-wide `selecting` check
(`Scheduler::admit_warm`), because reads no longer own an exclusive selection
per version.

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
  wire `VERSION` does not change, matching how remote warm holds were added.

### 5. `Racer-Ordered` is removed

- Rust: `ReadKind::Subscription.ordered` and the `racer-ordered` header are
  removed from `object-wire`. The server ignores unknown headers, so an older
  SDK that still sends `Racer-Ordered` keeps working.
- Go: `SubscriptionOptions.Ordered`, the header, its allowlist entries and the
  `ordered` flag on `Sequence` are removed. `Sequence` always enforces order.

Rollout: an older dataplane treats a missing header as unordered, and the new
SDK rejects out-of-order pages. Roll the dataplane before the SDK.

## Testing

- Warm: out-of-order completion wakes exactly the completed page's waiters;
  revision change clears the set; existing warm tests updated from prefix to
  set semantics.
- Holds: refcounted reserve/release, release to zero makes the page
  evictable, warm hold unaffected by a read release, expiry still applies.
- Read job: memory hits in the window are delivered without fetching earlier
  pages first; an out-of-order completion is buffered and delivered in order;
  spill when the window exceeds memory; fallback when the hold is refused;
  selection fan-out to multiple reads; cancellation releases holds and credits.
- Peer: multiple pending selections per subscription up to the granted
  limit; old-peer refusal falls back to one in flight; budgets and replay
  checks unchanged.
- Go SDK: `Sequence` rejects out-of-order frames; no `Racer-Ordered` header is
  sent.
