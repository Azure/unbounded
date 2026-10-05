# Racer dataplane topology

This unpublished workspace crate owns the runtime-independent membership,
placement, and path algorithms used by `racer-dataplane`. Its small neutral
`Member` trait keeps application records outside the algorithms; this is not a
general distributed-systems framework. Racer still owns membership publication
and leases, authenticated forwarding, deadlines, cancellation, health, transport,
and application key encoding in the [topology adapter](../src/topology.rs) and
[control publication owner](../src/control.rs).

The crate API documentation contains an executable example using a neutral
member model, placement, explicit path limits, and a zero-link self route.
`unsafe_code` is forbidden and missing public documentation is denied by the
existing crate attributes in `src/lib.rs`; Cargo does not duplicate those rules.

## Membership and stable overlay

`Membership::new` snapshots each ID and positive weight once, rejects duplicate
IDs, IDs longer than `u32::MAX` bytes, and NUL-containing `Member::DOMAIN` values,
and sorts by frozen ID bytes.
Returned positions belong to that snapshot, not a durable cluster-wide index.
Interior changes to application records do not change frozen algorithm inputs.
An empty membership is valid, but has no valid route endpoints.

The overlay replaces position-based radix routing with the symmetric union of
**32 independently domain-separated SHA-256 rings over stable IDs**. Each ring
sorts by digest with ID-order tie breaking, connecting consecutive members and
the wraparound pair. Neighbors are unique, sorted, self-free, and capped at 64.
Every nonempty membership is connected; weights do not change its edges. A
single join or leave changes only predecessor/successor edges in each ring,
even when membership positions shift. Stable ID edges do not mean stable numeric
positions, nor do connectivity and degree imply a universal four-hop guarantee.

For N members with bounded-size IDs, construction costs O(32 N log N) time.
More explicitly it hashes total ID bytes 32 times, sorts 32 N-entry arrays, and
sorts/deduplicates each at-most-64-entry adjacency row. Retained adjacency is
approximately 64N `usize` slots plus N vector headers, not 64N bytes. At 100,000
members on a 64-bit target, the adjacency slots alone are about 48.8 MiB. One
N-entry `(digest, position)` array is reused as construction scratch; ring orders
are not retained. Frozen IDs, member records, weights, deltas, and allocator
overhead are additional. Membership construction is synchronous, not a
cooperative request-path operation (see `build` in [overlay.rs](src/overlay.rs)).

`Membership::new_with_predecessor` freezes and validates new records, reuses the
predecessor's immutable `Arc` graph when the sorted frozen IDs are identical,
and prepares incremental placement hints. Weight-only and metadata-only changes
therefore avoid ring construction, but still snapshot, sort, hash, and measure
the new membership. Changed IDs require rebuilding the rings. Graph sharing
does not retain the predecessor membership itself. Membership is `Send` and
`Sync` when its application record type is; algorithm caches remain worker-local.

`with_predecessor` alone only attaches hints to an already-built membership; it
cannot undo construction work or replace that membership's graph. Both paths
retain at most 64 changed IDs for incremental placement. Larger changes discard
hints and use exact cold placement instead. Placement identity includes domain,
IDs, and weights; path-cache topology identity includes domain, IDs, and the
overlay algorithm but excludes weights. See `Membership::new_with_predecessor`
in [membership.rs](src/membership.rs) and
`predecessor_constructor_shares_graph_only_for_identical_frozen_ids` in
[membership tests](src/membership/tests.rs).

### Racer publication preparation

The application control poll uses `SnapshotStore::prepare_async` in
[control.rs](../src/control.rs) to run CPU-heavy validation and membership
construction on an owned background thread, off the I/O polling thread and
outside the publication lock. A validated publication with unchanged membership
version and content shares the current whole membership. A new version uses
`Membership::validate_with_predecessor` in the [adapter](../src/topology.rs),
which selects the graph-sharing constructor when a predecessor exists.

Each `SnapshotStore` admits at most one unfinished preparation job, with no
queued backlog; another preparation returns `Overloaded` until the job finishes.
Cancellation or a request deadline stops the waiter, not CPU work already
started. The store retains the job handle and admission slot, so a canceled
waiter cannot cause repeated background jobs to accumulate. Preparation alone
never publishes; installation separately rechecks accepted state.

Dropping the store joins its owned job rather than detaching allocation work.
This bounds job count and preserves cleanup ownership, but it is not a hard
execution-time bound: shutdown can wait for the remaining preparation work, and
the join has no independent timeout or cooperative cancellation. Wire input
bounds and the application's member limit bound input size, not elapsed time.
The synchronous `prepare` entry point remains for startup, tests, and the
background job, not I/O-worker polling. See the `publication_tests` module in
[control.rs](../src/control.rs) for admission and canceled-wait assertions.

## Placement and routing compatibility

Placement still uses 2^20 slots and integer weighted rendezvous ranking of up
to three members. The `/slot/v1`, `/hrw/v1`, and `/placement-identity/v1` hash
contracts and integer scoring are unchanged. For the same application domain,
encoded key, IDs, and weights, this overlay change does not move placement.
Neutral-model golden assertions are in `golden_slot_and_weighted_ranking_vectors`
in [placement tests](src/placement/tests.rs). Racer-specific compatibility
vectors and `encoded_key` remain in the [adapter](../src/topology.rs).

