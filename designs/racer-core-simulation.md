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
   Resolved: woken long polls queue for a write slot and `Signal` wakes in O(W); see
   "Round 3: ctl" (commits 75ee86562, 0dde5abdd).

2. Resolved: `RouteBudget::forwarded` and `RouteBudget::validate_forwarded`
   (formerly `cmd/racer-dataplane/src/topology.rs:536`, `:552`) were called only by tests and used the
   search form of `visited`. Both were removed in round 2. Their test
   (`topology::tests::scenarios::forwarded_budget_is_monotonic_and_rejects_cycles`) now drives
   `racer_core::route::forward` and `validate` on a `RouteBudget`'s fields, so search-form forwarding
   has one rule; the signed wire form stays in `racer_core::route::check_wire_transition`.

3. Resolved: a `StaleMembership` answer with nothing newer to refresh to now skips that owner
   (`racer_core::read::walk::stale_action`, `Outcome::StaleDestination`), commit 296a54117. See
   "Round 3: route". Original text:
   A requester that adopted a newer membership than its destination fails the read. The
   destination cannot resolve the newer version and answers `StaleMembership`; the requester then
   refreshes, but its latest version is not newer than the one it used, so `refresh_candidates`
   returns `IncompatibleMembership` (`cmd/racer-dataplane/src/read/candidates.rs:1203-1204`,
   `racer_core::read::walk::refresh_allowed` at `core/src/read/walk.rs:430-437`). That error is not
   `recoverable` (`core/src/read/policy.rs:9-22`), so the walk does not try the next owner. In the
   `churn` scenario (seed 1, 1 s, `request_rate` 2, 256 objects) this fails 812 of about 13k page
   reads at 5,000 nodes and 6,939 of about 60k at 50,000 nodes, while nodes adopt each publication
   within 100 ms. No invariant is relaxed; this is reported as a behavior to review.

4. Resolved (round 3, track origin, ea016b68d): owners above rank 0 send an `Acquire` to rank 0
   instead of a copy-only probe, so rank 0 alone fetches a cold page; see "Round 3: origin".
   Original report: A cold page can reach the origin once per owner, up to 3 times, with no concurrency at all. The
   rank 0 owner never asks later owners (`CandidateWalk::new` sets the walk length to the rank,
   `core/src/read/walk.rs:146`), and readers probe owners in spread order (`visit_order`), so a
   later owner may fetch first (after rank 0 answers a copy-only miss) and rank 0 fetches again
   when a reader asks it. In `node::tests::single_reader_then_storm_reuses_copies` (64 nodes,
   seed 7) one reader and then 64 readers cause 33 origin requests for 16 pages. The simulator
   bounds origin requests per page by `REPLICAS` in tests, not 1.

5. Resolved: a local `HopBudgetExhausted` is walk evidence (`transport_outcome`), so the walk ends
   `Exhausted` (recoverable `Unavailable`) or at origin for an owner, commit 296a54117. See
   "Round 3: route". Original text:
   Under heavy overload a page read fails with `HopBudgetExhausted`, which is not `recoverable`.
   Each probe charges its route links up front (`plan_attempt`, `core/src/read/walk.rs:478`), and
   after a failed route each probe charges 8. With the page budget of 16 links (`PAGE_LINKS`,
   `core/src/read/range.rs:24`) two `Overloaded` relays use it up; the next `plan_attempt` returns
   `HopBudgetExhausted` (`walk.rs:481`), `transport_outcome` does not map it (`walk.rs:420`), so the
   walk is abandoned with that error instead of ending as `Exhausted` (`Unavailable`). Production
   does the same (`src/read/candidates.rs:1037-1045`). In the `storm` scenario at 50,000 nodes
   (seed 1, 1 s, `request_rate` 2) 99,562 page reads fail this way while relays reject 1.25M
   transfers on the `Relay` quota (16 per worker).

6. Resolved (round 4, track handoff, cda9f9ea6, 57792bc40, 2733abafd): origin requests for a page
   are at most 1 + (rank 0 changes) + (fetches after the previous fetcher lost the page) +
   (fetches while it still held it). The last class needs a transient failure, a concurrent cold
   miss, or an owner without credits or links to delegate; tests keep it under 2% of origin requests
   in small churn runs (24 of 13,644 in 5k `full-churn`, 1,603 of 58,414 in 50k `full-churn`).
   50k `full-churn`: 75,267 origin requests to 58,414, max per page 11 to 9. See "Round 4: handoff".
   Earlier (round 3, track origin, 07a932713): an owner asks the page's owners under
   membership version - 1 copy-only before origin.
   Original report: Origin requests per page are bounded by `REPLICAS` only within one membership version. When a
   membership change moves a page's ranked owners, the new owners have no copy, and a reader that
   walks the new owners ends in `End::Origin` at the new rank 0 (Known issue 4 applies per
   version). Nodes still on the old version keep fetching from the old owners. In `full-churn` at
   50,000 nodes (seed 1, 11 membership versions, `request_rate` 0.005) one page reached the origin
   11 times and 20,035 of 45,938 pages more than once. No invariant is relaxed: `only_owner_hits_origin`
   holds on each request's own version; this is the cost of ownership moves, reported for review.

7. Resolved (round 4, liveness, af267b813 and f3be4c1cc): the primary drops a member whose Node
   has not been `Ready` for 30 s, so a crashed member is chosen for at most about 90 s (76 s
   observed at 50k); see "Round 4: liveness". Earlier: reads near crashed members no longer fail
   with `HopBudgetExhausted` (issue 5), commit 296a54117. Crashed members still stay routable until link backoff; the remaining failures
   are recoverable `Unavailable` after client retries. See "Round 3: route". Original text:
   Crashed members stay in the membership (their Node and Pod objects remain, so `plan_pass` keeps
   them), and peers keep routing through them until link backoff opens their circuits. Each failed
   route makes the next probe charge 8 links (Known issue 5), so reads near a crashed member fail
   with `HopBudgetExhausted`. In `full-failover` at 50,000 nodes (4 crashes, seed 1) 11,950 of about
   283,000 page reads fail this way. Same root cause as issue 5; no invariant is relaxed.

8. Resolved (round 3, track origin, a27f1f7ac): production is the intended behavior (a rejected
   caller must not be served a page fetched with another caller's credentials); core gained
   `PageFlights::reject`, and the simulator re-elects a waiter on 403. Original report:
   Core `PageFlights` has no origin-rejection error. Production `Flights::fail(OriginRejected)`
   (`cmd/racer-dataplane/src/read/flight.rs:1441`) cancels the supplying caller with
   `OriginRejected` before `Outcome::Retry`, so that caller fails even when another caller later
   publishes. `PageFlights::finish(Outcome::Retry)` (`core/src/read/flight.rs:295`) only makes the
   leader ineligible; it stays parked and receives the later result. Both re-elect the next
   eligible waiter. The simulator never finishes a flight with `Retry`, so runs are unaffected.
   Pinned by `read::core_equivalence::origin_rejection_is_retry_in_core`.

9. Resolved (round 3, track origin, a27f1f7ac): the simulator no longer touches on a duplicate
   publish. Original report: A duplicate memory publish touches only in the simulator. Production `MemoryCache::publish`
   returns early for a resident page without touching it (`cmd/racer-dataplane/src/memory.rs:1415-1418`),
   while the simulator's `memory_insert` touches it (`sim/src/node/world.rs:1214-1216`), so such a
   page lives slightly longer in the simulator. It only happens when a fill publishes or a disk hit
   promotes a page that is already in memory. Pinned by
   `read::core_equivalence::duplicate_publish_touches_only_in_the_simulator`.

10. Resolved: searches beyond the 8 slots wait in a bounded FIFO queue (`Paths::with_queue`,
   `DEFAULT_SEARCH_QUEUE` 64) instead of failing, commit 5528e6066. See "Round 3: route".
   Original text:
   Route search admission is per node and has no queue. A node runs at most 8 distinct searches at
   once (`Paths::route`, `cmd/racer-dataplane/topology/src/paths.rs:530`), and any further search
   fails at once with `Overloaded`. The send then fails: a requester sees a transport error, and a
   relay answers `Overloaded` through `relay_failure`. Either way the walk pays the failure-route
   charge from Known issue 5. In the simulator, searches run as yielding tasks ("Round 2:
   fidelity"), so they now overlap in time. In `full-cold-start` at 50,000 nodes (seed 1,
   `request_rate` 0.005), 720,628 searches were rejected this way during the storm, along with 502k
   `HopBudgetExhausted` failures. No search was rejected in the other combined runs. No invariant
   is relaxed. This is reported as a behavior to review.

11. Resolved in two halves (round 4, enroll and capacity). Combined numbers: "Round 4: combined
   (wave 1)".
   Data-plane half (round 4, capacity, 90544e2ca and 4654ec26e): the simulator
   streamed page bodies to requesters that had timed out, and the 1 s storm was 14 times over the
   owners' egress bound; 50k now reads 884k ok / 11.8k failed (background reads, issue 17), with the
   bound enforced by a test. See "Round 4: capacity".
   Control-plane half resolved: enrollment is no longer limited by API 429s. Each authorization
   reuses cached TokenReviews and bindings, a whole authorization is reused for 5 s, the immediate
   keyring recheck is skipped, and a per-seat adaptive API budget keeps calls under the server's
   rate. In 50k `enrollment-storm`, 429s fell from 462k to 23.5k and the all-enrolled time from
   282 s to 171 s. See "Round 4: enroll" (commits 9c36377e8..50a1963bf).
   Original text:
   The 50k `full-cold-start` storm still fails most page reads. With issues 5 and 10 fixed, no read
   fails with `HopBudgetExhausted` and no search is rejected, but walks that end `Exhausted` are now
   retried by the client (`Unavailable` is `recoverable`) and run into the read deadline instead:
   180k ok / 708k failed (696k `DeadlineExceeded`, 11.9k `Unavailable`), against 153k / 735k
   (502k hop budget, 230k deadline) before (seed 1, `request_rate` 0.005). The storm (50,000 nodes
   times 16 pages while nodes enroll) exceeds what the read deadline allows; 1.34M walks end
   `Exhausted`. No invariant is relaxed. Reported for review: the failure is now a capacity limit,
   not an error classification.

12. Resolved (round 4, track handoff, 57792bc40): an owner asks a predecessor that held the same
   rank under the previous membership under that membership, for 10 s after replacing it, so a
   lagging predecessor serves. Owner stale skips in 5k `churn-r` went from 143 to 3. See "Round 4:
   handoff". Original report:
   An owner whose predecessor is behind (Known issue 3) now reaches the origin with transient
   evidence for that predecessor, where it used to fail with `IncompatibleMembership`. This adds
   origin requests during churn: in `churn` at 5,000 nodes (seed 1) the maximum per page rose from 3
   to 4 (mean 1.35 to 1.41); at 50,000 nodes the maximum stays 5 (mean 2.39 to 2.47).
   `only_owner_hits_origin` holds. Same family as Known issue 6.

13. Resolved (round 4, track handoff, cda9f9ea6): handoff targets no longer reserve credits or
   links that the delegation to rank 0 needs, and an owner with handoff targets forwards spare
   credits to rank 0. 5k `full-churn` max per page 9 to 5. See "Round 4: handoff". Original report:
   Repeat origin fetches across membership versions have a heavier tail with handoff probes than
   without in `full-churn` at 50,000 nodes (seed 1, max 13 vs 10 per page), though fewer pages are
   fetched more than once. Owners left with one credit after delegating cannot ask previous owners
   (handoff needs two), and a rank 0 reached through delegation has one credit, so it never asks
   them. Root cause not fully established; see "Round 3: origin". No invariant is relaxed.

14. Members whose control client has not applied a membership yet (enrolling, or restarted) are
   routable but refuse peer links. In `full-failover` at 50,000 nodes (seed 1) about 145k of
   165,343 link failures go to them, coinciding with most of the 12.6k failed reads (60 to 200
   s). Inference from counters; see "Round 4: liveness". No invariant is relaxed.

15. Dropping a dead member (issue 7 fix) is an ownership move and inherits issues 6 and 12: at
   50,000 nodes in `full-failover` it added 171 recoverable `Unavailable` reads and moved the
   worst page from 7 to 9 origin requests. See "Round 4: liveness".

16. Hot-page peer throughput is capped by the page's `REPLICAS` owners: candidates are only the
   ranked owners (`src/read/candidates.rs:628-635`), so readers holding a copy never serve other
   readers, and a storm on one page gets at most `REPLICAS * nic_bps / page_bytes` deliveries per
   second. Enforced by
   `contention_scenarios::overloaded_hot_page_owners_do_not_stream_to_abandoned_requesters`. Under
   overload relays shed on the `Relay` quota before the owners' NICs saturate (50k, 1 s storm: about
   177k storm pages against a 570k bound). Raising it is a design change. See "Round 4: capacity".

