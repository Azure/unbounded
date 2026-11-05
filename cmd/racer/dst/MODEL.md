# Model fidelity: what is modeled

`Fidelity::Model` runs `racer_dst::model`: a byte-free interpreter over the real
`racer_core::read::worker::WorkerCore` machines. It is dispatch-only: every decision
(where to probe, when to reach origin, whom to shed, which flight to join) is made by
the cores. The interpreter completes each effect on the FIFO cost model
(`src/resources.rs`) priced from `costs.toml`, and answers it with facts of the world,
such as which node holds which page.

## Modeled

- N nodes with W real `WorkerCore`s each. A page's flight lives on the worker that
  `owner_worker` maps it to; client page reads land on that worker. A client range read
  runs its `ReadWindow` on worker `node % W`, so pages owned by other workers cross as
  `WorkerMsg::Fetch`/`Fetched`.
- Client: racer-sim's retry rule (`resolve` in `sim/src/node/world.rs`): a recoverable
  failure is retried up to 4 attempts with a linear backoff; latency runs from the first
  attempt.
- Range reads (`Storm::range`): windows with client credits (`window_pages`), probes of
  the node store, spill holds and spill fills, and buffered-page discards. A provider
  selection is answered as not started, so its pages are filled directly.
- Metadata (`ModelSpec::metadata`): each page read first refreshes its object through
  the worker's `MetadataCohort`; the refresh is priced as an origin HEAD and draws the
  origin error mix.
- Origin errors (`ModelSpec::origin_errors`): racer-sim's mapping (412 to
  `VersionUnavailable`, 403 to `OriginForbidden`, 5xx to `Unavailable`).
- Cache outages (`ModelSpec::outages`): the node loses its stored pages and answers peer
  probes with `Miss` (`ServeFacts::cache_available` false) until the outage ends.
- Page store: published pages per node, bounded by `ModelSpec::stored_pages` with
  least-recently-used eviction (0 is unbounded).
- Placement: a real `topology::Placement` over `Phantom` rankings (copied from
  racer-sim's `phantom.rs`) and a real `topology::Membership`. Member positions are
  node ids.
- Page flights: joins, leader election, handover and settlement run in the core's
  `FlightTable`, including on owners that serve peer `Acquire` probes.
- Acquisition walks: ranking, peer probes, validation, origin fetches and publication
  run in the core's `PageAcquisition`.
- Serve side: `WorkerCore::serve` (dispatch table, hot-page shedding over live body
  counters, copy-only answers, successor deferral). A body counts as streaming from
  its first byte until its last byte lands.
- Visit order: a non-owner's Rank is answered with production `visit_order` (`Spread` over
  `replica_score`, mesh neighbors first) and `demote_unanswered` over a node-wide
  `liveness::Silence`, fed by the core's `Effect::Silence` (production `CandidatePolicy::apply`
  into `LinkHealth`; racer-sim `apply`). Silence pings are not modeled, so a timed-out owner
  stays demoted until it answers this node again.
