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
- Replacing the existing deterministic tests (`src/app/tests/dst.rs`) or `src/contention.rs`.
  `src/contention.rs` stays until `racer-sim` covers its scenarios.
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

2. `RouteBudget::forwarded` and `RouteBudget::validate_forwarded`
   (`cmd/racer-dataplane/src/topology.rs:536`, `:552`) are called only by tests. They use the search form, where
   `visited` excludes the sender, while the signed wire form includes it (`racer_core::route`). The two
   conventions are inconsistent; a future caller must pick the right one.

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
