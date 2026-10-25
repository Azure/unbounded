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

14. Resolved: a new member joins only once its dataplane serves peers (it reports its Pod after
   opening the peer listener) and joins are batched by a hold (round 4, join, 789725d9c,
   19ed37c90). Was: members whose control client has not applied a membership yet (enrolling,
   or restarted) are routable but refuse peer links. In `full-failover` at 50,000 nodes (seed 1)
   about 145k of 165,343 link failures go to them, coinciding with most of the 12.6k failed reads
   (60 to 200 s). Measured in round 4: 145,222 link failures and 12,424 of 12,759 failed reads.

15. Resolved (round 4, track repeat, 34a4bf93f, 293eaa5d4): a repeat while the previous fetcher
   still holds the page is a skip (an owner above rank 0 fetched after its probe of rank 0
   failed) or the one rank 0 refetch of the copy a skip left behind, so `copy_held <= skips +
   orphans <= 2 * skips`; anything else is the hard invariant `origin_repeat_explained`, enforced on
   every run. Partitions were not the cause: in 50k `full-failover` the 4,531 orphans come from
   rank 0's own origin failure (1,818), members that do not serve yet (1,024, issue 14), no route
   within the probe's links (948), an owner that could not fund a delegation (595) and partitions
   (145). The budget-limited part is issue 21. See "Round 4: repeat".
   Original report: Dropping a dead member (issue 7 fix) is an ownership move and inherits issues
   6 and 12: at 50,000 nodes in `full-failover` it added 171 recoverable `Unavailable` reads and
   moved the worst page from 7 to 9 origin requests. See "Round 4: liveness".
   After the wave 1 merge (handoff fixes present), `full-failover` classifies no repeat as
   `after_owner_move` at 5k or 50k, and the worst page is 7 (5k) and 8 (50k, against 9 with
   liveness alone), so the drop itself no longer adds unbounded repeats. It stays open because 50k
   `full-failover` has 5,473 `copy_held` repeats (10.5% of 52,152 origin requests), above the 2%
   that `repeat_origin_fetches_are_classified_and_bounded` enforces at small scale; 5k is at 2.0%.
   Inference: the holders sit behind the scenario's partitions (19,325 partition link failures).
   See "Round 4: combined (wave 1)".

16. Resolved (round 4, fanout, 751b85b1e through f887fe173): owners shed readers of a page that is
   most of their queue, and a shed reader asks one of the page's helpers; see "Round 4: fanout".
   Residual bound: a page is sent by at most its `REPLICAS` owners plus `fanout_width(members)`
   helpers (integer square root, at most 256), and helpers never chain.
   Original report: hot-page peer throughput is capped by the page's `REPLICAS` owners: candidates
   are only the ranked owners (`src/read/candidates.rs:628-635`), so readers holding a copy never
   serve other readers, and a storm on one page gets at most `REPLICAS * nic_bps / page_bytes`
   deliveries per second. Enforced by
   `contention_scenarios::overloaded_hot_page_owners_do_not_stream_to_abandoned_requesters`. Under
   overload relays shed on the `Relay` quota before the owners' NICs saturate (50k, 1 s storm: about
   177k storm pages against a 570k bound). Raising it is a design change. See "Round 4: capacity".

17. Resolved: no read fails because a listed member cannot serve yet (round 4, join, same fixes
   as 14). Was: about 12% of background reads during enrollment fail `Unavailable` (11.7k at
   50,000 nodes in `full-cold-start`), suspected to share the cause of 14 (round 4, capacity).
   The remaining enrollment-time failures come from join versions (new issue 19).

18. Resolved (round 4, relay, wave 3, 0eba3d9c8): a simulator modeling artifact. A relay that
   forwarded a body still queued at a busy owner reserved its own NIC at the body's far-future send
   time (`Fabric::transfer_until`, `sim/src/resources.rs`), so its other sends queued behind an idle
   gap, backlogs spread from relay to relay, and relays held their `Relay` reservation for about
   190 ms per body instead of about 23 ms. 5k `zipf` (seed 1) now fails 17 reads (was 4,373) with
   59,758 relay rejections (was 339,136); seeds 2 and 3 fail 18 and 30 (were 5,152 and 7,254); 5k
   `storm` fails 0 (was 461). Production's hold (`src/peer/server.rs`, `relay.reservation`) is
   correct and unchanged. See "Round 4: relay (wave 3)".
   Was: the relay hold from the capacity track makes the 5k hot-page scenarios shed on the `Relay`
   quota (`zipf` 4,373 failed reads, 339,136 relay rejections after the wave 2 merge).

19. Partly resolved (round 4, join2, b08235140, 7aacc03ae): a non-owner whose owner is behind
   walks the page's owners under the recent previous membership once instead of spending its
   credits on each stale owner (`racer_core::read::stale`). 50k failed reads: `full-failover`
   1,527 to 479, `full-cold-start` 1,103 to 247. What remains are readers that joined in the
   version their owners lack (Known issue 26). See "Round 4: join2 (wave 3)".
   Was: join versions cause `StaleMembership` skips. A new member learns that it is listed at the
   same time as its peers, so a requester that already holds the version skips it as a stale
   destination; when all owners of a page are new, the walk is exhausted. At 50,000 nodes
   (seed 1, `full-cold-start` without the storm) 17,573 stale-destination skips and 1,411 failed
   reads; in `full-failover`, 1,756 failed reads (was 12,759). See "Round 4: join". No invariant
   is relaxed. Same family as Known issues 3 and 12.

20. Resolved (round 4, join2, 0925234b3): the seat egress was a simulator-model artifact. The
   shared `plan_pass` rebuilt the image on every primary pass and dropped its delta, and the step
   model installed it, so clients one version behind got full images. 50k seat replies 562 GB to
   302 GB (`full-failover`) and 576 to 302 GB (`full-cold-start`), the one-image-per-client floor;
   full fallbacks 97,931 to 1,398, all from the empty bootstrap version. Bound: no full fallback
   for lack of a delta (tests). The origin half is Known issue 25. See "Round 4: join2 (wave 3)".
   Was: join versions cost origin fetches and seat egress. Pages move from early members to later
   ones as the membership grows, and clients that fall two versions behind get full images.
   `full-failover` at 50,000 nodes: origin requests 57,163 to 66,620 (worst page 9 to 12), seat
   reply bytes 298 GB to 689 GB (9 versions instead of 2). See "Round 4: join". Same family as
   Known issue 6.

21. Resolved (round 4, orphan2, 4e66c2124 through f97b81bba): an owner holding a successor hint
   asks the hinted later owner copy-only before origin, on its own grant (one attempt, eight links,
   never added to the walk's budget), so a delegated rank 0 copies an orphan instead of fetching
   it again. 50k `full-failover`: orphans 1,205 to 893 (`copy_only` 140 to 44, `failed` 519 to
   343), `copy_held` 1,914 to 1,584, origin 52,200 to 51,834. Remainder (holder never reached
   rank 0, so no hint) is Known issue 211. See "Round 4: orphan2 (wave 4)".
   Earlier, partly resolved (round 4, orphan, a6c0d74a9 through 6b325b87e): an owner that answered
   another owner's page probe without a copy keeps a successor hint and answers a reader's
   `Acquire` with `Miss`, so the reader gets the orphan copy from that owner instead of rank 0
   fetching the page again. 50k `full-failover` (seed 1): orphans 1,679 to 1,481, `copy_only`
   orphans 228 to 103, `failed` 676 to 565, origin requests 52,461 to 52,304. What remains is
   rank 0 refetching for an owner's delegation (it carries one credit and two to four links, too
   few to reach the holder) and orphans whose owner never reached rank 0 (`route`, `partition`,
   `busy`: no hint). Bound unchanged: `origin_repeat_explained`. See "Round 4: orphan (wave 3)".
   Wave 3 merge (7c6a08583): an owner no longer defers while its own acquisition of the page runs
   (`racer_core::read::successor::defer_allowed`); see "Round 4: combined (wave 3)".
   Original report: an owner above rank 0 that cannot fund or route its delegation fetches the
   page itself, and rank 0, which never asks successors, fetches it again later: in 50k
   `full-failover` 1,643 such skips for lack of a route and 621 for lack of credits, leaving 944
   and 336 `copy_held` repeats.

22. Resolved (round 4, track repeat, ba709b40e): the simulator issued origin requests without
   debiting the attempt production debits after validating origin authority
   (`src/read/fill.rs:1810`), so an owner with a spent budget still fetched (135 requests in 5k
   `full-failover`). It now fails the fill like production (`origin_unfunded`).

23. Resolved: simulator artifact. The simulator's hot-page shed decided ownership from the
   placement cache only (the retention test `owned`), while production `PeerServer::shed_hot_page`
   ranks the page (`placement.rank`, `src/peer/server.rs`). After the join track's multi-version
   cold start (5 membership versions instead of 1) owners held object 0's pages without a
   ranking under the current version, so 10 of 16 pages were never shed and every reader queued
   at their owners. The simulator now ranks like production; the 1 Gbit/s `full-cold-start` 1 s
   storm completes 94,828 reads with 0 failures (63,637 / 31,191 before). Bisected to bdf231156
   (fanout on wave 1: 47 failed; with `report_at_start` the merged tree fails 63). See
   "Round 4: overload (wave 4)" (round 4, overload, wave 4, 91fd66397).

24. Resolved: `flight_leader_valid` failed in 5k `zipf` at production page size under origin
   overload because `racer_core::read::flight::PageFlights` did not model production's retained
   flight operations; the simulator now retains each fill like production (374 violations on
   3771e5c3f, 376 with the relay fixes, 0 after). See "Round 4: flight (wave 3)" (round 4,
   flight, wave 3).

25. Partly resolved (round 4, history, wave 4, 8ba959e3f..f8ec46f53): membership chains record
   when each member arrived (`topology::Membership::since`). Placement is rendezvous hashing, so
   a later owner that arrived before an owner ranked above it until then and may still hold the
   page, however many versions ago it moved. Owners ask such lineage targets copy-only before
   origin, Spread readers visit them first, and walks fund their probes from slack. Moved-page
   repeats (origin requests): 50k `full-failover` 3,612 to 2,878 (52,200 to 51,073, worst page
   10 to 9), 50k `full-cold-start` 2,629 to 1,919 (29,695 to 28,960), 5k `full-churn` 1,060 to
   894. What remains are holders ranked below the three owners (Known
   issue 201) and rank 0 reached by a delegation with too few credits to ask its lineage
   targets (Known issue 202). See "Round 4: history (wave 4)".
   Was: the new owner asks only the owners of membership version - 1, and peers keep older
   memberships only for the 30 s grace window, so a copy fetched two or more versions earlier is
   not asked. 50k `full-failover` (seed 1): of 3,610 `after_owner_move` repeats, 980 had a
   version - 2 holder and 1,240 an older one; 5k `full-churn`: 892 of 1,062 older than
   version - 2 (round 4, join2).

26. Partly resolved (round 4, history, wave 4, f0780ecab, 3e6dd0ed4): a reader that cannot walk
   the previous membership waits once for an owner that is behind (a third of the time left, at
   most 4 s) and asks it again (`racer_core::read::stale::stale_wait`), and a verified
   `StaleMembership` returns the remote credits the request carried
   (`AcquisitionBudget::refund_remote`), so a stale answer costs only its exchange. 50k failed
   reads: `full-cold-start` 248 to 95, `full-failover` 535 to 394; walks exhausted on stale
   owners 1,134 to 452 and 1,214 to 451. What
   remains is owners still behind after the wait (Known issue 203). See "Round 4: history
   (wave 4)".
   Was: readers that joined in the version their owners have not applied yet cannot use the
   stale fallback (they are not members of version - 1, so peers would reject them under it):
   50k `full-cold-start` 247 failed reads, all during enrollment (54 to 161 s); every one of the
   1,732 stale skips that could not fall back was by such a reader (`stale_fallback_not_member`)
   (round 4, join2).

27. Resolved: DST harness bug, not production. `connect_links`
   (`src/app/tests/dst.rs`) only asked node `a` to set up the link, but step 28
   (`InflightCrash`) crashed and restarted node `10` under `0d`'s `Ready` link. `0d` holds a
   half-open link it only learns about on use (`verbs/src/simulation.rs:917-918` returns
   `RETRY_EXC_ERR` for an unpaired queue pair), and `Links::wants` requires `Idle`
   (`src/rdma/links.rs:475-480`), so `ensure_link` from `0d` was a no-op forever. Production
   recovers either way: the restarted peer's setup replaces the stale link
   (`verbs/src/node.rs:881-885`), or the survivor's first use breaks it. The harness now has the
   not-ready side initiate and waits out either side's backoff. Regression tests
   `rdma::links::tests::half_open_link_after_peer_restart_*`. See "Round 4: orphan (wave 3)".

28. Resolved (round 4, leak, 8b8908455): `dst_generated_native_traffic_churn_oracle` seed 16
   leaked `RequestContext` on a node retired by `PendingWriteCrash`. A reference cycle: RDMA link
   setup tasks own an `Rc<Links>` (`connect_link`, `src/peer/transport.rs:741`) and live in
   `Links::tasks` (`src/rdma/links.rs:220`), cleared only by `Links::drain`. A worker dropped
   without a drain kept the tasks with their connections, I/O budget, and admission charges, plus
   the telemetry I/O they reached. `WorkerApplication` now abandons setups on drop. See "Round 4:
   leak (wave 3)".

29. Resolved: a fill whose leader's deadline passed kept running until origin answered, because
   the origin model (`sim/src/resources.rs`, `Origin::request`) reserved FIFO bandwidth up front
   and could not cancel. The origin model now cancels a tracked request like production closing
   the connection (slot and unsent bandwidth freed, later requests move earlier), and the
   simulator cancels the fill's origin request at its scope. 5k `zipf` at 16 MiB pages: 18,468
   origin requests canceled, completed reads 31,111 to 33,374, failed reads 211 to 90, and the
   new `origin_within_scope` invariant holds; 1 MiB scenarios keep their trace hashes. See
   "Round 4: cancel (wave 3)" (round 4, cancel, wave 3).

30. Resolved: an expired or canceled leader's running fill now continues for the live acquisition
   waiter with the latest deadline instead of being canceled and restarted (shared flight state
   `allow_handover`, production `Flights::allow_handover` and `FlightOperation::poll_cancellation`,
   core `PageFlights::allow_handover`). In 5k `zipf` at 16 MiB pages, origin requests fall from
   48,438 to 35,917, the most requests for one page from 24 to 4, origin requests canceled from
   18,468 to 2,186, and failed reads from 90 to 54. 1 MiB scenarios keep their trace hashes. See
   "Round 4: handover (wave 3)" (round 4, handover, wave 3).