17. Background reads during enrollment fail `Unavailable`: in `full-cold-start` about 12% of the
   Zipf reads (11.7k at 50k, ~100 at 5k, unchanged since round 3) fail while 138k link attempts hit
   members that are published but not yet enrolled (inference from `link_failures`; the storm alone
   reads 80,000 of 80,000 pages at 5k). Belongs with the enroll and liveness work.

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
| `full-cold-start` | `enrollment-storm` | 400 s | every node reads object 0 at 3/4 of the run, within `cold_start_window_ns` (1 s; 28 s at 50k, see "Round 4: capacity") |
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

1. Resolved: long poll replies queue for the 64 write slots instead of answering 429; notify is O(W)
   (75ee86562).
2. Resolved: the test-only `RouteBudget::forwarded` was removed; tests use `racer_core::route`.
3. Resolved: a requester on a newer membership than its destination skips it (round 3, route).
4. Resolved: owners above rank 0 delegate cold fetches to rank 0 (ea016b68d).
5. Resolved: a local hop budget shortfall ends the walk recoverably (round 3, route).
6. Resolved: origin requests per page are at most 1 + rank 0 changes + copies lost + a small,
   tested remainder (round 4, handoff).
7. Resolved: members whose Node has not been `Ready` for 30 s leave the membership, bounding how
   long a crashed member is chosen to about 90 s (round 4, liveness).
8. Resolved: core `PageFlights::reject` fails the rejected leader like production (a27f1f7ac).
9. Resolved: the simulator no longer touches on a duplicate publish (a27f1f7ac).
10. Resolved: route searches beyond 8 per node queue (up to 64) instead of failing (round 3, route).
11. Resolved: enrollment is no longer bound by API 429s (462k to 23.5k at 50k; round 4, enroll),
   and the storm no longer streams to abandoned requesters and fits the owners' egress (round 4,
   capacity).
12. Resolved: owners ask an unchanged predecessor under the previous membership, so a lagging one
   serves (round 4, handoff).
13. Resolved: handoff targets no longer block or starve the delegation to rank 0 (round 4,
   handoff).
14. Members not yet holding a membership are routable but refuse peer links; most 50k
   `full-failover` link failures (round 4, liveness).
15. Dropping a dead member costs one ownership move (issues 6, 12) (round 4, liveness).
16. Hot-page throughput is capped at `REPLICAS` owners' NICs; bound enforced by a test (round 4,
   capacity).
17. Background reads during enrollment fail `Unavailable` near unenrolled members (round 4,
   capacity).

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

## Round 3: ctl

Fixes Known issue 1 in production and in both control models.

### Root cause

- Write slots were taken without waiting after a long poll woke.
  `Server::snapshot` waited for a newer image (`wait_until`), then called `write_slot()?`, a
  non-queuing `Slots::try_acquire` (formerly `cluster-control/src/server.rs:510-512`, `:642`).
  Every waiter on a seat wakes on one `Signal::notify`, so all but the first 64 (`MAX_WRITES`)
  answered 429 and retried after client backoff. `keyring` (`:716`) and `replica` (`:751`) had the
  same pattern.
- Notify cost O(W^2). `Signal::changed` scanned the whole waker list with `will_wake` before pushing
  (formerly `cluster-control/src/exec.rs:357-361`). Each of the W woken polls that waits again
  re-registers, which costs W(W-1)/2 comparisons per notify.

### Changes

- `step::admission::WriteQueue` is the shared rule: a `Gate` plus a bounded FIFO of waiters. A
  released slot goes straight to the oldest live waiter, newcomers cannot jump the queue, and dead
  waiters are trimmed lazily. A full queue is `Full`, which still answers 429.
  `SeatAdmission::writes` is now a `WriteQueue<WriteWaiter>`.
- `exec::Slots` is built on `WriteQueue`. `try_acquire` keeps its old behavior (fail fast), and the
  new `Slots::acquire(deadline)` waits in FIFO order. A waiter that times out or is cancelled hands
  any slot it was granted, but never claimed, to the next waiter.
- `Server::queued_write_slot` serves snapshot, keyring, and replica replies. It waits until the poll
  deadline plus `limits::WRITE_QUEUE_GRACE` (5 s). The wait is clamped to seat authority and to the
  caller's credential. Clients allow `POLL_WAIT` + 10 s per turn (`src/control/session.rs:960`), so
  5 s are left to write. After the wait the server rechecks readiness, and a snapshot reply uses the
  newest image. Enrollment still uses fail-fast `write_slot`. `limits::MAX_WRITE_WAITERS` equals
  `MAX_CONTROL_CONNECTIONS`, so the deadline, not the queue length, turns sustained overload into
  429. The wire format is unchanged.
- `Signal::changed` returns an `exec::Changed` future that owns one entry in a waker slab. Register,
  re-register (`clone_from` only when `will_wake` is false), and drop are O(1). Notify takes the
  slab and wakes W entries, so a wake costs O(W). Dropped waiters no longer leave stale wakers until
  the next notify.
- Models: `step::cost::notify` now reports `registrations` (W) instead of `comparisons`.
  `step::cost::drain` models FIFO draining with a wait bound and a queue bound. `step::cluster`
  `serve_clients` uses `drain`. In the client simulator (`sim/src/control.rs`), `reply` admits
  through `SeatAdmission::writes`, `release_write` hands slots to queued polls, and expired waiters
  get 429 and back off. Each waiter's deadline is the end of its current `POLL_WAIT` cycle plus the
  grace. Seat loss drops queued polls along with the waiting ones. New counters are
  `ctl_write_queued` and `ctl_write_dequeued`, and `ctl_notify_comparisons` was renamed
  `ctl_notify_registrations`.

### Tests

- `exec`:
    - `signal_registration_is_one_entry_per_waiter`
    - `signal_dropped_waiters_free_their_entries`
    - `signal_wakes_many_waiters_in_linear_work` (2000 waiters, slab size stays W)
    - `slots_queue_in_order_and_hand_over`
    - `slots_wait_times_out_and_cancel_passes_slot_on`
    - `slots_granted_but_unclaimed_slot_moves_on`
- `server::long_poll_replies_queue_for_a_write_slot`: covers the queued grant, a deadline 429, a
  credential bound, readiness rechecked after the wait, and enrollment that still fails fast.
- `step::admission`: `write_queue_hands_slots_over_in_order` and `write_queue_trims_dead_waiters`.
- `step::cost`: `drain_queues_within_the_wait`.
- `step::cluster`: `mass_wake_queues_instead_of_429`, which counted 429s before this fix. The ignored
  50k test now asserts zero 429s.
- `racer-sim`: `mass_wake_queues_for_write_slots`, which would fail before because nothing queued,
  and `slow_writes_still_shed_with_429`, which covers overload protection.

### Evidence

All runs are release builds with seed 1. Before means base 01812bd3f, and after means this branch.

