# Racer core simulation

Status: in progress.

## Summary

`racer-sim` is a single-process, single-thread discrete-event simulator. It runs the real Racer
decision logic (read path, peer routing, placement, caches, control plane) for up to 50,000 nodes.
To make that possible, the decision logic moves into a sans-IO crate, `racer-core`, that the
production dataplane and the simulator both call.

## Goals

- Run the same decision code as production, not a model of it.
- 50,000 nodes, 2 workers per node, one thread, at most 32 GiB RSS.
- Deterministic: the same scenario, node count, and seed give the same trace hash.
- Report latency, origin load, peer traffic, cache behavior, control-plane load, and memory.
- A small smoke scenario runs in `cargo test`. Large runs use `make racer-sim ARGS=...`.

## Non-goals

- Simulating payload bytes, crypto output, or real disk contents.
- Replacing the existing deterministic tests (`src/app/tests/dst.rs`). The former test-only pressure
  model `src/contention.rs` was retired in round 2 once `racer-sim` covered its scenarios (see
  "Round 2: contention model retired").
- Changing production behavior. New generic code must have monomorphized defaults that compile to
  the current code.

## Architecture

### racer-core

Path: `cmd/racer-dataplane/core`, crate `racer-core` (lib `racer_core`).

- Depends only on `topology`, `racer-object-wire`, `racer-control-wire`, and `flow-control`.
- No I/O, no clocks, no threads, no OS randomness. Time is passed in as `now: Instant` plus
  `wall_ms: u64` (`racer_core::time::Now`). Randomness is an explicit RNG
  (`racer_core::time::SplitMix64`).
- Enforced two ways:
  - `core/clippy.toml` lists `disallowed-methods`.
  - `core/tests/purity.rs` scans `core/src/**/*.rs` and fails on `Instant::now`,
    `SystemTime::now`, `uring_runtime`, `environment::now`, `getrandom`, and `thread::spawn`.
- Code moves into `racer-core` and is re-exported from its old path, so callers do not change.

Pieces planned to move (phase 3): `AcquisitionBudget`, `visit_order` and `replica_score`,
`classify`, `OriginAuthority` validation, `origin_miss_error`, `recoverable`, `peer_error`,
`inherited_budget`, `optional_headroom`, retention and persist policy, and `WorkerMap::select`
(`src/read/dispatch.rs:1282`). Later phases add state machines: `CandidateWalk`, `PageFlights`,
`FillPlan`, `ServePolicy`, `MetadataCohort`, `decide_hop`, sans-IO subscriptions, and control-plane
step functions.

### Phantom caches

A phantom cache makes the same decisions as the real cache, but stores no payload. Each one sits
behind a trait with the real type as the default parameter.

1. Placement (`topology/src/placement.rs`): `Placement<R: Rankings = Scored>`. A phantom entry is a
   cursor and a referenced bit. Rankings come from a shared memo keyed by (membership identity,
   slot) that runs the real `Ranking::updated`. CLOCK hand, pinning, and `Overloaded` behavior must
   match exactly.
2. Disk segments: the simulator implements `SegmentEntries` (`alloc/src/segments.rs:692`) with a
   phantom index. Retention scoring (`src/store/catalog.rs:1358-1370`) moves to `racer-core` and
   takes `now`.
3. Memory LRU (`src/memory.rs:1106-1200`, `Entries` and `CiphertextEntries`) becomes generic over
   the value type.
4. Route cache (`topology/src/paths.rs`): per-node LRU of keys only, plus a shared route memo.

Each phantom has equivalence tests (real and phantom give identical decisions on seeded workloads
with churn) and size tests.

### racer-sim

Path: `cmd/racer-dataplane/sim`, crate `racer-sim`, binary `racer-sim`.

- Time is `u64` nanoseconds. Events sit in a `BinaryHeap` ordered by `(time, seq)`.
- Each role gets its own `SplitMix64` stream derived from the seed.
- Each node has: core state machines, phantom caches, real `flow_control` quotas (with interned
  `CacheId`), link health, 2 workers picked by `WorkerMap::select`, a CPU FIFO per worker, and a
  crypto helper.
- Network: per-node send and receive FIFOs in 1 MiB chunks, cut-through relay per chunk, and an
  RTT matrix per site pair.
- Disk: read and write FIFOs.
- Origin: per-cache concurrency cap, bandwidth FIFO, latency, and injected 412, 403, and 5xx.
- CPU costs are configurable: AEAD per byte, sign and verify per hop, ranking per chunk, path
  expansion per node, control handshakes.
- Workloads: Poisson, Zipf, cold-start storms, range reads, churn.
- Faults: partitions, crash and restart (disk survives), slow nodes, origin errors.

The JSON report has log-bucket latency histograms, origin requests and bytes per page, peer bytes,
hop counts, flight joins, rejections per `ResourceClass`, cache hits and evictions, control-plane
lag, 429s and API calls, peak memory per component, and the trace hash.

## Memory budget

32 GiB / 50,000 nodes = 687,194 bytes, about 671 KiB per node. The target is 640 KiB per node
(about 31.3 GiB total), leaving about 1.5 GiB for shared state: ranking and route memos, the event
heap, the RTT matrix, and the report.

Per-node state must therefore avoid payloads and per-node copies of anything that can be shared.
Rough budget per node:

| Component | Budget |
| --- | --- |
| Placement phantom entries | 128 KiB |
| Segment phantom index | 128 KiB |
| Memory LRU keys | 128 KiB |
| Route key cache | 64 KiB |
| Flow quotas, link health, 2 workers | 64 KiB |
| Flights, walks, control state, queues | 128 KiB |

The simulator reports peak RSS and the size of each component so this table can be checked.

## Determinism and replay

- All time comes from the event clock. All randomness comes from seeded `SplitMix64` streams.
- Hash maps in shared code use seeded hashers in simulation (phase 1: `SecondSight` in
  `alloc/src/retention.rs` and the flight table in `src/read/flight.rs:126`).
- Each processed event feeds a running trace hash. Two runs with the same inputs must give the same
  hash. A test checks this for the smoke scenario.

## Invariants

Checked during every run:

- Budgets never grow when passed along.
- Only ranked owners fetch from origin.
- Routes use at most 8 links.
- Membership versions only go up.
- At most one primary per term.

If the simulator finds a real bug, the invariant is weakened with a comment that gives the root
cause (`file:line`, scenario, seed), and an entry is added to Known issues.

## Phases

1. This document. Phase 0 read path: explicit `now` and wall time (`RequestScope::check` variant,
   budgets, retention); adaptive clock object instead of `fn()`; seeded hashers.
2. Phase 0 control plane: `now_micro` takes wall time; `new_key` takes an RNG; jitter and claim
   delay take an RNG.
3. Move pure pieces into `racer-core`. One typed `RouteBudget` rule for app and wire; tighten the
   wire to 8 links with boundary tests.
4. `Placement<R: Rankings>`, phantom, and equivalence tests.
5. Segment CLOCK and retention scoring, generic memory LRU, route key cache.
6. `racer-sim` skeleton: event loop, FIFO network, disk, origin, CPU, cache-only scenarios, CI smoke,
   make target.
7. `CandidateWalk` state machine from `resolve_epoch_traced` and `request_traced`, including the
   post-412 walk and hedging. The production async walk drives it; probe sequences must match on the
   existing scripted requester tests.
8. Payload-generic `PeerResponse`, payload-free `classify`, `PageFlights`, `FillPlan`, `ServePolicy`.
9. `MetadataCohort`, range stream scheduling, read-path scenarios.
10. `decide_hop`, typed response-validity rules, error tables.
11. Sans-IO subscriptions, link health and adaptive peers with explicit `now`, peer scenarios.
12. Control-plane step functions: lottery, seat renew and release, follower, contend, primary plan
    pass, seat server admission.
13. Plain-value fake API server, simulated host for `controlplane::Sync`, `Signal::notify` cost
    model, fix the `limits.rs` sizing comment.
14. Full-scale scenarios and the 50k / 32 GiB acceptance run. Results go in this document.

## Doc and code disagreements

Verified against the code at the start of this work.