31. Resolved: in 5k `zipf` at 16 MiB pages the origin is about 7.4 times overloaded, and owners
   above rank 0 fetched pages rank 0 was still fetching. 5,740 of the 18,030 overlapping requests
   came from a simulator artifact (a copy-only probe of rank 0 answered `Miss` instead of joining
   rank 0's running acquisition like production `Flights::join_copy`) and 11,352 from a production
   rule (a timed-out probe of rank 0 was taken as origin authority although rank 0's request was
   only queued). The simulator joins copy-only requests to the running flight (5bc1f2550), and an
   owner's page probe of rank 0 that is sent but unanswered within the attempt no longer gives
   origin authority (`Outcome::TimedOut`, 5466604de). Overlapping requests 18,030 to 1,088, origin
   requests 35,672 to 18,867, completed reads 32,668 to 37,538. The 1,088 left follow a route
   failure or an `Overloaded` rank 0 (new issue 222). See "Round 4: overload (wave 4)" (round 4,
   overload, wave 4).

221. An owner above rank 0 no longer fetches a page when its probe of rank 0 is accepted but never
   answered (rank 0, or a relay on the route, hangs without failing the connection): readers of
   that page fail until rank 0 answers, fails at the transport level, or leaves the membership.
   This is the price of issue 31's fix; crashed and partitioned members fail at the transport
   level and are unaffected. Open (round 4, overload, wave 4).

222. An owner above rank 0 still fetches when rank 0 answers `Overloaded` (its flight's waiter cap
   or admission) or the route to rank 0 fails, even though rank 0 is fetching the page: 958 route
   and 126 `Overloaded` skips of the 1,088 remaining overlapping requests in 5k `zipf` at 16 MiB
   pages. Open (round 4, overload, wave 4).

32. Resolved (round 4, leak, a763b576c): DST native seed 60 stalled in `connect_links` when a node
   held a `Ready` link to an earlier incarnation of a crashed and re-added peer. Harness
   precondition: production breaks such a link on first use or when its idle `Close` fails. The leak track's harness change (advance past `LINK_IDLE`) was dropped at the
   wave 3 leak merge in favor of Known issue 27's fix (the not-ready side initiates), which covers
   the same case.

33. Resolved (round 4, leak, a763b576c): DST native seed 59 left its injected fault unconsumed
   because the chosen receiver was not an overlay neighbor of the source and the read went through
   a relay. Harness precondition: the receiver is now a ranked candidate that neighbors the source.

201. Holders ranked below the page's three owners are not asked. A member displaced from the
   owners by later arrivals keeps its copy but now ranks fourth or lower, and placement keeps only
   the best three scores per slot (`topology::placement::Ranking`), so no lineage target names it.
   In an instrumented 5k `full-churn` run, 375 of 790 moved-page repeats whose holder was unasked
   had such a holder. Fixing it needs deeper rankings (keep more scores per slot, with the
   incremental update and memory bound that implies). Open (round 4, history, wave 4).

202. Rank 0 reached by an owner's delegation usually carries one credit, which it keeps for its own
   origin attempt, so it cannot ask its lineage targets (in the instrumented 5k `full-churn` run,
   385 of 415 repeats whose holder was a listed but unasked lineage target started with one
   credit). Readers fund the delegating owner's lineage credits only from slack beyond every
   later visit's minimum, which an 8-attempt page budget rarely has; funding them anyway cut
   moved-page repeats 894 to 678 but raised failed reads 1 to 29 (5k `full-churn`). Open
   (round 4, history, wave 4).

203. A reader waits only once for an owner that is behind (`stale_wait`); at 50k the propagation
   window (client lag p90 about 24 s) is longer than the wait, so 95 reads still fail in
   `full-cold-start` (57 to 161 s, during enrollment). Open (round 4, history, wave 4).

211. Orphans whose holder never reached rank 0 leave no hint, so rank 0 still refetches them (50k
   `full-failover`: `route` 285, `partition` 139, `busy` 79, most of `failed` 343). Needs the holder
   to tell rank 0 later (round 4, orphan2).

212. The simulator's repeat classification counts `Unavailable` signed by a relay on the route as
   `failed` (rank 0's own origin failure); split it (round 4, orphan2).

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
14. Resolved: a new member joins only after it reports serving, joins batched by a hold
   (round 4, join, 789725d9c, 19ed37c90).
15. Resolved: repeats while the previous fetcher holds the page are skips of rank 0 or their one
   orphaned refetch, `copy_held <= 2 * skips`, invariant `origin_repeat_explained` (round 4,
   repeat).
16. Resolved: owners shed readers of a dominant hot page to the page's helpers; a page is sent by at
   most `REPLICAS` owners plus `fanout_width` helpers (round 4, fanout, 751b85b1e..f887fe173).
17. Resolved: no read fails because a listed member cannot serve yet (round 4, join, 789725d9c).
18. Resolved: relays no longer hold their transmit side idle for bodies queued upstream (a
   simulator artifact); 5k `zipf` fails 17 reads (was 4,373), `storm` 0 (was 461) (round 4, relay,
   wave 3, 0eba3d9c8).
19. Partly resolved: non-owners whose owners are behind walk the recent previous membership;
   50k failed reads 1,527 to 479 (failover) and 1,103 to 247 (cold start); the rest is 26
   (round 4, join2).
20. Resolved: images keep their delta across idle primary passes; 50k seat replies 562 to 302 GB,
   the one-image-per-client floor; origin half split into 25 (round 4, join2).
21. Resolved: owners ask hinted successors copy-only before origin on their own grant; 50k
   `full-failover` orphans 1,205 to 893, origin 52,200 to 51,834 (round 4, orphan2). Earlier:
   readers are deferred to the owner holding an orphan copy (round 4, orphan);
   50k `full-failover` orphans 1,679 to 1,481. Delegation-led refetches and orphans of owners that
   never reached rank 0 remain.
22. Resolved: the simulator debits the origin attempt like production (round 4, repeat).
23. Resolved: simulator artifact; the hot-page shed ranks the page like production, so owners
   without a cached ranking shed (1 Gbit/s 1 s storm 31,191 to 0 failed reads) (round 4, overload,
   wave 4).
24. Resolved: core `PageFlights` retains leader operations like production `Flights`; the
   simulator retains each fill (round 4, flight, wave 3).
25. Partly resolved: owners ask, and readers visit first, later owners that arrived before them
   (lineage); 50k `full-cold-start` moved-page repeats 2,629 to 1,919; rest is 201 and 202
   (round 4, history, wave 4).
26. Partly resolved: readers that cannot fall back wait once for an owner that is behind, and
   stale answers refund their remote credits; 50k `full-cold-start` failed reads 248 to 95; rest
   is 203 (round 4, history, wave 4).
27. Resolved: DST harness only let the survivor set up a half-open link after a peer
   crash-restart; the restarted peer now initiates (round 4, orphan).
28. Resolved: a worker dropped without a drain no longer leaks RDMA link setup tasks and their
   admission charges (round 4, leak).
29. Resolved: the origin model cancels a tracked request (slot and unsent bandwidth freed), and
   the simulator cancels an expired fill's origin request at its scope like production (round 4,
   cancel, wave 3).
30. Resolved: a running fill passes to another live waiter when its leader expires; 5k `zipf` at
   16 MiB: origin requests 48,438 to 35,917, most per page 24 to 4 (round 4, handover, wave 3).
31. Resolved: copy-only probes join rank 0's running acquisition (simulator), and an owner does
   not take origin authority from a timed-out probe of rank 0 (production, core, simulator); 5k
   `zipf` at 16 MiB overlapping origin requests 18,030 to 1,088 (round 4, overload, wave 4).
32. Resolved: DST native harness recovers stale links to re-added peers (round 4, leak).
33. Resolved: DST native fault reads go to a neighbor of the source (round 4, leak).
221. A hung rank 0 (accepts, never answers) now blocks the owners above it from fetching (round 4,
   overload, wave 4).
222. Owners above rank 0 still fetch after a route failure or an `Overloaded` answer from a rank 0
   that is fetching (1,088 overlapping requests left) (round 4, overload, wave 4).
201. Holders ranked below the three owners are not asked (placement keeps three scores per
   slot) (round 4, history, wave 4).
202. Rank 0 reached by a delegation has too few credits to ask its lineage targets (round 4,
   history, wave 4).
203. One wait does not cover the 50k propagation window: 95 failed reads in `full-cold-start`
   (round 4, history, wave 4).
211. Orphans whose holder never reached rank 0 still refetch (50k 893 left) (round 4, orphan2).
212. Relay-signed `Unavailable` is classified as rank 0's `failed` (round 4, orphan2).

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

## Round 4: combined (wave 1)

The enroll, liveness, capacity, and handoff branches were cherry-picked in that order onto
`racer-core-sim`. Code merged without conflicts except `sim/src/control.rs`, where enroll's
authorization settings (`auth`, `auth_qps`, `auth_burst`) and liveness's `node_monitor_grace_ns`
were both kept. In this document the new issues were renumbered: liveness 51 and 52 to 14 and 15,
capacity 71 and 72 to 16 and 17. Commit hashes in the round 4 sections refer to the merged commits.

Evidence, release build, seed 1, scenario defaults; `full-*` and `enrollment-storm` use
`{"request_rate": 0.005}`. Origin is total, max per page, pages fetched more than once. "API 429s"
is `ctl_apiserver_throttled`. The 5k runs ran 7 in parallel; the 50k runs one at a time, alongside
one 50k run of another track.

| Scenario | Reads ok / failed | Origin total, max, pages>1 | API 429s | Wall | Peak RSS | Hard failures |
| --- | --- | --- | --- | --- | --- | --- |
| storm 5k | 177,361 / 18,013 unavailable | 1,029, 2, 5 | - | 1:28 | 1.42 GiB | 0 |
| zipf 5k | 148,195 / 4,627 unavailable | 42,763, 3, 4,673 | - | 2:06 | 1.50 GiB | 0 |
| churn 5k | 167,252 / 1,503 (3 overloaded) | 1,041, 2, 17 | - | 1:07 | 0.99 GiB | 0 |
| full-cold-start 5k | 95,054 / 50 | 7,009, 3, 79 | 65 | 0:35 | 0.53 GiB | 0 |
| full-failover 5k | 34,004 / 61 | 13,193, 7, 486 | 56 | 0:42 | 0.32 GiB | 0 |
| full-churn 5k | 34,199 / 76 | 13,640, 5, 987 | 0 | 0:45 | 0.29 GiB | 0 |
| enrollment-storm 5k | all 5,000 enrolled at 31.4 s | - | 65 | 0:03 | 0.09 GiB | 0 |
| full-cold-start 50k | 914,717 / 6,665 | 29,991, 3, 2,552 | 23,532 | 15:24 | 4.10 GiB | 0 |
| full-churn 50k | 336,739 / 1,739 | 57,696, 9, 9,107 | 13,783 | 36:46 | 2.54 GiB | 0 |
| full-failover 50k | 304,333 / 6,936 | 52,152, 8, 5,748 | 21,562 | 22:11 | 2.81 GiB | 0 |

Against each track's own numbers:
- enroll: 5k all enrolled at 31.4 s with 65 API 429s and 50k at 170.6 s with 23,532, identical.
- capacity: 50k `full-cold-start` reads 914,717 / 6,665 against 884,120 / 11,753 on the capacity
  branch; 5k 95,054 / 50 against 94,810 / 98.
- liveness: `full-failover` 5k 61 failed (122 on its branch) and 50k 6,936 (12,759), with 178
  reads failed while crashed (171) and 5 links to dead members.
- handoff: 50k `full-churn` 57,696 origin requests, max 9, 9,107 pages>1, against 58,414, 9,
  9,494; reads 336,739 / 1,739 against 334,957 / 1,779. 5k `full-churn`, `full-failover` and
  `churn` match within a few percent.
- Regression: 5k `storm` and `zipf` now fail 18,013 and 4,627 reads on the `Relay` quota, against
  0 and 11 on the handoff branch. A storm run of the capacity branch alone fails 23,040, so the
  cause is capacity's relay hold, not the merge; recorded as Known issue 18 rather than changed,
  because the hold matches production (`src/peer/server.rs:438-445`, `:660`) and the hot-page
  capacity is the fanout track's (issue 16).
- Known issue 15 was rechecked with handoff's bound: no repeat is `after_owner_move` in
  `full-failover` at either scale, but 50k `copy_held` is 10.5% of origin requests; it stays open.

## Round 4: fanout

Scope: Known issue 16 (track number 71; hot-page throughput capped by the page's `REPLICAS`
owners). Measurements include the round 4 capacity track's simulator commits 98a025f91 and 35ae1eb21 (cherry-picked here
as 522e1acc6 and 06bd58800 before this track's work), so "before" is the base plus those two.

### Does production already fan out?

No. What exists is node-local or per-owner coalescing, not more senders:

- Candidates are the placement-ranked owners only: `CandidatePolicy::candidates`
  (`src/read/candidates.rs:628`) ranks at most `REPLICAS` (`topology/src/placement.rs:10`, `:832`).
- Peer subscriptions (RangeStream `SELECTIONS`) go to the primary of the first demanded page only
  (`subscribe_provider`, `src/read/candidates.rs:462`) and a grant from any other node is rejected
  as `Unauthorized` (`:519`). `flow_control::subscriptions::Fanout` (`flow/src/subscriptions.rs:152`)
  fans one provider completion out to that provider's waiting subscribers, and the RangeStream
  `Reader` registry (`src/read/range_stream.rs:65`) shares selected pages among windows of one node.
  Neither adds a sender.
- `SingleFlightPeers` only remembers providers that refused multi-flight contracts; `Spread` only
  reorders the same three owners (`core/src/read/candidates.rs`, `visit_order`).
- A destination does serve `CopyOnly` and `Acquire` page requests without checking that it owns the
  page (`serve_action`, `core/src/read/serve.rs:36`; `Coordinator::serve_peer`, `src/read.rs:345`,
  `:402`). So the wire and the serve path already allow any member to act as a source; nothing
  ever sends a reader to a non-owner.

The simulator models the same rule, so the cap is real and not a modeling artifact.

### Design

All decisions are in `racer_core::read::fanout` (`core/src/read/fanout.rs`) and the candidate walk,
used by production and the simulator. No wire change: helpers are asked with an ordinary signed
`Page` `Acquire`, sheds are an ordinary signed `Overloaded`.

1. Helpers. Each page has `fanout_width(members)` helpers (integer square root, at most 256,
   `fanout.rs:71`): helper `j` is member `fnv(page, j) % members` (FNV-1a with a splitmix finish,
   stable across processes). A reader picks one by `fnv(reader, page)`, skipping owners and itself,
   and prefers a helper that is a direct overlay neighbor so the exchange needs no relay
   (`fanout_helper`, `fanout.rs:135`). Readers of a hot page therefore converge on the same small
   set, so owners send roughly `sqrt(n)` copies to helpers and each helper roughly `sqrt(n)` to
   readers.
2. Shed. An owner already writing `HOT_PAGE_BODIES` (32) bodies of a page, when that page is at
   least half of all bodies it is writing, answers a signed `Overloaded` to a requester that is
   neither an owner nor a helper of the page, before reserving a waiter or calling the page service
   (`shed_hot_page`, `fanout.rs:111`; production `PeerServer::shed_hot_page`,
   `src/peer/server.rs:386`, called at `:849`; bodies counted from the response head until the
   body is written or abandoned, `:725`). Helpers and owners are exempt (`is_helper`,
   `fanout.rs:117`, at most `fanout_width` hashes), and only a page owner sheds, so a helper queues
   the readers it was chosen for. The ranking is computed only for a page that already has 32 bodies
   in flight and stays in the server's placement cache.
3. Walk. `WalkConfig::helper` lets a non-owner page walk ask the helper once, only right after an
   owner itself shed it (`Outcome::Shed`, an `Overloaded` signed by the destination;
   `core/src/read/walk.rs:563`). A relay's `Overloaded` or a timeout says the route is congested,
   not the page, and does not ask the helper. `Shed` is recorded like `Probe(Overloaded)` but does
   not charge a route failure. The helper probe is an `Acquire` funded from what is left, at most
   `HELPER_ATTEMPTS` (4) attempts and 8 links, at least 3 and 4 (`helper_probe`, `walk.rs:303`).
   Owner probes are planned exactly as without a helper
   (`owner_probes_are_planned_exactly_as_without_a_helper`). Production maps a `PeerResponse::Overloaded`
   whose verified signer is the destination to `Outcome::Shed` (`src/read/candidates.rs:1043`) and
   chooses the helper only when the walk asks for it (`:966`, `:1159`); the simulator does the same
   (`Reply::Shed`, `sim/src/node/world.rs:2684`; `fanout_helper`, `:2526`; `shed_hot`, `:2547`).
4. Small budgets. A non-owner that cannot fund its first out-of-order owner at all now visits rank 0
   first instead of ending the walk on funding before any probe (`CandidateWalk::start`,
   `walk.rs:283`); a helper's inherited budget can take this path.

### Bounds and security

- Depth: only walks that start with at least `HELPER_MIN_ATTEMPTS` (5) attempts may ask a helper,
  and a helper inherits at most `HELPER_ATTEMPTS - 1` (3), so helpers never chain
  (`helper_never_chains_and_owners_never_use_helpers`, and a `const` assertion in `fanout.rs`).
- Origin: helpers are non-owners, so their walks end `Exhausted`, never `Origin`; origin authority
  and `only_owner_hits_origin` are unchanged. Origin requests per page did not grow in any run
  below.
- Senders: a page is sent by at most `REPLICAS` owners plus `fanout_width` helpers (at most 259),
  and at most `fanout_width` extra copies of a page are made per membership version. This is the
  residual bound on hot-page throughput (`width_is_square_root_and_bounded`,
  `every_chosen_helper_is_recognized_and_few_others_are`).
- Budgets never grow: the helper probe is partitioned from the walk's budget and reunited like any
  other probe (`budget_never_grows` held in every run).
- Security: a helper's answer is an ordinary signed peer `Page` response, validated exactly like an
  owner's (`validate_payload` checks the envelope against the immutable metadata, and the reader
  opens the AEAD ciphertext with its own key). The helper acquires with the reader's signed origin
  context, as an owner does. Every member can already be ranked an owner of some page of any object
and receive such requests, so a helper learns nothing a member could not already receive. A shed is
  a signed `Overloaded`; a forged one would need the destination's key, and the worst a member can
  do by shedding is what it can already do by answering `Overloaded`. The requester checks the
  signer against the destination it addressed, so a relay cannot make a reader leave an owner for
  a helper.

### Tuning

The shed thresholds were chosen by measurement (5,000 nodes, seed 1, release). Lower thresholds, or
no share rule, shed Zipf-popular pages that owners would have served within the deadline, and the
relayed helper hop failed more reads than it saved. Choosing a random helper rather than preferring
a neighbor sent most helper exchanges through relays, which shed them in `zipf`.

| Shed rule | Helper asked after | Helper choice | zipf ok / failed | full-cold-start 1G 1 s | storm |
| --- | --- | --- | --- | --- | --- |
| none (before) | - | - | 147,376 / 3,655 | 52,116 / 42,785 | 165,851 / 23,040 |
| 4 bodies | any `Overloaded` or timeout | hashed | not run | 94,646 / 238 | not run |
| 16 bodies | any `Overloaded` | hashed | 138,576 / 13,330 | 94,630 / 253 | 221,419 / 11,619 |
| 16 bodies | destination shed | hashed | 144,213 / 6,658 | 94,079 / 821 | 218,656 / 14,334 |
| 32 bodies, half of queue | destination shed | hashed | 148,026 / 4,013 | 94,524 / 376 | 210,250 / 12,668 |
| 32 bodies, half of queue | destination shed | neighbor preferred (final) | 152,799 / 2,635 | 94,798 / 102 | 267,367 / 476 |

Without any shed (helper asked only after a timeout or relay overload) the 1 Gbit/s 1 s storm
reached 88,131 / 6,752: readers waited out a 2 s attempt on an owner's queue before asking.

A time-based rule (shed only once the oldest body of the page had been in flight 50 ms to 1 s)
was also tried and dropped: short stalls still shed Zipf pages, and long ones let storm readers
queue until they timed out (storm failures 28k to 47k).

### Tests

- racer-core: `fanout::tests` (width, convergence and exclusion, set recognition, shed rule,
  neighbor preference, no helper); `walk::tests` `overloaded_owner_sends_the_reader_to_its_helper_once`,
  `helper_miss_falls_back_to_remaining_owners_and_never_repeats`, `only_a_destination_shed_asks_the_helper`,
  `owner_probes_are_planned_exactly_as_without_a_helper`, `helper_never_chains_and_owners_never_use_helpers`,
  `helper_is_skipped_when_too_little_is_left`, `small_budget_non_owner_visits_rank_zero_first`,
  `hedged_primary_overload_does_not_ask_the_helper`, `destination_shed_is_evidence_but_not_a_route_failure`.
- Production: `read::candidates::tests::shed_reader_asks_the_page_helper_once_then_the_other_owners`
  (signed responses from 16 members: a destination-signed `Overloaded` sends the walk to the helper
  once, with fewer than `HELPER_MIN_ATTEMPTS` credits; a transport `Overloaded` does not);
  `peer::tests::protocol_socket::hot_page_owner_sheds_non_helper_readers_with_signed_overload`
  (threshold, share rule, helper exemption, non-owner never sheds, page service not called).
  Both fail before the change (no helper probe; no shed).
- racer-sim: `contention_scenarios::hot_page_readers_fan_out_through_helpers` (one page, 512
  readers within 1 ms, 32 MiB/s NICs): with fan-out all 513 reads complete, 322 from helpers, the
  page reaches origin once, slowest read 3.56 s; with `node.fanout` 0 (the old behavior) 98 reads
  fail, origin twice, slowest 7.41 s. `overloaded_hot_page_owners_do_not_stream_to_abandoned_requesters`
  now runs with fan-out off so it keeps measuring the owner bound it was written for.

### Evidence

Release build, seed 1. "before" is 48ca50329 plus the two capacity commits; "after" is f887fe173.
"1G" sets `nic_bps` 125,000,000. `full-cold-start` uses `{"request_rate": 0.005}`; "1 s" also sets
`storms` to one 1 s storm of every node on object 0 at 300 s (the overloaded window the capacity
track kept available). "quiet" is `storm` with `request_rate` 0 and 15 s. Helper columns are helper
probes / helper copies / owner sheds. Read latency is log-bucket upper bounds.

| Run | Reads ok / failed | Origin total, max, >1 | Helper / copies / sheds | Peer bytes | Read p50 / p99 |
| --- | --- | --- | --- | --- | --- |
| 5k full-cold-start 1G 1 s, before | 52,116 / 42,785 (22,845 deadline, 19,940 unavailable) | 7,019, 3, 215 | - | 138 GB | 1.74 / 9.66 s |
| 5k full-cold-start 1G 1 s, after | 94,798 / 102 unavailable | 7,009, 3, 204 | 77,285 / 70,846 / 80,113 | 185 GB | 1.07 / 2.95 s |
| 5k storm quiet 1G, before | 35,876 / 44,124 | 43, 3, 16 | - | 87 GB | 5.37 / 9.66 s |
| 5k storm quiet 1G, after | 72,986 / 7,014 | 32, 3, 15 | 157,663 / 64,815 / 82,942 | 123 GB | 2.68 / 4.29 s |
| 5k storm, before | 165,851 / 23,040 | 1,195, 2, 171 | - | 542 GB | 0.30 / 1.34 s |
| 5k storm, after | 267,367 / 476 | 1,190, 2, 166 | 117,415 / 81,979 / 124,165 | 554 GB | 0.27 / 1.01 s |
| 5k storm 1G (2 s), before | 29,072 / 97,569 | 1,221, 3, 196 | - | 155 GB | 0.60 / 1.74 s |
| 5k storm 1G (2 s), after | 29,432 / 40,774 | 1,221, 2, 197 | 113,711 / 703 / 148,514 | 180 GB | 0.60 / 1.74 s |
| 5k zipf seeds 1, 2, 3, before | 3,655 / 4,101 / 3,731 failed | 44,168, 3, 5,932 (seed 1) | - | 432 GB | 0.18 / 1.48 s |
| 5k zipf seeds 1, 2, 3, after | 2,635 / 4,106 / 3,200 failed | 44,154, 3, 5,702 (seed 1) | 7,778 / 4,913 / 10,042 | 438 GB | 0.18 / 1.48 s |
| 5k churn seeds 1, 2, 3, before | 1,515 / 1,454 / 1,593 failed | 1,225, 3, 195 (seed 1) | - | 545 GB | 0.40 / 0.74 s |
| 5k churn seeds 1, 2, 3, after | 1,488 / 1,462 / 1,594 failed | 1,224, 3, 196 (seed 1) | 3,682 / 3,190 / 3,468 | 540 GB | 0.37 / 0.81 s |
| 5k full-cold-start, before | 94,810 / 98 | 7,017, 3, 204 | - | 246 GB | 0.05 / 0.13 s |
| 5k full-cold-start, after | 94,809 / 98 | 7,017, 3, 204 | 623 / 592 / 623 | 245 GB | 0.05 / 0.13 s |
| 50k full-cold-start 1 s, before | 261,113 / 634,751 unavailable | 27,448, 3, 4,510 (mean 1.22) | - | 0.81 TB | 0.74 / 2.01 s |
| 50k full-cold-start 1 s, after | 884,116 / 11,754 unavailable | 27,446, 3, 4,507 (mean 1.22) | 781,006 / 779,516 / 782,075 | 2.32 TB | 0.50 / 1.61 s |
| 50k full-cold-start 28 s window, capacity track "after" | 884,120 / 11,753 unavailable | mean 1.22, max 3 | - | 2.71 TB | 0.06 / 0.24 s |

- 50k: with fan-out the 1 s storm completes like the capacity track's 28 s window (same ok and
  failed counts within 4 reads; the 11.7k failures are background Zipf reads during enrollment,
  Known issue 17). 99.8% of helper probes returned a copy, origin requests are unchanged (27,448
  before, 27,446 after), and relay rejections fell from 11.4M to 3,310. All 50,000 nodes were
  enrolled before the storm. Wall 24:23 before and 22:01 after; peak RSS 7.2 and 6.5 GiB. The
  "before" run reproduces the capacity track's 1 s numbers exactly.

- No hard invariant failure and no relaxed invariant in any run; `only_owner_hits_origin` and
  `budget_never_grows` held throughout.
- The 2 s `storm` at 1 Gbit/s ends while most storm reads are still in flight (ok + failed is 70k
  of the 126k counted before); failures fell by 57k and successes are unchanged. The quiet 15 s run
  is the clean measurement of the same storm.
- `churn` successes vary by more than 10% between seeds with or without fan-out (153k to 176k);
  failures are unchanged.
- In the quiet 1 Gbit/s storm (every node reads all 16 pages within 10 ms), the remaining 7,014
  failures are helpers answering `Overloaded` from their own flight and waiter admission (27,585
  `helper_shed`) or ending their walks `Exhausted` (32,631 `helper_misses`): each of the about 70
  helpers per page serves about 70 readers through one flight, which is the residual bound above.

### Deviations and open items

- `HOT_PAGE_BODIES` (32) counts bodies, not bytes or time. With production's 16 MiB pages 32
  bodies are 512 MiB of queued egress, 0.4 s at 10 Gbit/s; with the simulator's 1 MiB pages 32 MiB.
  The share rule and the threshold were tuned in the simulator; production may need a different
  count, and a rate-aware rule (queued bytes over measured egress) would be more portable. Recorded
  as an open tuning item, not a known issue.