| Run | Write 429s | Queued (dequeued) | Notify work | Client lag p50 / p99 / max |
|-----|------------|-------------------|-------------|----------------------------|
| ctl-churn 5k, before | 30,417 | - | 21.5M comparisons | 3.2 s / 8.6 s / 10.3 s |
| ctl-churn 5k, after | 0 | 30,485 (30,485) | 32.6k registrations | 2.7 s / 8.6 s / 10.3 s |
| ctl-churn 50k, before | 454,295 | - | 548M comparisons | 4.3 s / 23.6 s / 49.5 s |
| ctl-churn 50k, after | 0 | 293,251 (293,251) | 304k registrations | 2.7 s / 23.6 s / 46.6 s |
| enrollment-storm 50k, before | 501 | - | 0 | 129 s / 258 s / 281.7 s |
| enrollment-storm 50k, after | 0 | 426 (426) | 0 | 129 s / 258 s / 284.4 s |
| full-cold-start 50k, before | 501 | - | 0 | 129 s / 258 s / 281.7 s |
| full-cold-start 50k, after | 0 | 426 (426) | 0 | 129 s / 258 s / 284.4 s |

- Lag percentiles are histogram bucket bounds.
- At 5k, enrollment-storm and full-cold-start had no write contention before or after.
- The 50k wall times were 3:26 (ctl-churn), 3:53 (enrollment-storm), and 27:53 (full-cold-start
  after; 29:10 before). The full-cold-start runs had two runs in parallel. Peak RSS was unchanged:
  0.89 GiB (ctl-churn), 0.93 GiB (enrollment-storm), and 6.6 GiB (full-cold-start).
- Enrollment-storm and full-cold-start lag is dominated by enrollment, which is bound by API 429s
  (about 462k, unchanged). Their all-enrolled time moved from 282.4 s to 285.1 s because the event
  schedule changed (no write backoffs). It is not a regression in write handling.
- Data-plane results of full-cold-start are not affected by this track.
- `step::cluster` `fifty_thousand_pods_cold_start` went from about 4.5M 429s to 0 with 86,749
  replies. Its worst lag stays 4.1 s, because the model's lag is dominated by seat follow intervals
  (`self.now - commit`), not by write rounds. The old 4.1 s figure was not caused by write slots, and
  the old model never added retry delays.

### Deviations and open items

- In the client simulator, a queued poll whose deadline passes learns its 429 at the next slot
  release or admission on that seat, not at its own timer. In production, its timer fires.
- Commit 75ee86562 changes `SeatAdmission::writes` before the simulator is updated in 0dde5abdd, so
  `racer-sim` builds only at the second commit. Cherry-pick the two commits together.
- New issues: none.

## Round 3: route

Known issues 3, 5, 7, and 10. One root runs through them: a failure that only concerns one owner
or one moment (a destination behind this node, a probe the remaining links cannot fund, a busy
search slot) ended the whole read with a terminal error instead of being treated as transient.

### Stale destinations (issue 3)

Root cause: `refresh_candidates` (`src/read/candidates.rs`, round 2 at about line 1203) and the
core `refresh_allowed` returned `IncompatibleMembership` when the local membership was not
strictly newer than the one used, or after the one refresh. `IncompatibleMembership` is not
`recoverable` (`core/src/read/policy.rs:9-22`), so the read failed although the other owners were
fine.

Fix (`core/src/read/walk.rs`): `stale_action(retried, previous, latest)` returns `Refresh` (strictly
newer and not yet retried, unchanged) or `Skip`. On `Skip` the caller completes the probe with the
new `Outcome::StaleDestination`: the walk records `Unreachable` evidence, sets the transient flag,
and moves to the next owner without charging a route failure (the route worked). An owner keeps
its origin authority with that evidence; `RemainingWalk` treats it as unavailable. No published
membership at all is still `Unavailable`. `refresh_allowed` was removed. The dataplane
(`CandidatePolicy::refresh_membership`, both the in-walk path and the stale hedge path, which now
walks the same epoch with bounded routes) and racer-sim (`NodeWorld::reply`, `hedge_result`) call
the same function. The wire format is unchanged.

### Local hop budget shortfall (issues 5 and 7)

Root cause: `plan_attempt` returns `HopBudgetExhausted` when the remaining links cannot fund the
next route (`core/src/read/walk.rs`, `route_links() == 0`), and `transport_outcome` did not map it,
so the dataplane (`src/read/candidates.rs`, the `transport_outcome` call in `resolve_epoch_traced`)
and the simulator abandoned the walk with a non-recoverable error. Overloaded relays (issue 5) and
links toward crashed members (issue 7) both spend links quickly because each failed route raises
the per-probe charge to 8.

Fix: `transport_outcome` maps `HopBudgetExhausted` to `ProbeOutcome::Unreachable`. The error is
local (no bytes were sent), so later probes that cannot be funded fail the same way without I/O;
the walk ends `Exhausted` (recoverable `Unavailable`, so the client retries within its own retry
limit) or, at an owner, with origin authority. Budgets are unchanged and never grow
(`budget_never_grows` holds in every run).

Issue 7 is only partly fixed: crashed members still stay in the membership and are routed through
until link backoff opens their circuits. The resulting failures are now recoverable `Unavailable`
after client retries (12.4k in `full-failover` at 50k, see below).

### Route search queue (issue 10)

Root cause: `Paths::route` (`topology/src/paths.rs`, round 2 line 530) failed any new search with
`Overloaded` when 8 searches were running.

Fix: `Paths::with_queue(limit)` keeps a FIFO of callers waiting for a slot. A waiting caller holds
no slot and does no work; it registers its waker and is woken when a search finishes or is
cancelled (`SharedSearch::poll`, `SearchAdmission::drop`) or when the caller ahead leaves the queue
(`QueueTicket::drop`). A free slot goes to the head of the queue; new callers do not bypass
waiting ones. Waiters still join a running search for the same key and take cache hits without a
slot. Dropping a waiting future (for example on its deadline; the dataplane's
`Paths::shortest_async` checks the request scope on every poll) leaves the queue. A full queue still
answers `Overloaded`, so overload protection stays bounded. `Paths::new`, used by the dataplane,
queues up to `DEFAULT_SEARCH_QUEUE` (64, eight per slot); `with_limits` keeps no queue.

racer-sim previously polled tasks with a no-op waker and rescheduled every slice. Tasks now get a
`TaskWaker`; a task that returns `Pending` without waking itself is parked (no CPU) until another
task wakes it (`route_search_queued`, `route_search_queue_wakes`). `node.search_queue` (default 64)
sets the queue; 0 restores rejection.

### Tests

- racer-core: `stale_destination_moves_to_the_next_owner`,
  `stale_destinations_everywhere_end_exhausted_or_at_origin`,
  `remaining_walk_treats_a_stale_destination_as_unavailable`, `spent_links_end_the_walk_exhausted`,
  and the extended `stale_membership_ends_and_returns_child_credits` and transport mapping test.
- topology: `queued_searches_wait_in_order_and_full_queue_rejects`,
  `queue_is_fair_cancellable_and_joins_running_searches`.
- dataplane: `stale_destination_without_newer_membership_tries_the_next_owner`,
  `all_stale_destinations_end_unavailable_after_one_probe_each`,
  `owner_with_stale_predecessor_reaches_origin`, `stale_answer_without_published_membership_is_unavailable`,
  `topology::tests::...::searches_beyond_the_slot_limit_wait_for_a_slot`. Changed expectations:
  `hedge_stale_membership_refreshes_once_without_fresh_credits` (a second stale answer now reaches
  the next owner) and `corrupt_copies_exhaust_sources_or_original_credits_without_origin_for_noncandidate`
  (4 links: `Unavailable` instead of `HopBudgetExhausted`).
- racer-sim: `requesters_ahead_of_their_destination_skip_it`, `queued_searches_wait_instead_of_failing`
  (`yielding_searches_hit_production_concurrency_limits` now sets `search_queue` 0). The cold start
  test (`contention_scenarios.rs`) now requires zero `HopBudgetExhausted` and at most 1% failures,
  all admission `Overloaded`. Before the fix it saw 851 of 6,400 (400 nodes) and 7,122 of 32,000
  (2,000 nodes) reads fail with `HopBudgetExhausted`; after, 0 and 1 (`Overloaded`).

### Evidence

Release build, seed 1. `storm` and `churn` with `--duration 1s` and
`{"objects": 256, "request_rate": 2.0}`; `full-*` with default durations and
`{"request_rate": 0.005}`. Read counts are the report's `reads_completed` / `reads_failed`.
The 50k `full-*` "before" rows are the round 2 table above (same base code); the others were rerun.

| Scenario | Nodes | Reads ok / failed before | after | Failures before | after |
| --- | --- | --- | --- | --- | --- |
| storm | 5,000 | 35,166 / 2,075 | 35,409 / 0 | 2,075 hop; 6,849 searches rejected | none; 3,403 searches queued |
| churn | 5,000 | 12,272 / 915 | 13,763 / 114 | 800 incompatible, 81 hop, 34 unavailable | 114 unavailable; 1,547 stale skips |
| full-cold-start | 5,000 | 92,738 / 2,141 | 94,798 / 103 | 2,104 hop, 37 unavailable; 6,613 rejected | 103 unavailable; 3,284 queued |
| full-failover | 5,000 | 33,544 / 348 | 33,934 / 116 | 305 hop, 43 unavailable | 116 unavailable |
| storm | 50,000 | 44,137 / 282,617 | 48,576 / 17 | 282,604 hop, 13 overloaded; 704,665 rejected | 4 unavailable, 13 overloaded; 446,186 queued |
| churn | 50,000 | 100,460 / 9,440 | 111,854 / 1,209 | 7,901 incompatible, 1,147 hop, 392 unavailable | 1,209 unavailable; 13,316 stale skips |
| full-cold-start | 50,000 | 153k / 735k | 180k / 708k | 502k hop, 230k deadline, 3.5k unavailable; 721k rejected | 696k deadline, 11.9k unavailable; 459k queued (Known issue 11) |
| full-failover | 50,000 | 263k / 18.4k | 274k / 12.4k | 14.7k hop, 3.7k unavailable | 12.4k unavailable |