- Flight tables use production admission limits (`admission::Limits::default`: 64 flights of 64
  waiters per worker, as racer-sim's `NodeConfig`); a join past them is answered `Overloaded`.
- Overflow helpers: the core resolves a shed requester's helper with `mesh_helper`
  over `Pool::helper` of the real membership mesh.
- Costs: ranking and dispatch on per-worker CPU servers, page transfers on per-node
  NIC transmit and receive servers (direct route, chunked), payload open on the
  crypto server, publication on the disk write server, origin fetches on the shared
  origin model (per-cache concurrency, shared bandwidth, latency), RTT from the
  per-site matrix.
- Invariants: every effect id is answered at most once, every client read ends, every
  serving read is answered, every body ends, every range read and metadata refresh ends,
  and every core is quiescent (no flights, requests, acquisitions, windows or metadata
  requests) at the end. Violations count as `hard_failures`. Scenario checks run on the
  JSON report.
- Coverage parity (plan section 9.2): `src/model/coverage.rs` lists every core effect and
  serve outcome variant. `make racer-dst` reruns the CI Model entries once and fails when
  a variant is not exercised, unless it is unreachable by construction or listed in
  `NOT_YET` with its reason. A `NOT_YET` variant that the CI set does exercise also fails,
  so the list cannot go stale.

## Not modeled yet

- Hedges (`Hedge` answers `NotStarted`; `FillConfig::hedge` is off), successor hint notes
  and orphan notices (`Hints` answers no hints, `Batch` answers no copy), so successor
  deferral (`Served::Defer`) never happens.
- Membership changes: every read uses version 1 of one membership, so `Refresh` and the
  stale-membership `Sleep` never run.
- Mesh relays and route failures (racer-sim's `SharedRoutes` in `sim/src/phantom.rs`):
  probes and transfers go directly between nodes. Relay quotas and relay sheds are absent.
- Silence pings (`Silence::due_pings`, `ping_result`) and relay detours.
- Node crash/restart, partitions and slow peers. Cache outages are the only node fault.
- Admission quotas, persistence shedding (`persist_no_headroom`), memory tiers and the
  DiskIndex; per-node memory accounting (`memory.accounted_bytes_per_node`).
- Provider selection streams (`Offered`), prefetch traffic.
- Rejections (`Served::Reject`): Model probes always match the object and operation.

## Ported scenarios

Every racer-sim number below is from the racer-sim release binary at this commit
(`racer-sim --scenario empty --config <json> --seed <seed>`, configs equal to the test's
`Config`), and every Model number from `cargo test -p racer-dst --test model_parity` (or
`--test model`) with the same seed. Latency is the client read latency in ms
(p50/p99/max). "Origin/page" is the most origin requests for one page.

| racer-dst scenario | racer-sim source (seed) | Checks (racer-sim's) |
|---|---|---|
| `model-concurrent-cold-readers` | `concurrent_cold_readers_join_flights` (2) | every read delivered, flight joins, origin at most `pages * REPLICAS`, at most `2 * REPLICAS` per page, origin under 1/8 of reads |
| `model-cold-start-400`, `model-cold-start-2000` | `cold_start_completes_with_bounded_origin_traffic`, `two_thousand_nodes_cold_start` (8) | every read ends, failures only `Overloaded`/`Unavailable` and at most 1%, no `HopBudgetExhausted`, origin/page in `1..=REPLICAS`, pages published |
| `model-hot-page-storm-512` | `hot_page_readers_fan_out_through_helpers`, `mesh_hot_page_readers_fan_out_through_column_helpers` (5) | every read completes, owners shed, helpers serve copies, origin once |
| `model-origin-forbidden` | `origin_forbidden_reelects_a_waiter` (11) | 403s fail reads with `OriginForbidden`, the rest complete |

| Scenario | Metric | racer-sim | Model |
|---|---|---|---|
| cold readers, 64 nodes | completed / failed | 1024 / 0 | 1024 / 0 |
| | origin requests, origin/page | 16, 1 | 16, 1 |
| | flight joins | 1008 | 1008 |
| | latency ms | 59 / 109 / 121 | 77 / 148 / 153 |
| cold start, 400 nodes | completed / failed | 6400 / 0 | 6400 / 0 |
| | origin requests, origin/page | 30, 3 | 16, 1 |
| | flight joins | 6061 | 9724 |
| | sheds / helper copies | 91 / 3332 | 31 / 3220 |
| | latency ms | 92 / 185 / 220 | 103 / 2047 / 2054 |
| cold start, 2000 nodes | completed / failed | 32000 / 0 | 31958 / 42 (14 `Unavailable`, 28 `Overloaded`) |
| | origin requests, origin/page | 43, 3 | 33, 3 |
| | flight joins | 30204 | 68214 |
| | sheds / helper copies | 1536 / 27734 | 6754 / 24629 |
| | latency ms | 126 / 2013 / 2125 | 160 / 4130 / 6110 |
| hot-page storm, 512 nodes | completed / failed | 513 / 0 | 513 / 0 |
| | origin requests | 1 | 1 |
| | sheds / helper probes / helper copies | 300 / 496 / 324 | 383 / 484 / 311 |
| | flight joins | 614 | 642 |
| | latency ms | 1879 / 2953 / 3192 | 1308 / 3204 / 3281 |
| origin 403, 64 nodes | completed / failed | 1004 / 20 | 1007 / 17 |
| | origin requests, origin/page | 36, 6 | 33, 12 |
| | latency ms | 92 / 218 / 225 | 74 / 400 / 407 |
| | flights reelected | 20 (`flight_reelected`) | 19 |

Tolerances. The Model passes every racer-sim check above, with these documented
differences:

- Origin requests: at most racer-sim's (exact on cold readers and the hot page; cold start
  400 and 2000 reach origin less, 16 against 30 and 33 against 43, because spread readers
  rarely send a non-rank-0 owner to origin before rank 0 publishes).
- Completed and failed reads: equal, except cold start 2000 (41 of 32000 failures, inside racer-sim's own 1% bound) and origin 403 (fewer `OriginForbidden`
  failures; the 403 draw order differs because the Model's origin draws come from the
  DST `Choices` stream).
- Latency: p50 within 1.5x of racer-sim; the Model is slower in the tail (p99 2x on cold
  start 2000, about 11x on cold start 400, where a few reads wait out one 2 s attempt). There
  are no relays. Treat Model tail latency as an upper bound, not a match.
- Sheds, flight joins and helper copies: within an order of magnitude, not closer. The
  Model counts a join for every client or serving read that finds a running flight on its
  worker, including retries; racer-sim's counter may count differently (inference, not
  yet verified). On cold start 400 the Model
  never sheds (its helper copies come from flight-full `Overloaded` answers), because
  without relays owners never stream enough bodies at once to cross the hot-page threshold.
- Origin/page under 403s: an interpreter draw-order difference, not a core decision
  difference. In both racer-sim and the Model every 403 fails exactly its leader and re-elects
  one waiter (`origin_forbidden_fails_only_the_leader` asserts one `OriginForbidden` failure
  per 403 and at least as many re-elections; a leader shed by every owner re-elects too, 2 of
  19 here), so origin requests are always pages + 403s
  (racer-sim 16 + 20 = 36, Model 16 + 17 = 33). The per-page maximum is the longest run of
  403 draws on one page; over Model seeds 1..20 it ranges 3..12 (mean 6), and seed 11 happens
  to be the 12 tail.

## Scale

Tolerance for the scale cold start (seed 8, every node reads one 16-page object in 10 ms):
Model failures within 2x of racer-sim's and the same order of magnitude. Met at 50,000; at
10,000 the Model fails 2.7x racer-sim's count (same order, 0.31% against 0.11%); at 2,000
racer-sim fails none, so a ratio does not apply (the Model fails 36 of 32,000).

| Nodes | racer-sim failed | Model failed | Model retries | Model origin requests | Model wall (release test) | Model peak RSS per node |
|---|---|---|---|---|---|---|
| 2,000 | 0 of 32,000 | 36 (0.11%) | 10,608 | 34 | under 20 s with build | 77,365 B (not rerun) |
| 10,000 | 180 (0.11%), 91,915 retries, 47 origin | 494 (0.31%) | 143,753 | 24 | 22 s with build | 77,869 B |
| 50,000 | 24,742 (3.1%); 8 min 45 s, 8,041,764 KiB RSS | 10,046 (1.3%) | 865,291 | 16 | 2 min | 80,915 B |

What closed the gap, with evidence:

- Signatures. Production verifies every peer request on the crypto helper before the worker
  dispatches it (`src/peer/server.rs` `verify_request_async`) and signs every response
  (`sign_response`); racer-sim charges `sign_ns` per request and response and `verify_ns`
  per hop (`send_reply`, `hop`). The Model charged neither, so an owner answered thousands
  of sheds at once and walks failed within 250 ms, before any copy spread; four attempts
  burned out in the first second. Now the requester signs, the destination verifies and
  then takes `cpu.request_ns` on the page's worker FIFO, the responder signs before the
  reply or body, and the requester charges `verify_ns` on arrival without waiting for it
  (as racer-sim does). 10k: 7,105 to 494 failures; 50k: 235,631 to 10,046.
  Divergence, production wins: racer-sim charges the destination's verify without delaying
  dispatch; production awaits it. With racer-sim's form the Model fails 1,574 at 10k.
- Visit order (`visit_order(Spread, ..)` for non-owners), production flight limits (64
  flights, 64 waiters) and silence demotion, as before.
- Silence pings are modeled (`poll_pings`, `ping_arrived`, `ping_done`; production
  `CandidatePolicy::poll_pings` in `src/read/candidates.rs`). Only rank-0 timeouts ask for
  one (`walk.rs` `RANK0_TIMEOUTS`); the cold start has none, so it sends no pings.
  racer-sim's 3,986 pings at 10k come from relay last-hop timeouts, which the Model lacks.
- The earlier 50k regression was the missing signature cost, not the waiter limit.

Flight failure semantics (checked, no change): when a leader's walk fails, production
fails every waiter with the leader's error unless it is an origin rejection or a
handed-over deadline/cancellation, which re-elect (`core/src/read/flight.rs` `fail`, used by
`src/read/flight.rs` `Flights::fail` and `src/read/fill.rs` at the `flights.fail` call;
waiters return the error in `fill.rs` `AcquisitionEvent::Failed`). racer-sim
(`finish_fill`) and the core `FlightTable::finish` the Model uses call the same function.

Remaining differences: racer-sim's p50 read latency is 335 ms at 10k, the Model's 2.2 s.
Model helpers fail 93k serving reads at about 1.75 s (their own client attempt reaching the
2 s cap), racer-sim helpers 36k. racer-sim routes requests through mesh relays (248k relays
at 10k, each signing and verifying); the Model sends every probe in one hop.

Not ported yet (they need the gaps above): `slow_disk_sheds_persistence...` (persistence
shedding), `reads_recover_after_partition_heals` (partitions), the relay, hung-peer and
quota tests in `contention_scenarios.rs`, `sim/tests/mesh_scenarios.rs` (relays and
routes) and `sim/tests/phantom_memory.rs` (racer-sim memory accounting; see the memory
section).

## Model-only scenarios

| Scenario | What it covers |
|---|---|
| `model-range-readers` | range reads through `ReadWindow` with credits, spill holds, selections, worker fetches; a second pass reads every page from disk |
| `model-metadata-readers` | page reads behind `MetadataCohort` refreshes with 10% origin 5xx; refreshes coalesce per cohort |
| `model-faults` | origin 403/5xx mix, four cache outages, page and range readers; failed windows discard buffered pages |

## Memory

`model_memory_per_node` in `tests/model_parity.rs` (ignored; run alone in release with
`MODEL_NODES=<n>`) reports the peak RSS of a Model cold start divided by the node count.
The racer-sim baseline is 103,189 B/node for `full-cold-start` at 50,000 nodes (peak RSS).

| Nodes | Peak RSS | Per node | Wall (release) | Notes |
|---|---|---|---|---|
| 2,000 | 154,730,000 B | 77,365 B | under 1 s | measured before signature costs |
| 10,000 | 778,698,752 B | 77,869 B | 4 s | 0.31% fail (see Scale) |
| 50,000 | 4,045,778,944 B | 80,915 B | 2 min | 1.3% fail (see Scale) |

At 50,000 nodes the Model uses about 78% of racer-sim's per-node peak (80,915 B against
103,189 B) and runs in minutes where racer-sim's `full-cold-start` takes 39 min. The
workloads differ: racer-sim's `full-cold-start` runs the full control plane, and the Model
runs the contention cold start (every node reads one 16-page object).