- Hedged reads (off by default) do not ask helpers: the hedge race reports only
  `ProbeOutcome::Overloaded`, not who signed it (`hedged_primary_overload_does_not_ask_the_helper`).
- Native (RDMA) page transfers are not counted as streaming bodies (`send_native` bypasses
  `send_http_response_inner`), so an owner serving mostly over native transfers sheds less. This
  only makes shedding rarer.
- The simulator counts a node's bodies across all its workers (one simulated node is one production
  worker).
- No new Known issues.


## Round 4: join

Known issues 14 and 17 (track numbers 51 and 72): members were listed, and so routable and ranked
as owners, before they could serve peers, and reads failed while they enrolled. Branch
`racer-sim4-join`, based on `racer-sim4-liveness` (0386dbed7). The enroll track's commits were not cherry-picked; enrollment
timing is the liveness base's (all 50,000 clients enrolled at about 285 s).

### Root cause

- When a node becomes a member: `members::reconcile_at` (base
  `cluster-control/src/authority/members.rs:355-386`) admits every authorized Node whose newest
  managed Pod has an IP (`select_pod`, `endpoint_ip` at line 225). Nothing about the process
  behind that IP is consulted. A DaemonSet Pod gets its IP when it is created, so in a cold start
  every Node is a member in the first version, long before its dataplane runs.
- When a node can serve peers: `WorkerApplication::start` (base `src/app.rs:2645-2651`) enrolls,
  accepts the first publication (`accept_initial_publication`), and only then opens the peer
  listener (`start_listeners`). Before that every link to it fails. The simulator models this
  exactly: a link succeeds only if `versions.enrolled(next)` (base `sim/src/node/world.rs:2351`).
- The gap is the whole enrollment: at 50,000 Pods the API server limits enrollment to about
  285 s. Measured with the new counters (`unready_member_links`,
  `reads_failed_while_members_unready`, `sim/src/node/world.rs`), the old model gives:
  - `full-failover`, 50k: 145,222 of 165,336 link failures go to members that never applied a
    membership (none to restarted ones), and 12,424 of the 12,759 failed reads happen while such
    members are listed. This confirms the inference of Known issue 14.
  - `full-cold-start` without the storm, 50k: 138,406 link failures, all to such members, and
    11,727 failed reads, all while they are listed. This is the 11.7k of Known issue 17, so 17
    has the same cause as 14.
- No existing signal said "serving". The Pod report annotation was written before enrollment
  (base `src/control/session.rs:625`, `report_rails` ahead of `enroll` at line 631), Pod
  readiness is not usable (see "Round 4: liveness"), and the admission policy
  (`deploy/racer/policies.yaml.tmpl:65-69`) lets a Pod change only its report annotation.

### Changes

Production, no wire or durable format change:

- `src/control/session.rs`: `report_rails` holds the rails until `Session::mark_serving`
  (line 505), which `WorkerApplication::start` calls right after `start_listeners`
  (`src/app.rs:2660-2665`). The report annotation now means "this Pod serves peers". Older
  dataplanes report before enrollment and so join as before.
- `cluster-control/src/authority/members.rs`: `reconcile_at` takes `Joins` (line 281). With
  `require_report`, a Node that is not already a member joins only once its selected Pod carries
  the report (diagnostic `serving`); with `hold`, new Nodes wait (line 433). Both count in
  `Result::pending`. A member is never held back, so a restarted process or a replaced Pod
  causes no membership change.
- `cluster-control/src/step/primary.rs` (`plan_pass`, used by production and the simulator):
  - always requires the report;
  - holds joins for `join_hold_secs` (line 97) after the term's last membership version change
    (`PassInputs::membership_changed_unix`, tracked per term in `primary.rs` `Term_`). The hold is
    one second per thousand of the larger of the membership and the Nodes with an eligible Pod,
    from 10 s to 60 s. Removals (dead members) are never held. Passes run at least every
    `LEASE_RENEW`, so a held join lands within one hold plus one pass;
  - sizes seats with the held joiners (`members + pending`), so a cold start does not shrink the
    seats that enrollment needs;
  - bootstrap: with no previous membership there is no hold, the first pass publishes an empty
    membership, and the first Pods to report join on the next pass. Nodes fetch publications
    without being members, so nothing needs a member to bootstrap.
- `src/app.rs` `membership_usable` (line 2625): a worker is ready (`/readyz`) only when its
  applied membership lists the node. Between opening its listener and applying the version that
  lists it, a node cannot route a read (`Membership::position` fails with
  `IncompatibleMembership`, `src/topology.rs:273`, and peers reject requests from non-members,
  `src/peer/server.rs:801`).
- `src/app/native.rs`: a node not yet listed starts without native rails instead of failing
  startup (`IncompatibleMembership` from `native_publication`, lines 179 and 210), and
  `poll_native` activates them from the first publication that lists it.

Why the hold scales: every version costs each client a reply, and a client two or more versions
behind gets the full image (about 120 bytes per member) instead of a delta. With a fixed 10 s
hold, `full-failover` at 50k published 33 versions during enrollment, with 646,517 full
fallbacks, 2.9 TB of seat replies, and 100,595 `StaleMembership` replies (8,589 failed reads).
Sizing by the membership alone gave 19 versions (1.2 TB, 2,631 failed reads), because the
membership is small early in a cold start. Sizing by eligible Pods gives 9 (689 GB, 1,756).

Simulator model:

- `cluster-control/src/step/cluster.rs`: Pods carry the report from creation only with
  `Settings::report_at_start` (the old dataplane). `ControlPlane::report_serving` (line 285)
  writes it (one API write per process start; a new annotation is one watch event) and wakes one
  debounced primary pass (`Ev::Wake`), as the production Pod watch does.
- `sim/src/control.rs`: a client reports when it first applies a publication after a (re)start
  (line 1066), which is when production opens its listener. Control knob `report_at_start`
  (default false) restores the old model; with it the binary reproduces the base exactly (5k
  `full-cold-start`: same 99 failed reads, 1,312 link failures and 7,014 origin requests).
- `sim/src/node/world.rs`: a node reads only when its adopted membership lists it (line 892,
  `reads_skipped_not_member`), matching production readiness. Before, the check used the latest
  committed version, which is the same thing only when every node is listed from the start.

### Tests

- `authority::members`: `new_nodes_join_once_serving` (gate, attributes from the report, members
  kept without a report, no pending without a Pod) and `hold_defers_only_new_nodes` (dead
  members still leave during a hold).
- `step::primary`: `unreported_pods_wait_but_size_seats` (empty bootstrap version, seats sized as
  if everyone were a member, first reporter joins), `join_hold_batches_new_members` (boundary,
  new primary, removals not held), `join_hold_grows_with_the_membership`,
  `join_hold_is_sized_by_eligible_pods`.
- `step::cluster`: `pods_join_once_serving_in_batches` (nobody listed until reports, joins
  batched, crash and restart cause no version).
- `tests.rs` (`cold_start_elects_commits_serves_and_fails_closed`): fixture Pods carry a report,
  as serving dataplanes do.
- Dataplane: `managed_report_waits_until_serving`, `serving_without_rails_reports_nothing`,
  `membership_is_usable_only_when_it_lists_the_node`,
  `native_activates_once_membership_lists_the_node`; `local_configuration_cannot_replace_missing_local_membership`
  now expects startup to continue without rails (it expected `IncompatibleMembership`).
- `combined::tests::cold_start_members_serve_before_they_are_routable` (300 nodes): with
  `report_at_start` there are unready-member links and failed reads; without, none, every
  Pod reports once, and versions stay within one per hold. It fails on the old model.

### Evidence

Release build, seed 1, `--config '{"request_rate": 0.005}'`. "Off" is the same binary with
`"control": {"report_at_start": true}`. `full-cold-start` runs here move the 1 s storm past the
end (`storms` with `at_ns` 1e14) to measure background reads during enrollment; with the storm,
50k `full-cold-start` fails 697k reads in the storm window (Known issue 16, capacity track, whose
simulator fix is not on this branch). No run had a hard invariant failure.

| Scenario | Nodes | Model | Reads ok / failed | Failed while unready | Unready-member links (all links) | Not-ready skips | Stale skips | Origin requests (max) | Versions | Seat replies | Wall |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| cold start | 5,000 | off | 14,804 / 99 | 99 | 1,312 (1,312) | 0 | 0 | 7,014 (3) | 1 | 3.0 GB | 0:15 |
| cold start | 5,000 | on | 14,660 / 0 | 0 | 0 (0) | 228 | 2 | 7,524 (5) | 5 | 4.9 GB | 0:20 |
| failover | 5,000 | off | 33,917 / 127 | 110 | 1,402 (3,218) | 0 | 83 | 13,432 (7) | 2 | 3.0 GB | 0:42 |
| failover | 5,000 | on | 33,762 / 12 | 0 | 0 (1,812) | 233 | 102 | 13,534 (7) | 7 | 4.9 GB | 0:53 |
| cold start | 50,000 | off | 84,165 / 11,727 | 11,727 | 138,406 (138,406) | 0 | 0 | 27,447 (3) | 1 | 298 GB | 7:26 |
| cold start | 50,000 | on | 85,855 / 1,411 | 0 | 0 (0) | 8,266 | 17,573 | 33,891 (12) | 8 | 677 GB | 12:23 |
| failover | 50,000 | off | 272,950 / 12,759 | 12,424 | 145,222 (165,336) | 0 | 1,859 | 57,163 (9) | 2 | 298 GB | 21:24 |
| failover | 50,000 | on | 275,691 / 1,756 | 151 | 0 (19,551) | 8,659 | 19,980 | 66,620 (12) | 9 | 689 GB | 31:42 |

Reading the table:

- Links toward members that cannot serve drop to zero at both scales. The remaining failover
  link failures are the site partition (19,543) and crashed members (8).
- Failed reads: 5k cold start 99 to 0, 5k failover 127 to 12 (the rest fall in the origin error
  and partition windows, 384 to 534 s), 50k cold start 11,727 to 1,411, 50k failover 12,759 to
  1,756. None of the remaining 50k cold start failures happen while a listed member cannot
  serve. They come from the join versions: a requester that already holds a new version skips
  owners that do not yet (17,573 stale-destination skips), and when every owner of a page just
  joined the walk is exhausted (new Known issue 19).
- "Not-ready skips" are reads a node did not issue because no membership it held listed it yet.
  This is the price of the fix: a new node is not ready for up to one hold plus one pass plus
  propagation after it starts serving (by the constants, about 20 s plus propagation at 5k and
  60 s plus propagation at 50k; inferred, not measured per node). In production
  `/readyz` is false meanwhile; a client that ignores readiness gets an immediate
  `IncompatibleMembership` failure rather than a read deadline. Before the fix the node was
  "ready" but its reads failed or routed to owners that could not serve.
- Costs (new Known issue 20): pages move from early members to later ones as the membership
  grows, so origin requests rise (50k cold start 27,447 to 33,891, worst page 3 to 12; 50k
  failover 57,163 to 66,620, worst page 9 to 12), and clients that fall two versions behind get
  full images (50k: 298 GB to about 680 GB of seat replies, from 0 to about 128k full
  fallbacks). Wall
  time rises with the extra versions' placement work.
- The report adds no API calls: every dataplane already patched it once per process start; it
  now patches after enrollment instead of before. API 429s are unchanged (467,296 vs 467,317 in
  50k failover).

Resolved bounds:

- No member is listed before its dataplane opened its peer listener, provided the dataplane
  reports only then (this branch). `unready_member_links` is 0 in every run above, and the
  combined test enforces it.
- Joins add at most one membership version per `join_hold_secs` (10 s up to 10,000 Pods, then
  one second per thousand, at most 60 s), and a serving node is listed within one hold plus one
  pass.
- Restarted members keep their place (the report outlives the process), so they are not
  covered: peers fail toward them until they re-apply a membership, bounded by link backoff as
  before. Measured: 0 such links (`unready_member_links_restarted`) in every run.

### Deviations and open items

- The data plane still treats a link failure to a member that cannot serve like any other
  failure; with the control fix it no longer happens in these scenarios, so nothing was changed
  there.
- New Known issue 19 (join version skew failures) belongs with the route and handoff work
  (issues 3 and 12): for example, a requester could fall back to the previous version's owners
  when every owner in its version is a stale destination.
- New Known issue 20 (origin fetches and seat egress of join versions) belongs with Known issue 6
  and with control deltas: seats keep only a one-version delta, so a client two versions behind
  gets a full image.
- A rollout that replaces a member's Pod keeps the member and switches it to the new Pod's IP at
  once, before the new Pod serves, as before (the Node is in the previous membership). Gating that
  would leave the old, dead endpoint listed instead.
- Production readiness changed: an operator relying on `/readyz` sees a new node become ready one
  membership version later than before.


## Round 4: repeat

Track repeat takes Known issue 15 and the "still held" class from "Round 4: handoff" (`copy_held`:
origin requests for a page while the node that fetched it last still holds it). Commits on
branch `racer-sim4-repeat`: beed76792 (repeat classes), 34a4bf93f (skip causes), ba709b40e (origin
debit), ad4e26225 (a test bound after the debit), 293eaa5d4 (invariant and failover test), plus
this section. All changes are in the simulator: no production or racer-core behavior changed (see
"Fixes tried and rejected").

### Root cause

The simulator was instrumented to record, for every repeated origin request, which node held the
page, its rank, whether it was down or cut off from the fetcher, the fetcher's rank and budget,
and every probe of the fetcher's walk with how it ended (temporary counters first, then the
committed classification below). Cited at 0d282020f.

1. The merge guess was wrong: partitions are a small part. In 50k `full-failover` (seed 1) 4,375
   of the 5,473 `copy_held` repeats were fetched by rank 0 while an owner above rank 0 held the
   page, and in 5,348 the fetcher could reach rank 0 at fetch time. Rank 0 never asks its
   successors: an owner walk only visits predecessors and handoff targets
   (`CandidateWalk::new`, `core/src/read/walk.rs:239-257`), so rank 0 goes straight to origin.
2. The copy sits above rank 0 because that owner fetched it itself earlier: an owner above rank 0
   reaches origin only after its probe of rank 0 ended without a copy (origin authority needs one
   piece of evidence per predecessor, `validate_origin_authority`). Each such fetch, a skip, leaves
   an orphan copy that rank 0 does not know about. Its next cold miss fetches the page again;
   after that rank 0 holds it, so one skip causes at most one such refetch.
3. Why rank 0 was skipped, at 50k (orphans charged to the skip that created them, of 4,531):
   - `failed` 1,818: rank 0 answered the delegation with `Unavailable` (`peer_error`,
     `core/src/read/policy.rs:33`; classified transient, `core/src/read/candidates.rs:191`):
     its own origin request failed in a 5xx burst, so the owner's fallback fetch was an origin
     retry from another node.
   - `not_serving` 1,024: rank 0, or a relay on the route, is a member that does not serve yet
     (Known issue 14).
   - `route` 948: no route within the probe's links.
   - `copy_only` 595: the owner could not fund a delegation (`delegated` needs `left + 2` credits,
     `core/src/read/walk.rs:351-355`), asked rank 0 copy-only, got a miss, and fetched itself.
   - `partition` 145.
   The other 937 `copy_held` repeats at 50k (and 4 raced) are skips themselves: an owner above
   rank 0 fetched while rank 0 held the page but could not be reached.
4. Two `copy_lost` repeats per run were really failed origin requests: the old classification
   called every repeat whose previous fetcher held nothing `copy_lost`, and a fetch that failed
   leaves nothing (370 of 372 at 5k, 1,915 of 1,918 at 50k).
5. Simulator fidelity (new Known issue 22): production debits one attempt from the page budget
   after validating origin authority (`budget.begin_attempt`, `src/read/fill.rs:1810`). The
   simulator issued the origin request without it, so an owner whose budget was spent still
   fetched (135 such requests in 5k `full-failover`).

### Changes

- `NodeWorld::classify_repeat` (`sim/src/node/world.rs:3169`) splits repeats into
  `after_owner_move`, `after_failure` (the previous request failed), `concurrent` (it is still in
  flight), `copy_lost` (it succeeded and the copy is gone) and `copy_held`. `copy_held` is split by
  why the fetcher did not get the copy (`held`): `raced` (the copy landed after the fetch started),
  `unreachable`, `busy`, `unfunded`, `missed`, or `unasked` (the holder was not a probe target).
- Every walk probe records how it ended (`Asked`, `sim/src/node/world.rs:326`) and, for transport
  failures, why (`Cut`, `:343`, set by `note_cut`, `:2579`): a node that does not serve yet, a
  partition, a down node, no route, an open circuit. Origin requests by owners above rank 0 are
  counted by why rank 0 did not serve (`origin_per_page.skips`), and a `copy_held` repeat whose
  holder fetched as such a skip is charged to that cause (`repeats.orphan`).
- The simulator debits the origin attempt like production (`sim/src/node/world.rs:3108`,
  counter `origin_unfunded`). The 120 to 300 node lagging-predecessor test now sees up to 3 owner
  stale skips (bound 4, was 2; 48 to 52 before Known issue 12).

### The bound (issue 15)

For a page whose rank 0 does not change, a repeat while the previous fetcher still holds the page
is either a skip (an owner above rank 0 fetched after its probe of rank 0 failed: rank 0 or the
route to it unreachable, not serving, partitioned, busy, its own acquisition failed, or the owner
could not fund a delegation), or the one rank 0 refetch of a copy such a skip left behind, or a
fetch that raced the copy. Each skip is the previous fetch of at most one later request, so:

- `copy_held <= skips + orphans` and `orphans <= skips`, hence `copy_held <= 2 * skips`;
- the remaining class, `unexplained` (rank 0 fetching while a copy it or its delegate fetched is
  still held, with no skip behind it), is zero. It is the hard invariant `origin_repeat_explained`,
  checked on every repeat at every scale, so every run below enforces it.

Inherent (needs a failure the walk cannot see past): `failed` (an origin error at rank 0;
retrying elsewhere is the availability the fallback exists for), `partition`, `down`. Avoidable
elsewhere: `not_serving` is Known issue 14 (join track); with those members not ranked until they
serve, the skip becomes an ownership move, which handoff probes and the `after_owner_move` bound
cover. Budget-limited: `route` and `copy_only` are new Known issue 21.

Tests: `combined::tests::failover_repeats_are_skips_or_their_orphans` (200 and 400 nodes,
`full-failover` faults: classes sum to the repeats, causes sum to `copy_held`, `unexplained` is 0,
`orphans <= skips`, `copy_held <= skips + orphans`); `repeat_origin_fetches_are_classified_and_bounded`
and `joined_owners_copy_from_previous_owners` now sum all five classes and check the `held` split.

### Fixes tried and rejected

Each was measured at 5k (release, seed 1) and reverted.

| Change | `copy_held` ff | Origin ff | Failed reads ff | Side effect |
| --- | --- | --- | --- | --- |
| Debit only (kept) | 247 | 13,171 | 77 | - |
| Rank 0 asks successors copy-only before origin, within budget | 266 | 13,196 | 59 | `storm` failed 18,013 to 23,856, `zipf` 4,627 to 5,589 |
| Plus readers fund a first-visited rank 0 with 8 links and 2 more credits | 211 | 13,145 | 57 | `storm` 31,362 failed, `zipf` 11,873, `churn` ok 167k to 157k |
| Delegate without keeping the owner's own origin credit | 175 | 13,093 | 113 | +36 failed reads in 5xx bursts |
| A delegation answered `Unavailable` ends the owner's walk (reader retries) | 181 | 13,099 | 100 | +23 failed reads, +224 retries |
| Delegation route up to 8 links | 247 | 13,171 | 77 | no change (route skips are not on that route) |

(The first three rows predate the debit and compare with 264 / 13,193 / 61.) Successor probes found
a copy 19 times in 4,285 probes at 5k and 39 in 16,713 at 50k: rank 0 inherits 1 to 6 links from
its requester, too few for a route to a successor (2,817 of its successor probes failed route
search at 5k), and when reached by delegation it holds only its origin credit. Every change that
moves the copy to rank 0 does so by giving up the owner's own fallback attempt, which costs reads
when rank 0 fails.

### Evidence

Release build, seed 1, `full-*` with `{"request_rate": 0.005}`, `churn` with defaults. Before is
the branch after beed76792 (classification only; origin numbers equal the wave 1 merge), after is
293eaa5d4. Origin: total, max per page, pages fetched more than once. `read_ns` in ms.

| 5,000 nodes | Origin before | after | Reads ok / failed before | after | p50 / p99 before | after |
| --- | --- | --- | --- | --- | --- | --- |
| full-failover | 13,193, 7, 486 | 13,171, 5, 471 | 34,004 / 61 | 33,987 / 77 | 60 / 256 | 60 / 288 |
| full-churn | 13,640, 5, 987 | 13,635, 5, 992 | 34,199 / 76 | 34,193 / 78 | 60 / 144 | 60 / 144 |
| churn | 1,041, 2, 17 | 1,041, 2, 17 | 167,252 / 1,503 | 167,252 / 1,503 | 384 / 704 | 384 / 704 |