All runs end with zero hard invariant failures and no relaxed invariant. Read latency p50 and p99
are unchanged in `churn` and `storm` at 50k (p99 738 ms, log buckets). Origin requests per page are
unchanged except as noted in Known issue 12. The 5k set ran in about 40 s wall (four runs in
parallel). At 50k, `full-cold-start` took 25:35 wall (round 2: 18:41) with 211M events and 10.1 GiB
peak RSS (6.2 GiB): woken queued searches and client retries of recoverable walks add events. `storm`
peak RSS rose from 4.9 to 6.6 GiB. `full-failover` link failures rose from 96k to 164k because walks
now continue past unreachable owners and retries probe again.

Deviation: the 50k `full-*` "before" numbers are taken from the round 2 table, not rerun.

## Round 3: origin

Track origin closes Known issues 4, 8 and 9 and partly addresses 6. Commits on branch
`racer-sim3-origin`: a27f1f7ac (issues 8, 9), afdd9c28a (origin-per-page histogram in reports),
ea016b68d (issue 4), 754c3a53d and 07a932713 (issue 6, simulator and production), 27f117aa7
(a stale handoff target is a miss), plus a clippy fix.

Issue 8. Production `Flights::fail(OriginRejected | OriginForbidden)` cancels the supplying caller
before `Outcome::Retry` (`src/read/flight.rs`, `fail`). That is the intended behavior: the rejection
is about that caller's credentials, and serving it a page another caller fetched would bypass the
origin's decision. Core gained `PageFlights::reject` (`core/src/read/flight.rs`), which does the same.
`read::core_equivalence` now drives both sides through `reject` and compares every poll
(`origin_rejection_fails_only_the_rejected_caller`). The simulator settles a leader with the shared
`racer_core::read::fill::acquisition_failure` mapping, as production `Fill` does, and a re-elected
waiter now leads its own fill (`node::tests::origin_forbidden_reelects_a_waiter`). Waiters keep their
budget for that (`Waiting::budget`).

Issue 9. `memory_insert` (`sim/src/node/world.rs`) no longer touches a resident page.
`read::core_equivalence::duplicate_publish_does_not_touch` asserts both sides evict the oldest
insert, the seeded LRU trace test now includes duplicate publishes, and
`node::tests::duplicate_memory_publish_does_not_touch` drives the simulator's own function.

Issue 4. Root cause: an owner at rank r probed ranks 0..r copy-only and then fetched itself
(`CandidateWalk::new` limited the walk to the predecessors, and `next` made every owner probe
`CopyOnly`), so each owner a reader reached first could fetch. Fix, in `racer_core::read::walk`:

- An owner above rank 0 sends an `Acquire` to rank 0 over a funded route that carries exactly the
  exchange and one origin credit. Rank 0 fetches (or joins its own flight), so concurrent misses at
  different owners coalesce there. The owner does this only when it keeps one credit per later probe
  and one for its own origin attempt; otherwise it probes copy-only as before. If rank 0 answers
  `VersionUnavailable`, the walk ends there instead of asking origin again.
- `owner_funding(r)` is one credit more than before for r > 0 (`owner_funding_min`). A non-owner pays
  it only from slack: the funded visits after it keep their minimum (`later_minimum`), so a walk's
  reach does not shrink. Page attempts stay at 8. Hedge eligibility is unchanged.
- No wire change: the Acquire is an existing page request.

Issue 6. An owner ranks the page under membership version - 1 when that publication is still
retained (`CandidatePolicy::handoff_targets`, `Published::resolve`; in the simulator
`Versions::get(version - 1)`). `racer_core::read::walk::handoff_targets` keeps the previous owners
that are current members, not this node and not a current predecessor, and is empty when the owner
set did not change, so a steady cold miss pays nothing. The walk asks them copy-only after the
predecessors (`WalkConfig::handoff`; probe canonical `owners + i`), only while one credit for origin
remains. Their outcomes are not predecessor evidence, and a stale reply is only a miss.

Tests: core walk tests for delegation, funding, slack, handoff gating and targets;
`read::tests::peer_copies::owner_asks_previous_membership_owners_before_origin` (copy found, all
miss, previous version not retained); the mesh test
`replica_selection_orders_first_acquire_and_non_primary_owner_fills` now checks that rank 0 alone
fetches. Seven scripted production tests changed their expected credits or probe modes for the
delegated Acquire. Simulator: `single_reader_then_storm_reuses_copies` now requires exactly one origin
request per page (33 before, 16 now), and `joined_owners_copy_from_previous_owners` requires one per
page after a join (19 without handoff).

Evidence, release, seed 1. 5,000 nodes, scenario defaults; `full-churn` with `request_rate` 0.005.
Origin requests per page: total, max, pages fetched more than once, histogram.

| scenario | before | after |
| --- | --- | --- |
| storm 5k | 2,630, max 3, 922 dup (1:102 2:238 3:684) | 1,196, max 2, 172 dup (1:852 2:172) |
| zipf 5k | 60,514, max 3, 16,108 dup | 42,607, max 3, 3,907 dup (3: 74 pages) |
| churn 5k | 2,638, max 4, 929 dup | 1,196, max 3, 171 dup |
| full-churn 5k | 15,673, max 7, 2,280 dup | 14,926, max 10, 1,785 dup |
| full-churn 50k | 75,533, max 10, 20,131 dup | 75,386, max 13, 18,455 dup |

Read latency (p50, p99 of `read_ns`): storm 268/1342 ms before, 302/1342 ms after; zipf 201/1611 ms
before, 168/1476 ms after; churn 470/738 ms before, 436/738 ms after. A cold miss at a non-primary
owner now takes one funded exchange to rank 0 instead of a copy-only probe followed by a local origin
fetch, so its latency is about the same; warm reads are unaffected. Failed reads: storm 363 to 300,
zipf 1,588 to 2,132, churn 15.5k to 15.5k, full-churn 50k 4,966 to 5,234. The 50k `full-churn` run
took about 34 minutes wall instead of 24, mostly the extra ranking under the previous membership.

What remains (issue 6 stays open, and new issue 13):

- Under churn, the total barely moves: in `full-churn` at 50k, single-fetch pages went from 25.8k to
  27.4k, but the tail grew (29 pages fetched 10 or more times, max 13). Traces at 5k show most repeat
  fetches come from owners left with one credit after delegating (handoff needs two), and from owners
  at rank 1 or 2 whose delegation to a new rank 0 failed. A rank 0 reached through delegation has
  only one credit, so it never asks previous owners. Giving it more is not free: remote credits stay
  spent. Not tuned further in this round.