1. `cluster-control/src/authority/election.rs:1` says "Pure: callers pass observations and clocks",
   but `now_micro` at `election.rs:124-130` calls `std::time::SystemTime::now()`. Phase 2 fixes the
   code.
2. Hop limit, 8 vs 9. The app rejects a budget when `visited + remaining_links > FAILURE_LINKS`
   (`src/topology.rs:543-544`, `FAILURE_LINKS = 8` at `src/topology.rs:507`). The wire allows
   `visited + links <= MAX_HOPS + 1`, which is 9 (`peer-wire/src/forwarding.rs:53`,
   `MAX_HOPS = 8` at `peer-wire/src/lib.rs:505`). Phase 3 tightens the wire to 8.
3. `cluster-control/src/limits.rs:44-51` sizes `MAX_CONTROL_CONNECTIONS` from snapshot and keyring
   long polls only. It does not count seat write slots or the cost of waking waiters:
   `Signal::changed` scans all registered wakers on each registration
   (`cluster-control/src/exec.rs:355-361`), so W waiters cost O(W^2), and `Signal::notify`
   (`exec.rs:340-345`) wakes all of them. Phase 13 models this and fixes the comment.
4. `controlplane/src/feed.rs:860` spawns a thread for preparation, which a single-thread simulator
   cannot use. Phase 13 adds an inline path.
5. `cluster-control/src/authority/keyring.rs:351-357`: `new_key` reads OS randomness directly.
   Not a doc conflict, but it blocks determinism. Phase 2 fixes it.

## Known issues

1. Long poll delivery is bounded by write slots, not connections. When a publication lands, every
   waiting snapshot poll on a seat wakes, but each must take one of `MAX_WRITES` (64) slots after the
   wait (`cluster-control/src/server.rs`, `Server::snapshot`, `write_slot()?` after `wait_until`);
   the rest answer 429 and retry. At 50k Pods over 16 seats (about 3125 polls per seat) the
   control model (`step::cluster`, `fifty_thousand_pods_cold_start`) counts about 4.5M 429 replies
   during a 300 s cold start and a worst client lag of about 4.1 s. Each wake also costs O(W^2) waker
   comparisons (`step::cost::notify`). The `limits.rs` comment now says so; the behavior is
   unchanged.

2. Resolved: `RouteBudget::forwarded` and `RouteBudget::validate_forwarded`
   (formerly `cmd/racer-dataplane/src/topology.rs:536`, `:552`) were called only by tests and used the
   search form of `visited`. Both were removed in round 2. Their test
   (`topology::tests::scenarios::forwarded_budget_is_monotonic_and_rejects_cycles`) now drives
   `racer_core::route::forward` and `validate` on a `RouteBudget`'s fields, so search-form forwarding
   has one rule; the signed wire form stays in `racer_core::route::check_wire_transition`.

3. A requester that adopted a newer membership than its destination fails the read. The
   destination cannot resolve the newer version and answers `StaleMembership`; the requester then
   refreshes, but its latest version is not newer than the one it used, so `refresh_candidates`
   returns `IncompatibleMembership` (`cmd/racer-dataplane/src/read/candidates.rs:1203-1204`,
   `racer_core::read::walk::refresh_allowed` at `core/src/read/walk.rs:430-437`). That error is not
   `recoverable` (`core/src/read/policy.rs:9-22`), so the walk does not try the next owner. In the
   `churn` scenario (seed 1, 1 s, `request_rate` 2, 256 objects) this fails 812 of about 13k page
   reads at 5,000 nodes and 6,939 of about 60k at 50,000 nodes, while nodes adopt each publication
   within 100 ms. No invariant is relaxed; this is reported as a behavior to review.

4. A cold page can reach the origin once per owner, up to 3 times, with no concurrency at all. The
   rank 0 owner never asks later owners (`CandidateWalk::new` sets the walk length to the rank,
   `core/src/read/walk.rs:146`), and readers probe owners in spread order (`visit_order`), so a
   later owner may fetch first (after rank 0 answers a copy-only miss) and rank 0 fetches again
   when a reader asks it. In `node::tests::single_reader_then_storm_reuses_copies` (64 nodes,
   seed 7) one reader and then 64 readers cause 33 origin requests for 16 pages. The simulator
   bounds origin requests per page by `REPLICAS` in tests, not 1.

5. Under heavy overload a page read fails with `HopBudgetExhausted`, which is not `recoverable`.
   Each probe charges its route links up front (`plan_attempt`, `core/src/read/walk.rs:478`), and
   after a failed route each probe charges 8. With the page budget of 16 links (`PAGE_LINKS`,
   `core/src/read/range.rs:24`) two `Overloaded` relays use it up; the next `plan_attempt` returns
   `HopBudgetExhausted` (`walk.rs:481`), `transport_outcome` does not map it (`walk.rs:420`), so the
   walk is abandoned with that error instead of ending as `Exhausted` (`Unavailable`). Production
   does the same (`src/read/candidates.rs:1037-1045`). In the `storm` scenario at 50,000 nodes
   (seed 1, 1 s, `request_rate` 2) 99,562 page reads fail this way while relays reject 1.25M
   transfers on the `Relay` quota (16 per worker).

6. Origin requests per page are bounded by `REPLICAS` only within one membership version. When a
   membership change moves a page's ranked owners, the new owners have no copy, and a reader that
   walks the new owners ends in `End::Origin` at the new rank 0 (Known issue 4 applies per
   version). Nodes still on the old version keep fetching from the old owners. In `full-churn` at
   50,000 nodes (seed 1, 11 membership versions, `request_rate` 0.005) one page reached the origin
   11 times and 20,035 of 45,938 pages more than once. No invariant is relaxed: `only_owner_hits_origin`
   holds on each request's own version; this is the cost of ownership moves, reported for review.

7. Crashed members stay in the membership (their Node and Pod objects remain, so `plan_pass` keeps
   them), and peers keep routing through them until link backoff opens their circuits. Each failed
   route makes the next probe charge 8 links (Known issue 5), so reads near a crashed member fail
   with `HopBudgetExhausted`. In `full-failover` at 50,000 nodes (4 crashes, seed 1) 11,950 of about
   283,000 page reads fail this way. Same root cause as issue 5; no invariant is relaxed.

8. Core `PageFlights` has no origin-rejection error. Production `Flights::fail(OriginRejected)`
   (`cmd/racer-dataplane/src/read/flight.rs:1441`) cancels the supplying caller with
   `OriginRejected` before `Outcome::Retry`, so that caller fails even when another caller later
   publishes. `PageFlights::finish(Outcome::Retry)` (`core/src/read/flight.rs:295`) only makes the
   leader ineligible; it stays parked and receives the later result. Both re-elect the next
   eligible waiter. The simulator never finishes a flight with `Retry`, so runs are unaffected.
   Pinned by `read::core_equivalence::origin_rejection_is_retry_in_core`.

9. A duplicate memory publish touches only in the simulator. Production `MemoryCache::publish`
   returns early for a resident page without touching it (`cmd/racer-dataplane/src/memory.rs:1415-1418`),
   while the simulator's `memory_insert` touches it (`sim/src/node/world.rs:1214-1216`), so such a
   page lives slightly longer in the simulator. It only happens when a fill publishes or a disk hit
   promotes a page that is already in memory. Pinned by
   `read::core_equivalence::duplicate_publish_touches_only_in_the_simulator`.

10. Route search admission is per node and has no queue. A node runs at most 8 distinct searches at
   once (`Paths::route`, `cmd/racer-dataplane/topology/src/paths.rs:530`), and any further search
   fails at once with `Overloaded`. The send then fails: a requester sees a transport error, and a
   relay answers `Overloaded` through `relay_failure`. Either way the walk pays the failure-route
   charge from Known issue 5. In the simulator, searches run as yielding tasks ("Round 2:
   fidelity"), so they now overlap in time. In `full-cold-start` at 50,000 nodes (seed 1,
   `request_rate` 0.005), 720,628 searches were rejected this way during the storm, along with 502k
   `HopBudgetExhausted` failures. No search was rejected in the other combined runs. No invariant
   is relaxed. This is reported as a behavior to review.