Repeat classes after (`after_owner_move` / `after_failure` / `copy_lost` / `copy_held`, then skips
and `unexplained`): `full-failover` 0 / 376 / 2 / 247 (skips 791, orphans 180: `failed` 82,
`copy_only` 41, `not_serving` 31, `partition` 19, `route` 7; unexplained 0), `full-churn`
1,044 / 52 / 2 / 18 (skips 89, unexplained 0), `churn` 13 / 0 / 0 / 4 (skips 16, all `busy`;
unexplained 0). The added failed reads in `full-failover` are the debit: fills that reached origin
without a credit now fail as in production.

50,000 nodes, `full-failover`, run one at a time next to other tracks' runs. Before is the branch
after 34a4bf93f (skip causes, no debit; origin numbers equal the wave 1 merge), after is 293eaa5d4.

| | Before | After |
| --- | --- | --- |
| Origin requests, max, pages>1 | 52,152, 8, 5,748 | 51,585, 8, 5,320 |
| Repeats move / failure / lost / held | 0 / 1,915 / 1 / 5,472 | 0 / 1,924 / 1 / 4,985 |
| `held`: unasked / unreachable / raced | 4,375 / 1,093 / 4 | 3,930 / 1,052 / 3 |
| Skips (fetches above rank 0) | 7,903 | 7,280 |
| Orphans: failed / not serving / route / copy-only / partition | 1,818 / 1,024 / 948 / 595 / 145 | 1,712 / 931 / 944 / 336 / 129 |
| `unexplained` (hard invariant) | not measured | 0 |
| Reads ok / failed | 304,333 / 6,936 | 303,786 / 7,315 |
| `read_ns` p50 / p99 (ms) | 64 / 416 | 64 / 416 |
| Wall, peak RSS | 21:27, 2.82 GiB | 21:34, 2.82 GiB |