- In visit orders where a non-owner cannot fund delegation from slack (for example [1, 2, 0] when
  rank 2's minimum must stay fundable), rank 1 still fetches itself; the result can be one extra
  origin request per page per membership version.

13. Repeat origin fetches across membership versions have a heavier tail with handoff
    probes than without in `full-churn` at 50,000 nodes (max 13 vs 10), though fewer pages are fetched
    more than once. Root cause not established; see "Round 3: origin". No invariant is relaxed. Resolved in round 4;
    see "Round 4: handoff".

## Round 3: combined

The ctl, route, and origin tracks were merged in that order. The route track's stale-destination
skip and the origin track's handoff probes meet in one place: a `StaleMembership` answer from a
handoff target (a previous-membership owner) is only a miss. Callers check
`CandidateWalk::pending_handoff` before `stale_action`, so such an answer never refreshes
membership, never adopts a version in the simulator, and never counts as a stale-destination skip.
`Outcome::StaleDestination` from a handoff target is also only a miss
(`pending_handoff_marks_handoff_probes_and_stale_destination_is_a_miss`). The 400-node cold start
test (`cold_start_completes_with_bounded_origin_traffic`, seed 8) now also accepts a recoverable
`Unavailable` after the client's retries (1 of 6,400 reads with both tracks); hop budget failures
stay at zero, and failures stay at or below 1 in 100.

Known issue 12 is not changed by the handoff probes. At 5,000 nodes, `churn` with the route
settings still has a maximum of 4 origin requests per page. An owner whose predecessor is behind
usually has the same owner set under the previous version, so `handoff_targets` is empty and there
is no one to ask before origin.

Evidence, release build, seed 1, 5,000 nodes. The "-r" rows use the route track settings (`--duration
1s`, `{"objects": 256, "request_rate": 2.0}`). The other rows use scenario defaults, and `full-*` use
`{"request_rate": 0.005}`. There were no hop budget failures, no write 429s (`client_429s`), and no
hard invariant failures in any run. Four runs ran in parallel.

| Scenario | Reads ok / failed | Origin total, max, dup | Track "after" | Wall | Peak RSS |
| --- | --- | --- | --- | --- | --- |
| storm-r | 36,724 / 0 | 2,478, 2, 136 | route 35,409 / 0 | 0:27 | 0.56 GiB |
| churn-r | 13,811 / 113 unavailable | 2,835, 4, 256 | route 13,763 / 114 | 0:12 | 0.16 GiB |
| storm | 171,391 / 0 | 1,196, 2, 172 | origin 1,196, 2, 172 | 1:04 | 1.22 GiB |
| churn | 167,231 / 1,479 (1,478 unavailable, 1 overloaded) | 1,220, 3, 191 | origin 1,196, 3, 171 | 1:07 | 0.94 GiB |
| zipf | 147,684 / 11 unavailable | 42,755, 3, 3,935 | origin 42,607, 3, 3,907 | 1:54 | 1.42 GiB |
| full-cold-start | 94,805 / 99 unavailable | 7,014, 3, 203 | route 94,798 / 103 | 0:34 | 0.63 GiB |
| full-failover | 33,924 / 122 unavailable | 13,421, 7, 722 | route 33,934 / 116 | 0:29 | 0.31 GiB |
| full-churn | 34,196 / 76 unavailable | 14,784, 9, 1,699 | origin 14,926, 10, 1,785 | 0:43 | 0.28 GiB |

`full-churn` queued 30,485 long poll replies for write slots and answered none with 429, which
matches the ctl track's `ctl-churn` count. The combined numbers stay within a few percent of each
track's own results.

## Round 4: enroll

This track fixes the control-plane half of Known issue 11: enrollment was limited by API server
429s. The data-plane half (reads failing on the read deadline in the 50k `full-cold-start` storm)
belongs to the capacity track and is not addressed here.

### Root cause

Line numbers are from base 48ca50329.

- Every bearer authorization made five API calls: a TokenReview POST
  (`cluster-control/src/auth.rs:129`), then GETs of the Pod, the DaemonSet (`:161`), the
  ServiceAccount (`:179`), and the Node (`:190`). The DaemonSet and ServiceAccount are the same two
  objects for every caller. Nothing was reused between authorizations, including for the same token.
- An unenrolled client authorizes three times before it holds a certificate. `start_inner`
  (`src/control/session.rs:611`) runs the bearer keyring feed until it has a bundle, then enrolls
  (`:631`). The keyring handler authorizes on entry (`server.rs:707`) and again before data leaves
  (`server.rs:735`), even when the data was already there and nothing was waited for. The bootstrap
  authorizes a third time (`server.rs:557`). That is 15 API calls per enrollment.
- Nothing bounded the rate of these calls except the 32 bootstrap slots per seat
  (`limits::MAX_BOOTSTRAPS`). Each slot is held only for the calls' latency, so 16 seats could
  start several thousand authorizations per second against an API server that admits about 1,000.
  A 429 part way through an authorization wasted the calls already made, and it was reported to the
  client as `Unavailable` (`kube/mod.rs:54` maps 429 to `KubeError::Unavailable`, and `auth.rs:65`
  maps that to `ProtocolFailure::Unavailable`). Seats kept the same rate after a 429.
- The simulator modeled only the bootstrap authorization (`sim/src/control.rs:284`
  `API_CALLS_PER_ENROLLMENT = 5`, `enroll` at `:802`). It undercounted production's API calls per
  enrollment by a factor of three.

### Changes

- `step::authz` (new, sans-IO, used by `auth.rs` and racer-sim) adds the following:
  - `AuthCache`, keyed by the SHA-256 of the bearer token. It holds a successful TokenReview for
    `REVIEW_TTL` (10 s), a whole successful authorization for `CALLER_TTL` (5 s), and the DaemonSet
    and ServiceAccount UIDs (one entry shared by all callers) for `BINDINGS_TTL` (5 s). No entry
    outlives the token's `exp`. Failures are never cached. A refusal drops the token's entries, and
    a refusal that may come from stale bindings drops the bindings too. Each map holds at most 8,192
    entries: expired entries are purged first, then the entry closest to expiry is evicted.
  - `ApiBudget`, a per-process token bucket for authorization calls (`limits::AUTH_API_QPS` 400,
    burst 100):
    - An authorization reserves all of its calls before the first call. It may wait up to
      `limits::AUTH_BUDGET_WAIT` (2 s), holding its bootstrap slot. Beyond that it answers
      `Overloaded` without calling the API.
    - The rate is additive increase, multiplicative decrease. An API 429 halves it, at most once per
      second, and drops the saved burst. The rate never falls below 1/16 of the ceiling and recovers
      linearly over 30 s.
    - With 16 seats sharing an API server without coordinating, the 429 feedback, not the ceiling,
      sets the rate in a storm. The ceiling is high so that a 3-seat cluster is not held below what
      its API server admits.
- `auth::authorize` uses the caller cache first, then reserves budget, then makes only the calls
  not cached. The Pod and Node are read live unless the whole authorization is cached.
  `authorize_seat_caller` reuses cached reviews.
- `KubeError::Throttled` (new) maps 429. An authorization that hits it slows the budget and answers
  `Overloaded`, a 429 with `Retry-After: 1`. Before, it answered `Unavailable`.
- The keyring handler skips the bearer recheck when the bundle was already there before waiting
  (`server::bearer_recheck`). The authorization made a moment ago is current. A recheck after a
  real wait still runs, and after `POLL_WAIT` (30 s) it misses `CALLER_TTL`, so it reads live.
- Per enrollment, the API calls drop from 15 to 3 on a warm seat: the TokenReview, Pod, and Node for
  the keyring poll. The bootstrap hits the caller cache. The bindings are shared.
- racer-sim:
  - `control::Settings::auth` selects the model. `"legacy"` is the production behavior before this
    round: three full authorizations, no cache, no budget. `"cached"` (the default) uses
    `step::authz` for a keyring authorization and then a bootstrap authorization.
  - Each seat holder process has its own cache and budget. They are reset when the seat changes
    holder.
  - New counters are `ctl_auth_overloaded`, `ctl_auth_waited`, `ctl_auth_caller_hits`, and the
    histogram `ctl_auth_wait_ns`.
  - The convergence bound uses each model's worst case for API calls per enrollment (15 legacy, 10
    cached).
- No wire format change. Clients already floor their backoff at `Retry-After`
  (`controlplane/src/feed.rs:692`, `src/control/session.rs:541`), so no client change was needed.

### Why the caches are safe

- **TokenReview, 10 s.** kube-apiserver caches successful token authentication for the same 10 s.
  For a bound token, the review says only that the signature is valid and that the bound objects
  existed. The bound objects are checked live by UID. Deleting the ServiceAccount is seen through
  the bindings within 5 s.
- **DaemonSet and ServiceAccount UIDs, 5 s.** Operators rarely change these objects. A replaced or
  deleting DaemonSet or ServiceAccount is refused at most 5 s later. A refusal that may come from
  stale bindings drops them, so the next attempt reads them live.
- **Whole authorization, 5 s.** Deleting a Pod or Node, or replacing a Pod, is seen within 5 s
  instead of at once. That is the same order as the bindings TTL and short against the issued
  certificate's lifetime. The primary removes a deleted Pod's Node from membership through its own
  watch regardless. The test `authorization_caches_review_and_bindings_under_budget` pins this
  window, showing that a replaced Pod is accepted within it and refused after it.
- Cache keys are SHA-256 of the token, so the raw token is not stored as a key. Failed reviews and
  refusals are never cached, so an attacker cannot poison the cache.

### Tests

- `step::authz`:
  - `review_ttl_is_capped_by_token_expiry`
  - `whole_authorizations_are_reused_briefly`
  - `distinct_tokens_do_not_share_reviews`
  - `bindings_expire_and_drop`
  - `review_cache_is_bounded`
  - `budget_reserves_whole_authorizations`
  - `budget_reservations_wait_up_to_a_bound`
  - `budget_backs_off_on_throttling_and_recovers`
- `auth::authorization_caches_review_and_bindings_under_budget` runs against the fake API with
  request counts:
  - The first authorization makes 5 calls. A repeat within `CALLER_TTL` makes 0.
  - After `CALLER_TTL`, an authorization makes 2 calls (Pod and Node).
  - A replaced Pod is refused, and its review is dropped. A deleted Pod is refused.
  - Failed reviews are not cached.
  - An API 429 answers `Overloaded` and halves the budget.
  - Out of budget, it answers `Overloaded` with no API call.
- `auth::live_bindings_are_all_checked` now clears the cache between cases, which models the TTL
  passing.
- `kube` maps 429 to `Throttled` and 503 to `Unavailable`.
- `server::bearer_keyring_rechecks_only_after_a_wait`.
- racer-sim `control::cached_authorization_cuts_api_calls_and_429s` runs a 3,000-Pod storm against
  a 300 qps API server. Legacy makes at least 15 calls and 3 TokenReviews per enrollment. Cached
  makes at most 4 calls per enrollment and fewer than 2 TokenReviews. Cached has under a tenth of
  the 429s and enrolls everyone sooner. The test fails with the base model.
- racer-sim `control::auth_budget_sheds_locally_and_everyone_enrolls`: with a tight budget, seats
  refuse locally (`ctl_auth_overloaded` > 0) with zero API 429s, and every client enrolls.

### Evidence

All runs are release builds with seed 1. "Base" is the base binary, which models one
authorization per enrollment. "Legacy" is this branch with `{"control": {"auth": "legacy"}}`, a
faithful model of production before this round. "After" is this branch. `full-cold-start` uses
`{"request_rate": 0.005}`. API calls counts successful TokenReviews plus GETs.

| Run | All enrolled | Enroll p50 / p99 | API 429s | API calls (TokenReviews) | Local refusals |
| --- | --- | --- | --- | --- | --- |
| enrollment-storm 5k, base | 32.3 s | 16 s / 30 s | 7,932 | 25,734 (5,332) | - |
| enrollment-storm 5k, legacy | 106.2 s | 43 s / 95 s | 16,114 | 75,833 (15,350) | - |
| enrollment-storm 5k, after | 31.4 s | 11 s / 28 s | 65 | 15,036 (5,000) | 0 |
| enrollment-storm 50k, base (round 3) | 282 s | 129 s / 258 s | 462k | not reported (58k) | - |
| enrollment-storm 50k, legacy | not within 600 s (37,598 of 50,000) | 309 s / 550 s | 1,023,547 | 600,712 (123,771) | - |
| enrollment-storm 50k, after | 170.6 s | 69 s / 137 s | 23,532 | 151,443 (50,254) | 125,906 |
| full-cold-start 5k, base | 32.3 s | 16 s / 30 s | 7,932 | 25,734 (5,332) | - |
| full-cold-start 5k, legacy | 106.2 s | 43 s / 95 s | 16,114 | 75,833 (15,350) | - |
| full-cold-start 5k, after | 31.4 s | 11 s / 28 s | 65 | 15,036 (5,000) | 0 |
| full-cold-start 50k, base (round 3) | 282 s | 129 s / 258 s | 462k | not reported (58k) | - |
| full-cold-start 50k, after | 170.6 s | 69 s / 137 s | 23,532 | 151,443 (50,254) | 125,906 |