## Simulator engine (racer-sim substrate)

`cmd/racer-dataplane/sim` provides the substrate the extracted logic plugs into:

- `engine`: `Engine<E>` event queue ordered by `(time_ns, seq)`, `Streams` (one `SplitMix64` per role and
  index, so adding a role never shifts another role's sequence), and an order-sensitive `TraceHash`.
- `resources`: FIFO servers reserved at issue time (no per-chunk events). `Fabric::transfer` relays 1 MiB
  chunks cut-through over multi-hop routes; per-node disk read/write, CPU per worker, crypto helper, slow-node
  multiplier; `RttMatrix` per site pair; `Origin` with per-cache concurrency slots, a shared bandwidth FIFO,
  fixed latency and injected 412/403/5xx.
- `workload`, `faults`, `config`: Poisson, Zipf, range reads, cold-start storms, churn; partitions,
  crash/restart (disk survives), slow nodes, origin error windows; every knob overridable with `--config`.
- `report`: fixed-memory log-bucket histograms, counters, `Invariants` (named checks, capped violation log,
  relaxed invariants with a reason), `MemoryUsage::approximate_bytes`, peak RSS from `VmHWM`.
- `logic::NodeLogic`: owner, route, tier lookup/insert and crash hooks. `StandIn` (hash ownership,
  direct or one-relay routes, direct-mapped tiers) is a placeholder; the module docs list where phantom
  `Placement`, `Paths`, memory LRU and `SegmentClock` plug in. `scenario::World` holds the fixed caches-only
  read flow (owner fetch with per-page singleflight) that the `racer-core` walk and fill plan replace.

With the stand-ins, a 2 s caches-only run at 50,000 nodes processes about 6M events in about 8 s (release)
with 180 MiB peak RSS; the empty substrate accounts about 2.6 KiB per node. The stand-in owner check re-ranks
at the owner and retries when membership changed in flight, so `only_owner_hits_origin` holds by
construction until real placement replaces it.

## Cache track (phases 4 and 5)

The phantom caches reuse the production CLOCK, LRU, and admission code. Only
the storage of each entry changes, through a type parameter whose default
keeps production unchanged.

- Placement: `topology::Placement<R: Rankings = Scored>`. `Phantom` keeps a
  cursor and the CLOCK bit per entry (`PhantomEntry`, 8 bytes) and reads
  results from a memo shared by every clone, keyed by (membership identity,
  slot). The memo runs the real scoring and the real incremental update once
  per key. `Phantom::forget` drops old memberships; a forgotten predecessor
  makes `updated` return `None`, which forces a full rescore, not a wrong
  answer. Equivalence test:
  `placement::tests::phantom_matches_scored_on_seeded_workloads_with_churn`.
- Routes: `topology::Paths<S: Searches = Exhaustive>`. `SharedRoutes` runs the
  real search once per `PathKey` (which includes the graph identity),
  records its step count and result, and replays both, so hits, misses,
  evictions, byte charges, admission, and polls per search are identical.
  Cached results are one shared `Rc` across nodes. Equivalence test:
  `paths::tests::shared_routes_match_exhaustive_on_seeded_workloads`.
- Disk: `page_alloc::PhantomEntries<K>` implements `SegmentEntries` with a
  page index and page lengths but no data, removing mappings in publication
  order like the catalog. Segment value moved to
  `racer_core::cache::segment` (`SCAN_BUDGET`, `unseen_value`, `page_value`,
  `segment_value`); the catalog calls it, and `catalog::SegmentClock::reclaim_at`
  takes an explicit `now`. Equivalence test:
  `store::catalog::tests::phantom_segment_entries_match_catalog_reclamation`.
- Memory: the verified and unverified recency indexes moved to
  `racer_core::cache::lru` as `TickLru<K, V, H>` and `OrderedLru<K, V, H>`,
  with the admission victim, bounded reclamation pass (`Visit`), and idle
  expiry batching. `src/memory.rs` stores `CachedPage` and `UnverifiedPage`
  in them; the simulator stores payload-free values and passes a seeded
  hasher.

Measured per-entry heap cost (counting allocator tests in
`topology/tests/phantom_memory.rs`, `alloc/tests/phantom_memory.rs`,
`core/tests/lru_memory.rs`):

| Structure | Bytes per entry | At production defaults, per worker |
| --- | --- | --- |
| Phantom placement entry | 99 | 1.6 MiB (16 MiB budget, 16,384 entries) |
| Replayed route entry | 199 | about 1 MiB (8 MiB charge budget, about 5,100 entries) |
| Phantom segment mapping (`u64` key) | 121 | depends on disk pages |
| `TickLru<u64, u32>` / `OrderedLru<u64, u32>` | 67 / 82 | depends on memory pages |

Production defaults therefore cost about 2.6 MiB per worker for placement and
routes alone, well over the 640 KiB per node target at 50,000 nodes. Full
scale runs must scale the placement and path cache budgets down (the
decision logic is identical at any capacity) or accept fewer nodes. The
shared memos grow with distinct (membership, slot) and route keys and must be
pruned with `forget` on membership changes.

## Track ctl: control plane steps

Phase 0 (`cluster-control`): `election::now_micro(wall)` takes the wall time, so the "pure" header
holds. `entropy::{Entropy, OsEntropy, Seeded, Failing}`; `keyring::plan_with(.., rng)` and
`new_key(rng, ..)` take it, and `Shared::rng` feeds seat jitter, claim delay salts, and primary
contention waits. Production passes `OsEntropy`.

Phase 12 (`cluster-control/src/step/`, public as `racer_cluster_control::step`): `lottery::Lottery`
(plan, begin, observe, won), `renew::SeatRenew` (decide, put_ok, put_failed, conflict),
`contend::Contend`, `follower` (read classification, admit, need_image, `Follower`),
`primary::{plan_pass, discovery, Inventory}`, and `admission` (`Gate`, `check_ahead`, `body_for`,
`SeatAdmission`). `seat.rs`, `primary.rs`, `server.rs`, `exec::Slots`, and `Image::for_base` call
them, so there is one copy of each rule. Steps return decisions rather than `Vec<Effect>`; the I/O
between decisions stays in the async tasks. Deviation: the lottery draws the candidate index
before its sleep rather than after; with OS entropy this is indistinguishable.

Phase 13: `apiserver::Store` is the plain-value API server (CAS, watches with fan-out and a bounded
log, compaction, faults, `Admission`/`TokenBucket` for 429s and latency); `kube::fake` wraps it with
TLS. `controlplane::Host::preparing()` returns `Preparing::Thread` by default or `Inline` for a
simulated host; time in `controlplane` still comes from `uring_runtime::environment::now()`, which
the runtime's `simulation` feature already controls. `step::cost` models notify and write rounds.
`step::cluster::ControlPlane` simulates N Pods (enrollment lottery, seats, primary failover,
follower image copies, client long polls) from the real steps with typed Leases; 50k Pods for 300
simulated seconds takes about 66 s in release, dominated by `plan_pass` at 50k members.

## Track: peer

- Route budget: `racer_core::route` is the single rule for both forms. The hop limit is 8 links and is enforced.
  On the wire `visited` includes the sender, so the check `visited + links <= 9` is exactly 8 links; it is not
  tightened further.
- Per-hop decisions: `racer_core::peer::hop::decide_hop` returns `StaleMembership`, `Serve`, or
  `Relay(RelayPlan)`. The failure tables (`relay_failure`, `serve_failure`, `waiter_failure`,
  `operation_link_outcome`, `exchange_link_outcome`, `exchange_admission_outcome`) take the core `Fault`; the
  dataplane maps its `Error` with `Error::fault`. `Server::dispatch`, the opaque HTTP relay path,
  `dispatch_verified`, `Requester::exchange`, and `LinkHealth::run` call them.
- Response validity: `racer_core::peer::validity` holds the typed rules (authority, not-found, grant membership,
  receiver, deadline, page and byte budgets, flights, hold, bootstrap). `response_matches` and
  `response_authority` parse headers and call them in the same short-circuit order.
- Subscriptions: `racer_core::peer::subscriptions::Scheduler` is the token-based scheduler. Every transition is
  a method (`admit`, `release`, `select`, `complete`, `cancel_work`, `take_work`, `poll`, `cancel_waiter`,
  `advance`); methods with `now_ms` expire first; `take_wakes` returns the tokens a transition readied. The
  dataplane wrapper owns the mutex, the `Arc` handles, and a token-to-`Waker` map; handle `Drop` calls
  `cancel_work`, `cancel_waiter`, or `release`.
- Clocks: `LinkHealth::acquire_at` and `AdaptivePeers::with_clock` take time explicitly; the existing
  constructors and methods pass the runtime clock.

## Read path in racer-core

`racer_core::read` holds the read-path decisions. The dataplane calls them; there is no second
copy of these rules.

- `budget`: `AcquisitionBudget` (re-exported as `racer_dataplane::read::flight::AcquisitionBudget`)
  and `inherited_budget`. It returns `racer_core::read::Error`. The dataplane converts it with `?`
  (`From`), and `TryFrom<dataplane Error>` goes the other way. Some tests compare results directly,
  so they now expect the core error.
- `candidates`: `visit_order`, `replica_score`, and `ProbeOutcome`. `classify` works on a
  payload-free `ResponseKind`/`OperationKind` and calls back to validate the payload. Also
  `origin_miss_error` and `validate_origin_authority`.
- `walk`: `CandidateWalk` is the sans-IO walk for one membership epoch (visit order, copy-only
  rule, funded and partitioned routes, evidence, `End`). `RemainingWalk` is the walk after a 412.
  `plan_attempt` holds the arithmetic from `request_traced`. The module also has the hedge rules
  (`hedge_eligible`, `hedge_budgets`, `hedge_fallback`), `transport_outcome`, and
  `refresh_allowed`. Production `resolve_epoch_traced`, `remaining_validated_copy`,
  `request_traced`, and `hedge_page` drive these.
- `response`: `PeerResponse<P, G>` is generic over payload and grant, and `Payload` has
  `PhantomPage` for the simulator. The dataplane's `PeerResponse` is an alias with `CiphertextPage`
  and `TransferGrant`.
- `flight`: `PageFlights` is a sans-IO singleflight with `Token` wakes, an admission trait, and a
  seedable hasher. Its join gates (`join_budget_gate`, `waiter_cap_gate`, `entry_cap_gate`) are the
  ones production `Flights::join_prepared` uses. Transitions run in
  `flow_control::coalesce::flight::state::State`. That type is now generic over its wake target
  (default `Waker`), so production and the simulator share the engine.
- `fill`: the `FillPlan` state machine (local, hedge, walk, origin, remaining copy, publish).
  Production uses `leader_source`, `acquisition_failure`, and `ciphertext_publish_tolerated`.
- `serve`: `serve_action` and `serve_deadline` from `Coordinator::serve_peer`. `policy` has
  `peer_error`, `recoverable`, `optional_headroom`, the retention eligibility and score rules, and
  `persist`.
- `metadata`: the sans-IO `MetadataCohorts` (`MAX_WAITERS`, `MAX_REFRESH_ATTEMPTS`),
  `RefreshPurpose`, and `refresh_settlement`. `range`: `direct_fills`, `may_select`, `share`,
  `selection_demand`, `PageBudget`, `DIRECT_AHEAD`, and `SELECTIONS`.
- `worker`: `WorkerMap::select`.

Phase 0 changes outside racer-core:

- `RequestScope` has `check_at`, `candidate_progress_at`, and `candidate_body_progress_at`.
- `Retention` has `observe_at`, `track_at`, `touch_at`, `score_at`, `configured_at`, and
  `snapshot_at`.
- `flow_control::Adaptive<K, O, C: Clock = fn() -> Instant>` adds `with_clock`.
- `SecondSight<S>` and `Heat<K, S>` have `with_hasher` constructors.

Three production paths did not move fully onto a state machine. The async code has too many
resource fences (driver permits, quota charges, oneshot handoffs) to drive from a plain step
function. For each, the sans-IO version has its own tests, and production calls the shared rules:

- Production `Flights` keeps its async table and calls the core gates and the shared `State`.
- Production `Fill` calls the `fill` helpers rather than driving `FillPlan`.
- Production `MetadataService` keeps `flow_control::coalesce::Table` and uses the core constants
  and `refresh_settlement`. `MetadataCohorts` reproduces the `coalesce::Table` election rules,
  with a deterministic wake order.

`request_traced` now reserves remote credits (`plan_attempt`) before it builds the attempt scope,
where it used to reserve them after. These can only fail in different order if `Cancellation::new`
fails.

## Control plane integration

`racer_sim::control::ControlSim` runs the `cluster-control` model (`step::cluster::ControlPlane`: lottery,
seats, primary, `plan_pass`, commits) and a per-client model on one timeline. `advance_to` runs whichever
queue is earlier (the model first on ties), so control and client events share one trace hash. A data-plane
world can co-step it with `next_event_ns`/`advance_to` and read `membership_version(node)` or
`applied_sequence(node)` to make a node lag like its control client.

The client model uses the real seat server rules: a bootstrap slot (32 per seat), then the bearer
authorization calls (one TokenReview and four GETs, `auth.rs` `authorize`) through `apiserver::Store` with a
`TokenBucket` (default 1000 QPS, burst 2000, 5 ms). Snapshot long polls use `SeatAdmission::snapshot`
(`check_ahead` 503), a write slot (64 per seat, none free is a 429), and `body_for` for delta or full bodies.
Waking a seat's waiters costs `cost::notify` comparisons. Base hashes are not stored per client: a client at
the delta base sequence is assumed to hold the matching hash. The model gained read-only accessors
(`record`, `image`, `seat_served`, `commit_ns`, `alive`, `pod_count`) and `remove_pod`.

Scenarios (CLI `--scenario`): `enrollment-storm` (every Pod starts within 5 s), `failover` (crash the
primary at d/3, two seat holders at d/2, the new primary 5 s later; restarts after 60 s), `ctl-churn`
(1/16 of the Pods at start, eight growth waves until d/2, two removal waves of 2.5%), `lagging` (10% of
clients partitioned for d/6, 5% slow, a 1% removal and a seat crash while they lag). `--config` takes a
`"control"` object of `control::Settings` overrides.

Invariants: `ctl_single_primary` (the model's `dual_primary` never grows), `ctl_terms_monotonic`,
`ctl_sequence_monotonic` (sequence grows, membership version never drops), `ctl_client_membership_monotonic`
(per client), and `ctl_client_converges`: once faults have healed for 180 s, every live client that started
at least 180 s plus the API time for all enrollments ago has applied every sequence committed 180 s ago.
None needed relaxing.

Results at 50,000 Pods, seed 1, default durations, release build, each run alone on one thread (the four ran
at the same time):

| scenario | sim time | wall | peak RSS | key results |
|---|---|---|---|---|
| enrollment-storm | 600 s | 2:34 | 0.91 GiB | all enrolled at 282 s; 462k API 429s, 501 write 429s; 58k TokenReviews; enroll p50 129 s, p99 258 s |
| failover | 900 s | 2:48 | 0.97 GiB | 3 terms; time to new primary 32 s and 35 s; dual primaries 0 |
| ctl-churn | 1200 s | 3:05 | 0.87 GiB | 11 membership versions, commit interval 60 to 180 s; 295k delta vs 54k full replies (3.9k full fallbacks); publication up to 5.7 MiB, delta up to 0.7 MiB; 454k write 429s; client lag p50 4.3 s, p99 24 s |
| lagging | 900 s | 3:26 | 0.97 GiB | 45k unreachable attempts; 4.1k full-image fallbacks; client lag p50 21 s, p90 206 s (partitioned clients); 0 503 ahead rejections |

The combined API peak (Lease reads and writes plus enrollment calls, including rejected ones) is 42k in
the first second of the storm, from every Pod's lottery read. No run saw a 503 ahead rejection: seats serve a
new sequence within one follow interval of each other, so clients that move between seats are not ahead.
The control-plane share of the 32 GiB budget is about 1 GiB peak RSS (about 20 KiB per Pod); the accounted
model state is 135 MB, mostly the model's inventory and image.

## Node integration

`racer-sim` now runs the real read path for every scenario except `standin` (the old stand-in world,
kept for substrate comparisons). Code: `cmd/racer-dataplane/sim/src/node/`.

Per node, with 2 workers:

- Per worker: `Placement<Phantom>` (shared ranking memo), `PageFlights` with a seeded hasher,
  `TickLru<u64, u32>` memory index, and its own `flow_control::Quotas`. `WorkerMap::select` picks
  the worker for a page.
- Per node: `Paths<SharedRoutes>` (shared route memo), `flow_control::Circuits<u32>` (the state
  behind `LinkHealth`), a real `Segments` table with `SegmentClock` and `PhantomEntries<u64>`, and
  `SecondSight` plus `Heat` for retention.
- Shared: one `topology::Membership` per published version (`Rc`), the phantom and route memos, and
  per-class rejection counters.

Read flow (`node/world.rs`): local memory, then disk, then `PageFlights::join`. Waiters sleep on a
token. The leader runs `FillPlan`: rank with `rank_async` (one CPU ranking chunk per poll), then
`CandidateWalk`. Each probe is sized by `plan_attempt` and sent hop by hop. Every holder runs
`decide_hop`; relays take a `Relay` quota, search their own route with the budget from
`search_budget` and `relay`, and forward. Failed links feed the circuits and the path `blocked`
list. The destination applies `serve_action`: copy-only checks local tiers, acquire gets
`inherited_budget` and runs the same flight and fill. Replies go back on the reverse route; pages
are cut-through transfers. Responses are checked with `classify`. Before the origin, the leader
re-ranks and checks `validate_origin_authority`. 412 goes to `RemainingWalk`; 403 and 5xx fail the
fill. Publication inserts into memory and applies `persist`; disk writes reclaim with
`reclaim_scored` and the catalog's segment value. Range reads schedule slots with `direct_fills`.

Membership: churn is batched every `membership_batch_ns` (250 ms) into a new version built with
`new_with_predecessor`. Each node adopts it after a fixed per-node lag (at most 100 ms) and can
resolve its version plus `retained_versions` (2) older ones. On adoption each worker runs
`Placement::maintain`. Dropped versions are removed from both memos with `forget`, which needed
`Membership::topology_identity` to be public.

Invariants checked: `budget_never_grows` (attempt plans, relays, inherited budgets),
`route_within_hop_limit`, `only_owner_hits_origin` (rank, owner, and authority on the walk's
version), `membership_version_monotonic` (per node), `relay_route_valid`, and
`flight_leader_valid`. None is relaxed. Three behaviors are listed in Known issues (3, 4, and 5).

Choices and differences from production:

- Sim capacities: placement 256 entries per worker (production about 16,384), route cache 128
  entries and 256 KiB per node (production 128 entries, 8 MiB). Decisions are the same code at any
  capacity; these keep per-node memory small. Flight, waiter, relay, and fill quota limits use
  production defaults. All are set under `"node"` in `--config`.
- Admission runs the production rules (`racer_core::admission::Rules`, see "Round 2: production
  admission"). Only rejection reporting differs (per-class counters instead of telemetry), and the
  cache id is interned to `u32`.
- Hedges are raced as in production when `node.hedge_slots` is nonzero (default 0, as in production);
  see "Round 2: fidelity".
- Retention observes every client read, as production does; see "Round 2: fidelity".
- Link backoff has the production shape, with jitter from a fixed mix instead of SHA-256.
- Searches and rankings yield between CPU slices; see "Round 2: fidelity".
- Reads at nodes outside the current membership are skipped. Clients retry `recoverable` errors
  up to 4 times.
- `page-alloc` is built with its `simulation` feature (for `Segments::configure`). The
  dataplane image builds `--bin racer-dataplane`, which does not unify features with `racer-sim`.

Scale (release, `--duration 1s`, `--config '{"objects": 256, "request_rate": 2.0}'`, seed 1,
one thread). Peak RSS is `VmHWM`. Accounted bytes are the sum of `approximate_bytes` estimates.

| Scenario | Nodes | Wall | Events | Peak RSS | RSS per node | Accounted per node |
| --- | --- | --- | --- | --- | --- | --- |
| zipf | 5,000 | 7.3 s | 93k | 145 MiB | 30 KiB | 15 KiB |
| zipf | 50,000 | 97.5 s | 620k | 1.23 GiB | 26 KiB | 14 KiB |
| storm | 5,000 | 23.3 s | 434k | 560 MiB | 115 KiB | 50 KiB |
| churn | 5,000 | 8.9 s | 84k | 151 MiB | 31 KiB | 15 KiB |
| churn | 50,000 | 183 s | 612k | 1.30 GiB | 27 KiB | 14 KiB |
| storm | 50,000 | 667 s | 6.0M | 5.27 GiB | 111 KiB | 64 KiB |

All runs end with zero hard invariant failures. At 50,000 nodes the host time goes to real
topology work: in zipf, 39 s placement ranking (each new slot scores all members) and 54 s route
search (each new (from, to, budget, visited) key runs `EqualCostSearch`); in churn, 124 s ranking
because batches of more than `MAX_INCREMENTAL_CHANGES` (64) changes force full rescoring. The
report's `profile_wall_ms` shows this split. Memory is far below the 32 GiB target; the default
request rate (20 per node per second) and 4,096 objects make 5,000 nodes take about 60 s per
simulated second, again mostly in route search. The 50,000 node storm (every node reads the same 16
pages at once) is the largest: 588 s of route search, and the shared route memo (1.1 GiB) and
per-node route caches (0.6 GiB) dominate memory.

## Combined control and data plane

Code: `cmd/racer-dataplane/sim/src/combined.rs`. `Combined` holds a `ControlSim` and a
`NodeWorld` built with `NodeWorld::new_combined`. Pod `i` is data-plane node `i`.

Coupling:

- One timeline. Before the data plane pops an event at time `t`, every control event at or before
  `t` runs (`ControlSim::advance_to`), control first on ties. The report's `trace_hash` mixes both
  hashes; `data_trace_hash` is the data plane's alone.
- `ControlSim::record_changes` makes the control side log what the data plane needs, drained after
  each control step (`ControlSim::drain`): newly committed membership versions with their member
  Pods (from the committed image's `publication.members`; the model names Node `i` with the UID
  `{i:08x}-0000-...`), each client's applied membership version (0 when its process restarts),
  and Pod crashes, restarts, and removals.
- Membership: `Versions::external` has no version until the first commit. `Versions::commit` builds
  the shared `topology::Membership` for each committed version once, with the previous version as
  predecessor, cached by version number (numbers may skip). A node adopts exactly the version its
  client applied (`Versions::apply`) and resolves it plus `retained_versions` older ones, so a
  lagging or partitioned client gives real data-plane skew (`StaleMembership`,
  `IncompatibleMembership`). Versions no live node can resolve are pruned (at most once per
  simulated second) and forgotten in the shared memos.
- A node whose client has applied nothing (not enrolled yet, or restarted) does not read and does
  not accept peer links (`Versions::enrolled`). A Pod crash crashes the node (disk survives), a
  restart restarts it, and a removed Pod stays down. Crashed members stay in the membership, as in
  the control model.
- The synthetic churn, batching, and per-node lag of standalone runs are not used. Standalone
  scenarios are unchanged: with `Versions::new`, `enrolled` is always true and `get` returns the
  same version as before, so their trace hashes do not change.

Scenarios. Each uses the control plan of the named control scenario and a light Zipf read load
(`objects` 4096, `zipf_s` 1.0, `request_rate` 0.02 per node per second by default):

| scenario | control plan | default duration | data plane |
| --- | --- | --- | --- |
| `full-cold-start` | `enrollment-storm` | 400 s | every node reads object 0 within 1 s at 3/4 of the run |
| `full-failover` | `failover` | 900 s | Zipf reads while the primary and seats crash and restart |
| `full-churn` | `ctl-churn` | 1200 s | Zipf reads while the cluster grows from 1/16 in 8 waves, then 2 removals |
| `full-lagging` | `lagging` | 900 s | Zipf reads while 10% of clients are partitioned, 5% slow, 1% removed |

How to run:

```sh
make racer-sim ARGS='--scenario full-churn --nodes 200 --duration 300s'
make racer-sim ARGS='--scenario full-lagging --nodes 50000 --config cfg.json --out lagging.json'
```

`--config` takes data-plane keys (for example `{"request_rate": 0.005}`) and a `"control"`
object of control settings. Every data-plane and control invariant applies; the report's
`invariants.hard_failures` is the sum of both, and `combined` carries the control invariants,
control metrics, and the membership skew at the end. Tests: `combined::tests` (80 to 320 nodes,
every scenario, replay).

### Combined acceptance (50k)

Release build, seed 1, 50,000 nodes, default durations, `--config '{"request_rate": 0.005}'`
(250 reads per second cluster-wide; at 0.02 the placement ranking at 50,000 members would make each
run several times longer). Runs went two at a time on a 48-core host, each on one thread. Peak RSS
is `VmHWM`; histogram values are log-bucket upper bounds.

These numbers are from the rerun after round 2 ("Round 2: fidelity"), with yielding searches,
client-time retention, held base hashes, and the default data-plane faults of each scenario. Hedging
is off, the production default. Events now include one per ranking or search CPU slice.

| scenario | sim time | wall | peak RSS | accounted per node | events |
| --- | --- | --- | --- | --- | --- |
| full-cold-start | 400 s | 18:41 | 6.21 GiB | 61 KB | 197.5M |
| full-failover | 900 s | 13:50 | 2.64 GiB | 24 KB | 73.0M |
| full-churn | 1200 s | 23:57 | 2.50 GiB | 25 KB | 76.5M |
| full-lagging | 900 s | 16:42 | 2.65 GiB | 24 KB | 71.1M |

All four end with zero hard invariant failures and no relaxed invariant. Peak RSS is far below
32 GiB. The cold-start storm uses the most memory (route memo and in-flight state, as in `storm`).

| scenario | page reads ok / failed | failures by cause | origin requests per page (mean, max) | control |
| --- | --- | --- | --- | --- |
| full-cold-start | 153k / 735k | 502k hop budget, 230k deadline, 3.5k unavailable | 1.45, 3 | all enrolled at 282 s; 462k API 429s, 501 write 429s; client lag p50 129 s, p99 258 s (enrollment) |
| full-failover | 263k / 18.4k | 14.7k hop budget, 3.7k unavailable | 1.42, 8 | 3 terms, 4 crashes, 3 restarts; all enrolled at 284 s; 1.6k origin 5xx, 96k link failures |
| full-churn | 329k / 5.0k | 3.8k incompatible membership, 1.0k hop budget, 0.2k unavailable | 1.64, 10 | 11 versions, 2,500 Pods removed; 454k write 429s; client lag p50 4.3 s, p99 24 s; 4.5k `StaleMembership` replies; 285 origin 412s; 295k delta and 54k full replies |
| full-lagging | 254k / 21.5k | 12.3k hop budget, 5.6k incompatible membership, 3.6k unavailable | 1.37, 4 | 45k unreachable attempts, 50k write 429s; client lag p50 21 s, p99 258 s; 6.8k `StaleMembership` replies; 45k delta and 54k full replies |

Observations:

- The cold-start storm (50,000 nodes times 16 pages in one second) fails most reads through Known
  issue 5 and read deadlines, as the standalone `storm` does. Origin load stays at most 3 per page.
  With yielding searches, nodes reject 721k route searches with `Overloaded` at the production
  limit of 8 concurrent searches (Known issue 10). This limit causes no failures in the other three
  runs.
- In `full-failover`, origin 5xx bursts fail fills, and client retries fetch again, so one page
  reached the origin 8 times. The site partition adds link failures and hop budget failures (Known
  issues 5 and 7).
- Membership skew from lagging control clients shows up as `StaleMembership` replies and
  `IncompatibleMembership` failures (Known issue 3). At the end of every run all live nodes are on
  the latest committed version.
- Ownership moves raise origin requests per page above `REPLICAS` (Known issue 6), and crashed
  members cause hop budget failures (Known issue 7).
- No placement ranking was rejected (`ranking_overloaded` 0): 256 entries per worker are enough
  for this load.
- No control reply fell back to a full image because of a held hash mismatch
  (`ctl_base_hash_mismatches` 0). Delta and full counts follow the sequence check, as before.
- Host time goes to real placement ranking at 50,000 members (252 to 965 s per run, from the
  report's `profile_wall_ms`), then route search (596 s in the cold-start storm).
- About 34k reads per run are skipped while their nodes are not yet enrolled (73k in
  `full-churn`, where Pods join late).

### Remaining gaps

Closed in round 2 (see "Round 2: fidelity"): hedges are raced, retention observes client reads,
searches and rankings yield, control clients send the base hash they hold, and the combined
scenarios carry data-plane faults.

### Known issues, summary

1. Long poll replies are bounded by 64 write slots per seat, so mass wakeups answer 429.
2. Resolved: the test-only `RouteBudget::forwarded` was removed; tests use `racer_core::route`.
3. A requester on a newer membership than its destination fails with `IncompatibleMembership`.
4. A cold page can reach the origin once per owner (up to `REPLICAS`).
5. Under overload or failed routes, page reads fail with `HopBudgetExhausted` instead of trying
   other owners.
6. Ownership moves on membership changes add origin requests beyond `REPLICAS` per page.
7. Crashed members stay routable until link backoff; reads near them hit issue 5.
8. Core `PageFlights` lets an origin-rejected leader receive a later result; production fails it.
9. The simulator touches a page on a duplicate memory publish; production does not.
10. Route searches beyond 8 per node fail at once with `Overloaded`; storms reject many of them.

## Round 2: production admission

`ResourceClass`, `Limits` (with its per-worker defaults as `Limits::default()`), and the pure policy
rules (`limit`, `floor`, `max_keys`, `wakes`, `allows_stopped`, `covers`) moved to
`racer_core::admission` (`cmd/racer-dataplane/core/src/admission.rs`). The dataplane re-exports them
from `src/admission.rs` and `src/config.rs`, and `AdmissionPolicy` delegates to `Rules`, keeping the
telemetry `Observer` and `rejected` hook. `Config::from_lookup` takes unset limit defaults from
`Limits::default()`. `rules_equivalence_tests::moved_rules_match_legacy_policy` checks the delegated
policy against a verbatim copy of the old rules for the default, environment, scaled, and edge
(1 to 65,536 connections, saturating waiters, single-entry) configurations.

The simulator's `SimPolicy` (`sim/src/node/admission.rs`) now delegates to the same `Rules` over the
production `ResourceClass`; only `rejected` (per-class counters) differs. Sim-specific choices:

- Limits are `Limits::default()` (one simulated node is one production I/O worker, so no
  `scale_limits`), with plaintext, ciphertext, dirty, flights, waiters per flight, relay, route, and
  retained-snapshot dimensions taken from `NodeConfig`, whose defaults equal production's.
- Floors use the simulator's 1 MiB page (production 16 MiB) with the production record header
  bound, so the ciphertext floor is 1 MiB + 64 KiB (it was 1 MiB + 16).
- The report's `rejections` now lists all 15 production classes plus `keys`.
- Cache ids stay interned to `u32`, so a charge never clones a `CacheId` string; there is no
  per-charge clone cost to remove.

5,000 nodes, seed 1, release, same host (wall time is noisy on a shared host):

| Scenario | Wall before / after | Max RSS before / after | Rejections before / after |
| --- | --- | --- | --- |
| zipf | 105 s / 114 s | 1.59 / 1.60 GiB | relay 28,008 / relay 28,008 |
| storm | 61 s / 61 s | 1.32 / 1.33 GiB | relay 4,534 / relay 4,534 |

All other classes reject 0 in both. Metric counters are identical before and after, so the
production rules do not change simulated behavior for the classes the read path charges.

## Round 2: contention model retired

`cmd/racer-dataplane/src/contention.rs` (a test-only, payload-free discrete-event pressure model) was
removed. Its scenario tests now run the real node logic in `racer-sim`
(`sim/tests/contention_scenarios.rs`); its `fidelity` and `waiter_detach` oracles, which compared
model owner traces with production allocations, became production contract tests and equivalence
tests between production and the `racer-core` code the simulator drives:

- `src/read/core_equivalence.rs`: production `Flights` against core `PageFlights` on 48 seeded
  join, poll, cancel, deadline, detach, publish, fail, and origin-rejection traces (outcome of every
  step, waiter and flight admission); production `MemoryCache` against the simulator's `TickLru`
  admission on 32 seeded hit and insert traces (resident set after every step).
- `src/read/tests/persist_policy.rs`: real `Fill::observe_verified` and bootstrap reads against
  `racer_core::read::policy::persist` across ownership, second sight, and dirty headroom.
- `src/read/owner_contracts.rs`: production halves of the owner, reclamation, crypto, and admission
  checks.

Two divergences were found and pinned (Known issues 8 and 9). The old dirty-pressure oracle never
reached dirty pressure: its retention classifier owned nothing, so its page was never eligible for
persistence. The replacement sets ownership and checks both outcomes.

| Old test | Replacement |
| --- | --- |
| `scenarios::cold_nodes_contend_for_one_global_origin_queue` | `contention_scenarios::cold_nodes_contend_for_one_global_origin` |
| `scenarios::warm_peer_fanout_is_limited_by_source_node_network` | `contention_scenarios::warm_peer_fanout_is_limited_by_source_node_network` |
| `scenarios::workers_share_node_network_bandwidth` | `contention_scenarios::workers_share_node_network_bandwidth` |
| `scenarios::overlapping_ranges_join_cold_fills_and_deliver_each_read` | `contention_scenarios::concurrent_cold_readers_join_flights` |
| `scenarios::slow_disk_sheds_persistence_without_losing_reader_progress` | `contention_scenarios::slow_disk_sheds_persistence_without_losing_reader_progress` |
| `scenarios::partition_retries_recover_after_heal`, `retry_exhaustion_is_bounded_and_does_not_poison_later_requests` | `contention_scenarios::reads_recover_after_partition_heals` |
| `scenarios::two_thousand_nodes_keep_per_worker_bounds_under_cold_start` | `contention_scenarios::two_thousand_nodes_cold_start`, `cold_start_completes_with_bounded_origin_traffic` (400 nodes) |
| `scenarios::generated_seed_matrix_replays_identical_reports_and_drains` | `contention_scenarios::seed_matrix_replays_identical_reports` |
| `scenarios::canceling_one_joined_reader_preserves_the_surviving_read`, `cancellation_and_deadline_drain_inflight_owners_before_recovery` | `core_equivalence::flights_match_page_flights_on_seeded_traces` (cancel and deadline steps with survivors) |
| `waiter_detach::terminal_waiter_replacement_matches_production_before_completion`, `last_waiter_detach_keeps_peer_source_and_completion_owners`, `replacement_reads_finish_after_cancellation_or_deadline_detaches_waiters` | `core_equivalence::flights_match_page_flights_on_seeded_traces` (waiter cap 3, so replacements join only after a detach), `origin_rejection_is_retry_in_core` |
| `fidelity::duplicate_owners_match_full_page_occupancy_until_each_last_owner` | `owner_contracts::duplicate_owners_charge_once_until_each_last_owner` |
| `fidelity::fair_reclamation_matches_metadata_trace_and_keeps_other_cache` | `owner_contracts::fair_reclamation_keeps_other_cache`, `core_equivalence::memory_cache_matches_sim_lru_on_seeded_traces` |
| `fidelity::dirty_pressure_matches_metadata_skip_while_real_bootstrap_read_succeeds` | `persist_policy::fill_persistence_matches_core_policy`, `dirty_pressure_skips_persistence_while_bootstrap_read_succeeds`, `owner_contracts::non_owned_persistence_uses_optional_headroom` |
| `fidelity::canceled_crypto_matches_metadata_owner_trace_through_completion_reap` | `owner_contracts::canceled_crypto_holds_owners_until_completion_reap` |
| `scenarios::two_live_caches_can_use_global_transport_capacity_until_exhaustion`, `two_caches_with_spare_pipes_do_not_park_readers` | `owner_contracts::transport_capacity_is_global_across_live_caches` |
| `scenarios::cache_scoped_resources_still_enforce_fairness_with_global_room` | `owner_contracts::cache_scoped_resources_enforce_fairness_with_global_room` |
| `scenarios::same_tick_newer_pump_cannot_bypass_a_notified_head`, `cancellation_removes_every_queue_position_before_delayed_release`, `canceling_a_notified_head_wakes_its_successor_before_delayed_release`, `one_pipe_drains_a_full_bounded_queue_sequentially`, `canceling_the_only_queued_request_allows_later_recovery`, `canceling_a_queued_request_preserves_survivor_fifo_order`, `canceled_queue_tail_frees_capacity_for_a_replacement_waiter`, `canceled_send_keeps_page_and_pipe_until_their_scheduled_completions` | Not ported: they tested the model's own pipe queue. Production FIFO, cancellation, and charge retention are covered by `flow/src/pipe.rs` (`scheduled_acquisition_is_bounded_fifo_and_wakes_only_for_progress`, `pipe_waker_rearms_and_advances_fifo`, `canceled_waiter_storage_remains_charged`) and `flow/src/fifo.rs` (`fifo_cancellation_and_completion_ownership`) |
| `scenarios::request_context_bound_rejects_excess_arrivals_and_recovers`, `connection_rejection_rolls_back_the_new_request_context`, `idle_nodes_add_no_polling_events_or_logical_ownership` | Not ported: model scheduler mechanics. Admission rejection by class is covered in `racer-sim` (`node::tests::tight_quotas_reject_by_class`); idle nodes adding no events by the `empty` scenario (`racer-sim` `smoke_scenarios_hold_invariants`, `events == 0`) |
| `scenarios::slow_reader_keeps_a_bounded_window_and_eventually_makes_progress`, `larger_request_window_overlaps_fill_and_reader_service`, `cache_churn_reclaims_idle_bundles_and_allows_a_new_cache_to_progress`, `identical_object_numbers_in_distinct_caches_do_not_singleflight` | Not ported: the simulator has no client reader rate, request window, or multiple cache identities per object. Production range windows are covered in `src/read/range_stream.rs` (for example `scheduler_windows_are_bounded_and_drop_frees_capacity`), cache churn by `src/read/tests/fill.rs` (`cooperative_reclaim_fair_share_keeps_other_cache_and_live_ciphertext`), and the cache is part of the `PageId` that keys flights and the memory cache |

Differences from the old assertions:

- Cold start: the old model required every read to succeed. With real limits, flight-table
  rejections under a full-fleet storm end walks in `HopBudgetExhausted` (Known issue 5), about 38%
  of reads at 2,000 nodes. The test requires that every read ends and that failures are only that
  error or admission `Overloaded`.
- Warm fanout: copies come from up to `REPLICAS` owners and relays, not one warm node, so the
  test checks the aggregate NIC floor and that a 17-reader fanout takes at least twice as long as one
  reader on the same slow NIC.
- The simulator does not model quota-driven `evict_idle`, per-cache fair-share reclamation, client
  cancellation, or request deadlines on the data plane; those stay production contract tests.

`sim/tests/contention_scenarios.rs` runs in about 9 s under the workspace test profile.

## Round 2: fidelity

This round closes the five gaps listed under "Combined control and data plane". Code:
`cmd/racer-dataplane/sim/src/node/world.rs`, `node/state.rs`, `node/config.rs`, `control.rs`,
`combined.rs`, and `resources.rs`. Production behavior is unchanged.

### Hedged page reads

The simulator races hedges with the same rules as production `hedge_page`
(`src/read/candidates.rs:190`) and `uring_runtime::drivers::race` (`runtime/src/drivers.rs:599`):

- After ranking, a plaintext fill at a non-owner evaluates `hedge_eligible`. The race starts only when
  `node.hedge_slots` is nonzero, both of the first two visited owners are directly reachable (the
  requester's link circuits allow them), one page of plaintext and ciphertext is held as escrow, two
  pages fit as a trial reservation, and the node's shared `flow_control::Hedges` grants a permit for
  `DUPLICATE_BYTES`. Otherwise the plan reports no hedge (`hedge_suppressed` when eligible).
- `hedge_budgets` splits the fill budget. The primary is an `Acquire` to rank 0 and the duplicate is a
  `CopyOnly` to the second visited owner. Both are planned with `RouteMode::DirectHedge` and sent as
  pinned one-link exchanges that skip route search.
- The duplicate starts after `node.hedge_delay_ns`, unless the primary has already finished or the race
  is decided. Before it starts, the link and one page of headroom are checked again. Each started
  duplicate counts `DUPLICATE_BYTES` in `hedge_duplicate_bytes`, as production `Permit::started` does.
- The first page wins. A non-recoverable error is fatal, and recoverable errors wait for the other
  contender. When the race is decided, the losing probe and its message are dropped, which releases
  its relay charges. Production waits for the loser's completion fence before it returns. The
  simulator releases at cancellation.
- When both sides are done, the budgets are reunited. `budget_never_grows` checks the split, both
  attempt plans, and the reunited budget. A winner publishes as `Source::Hedge`. Otherwise
  `hedge_fallback` errors run the serial walk with `primary_consumed`, the primary's evidence, and
  bounded routes. A stale reply refreshes the membership (`refresh_allowed`) and walks the new epoch,
  as `resolve_epoch_traced` does.

Knobs under `"node"`: `hedge_slots` (default 0, hedging off, as production `page_hedge_slots`),
`hedge_delay_ns` (100 ms), and `hedge_bytes` (one `DUPLICATE_BYTES`; it must cover one duplicate when
hedging is on). Because the default byte budget allows one race per node at a time, `hedge_bytes`
must grow with `hedge_slots` for more concurrent races. Test: `node::tests::hedging_beats_a_slow_primary`.
It uses 64 nodes, seed 21, and makes each rank 0 owner 255 times slower. Without hedging, 20 page
fills are eligible and none is raced. With 4 slots and a 5 ms delay, 17 races start, the duplicate
wins all 17, the losers are canceled, and the mean read latency falls from about 137 ms to about
80 ms.

Fabric change found by this test: `Fabric::transfer` used to reserve the receiver's NIC at each chunk's
send time, so a slow sender's chunks, reserved up front into the future, blocked every other sender to
that receiver until the slow transfer ended. The receiver is now a work-conserving backlog of its own
service time from the transfer's issue (`resources.rs:186`). With equal-speed senders this matches the
old FIFO slot. Test: `resources::tests::slow_sender_does_not_hold_the_receiver`.

### Retention at client read time

Production observes retention once per client operation (`src/read/fill.rs:983-1014`): on a memory hit
it calls `Retention::observe` and then `persist_verified`. On a miss it observes before joining the
flight, and a peer acquire uses `owned_only`. The simulator now does the same:

- `NodeWorld::observe` (`world.rs:2740`) follows `Retention::observe_at` (`src/retention.rs:181`): it
  applies the ownership predicate, then records second-sight history and touches heat. Ownership is
  `NodeWorld::owned` (`world.rs:2725`), the worker's cached `Placement::is_owner` on the node's
  current snapshot. A missing or partial ranking is not owned, as in production `is_owned`.
- The observation is held by the read. Memory hits and completed joins call `persist_page`
  (`world.rs:2756`, production `persist_verified`, with ownership checked again when it runs). A
  flight leader carries its observation to publication, and waiters use their own when they wake
  with the page.
- Heat is no longer touched by peer disk hits, matching production, where a peer acquire does not
  observe.

Test: `node::tests::retention_observes_client_reads_including_memory_hits` (a non-owner persists a page
on its second read, which is a memory hit, not at publication).

### Yielding rankings and route searches

Rankings and route searches run as tasks (`spawn` and `slice`, `world.rs:1302-1440`). Each task holds
the real future (`Placement::rank_async` or `Paths::route`) together with `Rc` handles to the worker's
placement or the node's paths. Each poll is one CPU slice on the worker's CPU FIFO, and the next poll
runs in an event at the end of that slice, so work queued on the CPU in the meantime runs between
slices, as an executor runs other tasks after a yield:

- Ranking: every poll costs `ranking_chunk_ns` and scores up to `WORK_QUANTUM` (256) members
  (`topology/src/placement.rs:19`). The future pins its cache entry across yields. When the cache is
  full of pinned entries, `ranking()` returns `Overloaded` (`placement.rs:815`), counted as
  `ranking_overloaded`.
- Route search: a cache hit answers on the first poll at no cost. Each further poll expands up to
  32 nodes (`SEARCH_QUANTUM`) at `path_expand_ns_per_node` each. A search holds its in-flight slot,
  and the ninth concurrent distinct search on a node fails with `Overloaded`
  (`topology/src/paths.rs:530`, `active_searches` 8), counted as `route_search_overloaded`.
- When a search finishes, the blocked set is recomputed. If it changed, the hop fails with
  `Unavailable` (`route_blocked_changed`), as production `Paths::shortest` does (`src/topology.rs:681`).
- The phantom placement's shared memo still provides the result. Each worker's cache entry keeps its
  own cursor, so a cold slot takes the production number of polls and keeps its pin for that long.
  Route results are shared through `SharedRoutes`, and each node's `Paths` still charges and holds its
  own search.

A crash drops the node's tasks, which releases their pins and slots. Test:
`node::tests::yielding_searches_hit_production_concurrency_limits` (600 nodes, so each ranking takes
three polls, with 2 placement entries and 1 search slot: both limits reject work; with defaults no
ranking is rejected).

### Control clients send the base hash they hold

Each control client keeps a fingerprint of the content hash of the image it applied (`held` in
`control.rs`, the first 64 bits of the hex SHA-256). The fingerprint is cleared on a process restart and
set from the delivered image (or from the hash recorded per committed sequence when the seat answers
from an older sequence). A reply sends the hash the client actually holds. `reply_body`
(`control.rs:64`) passes it to the real `body_for` (`cluster-control/src/step/admission.rs:71`, used by
`Image::for_base`), so a seat sends a delta only when the client's applied sequence and held hash both
match the delta base. Otherwise it sends the full image (`ctl_base_hash_mismatches` counts full replies
at the base sequence). Tests: `control::tests::delta_needs_the_held_base_hash` (up to date gets a
delta; restarted, two behind, and a different hash at the same sequence get full images) and
`mismatched_held_hash_falls_back_to_full`.

In the existing scenarios, sequences are monotonic (`ctl_sequence_monotonic`) and each content hash
belongs to one sequence, so the delta and full counts do not change. Restarted and lagging clients
already fell back on the sequence check. The held hash only matters when a client holds a different
image at the same sequence.

### Data-plane faults in the combined scenarios

`DataFaults` (`combined.rs:72`, `"combined_faults"` in `--config`) adds data-plane faults to each
combined plan when `--config` gives no `faults` list. An explicit list replaces the defaults, and
`"enabled": false` turns them off. For a run of length `d`:

| scenario | data-plane faults |
| --- | --- |
| `full-cold-start` | 1% of nodes (`slow_permille` 10) run 8 times slower (`slow_factor`) from 3d/4 - d/40 to 3d/4 + d/20, around the storm |
| `full-failover` | sites 0 and 1 are cut apart from 2d/5 to d/2 (`partition`); origin 5xx at `p5xx` 0.3 for `burst_ns` (30 s) at d/5 and 3d/5 |
| `full-churn` | origin 412 at `p412` 0.1 for 30 s at d/2, while ownership moves |
| `full-lagging` | 1% of nodes run 8 times slower from d/10 to 9d/10 |

Slow nodes are every `1000 / slow_permille`-th node, starting at half that stride. The standalone
`faults` scenario and `--config` `faults` work as before. Test:
`combined::tests::data_faults_join_the_control_plans`.

### Scale

The four combined scenarios were rerun at 50,000 nodes with these models. The results are in
"Combined acceptance (50k)".