The bound holds at 50k: 4,985 `copy_held` <= 7,280 skips + 4,052 orphans, orphans <= skips, and no
hard invariant failed. `copy_held` is still 9.7% of origin requests. Of its 4,052 orphans, 45%
have inherent causes (rank 0's own origin failure 42%, partitions 3%), 23% belong to issue 14
(members that do not serve yet) and 32% to the budget (issue 21: route 23%, copy-only 8%); the
other 930 repeats are skips of a rank 0 that held the page but could not be reached. The 379 more
failed reads are the debit (1,576 fills reached origin without a credit and now fail).

## Round 4: combined (wave 2)

Branch `racer-sim4-merge2` from the wave 1 merge (0d282020f): fanout (751b85b1e through
f887fe173 plus its two doc commits; its cherry-picks of capacity commits were skipped), join
(789725d9c through da9e8c8b7, without the liveness commits it was based on, which wave 1 already
has), and repeat (beed76792 through e2e1b994d). Track numbers were renumbered: fanout 71 is 16,
join 51 is 14, 72 is 17, 81 is 19, 82 is 20, repeat 101 is 21, 102 is 22.

Conflicts and resolutions (both sides' behavior kept):

- `core/src/read/walk.rs` and its tests: handoff's `previous` (probes under the previous
  membership) and fanout's `helper` are both `WalkConfig`, `Pending`, and `Probe` fields; the
  helper probe sends `previous: false`.
- `src/read/candidates.rs`: owners use `previous_owners` (handoff targets and same-rank
  predecessors); non-owners get the lazily chosen overflow helper; the request goes to the
  probe's membership (previous or current) and its destination.
- `sim/src/node/world.rs`: helper probe counters and repeat's `asked` record both run on a probe
  outcome; the link check keeps repeat's `Cut` causes and join's `unready_member_links` counter.
- `sim/src/control.rs`: enroll's `auth`, `auth_qps`, `auth_burst` and join's `report_at_start`.
- No Cargo.toml changes; no duplicate dependency lines.

Issue 18: fanout removes the relay shedding in `storm` (18,013 to 461 failed reads, 692,964 to
14,756 relay rejections) but not in `zipf`, so 18 stays open with the new numbers.

Repeat invariant: `origin_repeat_explained` holds (`unexplained` 0) in every run below, and
`failover_repeats_are_skips_or_their_orphans` passes; join versions and helper copies needed no
new class. Join versions show up as `after_owner_move` repeats (Known issue 20). No invariant was
relaxed.

Verification: `cargo fmt --check`, `cargo clippy --workspace --all-targets -D warnings` and
`cargo test --workspace` pass; all 16 scenarios run at 200 nodes, 1 s with no hard invariant
failure.

Release, seed 1. `storm`, `zipf`, `churn` use scenario defaults; `full-*` use
`{"request_rate": 0.005}`. Origin is total, max per page, pages fetched more than once. Repeats
are `after_failure` / `after_owner_move` / `copy_held` / `copy_lost` / `concurrent`. Unready links
is `unready_member_links` (links toward members that do not serve yet).

| Run | Reads ok / failed | Origin | Repeats | Relay rejections | Unready links | Wall | Peak RSS |
| --- | --- | --- | --- | --- | --- | --- | --- |
| 5k storm | 268,449 / 461 | 1,024, 1, 0 | 0 / 0 / 0 / 0 / 0 | 14,756 | 0 | 1:28 | 1.28 GiB |
| 5k zipf | 149,525 / 4,373 | 42,216, 3, 4,171 | 0 / 0 / 565 / 0 / 3,734 | 339,136 | 0 | 2:14 | 1.53 GiB |
| 5k churn | 172,701 / 450 | 1,040, 3, 15 | 0 / 9 / 7 / 0 / 0 | 16,932 | 0 | 1:12 | 1.00 GiB |
| 5k full-cold-start | 94,829 / 1 | 6,946, 2, 52 | 0 / 47 / 5 / 0 / 0 | 0 | 0 | 0:43 | 0.55 GiB |
| 5k full-failover | 33,804 / 23 | 13,075, 7, 458 | 408 / 59 / 169 / 2 / 0 | 0 | 0 | 0:53 | 0.35 GiB |
| 5k full-churn | 33,988 / 4 | 13,653, 6, 991 | 51 / 1,061 / 13 / 0 / 0 | 0 | 0 | 0:52 | 0.30 GiB |
| 50k full-cold-start | 908,373 / 1,103 | 29,336, 5, 2,639 | 0 / 2,627 / 330 / 0 / 1 | 0 | 0 | 20:38 | 4.21 GiB |
| 50k full-failover | 299,717 / 1,527 | 52,461, 9, 6,136 | 2,138 / 3,498 / 2,451 / 1 / 0 | 0 | 0 | 28:37 | 2.95 GiB |

Against wave 1 (`/tmp/opencode/m4/`): 50k `full-cold-start` failed reads 6,665 to 1,103 (link
failures 79,581 to 0, all enrolled at 172 s against 171 s); 50k `full-failover` failed reads 6,936
to 1,527, link failures 97,579 to 20,019 (20,017 of them partitions), `copy_held` 5,473 to 2,451,
worst page 8 to 9 (join versions, Known issue 20). Logs and reports: `/tmp/opencode/m4b/`.

## Round 4: relay (wave 3)

Scope: Known issue 18 (5k `zipf` sheds on the `Relay` quota) and the fanout tuning items left by
"Round 4: fanout" (the 32-body shed threshold tuned on 1 MiB pages, hedges never asking the
helper, native transfers not counted). Branch `racer-sim5-relay` from 3771e5c3f.

### Root cause (issue 18)

The four candidates from the brief, checked against the code and a 5k `zipf` run with temporary
instrumentation (seed 1, release):

- Routing concentration: no. Of 169,568 relay requests refused at a relay (the report's
  `rejections.relay` shows 339,136 for the same run), 4,973 of 5,000 nodes refused at least one;
  the busiest refused 119, the mean was 34. Rejections were spread evenly.
- Mis-sized quota or production hold: no. Production holds the reservation from the relay's
  `reserve` until the body has passed (`src/peer/server.rs`, `relay.reservation`, in the opaque
  path; `Relay::forward_inner`, `src/peer.rs:383`, in the default path). That is what the quota is
  for: it bounds the relayed exchanges (and their pipes or buffered pages) a worker carries. The
  simulator's 16 per worker and 2 workers per node match the production per-worker default
  (`core/src/admission.rs:150`).
- Origin capacity: no. With `origin_bps` raised tenfold the run still failed 3,525 reads with
  280,954 relay rejections.
- Simulator modeling error: yes. Each relayed body held a relay for a mean of 191 ms of streaming
  (p50 184 ms) for a 1 MiB page that needs 0.84 ms per 10 Gbit/s link, while the average node sent
  under 4% of its NIC. The owner's and the relay's transmit queues (`tx_free`) were both about
  134 ms ahead at p50. `Fabric::transfer_until` (`sim/src/resources.rs:198-210` on the base)
  reserved each later hop's NIC as a slot at the chunk's arrival time: `start = ready.max(tx_free)`,
  then `tx_free = start + send`. A relay forwarding a body still queued at a busy owner therefore
  set its own `tx_free` to the body's future arrival, and every send it made afterwards, including
  its own replies as an owner, waited behind that idle gap. Those replies then made the relay a
  busy sender for the next relay, so backlogs spread through the cluster and relays held their
  `Relay` reservation for hundreds of milliseconds. The receive side already avoided this
  ("work-conserving backlog", the round 2 slow-sender fix); the transmit side did not. A real NIC
  sends other streams during the gap.

### Changes

- `Fabric::transfer_until` (0eba3d9c8): the transmit side is a work-conserving backlog like the
  receive side. A hop's NIC backlog grows by the chunk's service time from the transfer's issue
  (`tx_free = max(tx_free, now) + send`), and the chunk leaves no earlier than it arrives
  (`end = max(ready + send, tx_free, rx_free)`). With every hop ready at issue (one link) this is
  the old FIFO slot. The abandon cutoff still stops any chunk that would start at or after it.
- Relay hold metrics (57c03b03c): `relay_hold_wait_ns` (first relay's reservation until the
  destination's answer is ready) and `relay_hold_stream_ns` (answer ready until the body has
  passed or was abandoned).
- Hot-page threshold in bytes (9046daac7): `HOT_PAGE_BODIES` (32) became
  `hot_page_bodies(page_bytes)`, `HOT_PAGE_QUEUE_BYTES` (32 MiB) of queued bodies of one page, at
  least `HOT_PAGE_MIN_BODIES` (2) (`core/src/read/fanout.rs`). That is 32 bodies for the
  simulator's 1 MiB pages (unchanged) and 2 for production's 16 MiB pages (was 32, 512 MiB of
  queued egress). `shed_hot_page` takes the threshold; production uses
  `hot_page_threshold()` (`PAGE_BYTES`, `src/peer/server.rs`), the simulator its `page_bytes`
  with an override `node.hot_page_bodies` for measurement.
- Native transfers counted (17269023f): `send_native` (RDMA) page bodies now hold the same
  `page_streaming` guard as `send_http_response`, so an owner serving over native transfers sheds
  like one serving over HTTP.
- Hedges still do not ask the helper. Hedging is off by default (`page_hedge_slots` 0), and
  carrying "who signed the primary's `Overloaded`" through the hedge race needs a new
  continuation field in core, production and the simulator; left as documented
  (`hedged_primary_overload_does_not_ask_the_helper` still asserts it).

No wire change. No invariant relaxed.

### Shed threshold at production page size

At 1 MiB pages the threshold barely matters once the transmit artifact is gone (5k, seed 1;
failed reads, helper probes):

| Threshold (bodies) | storm | zipf | churn | storm quiet 1G 15 s |
| --- | --- | --- | --- | --- |
| 8 | 1, 78,823 | 21, 6,291 | 160, 7,433 | 7,125, 143,275 |
| 16 | 0, 64,577 | 12, 401 | 180, 3,004 | 7,107, 142,694 |
| 32 (1 MiB default) | 0, 59,056 | 17, 34 | 165, 524 | 6,721, 141,275 |
| 64 | 0, 56,434 | 15, 0 | 165, 30 | 5,416, 136,963 |

With 16 MiB pages (`page_bytes` 16,777,216) and the background rate scaled to the same bytes per
second (`request_rate` 1.25, `storm`, 8 s, 5k, seed 1) the byte rule matters:

| Threshold (bodies) | Reads ok / failed | Relay rejections | Helper probes | Read p50 / p99 |
| --- | --- | --- | --- | --- |
| 2 (16 MiB default, 32 MiB queued) | 155,548 / 152 | 7,348 | 82,505 | 184 ms / 1.88 s |
| 32 (old count, 512 MiB queued) | 152,337 / 3,406 | 7,884 | 83,132 | 469 ms / 2.15 s |

Other 16 MiB runs: a quiet 30 s storm (`request_rate` 0) fails 7,234, 7,204 and 7,180 reads at
thresholds 2, 8 and 32 (the residual helper bound from "Round 4: fanout", the same shape as the
1 Gbit/s quiet storm with 1 MiB pages). The default 2 s `storm` at the unscaled rate of 20 reads
per second is overloaded (each node asks for 320 MiB/s in the background): thresholds 1 to 32 fail
17,566 to 19,310 reads, without fan-out 72,178, and with `relay_transfers` 64 13,552.

### Tests

- `resources::tests::relay_transmit_is_not_held_by_a_body_queued_upstream` (fails before:
  the relay's later send ends about 100 chunks late).
- `contention_scenarios::relays_hold_their_reservation_only_while_the_body_streams` (300 nodes,
  Zipf over 1,024 objects): p99 relayed stream under 30 ms and no failed read; before the fix the
  p99 was 42 ms.
- `fanout::tests::hot_page_threshold_is_the_same_backlog_in_bytes` and the threshold-parameterized
  `owners_shed_only_non_exempt_requesters_of_a_dominant_hot_page` (both page sizes).
- Production: `peer::tests::opaque::page_bodies_count_toward_the_hot_page_shed_on_every_transport`;
  `protocol_socket::hot_page_owner_sheds_non_helper_readers_with_signed_overload` now runs at the
  production threshold (2).

### Evidence

Release, seed 1. "before" is the wave 2 merge (3771e5c3f, `/tmp/opencode/m4b/`); "after" is this
branch (logs `/tmp/opencode/r5-relay/`). `full-*` use `{"request_rate": 0.005}`. Origin is total,
max per page, pages fetched more than once. Repeats are `after_failure` / `after_move` /
`copy_held` / `copy_lost` / `concurrent`. Helper is probes / copies / owner sheds.

| Run | Reads ok / failed | Origin | Repeats | Relay rejections | Helper | Read p50 / p99 |
| --- | --- | --- | --- | --- | --- | --- |
| 5k storm, before | 268,449 / 461 | 1,024, 1, 0 | 0 / 0 / 0 / 0 / 0 | 14,756 | 116,951 / 82,374 / 123,769 | 301 ms / 1.01 s |
| 5k storm, after | 378,544 / 0 | 1,024, 1, 0 | 0 / 0 / 0 / 0 / 0 | 4,280 | 59,056 / 59,001 / 60,365 | 41 ms / 167 ms |
| 5k zipf, before | 149,525 / 4,373 | 42,216, 3, 4,171 | 0 / 0 / 565 / 0 / 3,734 | 339,136 | 8,713 / 4,939 / 12,570 | 201 ms / 1.48 s |
| 5k zipf, after | 202,878 / 17 | 40,880, 3, 1,897 | 0 / 0 / 191 / 0 / 1,733 | 59,758 | 34 / 34 / 34 | 46 ms / 1.48 s |
| 5k zipf seed 2, before / after | 146,270 / 5,152; 204,056 / 18 | 41,710; 40,397 | | 356,582; 55,484 | | |
| 5k zipf seed 3, before / after | 140,327 / 7,254; 202,484 / 30 | 41,826; 40,840 | | 409,242; 62,736 | | |
| 5k churn, before | 172,701 / 450 | 1,040, 3, 15 | 0 / 9 / 7 / 0 / 0 | 16,932 | 3,434 / 2,986 / 3,179 | 369 ms / 738 ms |
| 5k churn, after | 291,483 / 165 | 1,032, 2, 8 | 0 / 7 / 1 / 0 / 0 | 0 | 524 / 500 / 520 | 41 ms / 134 ms |
| 5k full-cold-start, before / after | 94,829 / 1 both | 6,946, 2, 52 both | 0 / 47 / 5 / 0 / 0 both | 0 | 82; 0 | 46 ms / 134 ms |
| 5k full-failover, before / after | 33,804 / 23 both | 13,075, 7, 458 both | 408 / 59 / 169 / 2 / 0 both | 0 | 0 | 62 ms / 268 ms |
| 5k full-churn, before / after | 33,988 / 4 both | 13,653, 6, 991 both | 51 / 1,061 / 13 / 0 / 0 both | 0 | 0 | 62 ms / 150 ms |
| 5k full-cold-start 1G 1 s, before | 56,399 / 38,430 | 6,959, 3, 63 | | 288,896 | 13,532 / 7,960 / 36,634 | 1.61 s / 9.66 s |
| 5k full-cold-start 1G 1 s, after | 63,745 / 31,084 | 6,954, 2, 60 | | 259,460 | 10,720 / 8,273 / 32,272 | 1.61 s / 9.66 s |

- `relay_hold_stream_ns` after: p50 23 ms, p99 38 ms in `zipf` (about two links' latency); before
  (instrumented) p50 184 ms, p99 369 ms. `relay_hold_wait_ns` (the destination's acquire, often an
  origin fetch or its own walk) is unchanged: p50 12 ms, p99 1.48 s.
- The remaining `zipf` relay rejections (59,758) are relays holding the reservation while the
  destination acquires the page, which is production's behavior and the quota's purpose; 17 reads
  fail. Successful reads rose by a third in `zipf` and `storm` and by 69% in `churn` because peer
  transfers no longer queue behind phantom gaps; the scenarios' arrivals are fixed, so more of them
  complete within the 2 s window.
- `full-*` at request rate 0.005 carry little relayed traffic and are identical except for 82 helper
  probes in `full-cold-start` that are no longer needed.
- 50k `full-cold-start` (`request_rate` 0.005): 908,373 / 1,103 before, 908,410 / 1,073 after;
  origin 29,336 and 29,358 (max 5 both), no relay rejections in either, wall 20:38 and 20:13, peak
  RSS 4.21 and 4.22 GiB. Its failures are join versions (Known issue 19), not relays.
- Wall and peak RSS fell slightly (5k `zipf` 2:14 and 1.53 GiB to 2:00 and 1.42 GiB).
- No hard invariant failure in any run above; the 16-scenario smoke at 200 nodes (1 s) is clean.

### Deviations and open items

- Issue 18 was a simulator artifact; production's relay hold is unchanged. The production changes
  are the byte-based shed threshold (2 bodies instead of 32 for 16 MiB pages) and counting native
  transfers.
- The 1 Gbit/s `full-cold-start` 1 s storm regressed on the wave 2 merge before this branch (new
  Known issue 23); this branch improves it but does not restore the fanout branch's result.
- `flight_leader_valid` fails in origin-overloaded `zipf` with 16 MiB pages on the base too (new
  Known issue 24).
- Hedges still do not ask the helper (off by default; see Changes).

## Round 4: flight (wave 3)

Branch `racer-sim5-flight`, based on `racer-sim5-relay` (b06192038). Resolves Known issue 24.
Evidence logs and configs: `/tmp/opencode/r5-flight/`.

### Root cause

The bug was in core `PageFlights`, which did not model a production mechanism. Production is
correct and is unchanged.

- Production retains each fill as a flight operation before submitting it (`src/read/fill.rs`,
  `retain_operation`), and every settlement passes `entry.operations.is_empty()` as the idle flag
  (`src/read/flight.rs`, `settle` and `refresh`). A leader revoked because its deadline passed or
  it was canceled therefore leaves the flight in `Draining` until the operation completes. No other
  waiter is elected in that time, and the entry is not removed (`quiescent` requires no waiters and
  no operations). The driver completes the operation, then publishes, and ignores the
  `StaleFlight` that a revoked leader gets (`let _ = flights.publish_acquired(..)`).
- Core `PageFlights` always passed `idle = true` (the old `finish`, `reject`, and `refresh`). Once
  the leader's deadline passed, the flight settled to a retry at once. When the leader detached,
  the flight was removed. A later reader then created a new flight and led a second fill while the
  first fill still waited on origin. When the first fill finished, `PageFlights::finish` rejected
  the stale leader (`validate_leader`) with `Err(Internal)`, and the simulator's
  `flight_leader_valid` check (`sim/src/node/world.rs`, `finish_fill`) counted that as a
  violation.
- This only happens when a fill outlives its leader's deadline. In the simulator that means an
  origin request still queued when the deadline passes, because the origin model reserves FIFO
  bandwidth and does not cancel (`sim/src/resources.rs`, `Origin::request`). With 16 MiB pages at
  16 times origin capacity, origin queues grow past the read deadline. With 1 MiB pages they do not.

### Changes

- `racer_core::read::flight::PageFlights` adds `retain_operation`, `complete_operation`, and
  `is_leader`, and counts retained operations per flight. Settlement, refresh, and entry removal use
  `operations == 0` as the idle flag, the same as production `Entry::operations`.
- `PageFlights::poll` checks the waiter's deadline before a recorded error, as production
  `AcquisitionWaiter::wait` checks the caller's scope first. Without this, a rejected leader polled
  after its deadline reported `OriginRejected` in core and `DeadlineExceeded` in production. The
  extended equivalence traces found this.
- The simulator retains each fill in `start_fill` and completes the operation in `finish_fill`
  before settling, as production does. It drops a revoked leader's outcome and counts it as
  `flight_stale_finishes`. `flight_leader_valid` now also covers retain and complete.
- Tests:
  - Core:
    - `expired_leader_with_retained_operation_blocks_reelection_until_complete` replays the exact
      sequence.
    - `expired_leader_without_operation_is_stale_after_new_leader` replays the old failure.
    - `operation_errors_and_completed_operation_settles_published_result`.
    - `deadline_takes_precedence_over_recorded_rejection`.
  - Production versus core:
    - `flights_match_page_flights_on_seeded_traces` (48 seeds) now retains and completes
      operations at random and requires both steps.
    - `expired_leader_with_retained_operation_drains_before_reelection` replays the issue 24
      sequence against both tables.
  - Simulator: `node::tests::expired_leaders_drain_before_a_new_fill` uses 64 nodes, a 1 MiB/s
    origin, and four storms. It fails with 4 `flight_leader_valid` violations when only
    `world.rs` is reverted, and passes after the fix.

### Evidence

5k `zipf`, seed 1, config `{"page_bytes":16777216,"request_rate":1.25,"duration_ns":8000000000}`:

| | relay branch (b06192038) | flight |
|---|---|---|
| `flight_leader_valid` violations | 376 | 0 |
| reads completed / failed | 34,929 / 230 | 31,111 / 211 |
| origin requests (max per page) | 39,669 (6) | 36,239 (4) |
| origin fills | 2,372 | 2,372 |
| stale finishes / re-elections | - / 2 | 683 / 550 |
| wall, peak RSS | 0:50, - | 0:48, 0.72 GiB |

The duplicate fills are gone: origin requests fell 8.6% and the most requests for one page fell
from 6 to 4. Completed reads fell 11% because later readers now wait for the draining flight
instead of starting another fill. Production behaves the same way, except that production cancels
the expired fill at about the deadline. The simulator cannot do that yet (Known issue 29), so its
flights drain later. A trial that canceled at the deadline without refunding origin bandwidth was
worse (origin fills 1,584) and was not kept.

1 MiB pages are unaffected: 5k `zipf` and `storm` have the same trace hashes as the relay branch
(`ef7607be6a3bb784`, `ca1b1acb8b9fc903`), so no fill outlives its leader there.

Checks: `cargo fmt --check` and `cargo clippy --workspace --all-targets -D warnings` are clean,
the workspace tests pass (3,400 tests), and the 16-scenario smoke at 200 nodes (1 s) has no hard
invariant failures.

### Deviations and open items

- Production is unchanged. The bug was in the core model and in the simulator's use of it.
- Known issue 29 is new: the simulator does not cancel an expired fill's origin request.

## Round 4: cancel (wave 3)

Known issue 29: the simulator let a fill run until origin answered after its leader's deadline
passed, while production cancels it at about the deadline.

### Root cause

What production does:

- The fill's owned scope uses the leader caller's deadline (`src/read/fill.rs:1133`).
- The fill driver cancels that scope once the caller's scope fails or the flight operation asks
  for cancellation (`src/read/fill.rs:1229-1238`). The operation asks for cancellation when the
  leader's waiter scope has expired or the leader was replaced (`src/read/flight.rs:314-325`).
- The origin adapter checks the scope at each step (`src/origin.rs`). A canceled or expired
  exchange releases its `Connection` and `Plaintext` reservations, as the test
  `real_uds_canceled_and_expired_reads_release_owned_resources` (`src/origin/tests.rs:1136`)
  asserts. Closing the connection frees the per-endpoint concurrency and stops the body transfer.

The simulator did none of this. `Origin::request` (`sim/src/resources.rs`) fixed the slot and a
FIFO bandwidth reservation when the request was issued, and nothing could take them back.
`NodeWorld::issue_origin` scheduled `OriginDone` at that time, and the fill ran until then, even
far past its scope. The flight track tried canceling at the deadline without a refund. That
wasted origin bandwidth, so it was not kept.

### Changes

- `Origin::request_tracked`, `Origin::cancel`, `Origin::finish`. Tracked requests are kept in
  issue order. Canceling a request works like this:
  - A request that has not started is removed.
  - A started request gives up its slot at the cancel time. If its body is flowing, it keeps
    only the bandwidth it already used.
  - The requests issued after it are then replayed in issue order. Each moves earlier, never
    later (a debug assertion checks this).
  - With no cancellations, the schedule is identical to `Origin::request`. A unit test compares
    200 mixed requests.
  - Each cancellation costs one scan of the outstanding requests. The moved completions go into
    a heap that is rebuilt once per cancellation, so a cancellation never schedules one event per
    moved request.
- `NodeWorld`:
  - `issue_origin` uses tracked requests. If the answer would arrive after the fill's scope, it
    schedules `OriginCancel` at the scope.
  - `origin_cancel` closes the request and fails the fill's origin step with
    `DeadlineExceeded`.
  - `OriginWake` delivers completions that moved earlier. An original `OriginDone` is ignored
    when its request was canceled or moved (`Origin::due_as_issued`).
  - A fill removed by a crash or finished with its origin request still open also closes that
    request.
- A canceled fill refreshes the flight table clock before `complete_operation`. Without this,
  the table clock, which advances only on joins and polls, still showed the expired leader as
  active. The leader would then settle the flight with `DeadlineExceeded` for every waiter,
  where production drops its outcome as stale and re-elects.
- New invariant `origin_within_scope`: no origin answer reaches a fill after its scope. New
  counter `origin_canceled`.

### Tests

- `resources::tests`:
  - `tracked_requests_match_untracked_without_cancellation`.
  - `canceling_a_transfer_releases_bandwidth_to_later_requests`: bandwidth refund, moved
    completions, delivery order, and a repeated cancel.
  - `canceling_releases_the_concurrency_slot`: an unstarted request is removed, and a running
    one frees its slot.
  - `canceling_a_queued_request_moves_only_later_ones_on_shared_bandwidth`.
- `node::tests::expired_fills_cancel_their_origin_request`: 64 nodes, a 1 MiB/s origin, four
  storms. It requires `origin_canceled > 0` and stale finishes, and `run` checks
  `origin_within_scope`. It fails before the fix because no request was canceled.
- `node::tests::expired_leaders_drain_before_a_new_fill` (flight track) still passes. It failed
  until the clock refresh above was added.

### Evidence

5k `zipf`, seed 1, 16 MiB pages (`{"page_bytes":16777216,"request_rate":1.25,"duration_ns":8000000000}`):

| | flight (f02905375) | cancel |
|---|---|---|
| reads completed / failed | 31,111 / 211 | 33,374 / 90 |
| read p99 | 5.91 s | 4.83 s |
| origin requests (max per page) | 36,239 (4) | 48,438 (24) |
| origin requests canceled | 0 | 18,468 |
| origin fills / origin bytes | 2,372 / 39.8 GB | 1,710 / 28.7 GB |
| flight stale finishes / re-elections | 683 / 550 | 18,468 / 16,219 |
| violations | 0 | 0 |
| wall, peak RSS | 0:48, 0.72 GiB | 1:15, 0.71 GiB |

Reads improve because waiters no longer wait for an expired fill to finish. Origin requests rise
because each re-elected leader starts a new request that is often canceled as well. This is what
production does, so it is recorded as Known issue 30 and not hidden in the model. Wall time rises
by 27 s, mostly from replaying the queue of about 30k outstanding requests on each of the 18k
cancellations.

At default 1 MiB pages no origin request outlives its scope. These 5k runs (seed 1) have the same
trace hashes and counters as the flight branch: `zipf` `ef7607be6a3bb784`, `storm`
`ca1b1acb8b9fc903`, `churn` `681512f470c3c7d1`, `full-cold-start` `38885a3d7aaa1c9a`,
`full-failover` `ca1fbb12e1302c50`, `full-churn` `7a6dcad8908e8cf6`, `full-lagging`
`4e6be4c16c724572`. Wall time and RSS are within noise. The 16-scenario smoke at 200 nodes (1 s)
has no hard invariant failures, and every trace hash matches the flight branch.

Checks: `cargo fmt --check` and `cargo clippy --workspace --all-targets -D warnings` are clean,
and the workspace tests pass (3,405 tests).

### Deviations and open items

- Production is unchanged. Issue 29 was a modeling gap.
- Known issue 30 (new, production): a fill is canceled when its leader expires even if other
  waiters remain. Each re-elected waiter restarts the origin fetch and discards the partly
  transferred body. Possible fixes are handing the running operation to the next waiter's scope
  or letting the fill run to the latest waiter deadline within budget. Either fix changes flight
  and fill ownership in production, so it is left open.
- 50k was not run: the change has no effect at default page size (identical hashes), and 16 MiB
  pages at 50k are not a standard configuration.

## Round 4: handover (wave 3)

Branch `racer-sim5-handover`, based on `racer-sim5-cancel` (52d4dcd8b). Resolves Known issue 30.
Evidence logs and configs: `/tmp/opencode/r5-handover/`.

### Root cause

Production canceled a fill as soon as its leader's scope ended, even when other acquisition
waiters still wanted the page:

- The fill's owned scope used the leader caller's deadline (`src/read/fill.rs`, `owned_scope`
  in `drive_elected_acquisition`).
- The driver canceled that scope when the caller's scope failed or the operation asked for
  cancellation (`src/read/fill.rs`, the `poll_fn` around `work`). The operation asked for
  cancellation once the leader's waiter scope failed (`src/read/flight.rs`,
  `FlightOperation::cancellation_requested`).
- The shared flight state then revoked the leader (`flow/src/coalesce.rs`, `State::refresh` and
  `State::revoke`). Once the canceled operation completed, the next waiter was elected and
  started a new origin request at the back of the origin queue. The body already transferred was
  lost.

### Changes

- Shared flight state (`flow/src/coalesce.rs`, `State`):
  - `elected` records the waiter elected for the current generation. `leader` is the waiter
    the acquisition currently serves.
  - `allow_handover` enables handover for the running acquisition. `elect` and `settle` reset it.
  - In `refresh`, a failed leader with handover allowed and a running operation passes the
    acquisition to the live eligible acquisition waiter with the latest deadline. That waiter is
    not marked issued. Copy waiters never adopt. With no live adopter the acquisition is revoked
    as before.
  - `validate_leader` also accepts the elected waiter, so the driver's capability can still
    publish. `revoke` marks the elected waiter, not an adopter, so abandoning a handed-over
    acquisition does not fail the adopter.
- Production:
  - The fill driver allows handover unless its caller fences on the operation (`acquire_warm`
    sets `fence_acquisition`). Its owned scope runs to
    `racer_core::read::flight::handover_ceiling`: the leader's deadline plus 4 times its
    remaining span. This is only a hard cap for the scope's timers.
  - The driver no longer cancels on the leader caller's scope when handover is allowed. It calls
    `FlightOperation::poll_cancellation`, which refreshes the entry (so an adopter can take over)
    and registers a wake at the current leader's deadline in the worker's deadline registry. The
    fill is canceled once no live waiter remains.
  - `Flights::fail` turns the elected caller's own `DeadlineExceeded` or `Cancelled` into a
    retry after a handover, so the adopter leads with its own unspent budget instead of failing.
- Core `PageFlights`: `allow_handover`, `leader_deadline`, `handed_over`, `abandon` (production
  `Drop for FlightLeader`), and `finish` uses `handover_outcome` for the same retry rule.
- Simulator: fills allow handover (knob `node.flight_handover`, default on). The origin check runs
  at the deadline of the waiter the fill serves. If another waiter adopts, the elected leader's
  read fails with `DeadlineExceeded`, as production's caller ends at its deadline, and the check
  is rescheduled. Otherwise the fill is canceled as before. Peer attempts stay within the leader's
  budget deadline. New counter `flight_handovers` and invariant `origin_for_live_waiter`.

Budgets and authority:

- Budgets never grow. Attempts and links stay charged to the elected leader's budget, and an
  origin attempt still has to begin before that budget's deadline (`begin_attempt`). The time
  after the leader's deadline is within the adopter's own scope and budget deadline, because the
  adopter's policy check (`WaiterRecord::check`) ends the handover. The adopter spends none of its
  own budget. If the work ends without a result, it leads with its full budget.
- Origin authority is unchanged. The elected leader validated it under its membership snapshot
  before the origin request began. A handover only continues that exchange on the same node.
  Before this change a completed fill was already shared with every waiter of the flight, so the
  result relies on the same authority either way.

### Tests

- Shared state: `running_operation_hands_over_to_latest_eligible_waiter`,
  `handover_requires_permission_running_work_and_a_live_waiter`, and
  `handed_over_publication_completes_and_election_resets_permission`.
- Core: `expired_leader_hands_running_fill_to_latest_waiter`,
  `handed_over_fill_deadline_retries_and_last_expiry_revokes`, and
  `handover_outcome_maps_only_caller_scope_failures`.
- Production flights: `canceled_leader_hands_running_operation_to_live_waiter`,
  `handover_ends_with_last_adopter_and_caller_scope_failure_retries`, and
  `without_handover_canceled_leader_revokes_running_operation`.
- Production fill driver: `canceled_leader_fill_driver_continues_for_remaining_caller` checks
  that the driver never sees a cancellation request and that the remaining caller completes
  without spending attempts.
- Production versus core:
  - `flights_match_page_flights_on_seeded_traces` now allows handover at random, fails with the
    elected caller's deadline, and compares `handed_over` on both tables. Both steps are
    required.
  - `expired_leader_hands_over_running_operation_on_both_tables` covers handover, retry, and
    abandonment.
- Simulator:
  - `expired_leaders_hand_their_fill_to_later_waiters` compares runs with and without handover.
    With handover there are fewer origin requests and cancellations, the most requests for one
    page does not rise, and no violations occur.
  - The Known issue 24 and 29 tests now set `flight_handover = false`, so they still cover
    draining and cancellation.

### Evidence

5k `zipf`, seed 1, 16 MiB pages (`{"page_bytes":16777216,"request_rate":1.25,"duration_ns":8000000000}`):

| | cancel (52d4dcd8b) | handover |
|---|---|---|
| reads completed / failed | 33,374 / 90 | 32,691 / 54 |
| read p99 | 4.83 s | 5.91 s |
| origin requests (max per page) | 48,438 (24) | 35,917 (4) |
| origin requests canceled | 18,468 | 2,186 |
| origin fills / origin bytes | 1,710 / 28.7 GB | 2,372 / 39.8 GB |
| flight stale finishes / re-elections / handovers | 18,468 / 16,219 / 0 | 2,186 / 0 / 13,532 |
| violations | 0 | 0 |

The same run with `node.flight_handover = 0` reproduces the cancel branch exactly (trace hash
`c0ef323f96686117`).

Origin bandwidth now goes to complete bodies, with no body discarded partway. Failed reads fall
by 40%. Completed reads fall by 2% and p99 rises. Waiters now wait for one long transfer through
the overloaded origin. Before, each re-elected leader restarted the transfer; those restarts
were mostly canceled, but freed origin bandwidth for other pages. The workload's range reads
are closed loop, so higher latency also lowers the number of reads issued. This is an inference
from `range_reads_completed` (1,263 to 1,217) and lower peer request counts; it was not
instrumented further.

At default 1 MiB pages no fill outlives its leader with an origin request outstanding. These
5k runs (seed 1) keep the trace hashes and counters of the cancel branch: `zipf`
`ef7607be6a3bb784`, `storm` `ca1b1acb8b9fc903`, `churn` `681512f470c3c7d1`, `full-cold-start`
`38885a3d7aaa1c9a`, `full-failover` `ca1fbb12e1302c50`, `full-churn` `7a6dcad8908e8cf6`, and
`full-lagging` `4e6be4c16c724572`. Wall time was 6-12% higher. Those runs overlapped with
workspace tests and clippy on the same host, so this is likely load rather than the change. The
16-scenario smoke at 200 nodes (1 s) has no hard invariant failures, and every trace hash
matches the cancel branch.

Checks: `cargo fmt --check` and `cargo clippy --workspace --all-targets -D warnings` are clean,
and the workspace tests pass (3,417 tests).

### Deviations and open items

- Production behavior changed as approved. Wire format is unchanged.
- Warm gates (`acquire_warm`) do not hand over, because their caller fences on the operation.
- The handover ceiling (`HANDOVER_SPANS = 4`) is a fixed bound. It was not tuned, because the
  live-waiter check normally ends the fill first.
- 50k was not run: 1 MiB scenarios are unchanged (identical hashes), and 16 MiB pages at 50k are
  not a standard configuration.
- `origin_repeats_concurrent` stays high in 16 MiB `zipf` (18,275). These are concurrent
  requests for one page from different nodes, not restarts by one flight, so they are outside
  this issue.
## Round 4: orphan (wave 3)

Known issue 21. Branch `racer-sim5-orphan` from 3771e5c3f. Commits: a6c0d74a9 (racer-core
`read::successor`), b63e2d409 (simulator model), d80e0293a and 2f5911b11 (production and tests),
bc66e9100 (simulator test), 6d63164f5, d812b6b67, 6b325b87e (notes without ranking), plus this
section.

### Root cause

An owner walk visits only its predecessors and handoff targets (`CandidateWalk::new`,
`core/src/read/walk.rs:280`), so rank 0 never asks a successor. An owner above rank 0 delegates
only with `left + 2` credits (`walk.rs:455`); otherwise it asks rank 0 copy-only, misses, and
fetches (`copy_only`), or rank 0's own fetch fails and the owner falls back (`failed`). Either way
the copy lands above rank 0 and rank 0 fetches again on its next cold miss.

Instrumented 5k `full-failover` runs showed why the obvious fix (rank 0 asks the successor) does
not work: every request that reaches rank 0 in these cases carried one credit (a delegation) or
two to six links, and nine in ten hint probes from rank 0 failed route search. Owner requesters
cannot help either: rank 2 keeps one link for its rank 1 probe (`walk.rs`, delegation route first),
so deferring rank 2 to rank 1 made rank 2 fetch the page itself (`copy_only` skips 335 to 480 at
20k). Readers, however, visit every owner with their own eight links.

### Change

- `racer_core::read::successor`: `SuccessorHints` (per node, at most 4,096 pages, two nodes per
  page, 600 s), `should_note`, `successor_deferral`, `MAX_DEFERRALS` (8).
- Noting: an owner answering a copy-only page probe with `Miss` notes the requester without
  ranking; a failed page `Acquire` notes the requester when the ranking already cached by the
  owner's own walk shows both as owners (`src/read.rs` `note_successor`, simulator `note_hint`).
  Neither path ranks or touches the placement cache.
- Deferral: an owner without a copy that holds a hint naming another current owner answers a
  non-owner's page `Acquire` with `Miss` (production `defer_to_successor` in
  `Coordinator::serve_peer`, simulator `defer_to_successor` in `serve`). Owners are never
  deferred. A hint defers at most 8 requests. No wire change: `Miss` is an existing response, and
  a deferral never reaches origin, so origin authority and budgets are unchanged.
- Telemetry: `racer_successor_hints_total`, `racer_successor_deferrals_total`.
- Simulator switch `node.successor_hints` (default on).

Cost: a deferred reader whose holder lost the copy, or whose fetch failed, takes one more owner
probe and sometimes exhausts its walk and retries. 5k `full-failover`: `walk_exhausted` 558 to
615, failed reads 23 to 36; 50k: 12,192 to 12,575, failed reads 1,527 to 1,542.

### Tests

- racer-core: `successor::tests` (table, TTL, eviction, charge, `should_note`, deferral rules).
- Production (`src/read/tests/peer_copies.rs`): `rank_zero_defers_readers_to_the_owner_it_missed`
  (fails without deferral), `rank_zero_never_defers_owner_requesters`,
  `rank_zero_never_defers_to_a_reader`, `rank_zero_without_hints_acquires_for_readers`.
- Simulator: `combined::tests::failover_readers_are_deferred_to_orphan_holders` (400 nodes, seed
  2: `copy_only` plus `failed` orphans 152 without hints, 104 with).

### Evidence

Release, seed 1, same configs as wave 2. Before is wave 2 (`/tmp/opencode/m4b/`), after is this
branch (`/tmp/opencode/r5o/final/`). Origin: total, max, pages>1. Orphans: all, then
`copy_only` / `failed`.

| Run | Reads ok / failed | Origin | `copy_held` | Orphans | Deferrals | Wall |
| --- | --- | --- | --- | --- | --- | --- |
| 5k storm before | 268,449 / 461 | 1,024, 1, 0 | 0 | 0 | - | 1:28 |
| 5k storm after | 268,449 / 461 | 1,024, 1, 0 | 0 | 0 | 0 | 1:23 |
| 5k zipf before | 149,525 / 4,373 | 42,216, 3, 4,171 | 565 | 0 | - | 2:14 |
| 5k zipf after | 150,570 / 3,851 | 42,148, 3, 3,985 | 516 | 0 | 873 | 2:07 |
| 5k churn before | 172,701 / 450 | 1,040, 3, 15 | 7 | 0 | - | 1:12 |
| 5k churn after | 169,843 / 451 | 1,041, 2, 17 | 7 | 0 | 0 | 1:08 |
| 5k full-cold-start before | 94,829 / 1 | 6,946, 2, 52 | 5 | 5 (1 / 0) | - | 0:43 |
| 5k full-cold-start after | 94,828 / 2 | 6,947, 2, 53 | 6 | 4 (1 / 0) | 42 | 0:40 |
| 5k full-failover before | 33,804 / 23 | 13,075, 7, 458 | 169 | 97 (30 / 42) | - | 0:53 |
| 5k full-failover after | 33,781 / 36 | 13,050, 6, 450 | 150 | 76 (14 / 34) | 252 | 0:51 |
| 5k full-churn before | 33,988 / 4 | 13,653, 6, 991 | 13 | 10 (4 / 4) | - | 0:52 |
| 5k full-churn after | 33,987 / 4 | 13,662, 6, 998 | 21 | 10 (0 / 8) | 189 | 0:48 |
| 50k full-failover before | 299,717 / 1,527 | 52,461, 9, 6,136 | 2,451 | 1,679 (228 / 676) | - | 28:37 |
| 50k full-failover after | 299,710 / 1,542 | 52,304, 7, 6,099 | 2,305 | 1,481 (103 / 565) | 2,143 | 30:09 |

Peak RSS is unchanged (50k 3.1 GiB). `unexplained` is 0 and no hard invariant failed in any run;
all 16 scenarios at 200 nodes, 1 s, pass. 5k `churn` completes 2.3% fewer reads at seed 1
with no deferral at all (169,843 against 172,701); seeds 2 and 3 complete 2.4% and 1.2% more than
without hints, so this is run-to-run sensitivity, not a cost.

### Deviations and open items

- Issue 21 is only partly resolved: delegation-led refetches and orphans of owners that never
  reached rank 0 (`route` 448, `partition` 154, `busy` 208 at 50k) remain. Fixing them needs a
  way for rank 0 to learn of the copy without a route to it (for example the orphan owner pushing
  it), which is a protocol change.
- New Known issue 27: `app::tests::dst::dst_generated_native_traffic_churn_oracle` fails at seed 1
  on this branch with "link setup stalled": after one deferral changes the run's trajectory, node
  `0d` holds a `Ready` RDMA link to `10` while `10` reports `Idle`, and `connect_links` never
  recovers. Seeds 2 to 14 pass on both branches and base passes seed 1. Since resolved: a DST
  harness bug, not production (see the triage below and Known issue 27).

### Known issue 27 triage

Tracing link opens, accepts, and breaks for seed 1 showed `0d` opened epoch 7 to `10` at step
24 and `10` accepted it, then step 28 (`InflightCrash`) crashed and restarted `10`. The new
owner has no links, but `0d`'s link stays `Ready`: nothing used it, and an RC queue pair only
learns that its peer is gone when it posts (the simulator returns `RETRY_EXC_ERR`). The
deferral only changed which node the crash and the next `NativeFault` picked. `connect_links`
asked only `0d` to set up, and `Links::wants` is false for a `Ready` link, so it spun. The
production paths both recover (the restarted peer's setup replaces the stale link, and first use
breaks it); unit tests in `src/rdma/links/tests.rs` pin both. The harness now lets the side
that is not ready initiate. The native oracle passes seeds 1 to 15 and 17 to 40; seed 16 fails at
step 0 with a `RequestContext` leak that base also has (new Known issue 28).

## Round 4: join2 (wave 3)

Known issues 19 (join versions cause `StaleMembership` skips and failed reads) and 20 (join
versions cost origin fetches and seat egress). Branch `racer-sim5-join2` from racer-core-sim
3771e5c3f. Nothing was cherry-picked.

### Root causes

Seat egress and full-image fallbacks (issue 20, control half):

- The simulator split `ctl_full_fallbacks` by cause (`sim/src/control.rs`, `write`): client
  skipped a version (`_skipped`), client holds the first, empty bootstrap publication (`_first`),
  image without a delta from the client's version (`_no_delta`), or anything else (`_other`).
  Before the fix, every fallback at 5k (11,308 in `full-cold-start`) and 96,533 of 97,931 in 50k
  `full-failover` were `_no_delta` with a one-version gap. No client skipped a version, no base
  hash mismatched, and no delta hit the 4 MiB cap (`MAX_DELTA_BYTES`).
- Cause: `plan_pass` (base `cluster-control/src/step/primary.rs:246`) built a new `Image` on every
  primary pass from the previous image. When nothing changed, the previous image has the same
  sequence, and `Image::new` builds a delta only from an older one
  (`cluster-control/src/authority/publication.rs:148`), so the new image had none. The cluster
  step model installs every planned image (`cluster-control/src/step/cluster.rs:861`), so after
  the first idle pass (at least every `LEASE_RENEW`) every client one version behind got the full
  image.
- Production was not affected in its bytes: the primary installs a planned image only when the
  record changed (`install_image`, `cluster-control/src/primary.rs:679`), and seats build their
  own images from their previous one (`cluster-control/src/seat.rs:809`). It did re-encode the
  image and its delta on every pass. So the extra seat egress was a simulator-model artifact in
  the shared step code, fixed there.
- What remains is inherent: a client that applies every version receives each member once, so
  the total is about one full image per client, which is what the single-version model sent
  before the join gate (298 GB at 50k). The `_first` fallbacks are clients of the empty bootstrap
  version, whose delta is no smaller than the image.

`StaleMembership` skips (issue 19):

- A version reaches clients over a propagation window set by seat bandwidth (at 50k, the mean
  client lag `ctl_client_lag_ns` is 8 to 9 s and p90 about 24 s). A requester that applied version
  N asks an owner still on N - 1; the owner answers `StaleMembership`; with nothing newer locally
  (`stale_action`, `core/src/read/walk.rs`), the requester records a `StaleDestination`
  (`walk.rs:690`) and asks the next owner (base `src/read/candidates.rs:1083-1088`).
- Each such probe of an owner out of visit order is funded with a partitioned child budget
  (`walk.rs:560`) whose credits travel to the destination and are not returned. When every owner
  of a page is behind, three stale probes spend the read's credits: the production test below
  shows 0 of 8 attempts left after three stale owners, and the walk ends `Exhausted`.
- The previous membership would have served: every peer resolves N - 1 during the 30 s grace
  window whether it is still on N - 1 or already on N, and under N - 1 the page's owners are
  members that held the version.

Origin repeats after an owner move (issue 20, origin half): the simulator now classifies
`origin_repeats_after_move` by whether the earlier fetcher still holds the page and whether the
new owner asked it (`sim/src/node/world.rs`, `classify_repeat`). In 50k `full-failover`, of 3,610
such repeats: 786 asked the holder and it was unreachable (partitions and crashes), 601 had the
holder as a version - 1 owner (most of them handoff targets left unasked for budget), 980 had the
holder as a version - 2 owner, and 1,240 as an owner of an older version; 3 lost the copy. In 5k
`full-churn`, 892 of 1,062 are from versions older than N - 2. Handoff (round 4, handoff) only
ranks version - 1, and peers do not keep older memberships (grace 30 s, `Snapshot::retention`),
so those holders are not found. This part is new Known issue 25.

### Changes

- `cluster-control/src/step/primary.rs` (`plan_pass`, used by production and the simulator):
  when the prepared record equals the previous image's record, keep that image (and its delta)
  instead of rebuilding it. Production stops re-encoding the image and its delta every pass.
- `core/src/read/stale.rs` (new, racer-core): `previous_fallback` decides when a destination is
  behind whether to walk the page's owners under version - 1 instead: only a non-owner (owners
  keep their own previous-membership probes and handoff targets), only once per read (not after a
  membership refresh or a fallback), only when version - 1 is still resolvable here, was replaced
  within `PREVIOUS_MEMBERSHIP_WITHIN` (10 s), and lists this node (peers reject non-members).
  `fallback_rank_allowed` refuses the walk when this node owns the page under version - 1, so
  the fallback never gives origin authority.