- Enrollment percentiles are histogram bucket bounds.
- Data-plane reads in `full-cold-start` at 5k:
  - base: 94,805 ok and 99 failed, with 434 reads skipped before enrollment
  - legacy: 93,414 ok and 351 failed, with 1,136 skipped
  - after: 95,053 ok and 51 failed, with 321 skipped
- Data-plane reads in `full-cold-start` at 50k:
  - after: 213,609 ok and 700,096 failed, with 18,959 skipped before enrollment
  - round 3: 180k ok and 708k failed
  - Most reads still fail, which is the data-plane half of Known issue 11 (capacity track).
  - The run took 25:07 wall with a peak RSS of 10.3 GiB.
- "Local refusals" counts `ctl_auth_overloaded`. Bootstrap slot refusals (`ctl_bootstrap_overloaded`)
  rise because authorizations now hold their slot while they wait for budget: 5,344 at 5k after,
  against 4 at base.
- No run had hard invariant failures.
- At 5k only 3 seats exist (`MIN_AUTO_SEATS`), and the all-enrolled time is bound by seat readiness
  and client backoff, not API calls. The after time matches base, and the 429s nearly vanish.
- At 50k, the time is bound by the API server's 1,000 qps. Fewer calls per enrollment is what moves
  it.
- The remaining 50k 429s come from the first second of each seat's burst and from the additive
  increase probing back up. That is about 0.47 per enrollment, against 9.2 before.
- A 1,000 qps ceiling was also tried. At 50k it gave 106k 429s and 268 s, without the caller
  cache. At 400 qps without the caller cache, the 50k numbers were 30k 429s and 280 s. 400 was kept.

### Deviations and open items

- **The caller cache adds a revocation window.** Deleting a Pod or Node is seen within
  `CALLER_TTL` (5 s), not at once. This is a deliberate trade, documented in `step::authz` and
  pinned by a test.
- **The budget ceiling is a per-process constant, not configuration.** `AUTH_API_QPS` 400 matches
  the simulated API server well. A cluster whose API server admits far less relies on the 429
  feedback.
- **The simulator models the bootstrap right after the keyring poll on the same seat.** In
  production the client prepares a CSR in between, which is well within `CALLER_TTL`. A bootstrap
  that lands on another seat pays 3 to 5 calls there.
- **This round changes enrollment timing for the capacity track's `full-cold-start` runs.** Nodes
  enroll sooner at 50k, so reads start earlier in the storm.
- New issues: none.

## Round 4: liveness

Known issue 7: crashed members stayed in the membership and routable.

### Root cause

- `members::reconcile` (`cluster-control/src/authority/members.rs`, the loop after
  `if !node.authorized()`) admitted every authorized Node with a selected Pod, and kept the
  previous member when the Pod was missing (`previous.map(|p| p.peer_endpoint.clone())`, line
  393). No input said whether the node was alive, so a dead node stayed a member for as long as
  its Node and Pod objects existed, which for a crashed node is indefinitely.
- The step model matched: `ControlPlane::crash` (`cluster-control/src/step/cluster.rs`) left the
  objects untouched and nothing ever changed them, so `plan_pass` kept the member forever.
- Measured with new sim metrics (`dead_member_links`, `dead_member_link_age_s` in
  `sim/src/node/world.rs:2370`), at 50,000 nodes in `full-failover` the permanently crashed Pod
  was still chosen 370 s after its crash, at the end of the run. In a longer run there is no
  bound.
- The scale of the read failures blamed on issue 7 was wrong. Before the fix, the same run had
  13 link attempts toward crashed members and 0 failed reads while any member was down
  (`reads_failed_while_crashed`, `sim/src/node/world.rs:1258`). The 12,588 failed reads happen
  mostly between 60 and 200 s (`read_failed_at_s` p50 120 s, p90 192 s). Of the 165,343 link
  failures, 20,108 cross the site partition (`partition_link_failures`) and 13 reach crashed
  members. The rest, about 145k, reach members whose control client has not applied a membership
  yet, so they do not accept peer links (new issue 14). The round 1 and round 3 text attributed
  the failures to crashes without measuring this.

### Liveness signal

The fix uses the Node `Ready` condition, which is the kubelet heartbeat as judged by the node
lifecycle controller. It goes `Unknown` after `node-monitor-grace-period` (40 s by default) of
missed heartbeats. The other candidates were rejected:

- Pod readiness: the deployed DaemonSet is not in this repo and has no probe here, and
  `/readyz` requires `membership_usable` (`src/telemetry.rs:1205`). An operator-added readiness
  probe would therefore keep a new Pod out of the membership forever.
- The reporter annotation: it is written rarely and has no freshness stamp.
- Seat and peer reports: they would need a new wire field and do not exist today.

### Changes

- Production, `cluster-control/src/authority/members.rs`:
  - `NodeInfo` gains `ready: Option<Readiness>`, parsed from `status.conditions[type=Ready]`
    (`Readiness::from_json`, with a small RFC 3339 parser, `parse_rfc3339`, line 74).
  - `reconcile_at` (line 331) takes the wall time and leaves out a Node whose `Ready` condition
    has been other than `True` for `NOT_READY_GRACE_SECS` = 30 s (line 362), recording the
    diagnostic `ready`. This applies even when the node was a member.
  - `Readiness::dead` (line 63) needs positive evidence: a missing or unparsable transition
    time, or one in the future, never drops a member. A Node with no `Ready` condition is kept.
  - A not-ready Node inside the grace still joins, so readiness never delays enrollment.
- `plan_pass` (`cluster-control/src/step/primary.rs:131`), which production also uses, calls
  `reconcile_at` with `wall_unix`. Passes run at least every `LEASE_RENEW` (10 s), so a drop
  needs no extra trigger. There is no wire format change.
- Hysteresis and change rate: a member leaves only after 40 s of missed heartbeats plus 30 s of
  grace, so a Pod that restarts within about 70 s keeps its place and its pages without
  membership churn. Each node therefore adds at most one drop per 70 s of downtime, and the
  primary batches all drops of one pass into one version. A node returns on the first pass after
  its Node is `Ready` again.
- Step model, `cluster-control/src/step/cluster.rs`:
  - `crash` schedules `Ev::NodeNotReady` after `node_monitor_grace` (line 364, default 40 s).
    The event is handled for dead Pods and cancelled by the epoch bump on restart.
  - `restart` sets the Node `Ready` again. A crash in the combined model is a whole-node crash.
- racer-sim, `sim/src/control.rs`: new control key `node_monitor_grace_ns` (default 40 s). Setting
  it to 0 restores the pre-round-4 model exactly: the `full-failover` 5k trace hash is
  0fedb04dad20626a either way.

### Tests

- `authority::members` tests:
  - `rfc3339_parsing`
  - `node_ready_condition_parsing`
  - `readiness_needs_grace_and_a_time` (boundary, future stamps, overflow, missing time)
  - `dead_nodes_leave_and_return` (kept inside the grace, dropped after it even with history,
    unchanged without a time, back with the same attributes)
  - `not_ready_within_grace_still_joins`
- `step::cluster` tests:
  - `crashed_node_leaves_membership_after_grace_and_returns`: kept for 65 s, dropped by 85 s in
    exactly one version, back after restart.
  - `quick_restart_keeps_membership`: a 30 s outage causes no version change.
- `combined::tests::failover_drops_the_dead_member` checks that the oldest link toward a crashed
  member is at most 90 s after the crash, and that a second membership was published. With
  `node_monitor_grace_ns` 0 (the old model) it fails: 214 links, oldest at 143 s.

### Evidence

Release build, seed 1, `full-failover`, `--config '{"request_rate": 0.005}'`. "Off" is the same
binary with `node_monitor_grace_ns` 0, which reproduces the pre-fix model. No run had a hard
invariant failure.

| Nodes | Model | Dead-member links (oldest) | Reads ok / failed | Failed while crashed | Origin requests (max per page) | Wall |
| --- | --- | --- | --- | --- | --- | --- |
| 5,000 | off | 11 (388 s) | 33,924 / 122 | 0 | 13,421 (7) | 0:29 |
| 5,000 | on | 5 (72 s) | 33,917 / 127 | 5 | 13,432 (7) | 0:29 |
| 50,000 | off | 13 (370 s) | 273,215 / 12,588 | 0 | 57,221 (7) | 14:26 |
| 50,000 | on | 6 (76 s) | 272,950 / 12,759 | 171 | 57,163 (9) | 21:53 |

Reading the table:

- With the fix, crashed members stop being chosen within 76 s of the crash at both scales.
  Before, they were chosen until the end of the run.
- The cost is one extra membership version per node that stays dead. At 50k that version caused
  1,859 `StaleMembership` replies, 150 origin fetches on an older membership, and 171 more
  recoverable `Unavailable` reads (0.06%). The worst page moved from 7 to 9 origin requests
  (pages fetched more than once: 12,526 to 12,531). This is the ownership-move cost of Known
  issues 6 and 12 (handoff track), not a liveness cost.
- Wall time rose with the second version's placement ranking.

Resolved bound: a crashed member is chosen for at most `node-monitor-grace-period` (40 s) +
`NOT_READY_GRACE_SECS` (30 s) + one pass (10 s) + client propagation, which is under 90 s in
the model. `failover_drops_the_dead_member` enforces this. Within that window each holder fails
toward it at most once per link backoff (100 ms doubling to 20 s), as before.

### Deviations and open items

- The data-plane side (`LinkHealth`, ranked owner skip) is unchanged. The measurements show
  crashed members are not a significant source of failed reads at 5k or 50k, so there was
  nothing to speed up there.
- A Pod that crash-loops while its kubelet is healthy stays a member, because Pod readiness is
  deliberately not used. Its peers' link backoff covers it.