Routing is a separate compatibility contract. Both the new ring edges and the
`/next-hop/v5` selection schema change routing behavior. V5 independently
prefixes the seed, source ID, and destination ID with big-endian u32 lengths
before hashing; it must not be treated as the former unframed seed schema
(see `weighted_index` in [paths.rs](src/paths.rs)). Ring ordering uses its own v1
domain and u64 ID lengths.
Changing these domains or encodings requires deliberate compatibility review.

**Use a coordinated cluster version transition.** Quiesce traffic and ensure all
participating dataplanes use the same overlay and next-hop schema before
resuming. Neither matching membership version numbers nor unchanged placement
hashes establish mixed-routing safety. Do not assume automatic algorithm
negotiation, safe rolling coexistence, or rollback while mixed versions serve.

`Paths::route` searches equal-cost shortest routes within the supplied link
budget, retaining one canonical witness per eligible first hop rather than all
full paths. Selection uses each eligible next hop's frozen weight and the
caller's seed, with bounded integer rejection sampling. A cache hit reselects;
it does not pin the first request's seed or weights. `visited` excludes members
throughout the path; `blocked` removes only edges from the source. Inputs are
validated even for self routes, which return `[from]` with no search admission.

The crate accepts at most 255 total visited positions plus remaining links,
64 blocked inputs, and a 65,536-byte seed. Visited positions must be distinct
and exclude endpoints. Duplicate blocked positions are canonicalized. A zero
link budget reaches only self. Racer imposes its own tighter forwarding budget.

## Cache budgets and cooperative work

Caches are worker-local `Rc`/`RefCell` state, not a shared cross-thread service.
Callers own executor scheduling and total request admission.

| Owner | Retention and admission |
|-------|-------------------------|
| `Placement::new(capacity)` | At most `capacity` resident `(placement identity, slot)` entries; zero computes uncached. Resident requests coalesce and pin their entry. CLOCK eviction gives referenced entries a second chance and never evicts pinned work. |
| `Paths::with_limits(entries, bytes, active_searches)` | Independent retained-entry, accounted-byte, and distinct cold-search limits. Either zero cache limit disables retention without disabling searches. Zero search admission still permits validated self routes and cache hits. |
| `Paths::new(capacity)` | Entry limit supplied by caller, 8 MiB accounted cache bytes, eight distinct active searches. |

Placement cold async scoring handles at most 256 members per poll, with a
bounded delta on admission; CLOCK admission can inspect the bounded resident
cache. Async admission can report `Overloaded` when all eviction candidates are
pinned. Synchronous ranking instead falls back to uncached scoring and is not
cooperative. Maintenance migrates already-demanded predecessor slots rather
than eagerly populating the entire slot space.

Path search yields after at most 32 vertex expansions, each with at most 64
edges. Reconstruction retains at most 64 alternatives of at most 255 edges.
Identical canonical queries share one search admission even across seeds and
weight-only snapshots. Dropping one waiter does not strand another; dropping the
last releases unfinished search state. This search limit is not a waiter limit
or a byte limit on search scratch.

Path retention uses variable-byte LRU with capacity-based accounting for keys,
route vectors, the entry buffer, and Rc headers. An oversized result is returned
without flushing useful smaller cached routes. `cached_bytes()` stays within
the configured accounting budget, but excludes active search scratch, returned
results, membership graphs, allocator metadata, and the inline cache object.

Placement's `ENTRY_BYTES` (currently 1,024) is a conservative structural
estimate per resident entry, not an allocator hard bound. `retained_bytes()`
adds the inline cache object. `Membership::retained_bytes()` is O(1): immutable
storage is measured once during construction, then the getter adds the current
bounded delta buffer's capacity using saturating arithmetic. It uses retained
capacities, excludes nested heap allocations inside application records, and
includes the full graph allocation even when shared. It is not an incremental
ownership measurement: summing estimates across graph-sharing snapshots counts
that graph multiple times. Unique-allocation accounting must deduplicate it;
Racer's publication grace-budget sum instead conservatively counts each
snapshot's full estimate. See `retained_bytes` in
[membership.rs](src/membership.rs) and `SnapshotStore::publish_prepared` in
[control.rs](../src/control.rs). None of these estimates is an RSS cap; allocator
size classes, fragmentation, concurrent searches, and leased old snapshots
require separate headroom.

Racer's placement byte budget is converted to an entry count; its placement and
path cache budgets are node-wide and partitioned among final I/O workers.
`RACER_ACTIVE_PATH_SEARCHES` is instead **per I/O worker**, so total distinct
search concurrency can grow with worker count. See the public Racer reference
for defaults and validation ranges.

## API and cache boundaries

Keep public constants that describe actual caller contracts: replica/slot
geometry, maximum degree, incremental-delta limit, and the placement entry
estimate. Internal work quanta and hash domains need not become tuning APIs.
The compact non-exhaustive error enum distinguishes invalid input, overload,
unreachability, and exhausted unbiased sampling; splitting it into new error
crates would not improve these ownership boundaries.

Likewise, do not force placement CLOCK and path LRU into one shared cache merely
because both are bounded. Placement correctness depends on pinned cooperative
rankings and predecessor migration; path correctness depends on independent
search admission, immutable alternatives, variable-byte retention, and fresh
weighted selection. A common cache must preserve all those differences to be
useful. Separate implementations are intentional, not a request for broader
dependencies or more crates.