- `src/read/candidates.rs` (`resolve_epoch_traced`, `stale_fallback`): at the first stale
  destination, if allowed, rank the page under version - 1; when this node is not an owner there,
  abandon the current walk and walk the previous membership with the rest of the same budget;
  otherwise keep walking the current membership. No wire format change; budgets never grow.
- Simulator: the same (`stale_fallback`, `fallback_ranked`, `Then::Fallback` in
  `sim/src/node/world.rs`), node knob `stale_fallback` (default on, 0 measures without it),
  counters `stale_fallbacks`, `stale_fallback_denied`, `walk_exhausted_stale`, the
  `ctl_full_fallbacks_*` split, and the `origin_repeats_after_move_*` split.

### Tests

- `step::primary::first_pass_commits_then_settles`: a pass with nothing new keeps the image and
  its delta (fails before the fix).
- `control::tests::churn_grows_and_shrinks` (sim): `ctl_full_fallbacks_no_delta` is 0 across 21
  versions (12 before the fix). `combined::tests::cold_start_members_serve_before_they_are_routable`:
  no client skips a version.
- `racer_core::read::stale` unit tests: fallback once, only for non-owners with a recent previous
  membership listing them; never as an owner under version - 1.
- Dataplane `stale_owners_fall_back_to_the_recent_previous_membership`: after one stale owner, a
  non-owner reads from the owners under version 1 (0 and 9 s after the replacement); 11 s, or
  no version 1 listing the node, keeps one probe per owner then `Unavailable`; a fallback that
  misses ends `Unavailable`; never the origin. Fails before the fix.
  `stale_fallback_never_makes_a_previous_owner_fetch`: a node that owns the page under version 1
  keeps walking version 2 and reads from its last owner, with no origin request.
- Sim `node::tests::stale_owners_send_non_owners_to_the_previous_membership` (120 nodes, churn,
  adoption lag equal to the batch period): walks exhausted on stale destinations 1,179 to 8 and
  failed reads 53 to 3 with the fallback, asserted at least 10x and 4x fewer.

### Evidence

Release, seed 1; `storm`, `zipf`, `churn` with scenario defaults, `full-*` with
`{"request_rate": 0.005}` (the wave 2 run config). Wave 2 numbers are from
"Round 4: combined (wave 2)" (`/tmp/opencode/m4b/`). "Exh. stale" is walks that ended exhausted
after a stale skip; "Full fb." is `ctl_full_fallbacks`. Logs and reports: `/tmp/opencode/r5-join2/`.

| Run | Reads ok / failed | Origin (max, pages > 1) | Repeats fail / move / held | Stale skips | Exh. stale | Fallbacks | Versions | Full fb. | Seat bytes | Wall |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| 5k storm, wave 2 | 268,449 / 461 | 1,024 (1, 0) | 0 / 0 / 0 | 0 | - | - | - | - | - | 1:28 |
| 5k storm | 268,449 / 461 | 1,024 (1, 0) | 0 / 0 / 0 | 0 | 0 | 0 | - | - | - | 1:23 |
| 5k zipf, wave 2 | 149,525 / 4,373 | 42,216 (3, 4,171) | 0 / 0 / 565 | 0 | - | - | - | - | - | 2:14 |
| 5k zipf | 149,525 / 4,373 | 42,216 (3, 4,171) | 0 / 0 / 565 | 0 | 0 | 0 | - | - | - | 2:07 |
| 5k churn, wave 2 | 172,701 / 450 | 1,040 (3, 15) | 0 / 9 / 7 | - | - | - | - | - | - | 1:12 |
| 5k churn | 177,483 / 397 | 1,033 (2, 9) | 0 / 8 / 1 | 14,594 | 43 | 14,412 | - | - | - | 1:08 |
| 5k full-cold-start, wave 2 | 94,829 / 1 | 6,946 (2, 52) | 0 / 47 / 5 | 33 | - | - | 5 | 11,308 | 5.8 GB | 0:43 |
| 5k full-cold-start | 94,829 / 0 | 6,943 (2, 48) | 0 / 47 / 1 | 10 | 4 | 0 | 5 | 2,090 | 3.2 GB | 0:40 |
| 5k full-failover, wave 2 | 33,804 / 23 | 13,075 (7, 458) | 408 / 59 / 169 | 89 | - | - | 6 | 11,530 | 5.9 GB | 0:53 |
| 5k full-failover | 33,803 / 24 | 13,088 (6, 480) | 408 / 58 / 186 | 34 | 3 | 29 | 6 | 2,089 | 3.2 GB | 0:51 |
| 5k full-churn, wave 2 | 33,988 / 4 | 13,653 (6, 991) | 51 / 1,061 / 13 | - | - | - | 21 | - | - | 0:52 |
| 5k full-churn | 33,997 / 1 | 13,661 (6, 998) | 52 / 1,062 / 17 | 49 | 4 | 35 | 21 | 307 | 3.0 GB | 0:49 |
| 50k full-failover, wave 2 | 299,717 / 1,527 | 52,461 (9, 6,136) | 2,138 / 3,498 / 2,451 | 15,433 | - | - | 7 | 97,931 | 562 GB | 28:37 |
| 50k full-failover, intermediate | 301,836 / 959 | 52,385 (8, 5,952) | 2,079 / 3,566 / 2,173 | 8,925 | 3,863 | 3,005 | 7 | 1,398 | 302 GB | 28:40 |
| 50k full-failover | 302,582 / 479 | 52,384 (9, 5,938) | 2,059 / 3,610 / 2,109 | 4,495 | 1,239 | 1,908 | 7 | 1,398 | 302 GB | 28:30 |
| 50k full-cold-start, wave 2 | 908,373 / 1,103 | 29,336 (5, 2,639) | 0 / 2,627 / 330 | 12,800 | - | - | 6 | 103,840 | 576 GB | 20:38 |
| 50k full-cold-start | 911,461 / 247 | 29,680 (5, 2,505) | 0 / 2,629 / 162 | 4,254 | 1,182 | 1,797 | 6 | 1,530 | 302 GB | 20:41 |

"Intermediate" is a build with the image fix and an earlier fallback that ran only after the walk
was exhausted; by then the stale probes had spent the credits (2,862 of its fallbacks ended
`Exhausted` for funding), which is why the final version falls back at the first stale owner.
No run had a hard invariant failure; `origin_repeat_explained` holds (`unexplained` 0). Peak RSS
at 50k is unchanged (3.1 GB failover).

Reading the table:

- Seat egress: 50k 562 to 302 GB (failover) and 576 to 302 GB (cold start), full fallbacks 97,931
  to 1,398 (all `_first`); 5k 5.9 to 3.2 GB. This is the one-image-per-client floor (298 GB with
  the single-version model before the join gate), so the seat cost of join versions is gone. The
  number of versions is unchanged (7 and 6 at 50k).
- Failed reads at 50k: failover 1,527 to 479, cold start 1,103 to 247; stale skips 15,433 to 4,495
  and 12,800 to 4,254. 5k is unchanged within noise (its failures come from the origin error and
  partition windows); 5k `churn` gains 4,782 completed reads and 53 fewer failures (14,412
  fallbacks).
- Remaining stale failures: the reader cannot fall back. At 5k every such case is a reader that
  is not a member of version - 1, that is, a node that joined in the version its owners have not
  applied yet (`stale_fallback_not_member`: 8 in `full-cold-start`, 4 in `full-churn`). At 50k
  `full-cold-start` (rerun with the counters, same results) all 1,732 stale skips that could not
  fall back were for that reason (`stale_fallback_not_recent` 0), and the 247 failures all happen
  between 54 and 161 s, during enrollment. They are new Known issue 26.
- Origin: unchanged within noise (50k failover 52,461 to 52,384, worst page 9; cold start 29,336
  to 29,680, worst page 5). The fallback does not add origin requests: it never runs at an owner.
  The origin half of issue 20 is the holder that the handoff cannot find (new Known issue 25).

### Bounds

- Seat egress: no client gets a full image for lack of a delta. `ctl_full_fallbacks` is only
  `_first` (clients of the empty bootstrap version) in every run above; the tests assert
  `_no_delta` 0 under churn and `_skipped` and `_other` 0 in the cold start test.
- A non-owner spends at most one probe on owners that are behind before it walks the previous
  membership, and walks at most two memberships per read with one budget; a fallback never
  reaches the origin (tests above; `only_owner_hits_origin` holds in every run).

### Deviations and open items

- The seat egress half of issue 20 was a simulator-model artifact in shared step code, not a
  production cost: production installed planned images only on a record change. The fix is in
  the shared `plan_pass` and saves production a re-encode per pass.
- Issue 19 is not closed for readers that joined in the version the owners lack (Known issue
  26): they are not members of version - 1, so peers would reject them under it. Options:
  delay a new node's readiness by the propagation window, or a reader-side wait and retry within
  the read deadline (the simulator's client retries four times within milliseconds; production's
  client retry policy was not modeled further here).
- Known issue 25 (origin repeats from holders of versions older than version - 1) needs either
  older memberships kept for ranking (production keeps only the 30 s grace window) or rankings
  deeper than `REPLICAS` (`topology` keeps the best three per slot); both are design changes and
  were not attempted.
- `PREVIOUS_MEMBERSHIP_WITHIN` (10 s) gates the fallback as it gates owners' previous-membership
  probes. At 50k the propagation window is longer (client lag p90 24 to 30 s), so readers that
  adopted a version more than 10 s earlier still skip stale owners; a peer that already dropped
  version - 1 answers `StaleMembership` to the fallback, which is only a skip, so a longer window
  would cost probes, not correctness. Not tuned.

## Round 4: combined (wave 3)

Branch `racer-sim5-merge`: racer-core-sim 3771e5c3f plus the relay, flight, cancel and handover
tracks (one linear chain), then orphan and join2 cherry-picked. Logs and reports:
`/tmp/opencode/m5/`.

### Merge

- Conflicts were textual only: the `NodeConfig` knobs (`hot_page_bodies`, `flight_handover`,
  `successor_hints`, `stale_fallback`, all kept), the `racer_core::read` module list and the
  simulator's imports (`stale` and `successor`, both kept), and this document.
- The wave 3 Known issues were renumbered: 121 to 23, 122 to 24, 131 to 25, 132 to 26, 141 to 27,
  145 to 28, 151 to 29, 161 to 30, in this document and in code comments. New 31 records the
  handover track's concurrent origin overlap.

### Interplay

- Successor hints and stale fallback: a non-owner that falls back to version - 1 walks that
  version's owners; one of them may answer `Miss` on a successor hint, which is an ordinary miss
  in the fallback walk. `successor_deferral` checks ownership under the deferring node's own
  membership and never defers an owner requester, and the fallback walk ends when the reader owns
  the page under version - 1 (`fallback_rank_allowed`), so neither path gives a reader origin
  authority. A deferral costs a probe but no credit is added; `MAX_DEFERRALS` bounds it.
- Successor hints and flights (fixed in this merge, 7c6a08583): in 5k `zipf` at 16 MiB pages the
  merged tree failed 1,003 reads against 54 for the handover branch; with `successor_hints = 0`
  it failed 54 again. A hint only records that another owner asked for the page and got no copy;
  under origin overload that owner's fetch often fails or has not finished. 15,338 of 19,588
  deferrals (78%, temporary counter) happened while the deferring owner's own acquisition of the
  page was running, so readers were sent away from a fill they could have joined and exhausted
  their walks (`walk_exhausted` 20,292 to 29,491, `error_unavailable` 54 to 1,003). Production
  (`Coordinator::defer_to_successor`, `src/read.rs`) and the simulator now skip the deferral
  while `Flights::acquiring` / `PageFlights::acquiring` holds for the page
  (`racer_core::read::successor::defer_allowed`). After: 62 failed reads, 796 deferrals. Tests:
  `successor::tests::no_deferral_while_this_node_acquires`,
  `flight::tests::acquiring_only_while_a_leader_fetches`, and production
  `acquiring_only_while_a_leader_runs`.
- Fan-out helpers and handoff previous-owner probes are unchanged by orphan and join2; the hard
  invariants (including `origin_repeat_explained`, `unexplained` 0) hold in every run below and
  in all 16 scenarios at 200 nodes, 1 s. No invariant was relaxed.

### Evidence

Release, seed 1. 5k `full-*` and 50k use `/tmp/opencode/m4b/rr.json`; z16 is `zipf` with 16 MiB
pages (`/tmp/opencode/r5-handover/Z16.json`); cs1g is the 1 Gbit/s `full-cold-start` 1 s storm
of Known issue 23 (`/tmp/opencode/r5-relay/fin/cs1g.json`, 5k as in that issue). Origin: total,
worst page, pages fetched more than once. Wave 2 is `/tmp/opencode/m4b/`.