- New issue 14: about 145k of 165k link failures in 50k `full-failover` go to members whose
  control client has not applied any membership yet, during enrollment and after restarts. They
  coincide with the bulk of the failed reads (60 to 200 s). This is an inference from the
  counters; the link failure is not yet tagged per read. Either membership should not include
  members that cannot serve, or such members should accept links. It overlaps with the enroll
  and capacity tracks.
- New issue 15: dropping a dead member is an ownership move, so it inherits the issue 6 and 12
  origin and `StaleMembership` costs (171 recoverable failed reads at 50k). The handoff track
  fixes apply.

## Round 4: capacity

Scope: the data-plane half of Known issue 11 (the 50k `full-cold-start` storm fails most reads on
the read deadline after retries). The control-plane half (API-server 429s during enrollment) is the
enroll track's.

### Root cause

Two causes, one a simulator modeling artifact and one a real capacity bound of the design.

1. The simulator streamed page bodies to requesters that had already given up. A destination's
   page reply reserved NIC time on every link of the reverse route (`Fabric::transfer`,
   `sim/src/resources.rs`) no matter when it would arrive, and `send_reply` then scheduled the
   `Reply` event; when the requester's attempt had timed out (`attempt_timeout_ns`, 2 s) the reply
   only counted as `late_replies` (`reply`, `sim/src/node/world.rs`). The NIC time was spent anyway,
   so a hot owner's transmit queue (`tx_free`) grew without bound and every later reply queued behind
   bodies nobody was waiting for. In the round 3 run, 3.49M probes timed out and the same 3.49M
   replies arrived late, while peers moved 11.2 TB for 180k completed reads.
   Production does not do this: the requester drops the exchange at its attempt deadline, relays
   cancel the downstream exchange on upstream close (`disconnect_fenced_exchange`,
   `src/peer/server.rs:904`), and the destination's writes then fail. The simulator also released a
   relay's `Relay` reservation when the reply was scheduled, while production keeps it on the
   connection until the body has streamed through (`src/peer/server.rs:444`), so relays never
   rejected (`rejections.relay` 0).
2. Hot-page throughput is capped by the page's owners. Candidates are the placement-ranked owners
   only (`CandidatePolicy::candidates`, `src/read/candidates.rs:628-635`; the simulator's
   `walk_ranked` uses the same ranking, `topology/src/placement.rs:827`, up to `REPLICAS` = 3), and
   a reader that holds a copy is never asked by other readers. A warm page can therefore be sent at
   no more than `REPLICAS * nic_bps`. Object 0 has 16 pages on (about) 48 distinct owners, so the
   storm needs at least `nodes * page_bytes / (REPLICAS * nic_bps)` of owner egress: 14.0 s at 50,000
   nodes and 10 Gbit/s, against a 1 s arrival window and a 10 s read deadline. The 1 s window was 14
   times over the bound, so the scenario measured overload rather than the system. At 5,000 nodes the
   same bound is 1.4 s, which is why the 5k run was clean.

The control-plane cause does not contribute: all 50,000 nodes are enrolled at 282 s, before the
storm at 300 s, and reads issued by unenrolled nodes are skipped (`reads_skipped_not_enrolled`).
Retries did not add bytes once cause 1 was fixed (a retried probe that is abandoned sends nothing),
and search queueing and flight admission had no rejections.

### Changes

- `Fabric::transfer_until` (`sim/src/resources.rs`): no chunk starts on any link at or after the
  requester's abandon time; chunks already started keep their reservation. `transfer` is
  `transfer_until` with no cutoff.
- `Msg::abandon` is the requester's attempt end (`AttemptPlan::attempt_end`). `send_reply` streams a
  page body with `transfer_until` and counts `replies_abandoned` instead of a late reply; `hop`
  drops a request that reaches a node after its requester gave up (`requests_abandoned`). Relays hold
  their `Relay` reservation until the body has passed or was abandoned (`Event::Release`).
- `combined::cold_start_window_ns`: the `full-cold-start` storm's arrival window is twice the owner
  egress bound, at least 1 s (28 s at 50,000 nodes and 10 Gbit/s; 1 s up to about 3,500 nodes).
  The overloaded 1 s storm stays available through `storms` in `--config`.
- No production change: production already drops abandoned exchanges. The remaining limit is a
  design bound (below), not a bug.

### Bound (residual, enforced)

During a storm on one warm page, peer deliveries over a span `T` are at most
`REPLICAS * nic_bps * T / page_bytes` (owners only; non-owner copies are never candidates). With
abandoned exchanges dropped, the owners now deliver close to that bound instead of wasting it.
`contention_scenarios::overloaded_hot_page_owners_do_not_stream_to_abandoned_requesters`
(96 nodes, 4 MiB/s NICs, seed 5) requires no late replies and at least half of the bound over the
10 s read deadline. Before the fix it saw 375 late replies and 16 peer fills (19 of 97 reads ok);
after, 0 late replies and 86 peer fills (bound 120; 89 of 97 reads ok). Raising hot-page
throughput beyond `REPLICAS` owners (readers serving readers) would be a design change and is
recorded as Known issue 16.

### Tests

- `resources::tests::abandoned_transfer_stops_sending_at_the_cutoff`.
- `contention_scenarios::overloaded_hot_page_owners_do_not_stream_to_abandoned_requesters`
  (fails before the fix).
- `combined::tests::cold_start_window_fits_owner_egress`.

### Evidence

Release build, seed 1, `full-cold-start` with default duration. "before" is the base (48ca50329),
"fix 1" adds only the abandoned-exchange change, "after" is the branch head. `{"request_rate":
0.005}` unless noted; "1G" also sets `nic_bps` to 125,000,000, which puts the 5k storm at the same
14-times overload as 50k at 10 Gbit/s. Read latency is log-bucket upper bounds.

| Run | Reads ok / failed | Failures | Late replies | Peer bytes | Read p50 / p99 |
| --- | --- | --- | --- | --- | --- |
| 5k before | 94,805 / 99 | 99 unavailable | 0 | 246 GB | 470 ms / 1.34 s |
| 5k after | 94,810 / 98 | 98 unavailable | 0 | 246 GB | 46 ms / 134 ms |
| 5k 1G before | 23,714 / 70,268 | 70,171 deadline, 97 unavailable | 352,820 | 960 GB | 151 ms / 1.88 s |
| 5k 1G fix 1 | 52,116 / 42,785 | 22,845 deadline, 19,940 unavailable | 0 (212,612 abandoned) | 138 GB | 1.74 s / 9.66 s |
| 5k 1G after | 94,799 / 102 | 102 unavailable | 0 | 246 GB | 84 ms / 201 ms |
| 50k before | 178,986 / 709,178 | 697,451 deadline, 11,727 unavailable | 3,497,446 | 11.27 TB | 537 ms / 3.22 s |
| 50k after | 884,120 / 11,753 | 11,753 unavailable | 0 | 2.71 TB | 55 ms / 235 ms |
| 50k after, 1 s storm | 261,113 / 634,751 | 634,751 unavailable | 0 (35 abandoned) | 0.81 TB | 738 ms / 2.01 s |

- The storm alone completes: 5k with `{"request_rate": 0}` reads 80,000 of 80,000 pages. The
  remaining ~100 (5k) and 11.7k (50k) failures are background Zipf reads, unchanged by this track
  and present before it; see Known issue 17.
- At 50k, origin requests per page fell from mean 1.60 to 1.22 (max 3 both). All enrolled at 285 s
  in all three 50k runs (the storm starts at 300 s). Zero hard invariant failures, no relaxed
  invariant.
- Overloaded configuration (1 s storm, 14 times over the owner bound): no read fails on the
  deadline any more. Relays shed with `Overloaded` on the `Relay` quota (11.4M rejections) because
  they hold it while bodies stream, walks end `Exhausted`, and reads fail `Unavailable` after the
  client's retries; successful reads finish within 2 s (p99). The successes stay below the owner
  bound (about 177k storm pages against a 10 s bound of about 570k): shedding happens at the relays
  in front of the owners, so an owner's NIC is not kept saturated (Known issue 16).
- 50k wall and peak RSS: before 24:43, 10.3 GiB; after 14:55, 4.1 GiB; 1 s storm 26:26. The
  liveness track's 50k run shared the host.

### Deviations and open items

- No production change; the failing behavior was a simulator artifact plus an overloaded scenario.
  The brief's "fail fast with `Overloaded`" happens at relays (production's held `Relay`
  reservation), and the client-visible error is `Unavailable` after the walk is exhausted.
- Known issue 16 (new): hot-page throughput is capped by `REPLICAS` owners; the bound above is
  enforced by a test, raising it is a design change.
- Known issue 17 (new): background reads during enrollment fail `Unavailable`.

## Round 4: handoff

Track handoff closes Known issues 6, 12 and 13. Commits on branch `racer-sim4-handoff`:
a09fac015 (`Published::resolve_recent`), cda9f9ea6 (issue 13), 57792bc40 (issue 12), 2733abafd
(repeat classification and simulator tests), plus this section.

### Root causes

The simulator was given a temporary per-fetch trace (not committed) that recorded, for each
repeated origin request of a page, the fetcher's rank and credits, its handoff targets, the
previous fetcher, and whether that node still held the page. In 5k `full-churn` (seed 1) 1,106
of 2,265 repeats came while rank 0 was the same as at the previous fetch, nearly all while the
previous fetcher still held the page. Most were fetched by an owner above rank 0 that did not get
rank 0 to fetch. The causes, cited at 48ca50329:

1. Handoff targets blocked the delegation (issue 13). The delegation reserved one credit per
   remaining probe, handoff targets included (`let left = (self.count - index)`,
   `core/src/read/walk.rs:282`, used at `:297`). Rank 1 holds 3 credits when a reader funds it
   (`owner_funding(1)` minus the exchange); with any handoff target it needed 4 or more, so it probed
   rank 0 copy-only, got a miss, and fetched itself. The copy then sat at rank 1, and the next rank
   0 fetch missed it.
2. The delegation route was starved of links (issue 13). Links were split evenly over the remaining
   probes, again counting handoff targets (`walk.rs:326-331`). Rank 2 typically inherits 5 links and
   gave the Acquire 2, too few to reach rank 0. With cause 1 fixed, 2,313 of 11,811 delegations in
   that run failed before reaching rank 0 (`route_unreachable`), and the owner fetched itself.
   Handoff probes split the same way (`walk.rs:341`): with the delegation fixed, 5,300 of about
   7,700 handoff probes got one link and failed.
3. A predecessor that is behind made the owner fetch (issue 12). It answers `StaleMembership`; the
   requester has nothing newer, so the probe completes as `StaleDestination`
   (`src/read/candidates.rs:1022-1033`), which is transient evidence (`walk.rs:470`), and the owner
   reaches origin. In 5k `churn` with the route settings, 138 delegations got this answer; in a
   120-node churn test 48 owner skips led to 40 repeat fetches while rank 0 held the page.

Rank 0 reached through delegation held one credit (issue 13): the Acquire carried exactly the
exchange and the origin credit (`walk.rs:321`), and handoff probes need two (`walk.rs:283`).

### Changes

- Handoff probes reserve nothing (`left` counts only later predecessors, `walk.rs:332`).
- The delegation takes a full route and leaves one link per later predecessor; copy-only
  predecessors still split evenly; handoff targets take the remaining links in order, so the first
  (the previous rank 0, most likely to hold the page) gets a usable route (`sharing`, `walk.rs:394`).
- An owner with handoff targets forwards its spare credits (up to `MAX_HANDOFF`) with the Acquire,
  so rank 0 can ask previous owners too (`spare`, `walk.rs:384`). Readers do not fund owners more:
  a trial that added two credits from reader slack added about 50 failed reads in 5k
  `full-failover`, because credits sent to a crashed owner are lost (issue 7).
- An owner asks a predecessor that held the same rank under membership version - 1 under that
  version (`same_rank_previous`, `WalkConfig::previous`, `Probe::previous`). The authority is the
  same under either version, and a predecessor that has not adopted the current version still
  resolves the previous one. A `StaleMembership` answer to such a probe is transient evidence and
  never refreshes the walk (`pending_previous`).
- That rule applies only for `PREVIOUS_MEMBERSHIP_WITHIN` (10 s) after this node replaced the
  previous membership. Peers keep a replaced membership for the 30 s grace window
  (`Snapshot::retention`), but a node may also keep one alive through its own leases, which says
  nothing about a peer. A first version that used any locally retained previous membership failed
  `app::tests::dst::dst_native_faults_reach_warmed_peers_in_small_topologies`: 26 s after a change the
  peer had dropped version - 1 and answered `StaleMembership`. Production checks the window with
  the new `controlplane::Published::resolve_recent` (`controlplane/src/published.rs:130`), which
  only returns parts held for grace. `CandidatePolicy::previous_owners`
  (`src/read/candidates.rs:1159`) resolves the previous membership once for both handoff targets
  and same-rank predecessors. The simulator measures the window from the version's publication
  (no later than adoption, so it is stricter) and uses the rule only when nodes retain old versions.
- No wire change: requests already carry their membership version, and peers already serve any
  version they retain.

### The bound (issue 6)

`NodeWorld::classify_repeat` (`sim/src/node/world.rs:2923`) classifies each repeated origin request
for a page against the previous one, and reports list the counts under
`origin_per_page.repeats`:

- `after_owner_move`: the page's rank 0 under this request's membership differs from rank 0 under
  the previous request's. Two requests in a row with the same rank 0 never fall here, so this class
  has at most one request per rank 0 change.
- `copy_lost`: same rank 0, and the previous fetcher no longer holds the page (evicted or crashed).
- `copy_held`: same rank 0, and the previous fetcher still holds it. Reaching origin here needs a
  transient failure (rank 0 unreachable or overloaded, or behind by more than one version), a
  concurrent cold miss, or an owner without credits or links to delegate.

So origin requests for a page are at most 1 + (rank 0 changes) + `copy_lost` + `copy_held`. With no
failures, no concurrency and no evictions this is one fetch per page plus one per ownership move.
Tests enforce it: `repeat_origin_fetches_are_classified_and_bounded` (every repeat is classified,
and `copy_held` is at most 2% of origin requests under churn), `joined_owners_copy_from_previous_owners`
(no repeats after a join), and `single_reader_then_storm_reuses_copies` (one request per page).

### Tests

- racer-core: `handoff_targets_do_not_block_delegation`,
  `delegation_forwards_spare_credits_for_rank_zero_handoff`,
  `delegation_route_takes_priority_over_later_predecessors`,
  `handoff_probes_take_remaining_links_in_order`, `same_rank_previous_marks_unchanged_predecessors`,
  `predecessors_with_unchanged_rank_are_asked_under_the_previous_membership`,
  `stale_answer_under_the_previous_membership_is_transient_evidence`; the link split in
  `owner_with_tight_links_partitions_predecessor_routes` changed for the delegation.
- controlplane: `resolve_recent_returns_only_recently_superseded_grace_parts`.
- Production: `owner_with_handoff_targets_delegates_with_spare_credits` (3 credits now delegate;
  before, rank 0 got a copy-only probe) and
  `owner_asks_unchanged_predecessors_under_the_recent_previous_membership` (version, age, rank
  change, and stale answer cases).
- Simulator: `owners_ask_lagging_predecessors_under_the_previous_membership` (owner stale skips 48
  to 52 before, at most 2 now; `copy_held` 40 to 48 before, at most 8 now; never with no retained
  versions), `repeat_origin_fetches_are_classified_and_bounded`, and the repeat counts in
  `joined_owners_copy_from_previous_owners`.

### Evidence

Release build, seed 1, 5,000 nodes, scenario defaults; `churn-r` uses `--duration 1s` and
`{"objects": 256, "request_rate": 2.0}`; `full-*` use `{"request_rate": 0.005}`. Origin requests
per page: total, max, pages fetched more than once. Read latency `read_ns` p50 / p99 in ms.

| Scenario | Origin before | Origin after | Reads ok / failed before | after | p50 / p99 before | after |
| --- | --- | --- | --- | --- | --- | --- |
| storm | 1,196, 2, 172 | 1,024, 1, 0 | 171,391 / 0 | 163,808 / 0 | 302 / 1342 | 268 / 1342 |
| churn | 1,220, 3, 191 | 1,028, 2, 4 | 167,231 / 1,479 | 170,015 / 1,495 | 403 / 738 | 336 / 738 |
| churn-r | 2,835, 4, 256 | 2,613, 4, 37 | 13,811 / 113 | 13,825 / 112 | 50 / 151 | 50 / 151 |
| zipf | 42,755, 3, 3,935 | 40,722, 3, 1,932 | 147,684 / 11 | 148,922 / 11 | 168 / 1476 | 185 / 1476 |
| full-churn | 14,784, 9, 1,699 | 13,644, 5, 991 | 34,196 / 76 | 34,194 / 76 | 63 / 168 | 63 / 151 |
| full-failover | 13,421, 7, 722 | 13,170, 7, 466 | 33,924 / 122 | 33,922 / 125 | 63 / 302 | 63 / 302 |

Repeat classes after (`after_owner_move` / `copy_lost` / `copy_held`): storm 0 / 0 / 0, churn 4 / 0 /
0, churn-r 13 / 5 / 23, zipf 0 / 1,752 / 203, full-churn 1,046 / 55 / 24, full-failover 0 / 399 /
264. In `full-failover` no membership changes (crashed members stay, issue 7), so its repeats come
from crashed rank 0 owners: the page is lost with the node, or rank 0 is unreachable until link
backoff. The storm's lower `reads_completed` is reads still in flight at the 2 s cutoff; a 6 s run
completes 473,965 reads after against 472,630 before, with the same p90 and p99.

In `churn-r`, `owner_stale_destination_skips` went from 143 (a trial without the change) to 3, with
1,784 probes sent under the previous membership. The page fetched 4 times there is fetched after
ownership moves.

50,000 nodes, `full-churn` (`{"request_rate": 0.005}`). Before is the round 3 combined run on the
same base (48ca50329).

| | Before | After |
| --- | --- | --- |
| Origin requests | 75,267 | 58,414 |
| Max per page | 11 | 9 |
| Pages fetched more than once | 20,035 of 45,938 | 9,494 of 46,408 |
| Repeats (move / lost / held) | not measured | 10,171 / 232 / 1,603 |
| Reads ok / failed | 329,024 / 4,959 | 334,957 / 1,779 |
| `read_ns` p50 / p99 (ms) | 63 / 185 | 63 / 201 |
| Wall, peak RSS | 21:11, 2.54 GiB | 34:47, 2.60 GiB |

No hard invariant failed. Owners sent 2,707 probes under the previous membership and skipped 101
lagging predecessors. `copy_held` is 2.7% of origin requests at this scale, above the 2% the small
churn tests enforce; most of the remaining repeats follow ownership moves. The p99 rose by one
histogram bucket. Wall time is not comparable: four tracks shared the host during this run.

### Deviations and open items

- Issue 13 asked for rank 0 reached through delegation to ask previous owners. It does when the
  delegating owner has spare credits; readers fund owners as before (see above), so with the
  default page budget most delegations still carry one origin credit. The tail came from the three
  causes above, not from this.
- Handoff probes still only look one version back, and production keeps a replaced membership for
  30 s. After that, a new rank 0 cannot find the previous holder; those fetches count as
  `after_owner_move`. Asking current successors before every cold fetch at rank 0 would catch some
  of them (about 400 in 5k `full-churn`) but adds a probe to every first fetch; not done.
- `copy_held` in `full-failover` (264) belongs to issue 7 (crashed members stay routable).
- Workspace tests: `read::range_stream::tests::responses_stream_more_than_three_pages_only_with_client_sized_http_framing`
  and `alloc` `buffer_tests::mmap_threshold_zero_alignment_and_matching_unmap` each failed once under
  load and pass alone; neither touches code changed here.