| Run | Reads ok / failed | Origin | `copy_held` | Concurrent | Deferrals | Stale fallbacks | Wall |
| --- | --- | --- | --- | --- | --- | --- | --- |
| 5k storm, wave 2 | 268,449 / 461 | 1,024, 1, 0 | 0 | 0 | - | - | 1:28 |
| 5k storm | 378,544 / 0 | 1,024, 1, 0 | 0 | 0 | 0 | 0 | 1:26 |
| 5k zipf, wave 2 | 149,525 / 4,373 | 42,216, 3, 4,171 | 565 | 3,734 | - | - | 2:14 |
| 5k zipf | 202,884 / 21 | 40,804, 3, 1,830 | 198 | 1,651 | 181 | 0 | 2:02 |
| 5k churn, wave 2 | 172,701 / 450 | 1,040, 3, 15 | 7 | 0 | - | - | 1:12 |
| 5k churn | 291,640 / 95 | 1,030, 2, 6 | 0 | 0 | 0 | 15,975 | 1:18 |
| 5k full-cold-start, wave 2 | 94,829 / 1 | 6,946, 2, 52 | 5 | 0 | - | - | 0:43 |
| 5k full-cold-start | 94,829 / 0 | 6,945, 2, 50 | 3 | 0 | 37 | 0 | 0:41 |
| 5k full-failover, wave 2 | 33,804 / 23 | 13,075, 7, 458 | 169 | 0 | - | - | 0:53 |
| 5k full-failover | 33,797 / 31 | 13,057, 6, 467 | 154 | 0 | 266 | 30 | 0:52 |
| 5k full-churn, wave 2 | 33,988 / 4 | 13,653, 6, 991 | 13 | 0 | - | - | 0:52 |
| 5k full-churn | 33,996 / 2 | 13,661, 6, 1,000 | 16 | 0 | 188 | 31 | 0:49 |
| 5k z16, handover branch | 32,691 / 54 | 35,917, 4, 13,740 | 10 | 18,275 | - | - | 0:58 |
| 5k z16, merge before 7c6a08583 | 32,427 / 1,003 | 35,846, 4, 13,702 | 10 | 18,219 | 19,588 | 0 | 0:58 |
| 5k z16 | 32,668 / 62 | 35,672, 4, 13,715 | 8 | 18,030 | 796 | 0 | 0:53 |
| 5k cs1g, relay branch | 63,745 / 31,084 | - | - | - | - | - | - |
| 5k cs1g | 63,637 / 31,191 | 6,947, 2, 52 | 5 | 0 | 41 | 1 | 1:07 |
| 50k full-cold-start, wave 2 | 908,373 / 1,103 | 29,336, 5, 2,639 | 330 | 1 | - | - | 20:38 |
| 50k full-cold-start, join2 | 911,461 / 247 | 29,680, 5, 2,505 | 162 | 0 | - | 1,797 | 20:42 |
| 50k full-cold-start | 911,471 / 248 | 29,695, 5, 2,506 | 169 | 0 | 156 | 1,798 | 20:35 |
| 50k full-failover, wave 2 | 299,717 / 1,527 | 52,461, 9, 6,136 | 2,451 | 0 | - | - | 28:37 |
| 50k full-failover, orphan | 299,710 / 1,542 | 52,304, 7, 6,099 | 2,305 | 2 | 2,143 | - | 30:09 |
| 50k full-failover, join2 | 302,582 / 479 | 52,384, 9, 5,938 | 2,109 | 0 | - | 1,908 | 28:29 |
| 50k full-failover | 302,519 / 535 | 52,200, 10, 5,895 | 1,914 | 0 | 1,878 | 1,908 | 28:06 |

- The tracks combine: 50k `full-failover` has join2's failed reads (535 against 479) with fewer
  `copy_held` repeats than either branch (1,914) and the fewest origin requests (52,200); orphans
  fall from 1,481 (orphan branch) to 1,205. The worst page is 10 (7 on the orphan branch, 9 on
  join2 and wave 2); every repeat is explained.
- Known issue 23 is unchanged: 31,191 failed reads in the 1 Gbit/s storm (31,084 on the relay
  branch).
- Peak RSS: 50k `full-cold-start` 4.3 GiB, `full-failover` 3.0 GiB (wave 2 4.2 and 3.0).
- Workspace tests pass on an idle host. Under the load of eight concurrent 5k runs, 32
  deadline-based control and app tests (`DeadlineExceeded`) failed; they all passed on rerun.

### Open issues after wave 3

19 (partly; remainder 26), 21 (partly), 23, 25, 26, 31 (28 resolved by the leak track).

## Round 4: leak (wave 3)

Merged onto racer-core-sim 3542f1cc4. The track's issue numbers were renumbered: 145 to 28,
155 to 32, 156 to 33, in this document and in code comments.

### Root cause (issue 28)

`RACER_DST_SEEDS=16` with `dst_generated_native_traffic_churn_oracle` failed at step 0
(`PendingWriteCrash`): retired node 24 held 30,776 `RequestContext` bytes. The non-native oracle
passes this seed. Base 3771e5c3f fails the same way.

A temporary per-charge backtrace registry in `flow` (not committed) showed three live charges on
the retired node's admission authority, all reachable only from futures in `Links::tasks`:

- the I/O submission capacity of the telemetry `DiagnosticIo` (`Telemetry::attach_io`),
- an `AdmissionBudget` I/O charge taken in `link_exchange` (`src/peer/transport.rs:684`),
- an HTTP connection context taken in `PeerServer::serve_connection`.

`Transfers::ensure_link` (`src/peer/transport.rs:764-790`) spawns `connect_link`, which owns an
`Rc<Links>` (`src/peer/transport.rs:741`), into `Links::tasks` (`src/rdma/links.rs:220`, `spawn`).
`Links` therefore owns a future that owns `Links`. Only `Links::drain` (`src/rdma/links.rs:442`)
clears the tasks, and `WorkerApplication` calls it only at the end of a graceful drain
(`src/app.rs:2263`). The DST crash path drops the worker without draining (the process is gone),
so a link setup in flight at the crash kept itself, its connection, and the charges alive forever.
Only native nodes have `Links`, which is why the plain oracle passes.

This is a production ownership bug, not a harness artifact: any undrained drop of a worker
leaks. In production that is a startup failure after link setups started, or a drain that fails
before it reaches `links.drain()`. A real process crash frees everything anyway, so the impact is
bounded to those in-process paths (inference: no production code drops and recreates a worker in
the same process today).

### Fix

- `Links::abandon_setups` (`src/rdma/links.rs`) marks the facade draining and drops all setup
  tasks. It takes the vector out of the `RefCell` before dropping it, because dropping a task runs
  `SetupAttempt::drop`, which uses the same `Links`.
- `impl Drop for WorkerApplication` (`src/app.rs`) calls it, breaking the cycle for every undrained
  drop. A test-only accessor that moved a field out now clones it.

### Harness preconditions found in the 1-60 sweep

Both failed identically on base 3771e5c3f.

- Issue 32, seed 60, `link setup stalled`: after `InflightCrash` re-adds a node under the same
  id, a surviving node still holds a `Ready` link to the old incarnation while the new one is
  `Idle`. `ensure_link` does nothing for a `Ready` link and `connect_links` only waits out
  `Backoff`. Production breaks such a link on first use (error completion,
  `rdma_verbs::node` `break_link`) or when its idle `Close` fails after `LINK_IDLE` (120 s). The
  harness now advances the clock past `LINK_IDLE` when the local end is `Ready` and the remote end
  is `Idle` or `Backoff`, so production logic breaks the link and the normal backoff path
  reconnects. At the wave 3 leak merge this change was dropped: racer-core-sim already had Known
  issue 27's fix (the not-ready side initiates), which covers the same case. `Connecting` remote ends are still only settled; treating them as stale made the
  responder's setup time out.
- Issue 33, seed 59, `native fault not consumed`: with 22 nodes the chosen receiver
  (`candidates[1]`) was not an overlay neighbor of the source, so its read went through a relay
  over links the harness had not set up and used HTTP (no native operation ran). The receiver is
  now the best-ranked candidate that `Membership::is_neighbor` of the source, falling back to
  `candidates[1]`. The assertion is unchanged.

### Tests

- `rdma::links::tests::abandon_setups_breaks_the_task_cycle_and_releases_charges`: a pending task
  owning an `Rc<Links>` and a `RequestContext` charge; after `abandon_setups` the task's reference
  and the charge are gone, new setups are refused, and the call is idempotent.
- The whole-app DST oracle seed 16 is the integration regression.

### Evidence

- `RACER_DST_SEEDS=16` native: fails on base and before the fix (30,776 bytes), passes after.
- Seeds 1 to 60, one process per seed, native and plain: before the harness fixes 118 of 120
  passed (native 59 and 60 failed, also on base); after, 120 of 120 pass.
- `cargo fmt --check`, `cargo clippy --workspace --all-targets -D warnings`, and
  `cargo test --workspace` pass.

### Deviations

- No simulator scenario changes; the leak is in the production worker, so 5k scenario numbers are
  unaffected and were not rerun.

## Round 4: overload (wave 4)

Scope: Known issues 23 (the 1 Gbit/s `full-cold-start` 1 s storm) and 31 (overlapping origin
requests in `zipf` at 16 MiB pages). Branch `racer-sim6-overload` from 3542f1cc4. Logs and
configs: `/tmp/opencode/r6-overload/` (`k-base.json` is the issue 23 config, `Z16.json` the issue
31 config).

### Issue 23: root cause

The storm is not a capacity bound. Toggling the simulator knobs on the merged tree changed
nothing (failed reads: base 31,191; `successor_hints` 0: 31,181; `stale_fallback` 0: 31,207;
`flight_handover` 0: 31,191; `hot_page_bodies` 8: 31,686, 128: 29,912). Bisecting by building
each merge point (seed 1, 5k, same config):

| Tree | Reads ok / failed | Helper probes | Sheds | Probe timeouts | Membership versions |
| --- | --- | --- | --- | --- | --- |
| 0d282020f (wave 1, no fan-out) | 52,279 / 42,824 | 0 | 0 | 213,257 | 1 |
| 74c5e8d25 (fan-out on wave 1) | 95,057 / 47 | 79,139 | 80,945 | 10,036 | 1 |
| c5a446a9f (join: admit once serving, batch) | 95,057 / 47 | 79,139 | 80,945 | 10,036 | 1 |
| bdf231156 (join: sim models reporting once serving) | 55,793 / 39,037 | 12,904 | 38,825 | 192,129 | 5 |
| 3771e5c3f (wave 2) | 56,399 / 38,430 | 13,532 | 36,634 | 191,873 | 5 |
| 3542f1cc4 (wave 3) | 63,637 / 31,191 | 10,930 | 32,830 | 177,942 | 5 |
| 3542f1cc4 with `control.report_at_start` | 95,039 / 63 | 71,930 | 71,930 | 605 | 1 |

So the regression came with the cold start publishing 5 membership versions instead of 1. A
per-request trace of the first storm second (temporary instrumentation) showed owners answering
readers of only 6 of the 16 pages with sheds; for the other 10 pages only the 3 owners ever
received requests, and their per-page body queues reached 236 without a shed. The simulator's
`shed_hot` asked "is `dest` an owner?" with `NodeWorld::owned`, the retention predicate, which
reads the placement cache only (`Placement::is_owner`, `topology/src/placement.rs:723`: "Missing,
partial, and obsolete rankings return false"). Production's shed ranks the page
(`PeerServer::shed_hot_page`, `src/peer/server.rs:416`, `self.placement.rank(...)`). An owner that
fetched a page under an older membership (or has served it from memory since) holds no ranking
under the current one, so in the simulator it never shed; readers queued on its NIC, timed out
(2 s attempts), retried through relays (259k relay rejections) and failed. With a single version
the cached rankings matched and the bug was invisible.

### Issue 23: fix

`shed_hot` ranks the page with `owners_at(dest, w, page, true)` like production and exempts
requesters among those owners (91fd66397). Unit test
`node::world::shed_tests::owner_without_a_cached_ranking_sheds_a_hot_page` fails before (the owner
without a cached ranking does not shed) and passes after; it also checks the owner, helper and
threshold exemptions and that a non-owner never sheds. Production is unchanged.

### Issue 31: root cause

The configuration is origin-bound. `origin_bps` 5 GB/s serves about 298 16 MiB pages per second,
2,384 in the 8 s run (2,372 answered); the workload asks for 17,634 distinct pages, about 7.4
times that. Rank 0's origin request for a page therefore queues for seconds. New counters split
`origin_repeats_concurrent` by why the overlapping owner skipped rank 0 (ccc23d01d): of 18,030,
11,352 had rank 0 time out, 5,740 had rank 0 answer `Miss` to a copy-only probe, 838 a route
failure, 76 `Overloaded`.

- Copy-only (simulator artifact): production `FillEngine::copy_only` (`src/read/fill.rs:1385`)
  joins the page's running acquisition as a copy waiter (`Flights::join_copy`,
  `src/read/flight.rs:1360`) when there is no local copy. The simulator answered `Miss`, so an owner
  that could not fund a delegation and probed rank 0 copy-only got a miss from a rank 0 that was
  fetching the page, and fetched it too.
- Timeout (production rule): an owner above rank 0 delegates with an `Acquire`
  (`CandidateWalk::next`, `core/src/read/walk.rs`, `delegated`) and keeps one credit for its own
  origin attempt. When rank 0 does not answer within the attempt (its idle share or the attempt
  timeout), production maps the attempt-scope deadline to `Unavailable`
  (`request_traced`, `src/read/candidates.rs`), the walk records `Transport(Unreachable)` as
  transient evidence and ends with origin authority. Rank 0 had accepted the request and its
  origin request was simply queued; the owner's request queued behind it at the same FIFO origin,
  so it could not finish first, and it delayed every request behind it until canceled at its scope
  or handed over to a later waiter. A sim-only experiment that withheld origin after such a timeout
  completed 37,527 reads instead of 32,829 with half the origin requests.

### Issue 31: fix

- racer-core `PageFlights::join_copy` (`core/src/read/flight.rs`) mirrors production `join_copy`:
  `Miss` with no flight or after a failure or drain, the settled or partial result, the waiter cap,
  waiter admission, then a waiter registered without acquisition rights (it never leads or adopts).
  The simulator's copy-only serve uses it and answers with the acquisition's result (5bc1f2550;
  counters `copy_only_flight_joins`, `copy_only_flight_hits`, `copy_only_join_rejected`).
- `Outcome::TimedOut` (`core/src/read/walk.rs`): the request was sent and is unanswered within the
  attempt's own bound while the caller's scope still runs. For an owner's page probe of rank 0 (a
  delegated `Acquire`, or copy-only, under either membership; not a handoff probe) the walk ends
  `Exhausted` instead of `Origin` (`CandidateWalk::rank0_unanswered`); the caller fails with
  `Unavailable`, the reader's walk moves on or retries, and later attempts join rank 0's
  acquisition. Everywhere else `TimedOut` is recorded exactly like `Transport(Unreachable)`.
  A transport failure, `Overloaded`, a shed or a miss from rank 0 still gives authority, and
  metadata walks are unchanged. Production `request_traced` reports an attempt-scope deadline as a
  timeout and the candidate walk completes the probe with `TimedOut`; the simulator reports its
  `ProbeTimeout` the same way (counter `origin_left_to_rank0`) (5466604de).

Origin authority is unchanged (only fewer owners reach it), budgets never grow (no probe is
added), and `only_owner_hits_origin`, `origin_repeat_explained` and `budget_never_grows` hold.
No wire change.

Trade-off (new Known issue 221): an owner no longer fetches when rank 0 (or a relay on the route)
accepts its request and hangs without failing the connection. Crashed or partitioned members fail
at the transport level (connection refused or connect timeout) and still hand authority to the
next owner. Remaining overlap (new Known issue 222): an owner still fetches after a route failure
to rank 0 or an `Overloaded` rank 0 even when rank 0 is fetching.

### Tests

- racer-core `flight::tests`: `copy_join_waits_for_the_running_acquisition_and_never_leads`,
  `copy_join_misses_failed_and_takes_partial_results`,
  `copy_join_respects_the_waiter_cap_and_a_lost_leader_fails_copy_waiters`.
- racer-core `walk::tests`: `owner_leaves_the_fetch_to_rank_zero_after_a_timeout` (delegated and
  copy-only, rank 1 and rank 2, a later copy still wins) and
  `only_an_owner_probe_of_rank_zero_timing_out_withholds_origin` (transport failure, `Overloaded`,
  miss and shed still give authority; a later predecessor or a handoff target timing out does
  not withhold it; metadata walks and non-owners treat it as `Transport(Unreachable)`; the
  remaining walk treats it as unavailable).
- Production `read::tests::timeouts::owner_page_walk_leaves_the_fetch_to_a_rank_zero_that_times_out`
  (rank 1, a stalled rank 0 ends `Unavailable` after one exchange; a transport failure still gives
  authority); fails before the production change.
  `candidate_final_remaining_copy_and_origin_miss_only_at_final_boundary` now takes its origin
  authority from a metadata walk, since a page walk no longer reaches origin through a timeout;
  its page-operation diagnostics checks are unchanged.
- racer-sim `owner_without_a_cached_ranking_sheds_a_hot_page` (above) and
  `contention_scenarios::owners_above_rank_zero_do_not_overlap_a_slow_origin_fetch` (300 nodes, 48
  readers of 64 cold pages at 8 MiB/s of origin: no overlapping request and one request per page;
  98 overlapping requests and up to 3 per page without the rule).
- `overloaded_hot_page_owners_do_not_stream_to_abandoned_requesters` asserted no late reply of any
  kind. With the rule an owner above rank 0 whose delegation timed out answers its waiters
  `Unavailable` instead of fetching, and some of those short answers reach readers whose attempt
  already ended (81 late replies). The test now asserts what it was written for, no late page body
  (new counter `late_page_replies`, 0).

### Evidence

Release, seed 1, 5k. "before" is the wave 3 merge (3542f1cc4, `/tmp/opencode/m5/`), "after" is
5466604de (`/tmp/opencode/r6-overload/fin-*`). cs1g is `k-base.json`
(`{"nic_bps":125000000,"request_rate":0.005,"storms":[one 1 s storm of all 5,000 nodes on object
0 at 300 s]}`), z16 is `Z16.json` (`zipf`, 16 MiB pages, `request_rate` 1.25, 8 s); `full-*` use
`/tmp/opencode/m4b/rr.json`. Origin is total requests, most per page, pages fetched more than once.

| Run | Reads ok / failed | Origin | Concurrent | `copy_held` | Left to rank 0 | Sheds / helper copies | Wall |
| --- | --- | --- | --- | --- | --- | --- | --- |
| cs1g before | 63,637 / 31,191 | 6,947, 2, 52 | 0 | 5 | 0 | 32,830 / 8,215 | 1:07 |
| cs1g after | 94,828 / 0 | 6,945, 2, 50 | 0 | 3 | 0 | 72,019 / 71,343 | 0:50 |
| z16 before | 32,668 / 62 | 35,672, 4, 13,715 | 18,030 | 8 | 0 | 2,402 / 2,130 | 0:53 |
| z16 after | 37,538 / 29 | 18,867, 3, 1,180 | 1,088 | 5 | 20,350 | 2,557 / 2,184 | 0:49 |
| zipf before | 202,884 / 21 | 40,804, 3, 1,830 | 1,651 | 198 | 0 | 34 / 34 | 2:02 |
| zipf after | 202,961 / 24 | 40,324, 3, 1,388 | 1,208 | 199 | 0 | 34 / 34 | 1:59 |
| storm before / after | 378,544 / 0 both | 1,024, 1, 0 both | 0 | 0 | 0 | 60,365 / 59,001 both | 1:26 |
| churn before | 291,640 / 95 | 1,030, 2, 6 | 0 | 0 | 0 | 519 / 501 | 1:18 |
| churn after | 291,695 / 93 | 1,032, 2, 8 | 0 | 0 | 0 | 578 / 540 | 1:15 |
| full-cold-start before / after | 94,829 / 0 both | 6,945, 2, 50 both | 0 | 3 | 0 | 0 / 0; 2 / 2 | 0:41 |
| full-failover before / after | 33,797 / 31 both | 13,057, 6, 467 both | 0 | 154 | 0 | 0 / 0 | 0:52 |
| full-churn before | 33,996 / 2 | 13,661, 6, 1,000 | 0 | 16 | 0 | 0 / 0 | 0:49 |
| full-churn after | 33,996 / 2 | 13,658, 6, 995 | 0 | 14 | 0 | 0 / 0 | 0:49 |

- Issue 23 by step: the shed fix alone gives cs1g 94,828 / 0 (`f1-cs1g`); the issue 31 changes do
  not touch it. Probe timeouts 177,942 to 610, relay rejections 258,904 to 0, read p50 1.61 s to
  302 ms and p99 9.66 s to 1.21 s.
- Issue 31 by step (z16): the copy-only join alone moves the copy-only class into timeouts
  (overlap 18,030 to 15,389, `f2-z16`); withholding origin after a delegated timeout gives 4,726
  (`f4-z16`); extending it to copy-only probes of rank 0 gives 1,088 (`f5-z16`, final). Origin
  requests canceled 2,175 to 285, flight handovers 13,537 to 7,165, range reads completed 1,217 to
  1,577. Read p99 rises from 5.91 s to 6.44 s (bucket bounds): readers now wait for rank 0's
  queued request instead of having a second owner queue one behind it.
- With 10 times the origin bandwidth (`origin_bps` 50 GB/s) z16 has no overlapping request even
  without the timeout rule (`f2-z16o10`: 21,502 requests, one per page): the overlap only appears when origin latency
  exceeds the owners' attempt bound.
- Default 1 MiB `zipf` is origin-bound too (40,324 requests in 2 s against about 9,400 the origin
  can answer); its overlap falls from 1,651 to 1,208.
- No hard invariant failure in any run. The 16-scenario smoke at 200 nodes (1 s) is clean, and all
  trace hashes match the wave 3 merge except `faults`.
- `cargo fmt --check` and `cargo clippy --workspace --all-targets -D warnings` are clean; the
  workspace tests pass (3,449 tests, release, `--no-fail-fast`).
- 50k was not run: the 5k `full-*` runs (the 50k configurations) are unchanged apart from 2 sheds,
  and the wave 3 50k `full-cold-start` and `full-failover` had 0 concurrent origin requests.

### Deviations and open items

- Issue 23 was a simulator artifact; production already ranks in the shed. The production
  changes are for issue 31 (timeout classification in `request_traced` and the walk rule).
- New Known issues 221 (a hung rank 0 now blocks the owners above it) and 222 (route failure or
  `Overloaded` from a rank 0 that is fetching still lets owners fetch).

## Round 4: history (wave 4)

Known issues 25 (a page that moved over more than one membership version is fetched again) and
26 (readers that joined in the version their owners lack cannot fall back). Branch
`racer-sim6-history` from racer-core-sim 3542f1cc4. Nothing was cherry-picked.

### Root causes

Issue 25:

- An owner asks copy-only, before origin, only the owners of version - 1 that are not its
  predecessors (`handoff_targets`, `core/src/read/walk.rs`; production `previous_owners`,
  `src/read/candidates.rs`). Production can rank only versions it still holds: two old parts for
  the 30 s grace window (`Snapshot::retention`, `src/control/publication.rs`;
  `Published::resolve`). So a holder from version - 2 or older is never asked.
- Instrumenting the 5k `full-churn` repeats (temporary counters, seed 1) showed what the holders
  were: of 929 moved-page repeats whose holder still had the copy and was not asked, 579 holders
  were current owners ranked after the fetching owner (578 fetches by rank 0), and the other 350
  ranked below the three owners. 646 of the 929 had no handoff targets at all: the move happened
  versions earlier, so version - 1 and the current version had the same owners.
- Why such a holder exists: placement is weighted rendezvous hashing
  (`topology/src/placement.rs`), so a member's score for a slot never changes. A member that
  joins above an owner pushes it down a rank. The pushed-down owner keeps the copy it fetched
  while it ranked higher. Rank 0 never asks its successors, and nothing remembers the move once
  version - 1 is gone.

Issue 26:

- A reader that joined in version N is not a member of N - 1, so `previous_fallback` refuses
  (`core/src/read/stale.rs`), and an owner still on N - 1 cannot resolve N and answers
  `StaleMembership` (`src/peer/server.rs`, `decide_hop` in `core/src/peer/hop.rs`). The reader
  skips each owner and the walk ends `Exhausted`.
- Each skipped probe also lost its remote credits: `plan_attempt` moved them into the request
  (`core/src/read/walk.rs`) and nothing returned them, although the node that answered
  `StaleMembership` did not act on the request (it answers before dispatch when it cannot
  resolve the membership).

### Changes

- `topology::Membership` (shared): a membership built with a predecessor records a generation
  and, per member, the generation it arrived in (`since`); members of the first membership in a
  chain have 0. Local only, no wire change; 4 bytes per member, counted in `retained_bytes`. A
  process's chain starts at the first publication it accepts, so a restarted node sees members
  that joined before it as equal, and members that join later as newer.
- `racer_core::read::walk`: `lineage_targets` (later owners that arrived before an owner),
  `lineage_counts`, and `merge_handoff` (lineage targets first, then version - 1 targets, at most
  three). `WalkConfig::lineage` funds them from slack only: a delegation to rank 0 forwards spare
  credits for rank 0's targets, and a non-owner's out-of-order `Acquire` carries one more credit
  per target of the destination and of rank 0 when every later visit still keeps its minimum
  (`later_reserve`).
- `racer_core::read::candidates::lineage_first`: a Spread reader whose first choice arrived after
  a later owner visits that older owner first. Members that arrived together keep the order.
- `racer_core::read::stale::stale_wait` and `CandidateWalk::retry_pending`: a non-owner that
  cannot fall back waits once for a destination that is behind (a third of the time left, at
  most 4 s, at least 250 ms) and asks it again.
- `AcquisitionBudget::send_remote` / `refund_remote`: a verified `StaleMembership` returns the
  remote credits its request carried, never more than were sent.
- Production (`src/read/candidates.rs`, `src/app.rs`): owners merge lineage targets into their
  handoff probes, `visit_order` applies `lineage_first` under Spread, `request_traced` refunds
  remote credits on `StaleMembership`, and `resolve_epoch_traced` waits on the node's reactor
  (`with_reactor`). No wire change; budgets never grow.
- Simulator: the same, with `Versions::chain_base` (where each node's chain starts: its first
  applied version since restart), `Event::StaleWait`, refunds per probe, and knobs `lineage` and
  `stale_wait` (default on).

### Tests

- `topology`: `lineage_records_arrival_generations`; the retained-bytes test counts `since`.
- `racer-core`: `lineage_targets_are_later_owners_that_arrived_earlier`,
  `merge_handoff_keeps_lineage_targets_first_without_duplicates`,
  `delegation_forwards_spare_credits_for_rank_zero_lineage`,
  `non_owner_funds_destination_lineage_from_slack_only`,
  `retry_pending_plans_the_same_destination_again`,
  `lineage_first_visits_an_older_later_owner_first`,
  `a_reader_that_cannot_fall_back_waits_once`,
  `no_wait_for_owners_fallbacks_repeats_or_short_deadlines`,
  `remote_credits_refund_only_what_was_sent`.
- Production: `a_newer_owner_asks_older_later_owners_before_origin` (holder at rank 1 or 2: no
  origin; no holder: asks both, then version 1's other owner, then origin; a chain whose owners
  arrived together asks no one), `spread_visits_an_older_later_owner_before_a_newer_owner`,
  `readers_that_cannot_fall_back_wait_once_for_an_owner_that_is_behind` (owner adopts during the
  wait: copy; still behind: one more probe, then `Unavailable`, only exchanges spent; no timer:
  as before).
- Simulator: `combined::tests::lineage_targets_find_copies_of_pages_that_moved` (500 nodes
  `full-churn`: moved-page repeats 91 to 82, origin requests 2,178 to 2,169) and
  `node::tests::readers_that_cannot_fall_back_wait_for_owners_that_are_behind` (walks exhausted
  on stale owners 6 to 2). `stale_owners_send_non_owners_to_the_previous_membership` now turns
  waits off to measure the fallback alone.

### Evidence

Release, seed 1; `full-*` with `/tmp/opencode/m4b/rr.json`, `churn` with scenario defaults. Base
is 3542f1cc4 built in this worktree (5k) and `/tmp/opencode/m5/` (50k). Origin: requests, worst
page, pages fetched more than once. "Move" is `after_owner_move` repeats; "older" is the moved
repeats whose holder was a version - 2 or older owner and not asked (`_unasked_two` +
`_unasked_older`). "Exh. stale" is walks that ended exhausted after a stale skip. Logs:
`/tmp/opencode/r6-history/`.

| Run | Reads ok / failed | Origin | Move | Older | `copy_held` | Handoff probes | Exh. stale | Stale waits | Wall |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| 5k churn, base | 291,640 / 95 | 1,030, 2, 6 | 6 | 4 | 0 | 1 | 37 | - | 1:15 |
| 5k churn | 291,441 / 87 | 1,031, 2, 7 | 7 | 6 | 0 | 1 | 0 | 110 | 1:20 |
| 5k full-churn, base | 33,996 / 2 | 13,661, 6, 1,000 | 1,060 | 918 | 16 | 1,286 | 5 | - | 0:49 |
| 5k full-churn | 33,997 / 1 | 13,479, 5, 852 | 894 | 772 | 6 | 1,345 | 3 | 1 | 0:48 |
| 5k full-failover, base | 33,797 / 31 | 13,057, 6, 467 | 58 | 38 | 154 | 424 | 3 | - | 0:50 |
| 5k full-failover | 33,796 / 33 | 13,021, 7, 441 | 51 | 39 | 131 | 316 | 3 | 0 | 0:50 |
| 5k full-cold-start, base | 94,829 / 0 | 6,945, 2, 50 | 47 | 34 | 3 | 448 | 4 | - | 0:40 |
| 5k full-cold-start | 94,829 / 0 | 6,934, 3, 39 | 39 | 29 | 1 | 299 | 0 | 2 | 0:40 |
| 50k full-cold-start, base | 911,471 / 248 | 29,695, 5, 2,506 | 2,629 | 1,355 | 169 | 4,794 | 1,134 | - | 20:35 |
| 50k full-cold-start | 911,699 / 95 | 28,960, 4, 1,913 | 1,919 | 1,158 | 131 | 3,231 | 452 | 325 | 20:55 |
| 50k full-failover, base | 302,519 / 535 | 52,200, 10, 5,895 | 3,612 | 2,229 | 1,914 | 4,823 | 1,214 | - | 28:06 |
| 50k full-failover | 302,786 / 394 | 51,073, 9, 5,022 | 2,878 | 1,939 | 1,590 | 3,970 | 451 | 331 | 28:42 |

Reading the table:

- Issue 25: moved-page repeats fall 16% at 5k `full-churn`, 27% at 50k `full-cold-start` and
  20% at 50k `full-failover`; origin requests fall by 182, 735 and 1,127, and `copy_held`
  repeats at 50k `full-failover` 1,914 to 1,590. The 50k `full-cold-start` repeats where a holder was
  asked but unreachable fall 746 to 248: readers now ask the older owner themselves instead of
  rank 0 asking it with a delegation's few links. The worst page goes 5 to 4 at 50k cold start;
  at 5k `full-failover` 6 to 7 (one page; the failover partitions dominate that scenario).
- Issue 26: walks exhausted on stale owners 37 to 0 (5k `churn`) and 1,134 to 452 (50k
  `full-cold-start`); 50k failed reads 248 to 95. The 5k `churn` completes 199 fewer reads in the
  same time: waiting readers finish later. In 50k `full-failover` failed reads go 535 to 394 and
  walks exhausted on stale owners 1,214 to 451; the remaining failures are mostly the failover's
  crashes and partitions (`read_failed_at_s` p50 384 s, after enrollment).
- Hard invariants hold in every run, including `origin_repeat_explained` (`unexplained` 0), and
  in all 16 scenarios at 200 nodes, 1 s. Peak RSS at 50k: `full-cold-start` 4.3 GiB, `full-failover` 3.0 GiB (unchanged).

### Bounds

- Lineage probes are copy-only and opportunistic: at most three handoff probes per owner fetch
  (lineage and version - 1 targets together, as before), each only with credits left after the
  owner's own origin credit; readers fund them only from slack beyond every later visit's
  minimum. A cluster whose members arrived together, or a node whose chain began after them,
  sees no lineage targets and pays nothing.
- A read waits at most once, for at most a third of its remaining time and at most 4 s. A refund
  returns only credits a verified `StaleMembership` shows were never used; refunds never exceed
  what was sent (`budget_never_grows` checks each refund in the simulator).
- No reader gains origin authority: lineage targets and waits are copy-only or reader-side
  (`only_owner_hits_origin` holds in every run).

### Deviations and open items

- Issue 25 is partly resolved. Holders ranked below the three owners (Known issue 201) need
  deeper rankings, a topology change not attempted here. Rank 0 reached by a delegation still has
  one credit too few to ask its lineage targets (Known issue 202); more funding from the reader's
  8-attempt page budget traded failed reads for origin requests and was not kept.
- Issue 26 is partly resolved: one wait does not cover the 50k propagation window (Known issue
  203). Longer or repeated waits spend more of the read deadline; production client retries were
  not modeled further.
- The refund changes production behavior for every `StaleMembership` answer, not only for the
  wait: a refreshed walk (Known issue 3) now keeps those credits too.
- The temporary counters used for the root cause were removed before committing; the numbers
  above that come from them are marked as instrumented runs.

## Round 4: orphan2 (wave 4)

Known issue 21, remainder. Branch `racer-sim6-orphan2` from 3542f1cc4. Commits: 4e66c2124
(racer-core grant and successor targets), a33874384 (production), 44bc87db1 (simulator),
454e315d4 (clippy), 8255e91ca (time share and miss mapping), f97b81bba (`PROBE_AFTER`), plus this
section. Evidence: `/tmp/opencode/r6-orphan2/`.

### Root cause

Temporary simulator counters (5k `full-failover`, seed 1, base 3542f1cc4) recorded, for each
`copy_held` repeat that rank 0 fetched while an orphan holder kept the page (68 of 154):

- Who made rank 0 fill: 49 were owner delegations (rank 2 while rank 1 held the page, 33; rank 1
  while rank 2 held it, 16), 19 were readers after their deferrals ran out, none were local reads.
- Budget: 59 of the 68 fills started with fewer than 3 attempts or fewer than 4 links. A delegation
  carries rank 0's origin credit only (`owner_attempts`, `core/src/read/walk.rs`), so handoff
  probes stop before they spend it (the `remaining_attempts() < 2` break in `CandidateWalk::next`).
  That is why rank 0 never asks a successor, even when it noted one.
- Hints: rank 0 held a hint naming the holder in 28 of the 68 cases; in the other 40 it never
  noted it: the holder never reached rank 0 (`partition`, `route`, and `failed` answers that were
  signed by a relay on the route, `fail_hop`/`relay_failure`, `sim/src/node/world.rs`; the
  "failed" class of "Round 4: repeat" includes those relay failures).
- Deferring owner requesters (the wave 3 alternative, retried here) made things worse (origin
  13,057 to 13,071, `copy_held` 154 to 170): rank 2's later probe of rank 1 failed route search
  286 times and missed 154 times at 5k, because the delegation used most of rank 2's links.

### Change

- racer-core `walk`: `WalkConfig::successors`. The first `successors` handoff targets are asked
  copy-only before origin, each on its own grant (`SUCCESSOR_GRANT_ATTEMPTS` 1,
  `SUCCESSOR_GRANT_LINKS` 8). The grant is never reunited with the walk's budget (also not on
  `abandon`), and its route failures do not mark the walk's route failed, so the walk's credits
  never grow and the delegation's origin credit stays. The probe's time share is divided by
  `SUCCESSOR_SHARE_DIVISOR` (4) more opportunities, so a hung successor cannot hold the
  requester past its own attempt timeout. `pending_successor()` reports it.
- racer-core `successor`: `successor_targets` (hinted owners ranked after this node, not already
  previous-membership targets, newest first), `with_successors` (successors first, at most
  `MAX_HANDOFF`), `SuccessorHints::probe` (returns a hint once, re-armed by a renewed note, and
  only once it is `PROBE_AFTER` = 2 s old: a younger hint names an owner whose own fetch is most
  likely still running). Charging deferrals no longer drops the hint, only stops deferring on it,
  so the probe can still use it. `successor_deferral` defers only to owners ranked after this
  node: a predecessor that asked (rank 0's own successor probe answered `Miss` at rank 1) has no
  copy either, and deferring to it bounced readers between two owners without one.
- Production: the hint table moves from `Coordinator` to `CandidatePolicy::successors`
  (`src/read/candidates.rs`); `resolve_epoch_traced` adds the successor targets for page walks of
  owners. A successor probe error other than this request ending is a miss (DST
  `dst_generated_traffic_churn_oracle` seed 7 returned a 503 before both fixes). No wire change:
  the probe is an ordinary copy-only page request.
- Simulator: `add_successor_targets` (same core helpers), switch `node.successor_targets`,
  counters `successor_probes`, `successor_copies`, `successor_misses`, `successor_failures`, and
  `origin_repeats_orphan_noted` (rank 0 refetched a holder it had noted).

Bound: one granted copy-only probe per noted hint (`HINT_CAPACITY` 4,096 per node, re-armed only
by a new miss answered to that owner). Origin authority, credits and budgets are unchanged;
`origin_repeat_explained` is unchanged and holds (`unexplained` 0 in every run). No invariant was
relaxed.

### Tests

- racer-core: `walk::tests::delegated_rank_zero_asks_successors_on_their_own_grant`,
  `successor_grant_never_grows_the_walk_budget`, `owner_above_rank_zero_asks_successors_after_its_delegation`;
  `successor::tests::probe_returns_each_note_once`, `successor_targets_are_later_owners_newest_first`,
  `successors_come_first_within_the_handoff_cap`, `defers_only_to_owners_ranked_after_this_node`;
  `charge_drops_hints_after_max_deferrals_and_note_resets` became
  `charge_stops_deferrals_after_max_and_note_resets` (the hint now stays for the probe).
- Production (`src/read/tests/peer_copies.rs`): `rank_zero_copies_from_the_owner_it_missed` (no
  origin call; fails without the change), `rank_one_never_defers_to_rank_zero`;
  `rank_zero_never_defers_owner_requesters` and `rank_zero_defers_readers_to_the_owner_it_missed`
  now script the successor probe's miss and check it was the only peer call.
- Simulator: `combined::tests::failover_owners_copy_orphans_from_successors` (400 nodes, seed 2:
  successor copies, fewer `copy_only` plus `failed` orphans than without targets, and
  `origin_repeats_orphan_noted <= successor_misses`).

### Evidence

Release, seed 1 unless noted, 5k `full-*` with `/tmp/opencode/m4b/rr.json`. Before is the wave 3
merge (`/tmp/opencode/m5/`). Origin: total, worst page, pages>1. Orphans: all, then `copy_only` /
`failed`. Probes: successor probes / copies.

| Run | Reads ok / failed | Origin | `copy_held` | Orphans | Deferrals | Probes | Wall |
| --- | --- | --- | --- | --- | --- | --- | --- |
| 5k full-failover before | 33,797 / 31 | 13,057, 6, 467 | 154 | 76 (13 / 38) | 266 | - | 0:52 |
| 5k full-failover after | 33,801 / 26 | 13,034, 8, 446 | 129 | 61 (3 / 21) | 209 | 33 / 31 | 0:51 |
| 5k full-failover seed 2 before | 33,487 / 38 | 13,084, 7, 453 | 164 | 93 (22 / 53) | 329 | - | 0:50 |
| 5k full-failover seed 2 after | 33,491 / 28 | 13,030, 6, 436 | 116 | 49 (5 / 21) | 204 | 33 / 31 | 0:51 |
| 5k full-churn before | 33,996 / 2 | 13,661, 6, 1,000 | 16 | 10 (2 / 6) | 188 | - | 0:49 |
| 5k full-churn after | 33,995 / 2 | 13,652, 6, 995 | 7 | 3 (0 / 2) | 26 | 7 / 7 | 0:48 |
| 5k churn before | 291,640 / 95 | 1,030, 2, 6 | 0 | 0 | 0 | - | 1:18 |
| 5k churn after | 291,640 / 95 | 1,030, 2, 6 | 0 | 0 | 0 | 0 / 0 | 1:16 |
| 5k zipf before | 202,884 / 21 | 40,804, 3, 1,830 | 198 | 0 | 181 | - | 2:02 |
| 5k zipf after | 202,884 / 21 | 40,804, 3, 1,830 | 198 | 0 | 181 | 0 / 0 | 2:02 |
| 50k full-failover before | 302,519 / 535 | 52,200, 10, 5,895 | 1,914 | 1,205 (140 / 519) | 1,878 | - | 28:06 |
| 50k full-failover after | 302,509 / 530 | 51,834, 10, 5,796 | 1,584 | 893 (44 / 343) | 1,953 | 360 / 310 | 28:23 |

- 50k orphans by cause, before and after: `failed` 519 / 343, `route` 309 / 285, `partition`
  145 / 139, `copy_only` 140 / 44, `busy` 88 / 79. Origin requests fall by 366 (0.7%), `copy_held`
  by 17%. 101 refetches remain where rank 0 had noted the holder (`origin_repeats_orphan_noted`:
  its one probe missed or the hint expired). Peak RSS 3.0 GiB (unchanged). No hard invariant
  failed; `unexplained` is 0 everywhere; all 16 scenarios at 200 nodes pass.
- Before `PROBE_AFTER`, 5k `zipf` sent 131 successor probes, copied none (the hinted owners were
  still fetching) and failed 29 reads instead of 21; 5k `full-failover` sent 111 probes for 25
  copies. With it, probes copy 94% of the time and `zipf` is unchanged.
- Workspace tests pass; clippy `-D warnings` clean.

### Deviations and open items

- Known issue 21 is resolved for its avoidable part: rank 0 now copies an orphan whenever it
  learned of it and the holder still has it. New Known issue 211: what remains are orphans whose
  holder never reached rank 0 (50k: `route` 285, `partition` 139, `busy` 79, and most of
  `failed` 343, which includes `Unavailable` answers signed by a relay on the route rather than
  by rank 0), so rank 0 holds no hint. Fixing them needs the holder to tell rank 0 later (a push
  or offer once a route exists), a protocol addition not made here.
- New Known issue 212: the repeat classification counts relay-signed `Unavailable` as `failed`
  (rank 0's own origin failure); the simulator should split it from relay failures.
- Production has no counter for successor probes yet: `CandidatePolicy` holds no `Metrics`.
