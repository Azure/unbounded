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
- Node and network faults from the scenario's schedule (`Scenario::faults` and, when
  `fault_mix.max_faults` is set, the drawn `fault_schedule`): `Crash`/`Restart` take a node's
  peer server down (racer-sim `link`, `hop`: a link to it fails after the 100 ms connect
  timeout, a request at it is lost, a reply from or through it is lost); `Partition`/`Heal`
  cut a node pair (links fail, replies over the cut are lost, racer-sim `send_reply`);
  `SlowPeer` scales the node's service times (`Fabric::slow`, as racer-sim `Fault::Slow`).
  Hung peer servers (`ModelSpec::hangs`, racer-sim `Fault::Hang`) accept requests as
  destination or relay and never answer; pings to them time out.
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
  into `LinkHealth`; racer-sim `apply`) and by last-link timeouts, and cleared by silence
  pings (`poll_pings`).
- Flight tables use production admission limits (`admission::Limits::default`: 64 flights of 64
  waiters per worker, as racer-sim's `NodeConfig`); a join past them is answered `Overloaded`.
- Overflow helpers: the core resolves a shed requester's helper with `mesh_helper`
  over `Pool::helper` of the real membership mesh.
- Mesh relays (`Model::forward`, `hop`, `link`, `fail_hop`; racer-sim `forward`/`hop`/
  `link`/`fail_hop`; production `src/peer/forwarding.rs`): every probe is routed by the real
  `topology::Paths` with the core's route budget (`route::search_budget`, `decide_hop`,
  `relay_failure`). Each holder skips silent neighbors and its `liveness::Detours` (fed by
  the core's timed-out `Effect::Silence`), signs, and the next holder verifies before it
  relays or serves. Replies and page bodies travel the route back. A last link still
  unanswered when the requester gives up is a silence timeout for its sender
  (`LINK_TIMEOUTS`), which can start pings. The serving read's inherited budget counts the
  route's links.
- Costs: ranking and dispatch on per-worker CPU servers, page transfers on per-node
  NIC transmit and receive servers (over the route, chunked), route searches on the
  holder's worker, payload open on the
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
- Relay quotas (`ResourceClass::Relay`) and so relay sheds; link circuits (`Circuits`) and
  route search queueing. Route searches run synchronously against one shared `Paths`
  cache, so a search never waits for a slot (racer-sim queued 514,580 at 50,000 nodes).
- Pings travel one direct link; racer-sim routes them (`launch_control`).
- A crash keeps the node's cores and stored pages (production keeps the disk; racer-sim
  drops memory state and in-flight reads). A crashed node's own client reads keep running;
  only its peer server and its replies are gone. Clock faults (`ClockSkew`, `ClockRate`,
  `ClockJump`) are ignored: Model nodes share one clock.
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
| `model-partition-heals` | `reads_recover_after_partition_heals` (6) | at least one storm's reads complete (Model: also every read ends, links crossed the cut) |
| `model-hung-peers` | `hung_relays_are_routed_around`, `hung_peers_are_found_and_recovered_peers_are_used_again` (`GRID`, silence on; 5) | requests are lost to hung peers and found by silence; racer-sim's bounds are at most 2 failures in the first storm and 1 in the second, the Model checks at most 3 in total (its peers hang until 40 s rather than for good, so the run drains) |

| Scenario | Metric | racer-sim | Model |
|---|---|---|---|
| cold readers, 64 nodes | completed / failed | 1024 / 0 | 1024 / 0 |
| | origin requests, origin/page | 16, 1 | 16, 1 |
| | flight joins | 1008 | 1008 |
| | latency ms | 59 / 109 / 121 | 67 / 109 / 120 |
| cold start, 400 nodes | completed / failed | 6400 / 0 | 6400 / 0 |
| | origin requests, origin/page | 30, 3 | 36, 3 |
| | route unreachable | 7 | 8 |
| | relays | 4167 | see Scale |
| | flight joins | 6061 | 9417 |
| | sheds / helper copies | 91 / 3332 | 35 / 3265 |
| | latency ms | 92 / 185 / 220 | 104 / 167 / 2094 |
| cold start, 2000 nodes | completed / failed | 32000 / 0 | 31998 / 2 (`Overloaded`) |
| | origin requests, origin/page | 43, 3 | 43, 3 |
| | flight joins | 30204 | 56141 |
| | sheds / helper copies | 1536 / 27734 | 3967 / 26805 |
| | latency ms | 126 / 2013 / 2125 | 139 / 2105 / 2146 |
| hot-page storm, 512 nodes | completed / failed | 513 / 0 | 513 / 0 |
| | origin requests | 1 | 1 |
| | sheds / helper probes / helper copies | 300 / 496 / 324 | 359 / 460 / 314 |
| | flight joins | 614 | 617 |
| | latency ms | 1879 / 2953 / 3192 | 1422 / 3087 / 3132 |
| origin 403, 64 nodes | completed / failed | 1004 / 20 | 1007 / 17 |
| | origin requests, origin/page | 36, 6 | 33, 12 |
| | latency ms | 92 / 218 / 225 | 74 / 400 / 407 |
| | flights reelected | 20 (`flight_reelected`) | 18 |
| partition heals, 32 nodes | completed / failed | at least 512 (racer-sim's check) | 1008 / 16 |
| | link failures | not recorded | 191 |
| hung peers, 200 nodes (`GRID`) | failed | at most 3 (racer-sim's bounds) | 0 of 12800 |
| | requests lost to hangs / silence timeouts / pings | not recorded | 691 / 505 / 1452 (seed 1) |

Tolerances. The Model passes every racer-sim check above, with these documented
differences:

- Origin requests: at most racer-sim's (exact on cold readers, the hot page and cold start
  2000), except cold start 400: 36 against 30, within `REPLICAS` per page as racer-sim's
  check requires. Both see a handful of unreachable routes (8 against 7); a requester whose
  route search fails gets a transport failure and moves on to the next owner, which may go
  to origin. Over seeds 1..3 the Model reaches origin 35..36 times.
- Completed and failed reads: equal, except cold start 2000 (2 of 32000 failures, inside racer-sim's own 1% bound) and origin 403 (fewer `OriginForbidden`
  failures; the 403 draw order differs because the Model's origin draws come from the
  DST `Choices` stream).
- Latency: p50 within 1.5x of racer-sim; p99 within 1.1x except where a few reads wait out
  one 2 s attempt (the cold start 400 maximum). Treat Model tail latency as an upper bound,
  not a match.
- Sheds, flight joins and helper copies: within an order of magnitude, not closer. The
  Model counts a join for every client or serving read that finds a running flight on its
  worker, including retries; racer-sim's counter may count differently (inference, not
  yet verified). On cold start 400 most
  helper copies come from flight-full `Overloaded` answers rather than hot-page sheds.
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
Model failures within 2x of racer-sim's and the same order of magnitude. Met at 10,000 (79
against 180, origin 25 against 47, p50 356 ms against 335 ms). At 2,000 racer-sim fails none
and the Model 2. Not met at 50,000, where the Model fails 33x fewer reads than racer-sim (757
against 24,742); see below.

| Nodes | racer-sim failed | Model failed | Model retries | Model relays | Model origin requests | Model p50 / p99 | Model wall (release test) | Model peak RSS per node |
|---|---|---|---|---|---|---|---|---|
| 2,000 | 0 of 32,000 | 2 | 2,383 | not recorded | 43 | 139 ms / 2.1 s | under 2 s | not rerun |
| 10,000 | 180 (0.11%), 91,915 retries, 47 origin, 248k relays, 3,986 pings, p50 335 ms | 79 (0.05%) | 45,287 | 190,011 | 25 | 356 ms / 2.2 s | 13 s | 88,964 B |
| 50,000 | 24,742 (3.1%), 757,135 retries, 47 origin, 1,555,149 relays, 15,254 pings, p50 1.48 s | 757 (0.15%) | 371,017 | 1,132,451 | 16 | 1.73 s / 3.06 s | 100 s | 85,616 B |

What closed the gap, with evidence:

- Mesh relays (this change). The hypothesis held: before relays the 10,000-node cold start
  failed 494 reads with p50 2.16 s and 143,753 retries; with relays it fails 79 with p50
  356 ms and 45,287 retries. Relays charge each hop a signature and a verification, so
  the destination owner's crypto and request queues see requests spread over time instead
  of in one burst, and last-link timeouts (1,408 at 10,000; 7,967 at 50,000) feed silence,
  so later routes and visit orders skip hanging owners. At 50,000: 10,046 to 757 failures.
- Signatures. Production verifies every peer request on the crypto helper before the worker
  dispatches it (`src/peer/server.rs` `verify_request_async`) and signs every response
  (`sign_response`); racer-sim charges `sign_ns` per request and response and `verify_ns`
  per hop (`send_reply`, `hop`). Without them, 10k failed 7,105 and 50k 235,631.
  Divergence, production wins: racer-sim charges the destination's verify without delaying
  dispatch; production awaits it. The Model awaits it at relays too.
- Visit order (`visit_order(Spread, ..)` for non-owners), production flight limits (64
  flights, 64 waiters) and silence demotion.

Remaining 50,000-node gap (inference from the racer-sim report of a local run, not yet
verified by experiment): racer-sim's routes are searched as asynchronous tasks on worker CPU
with 8 active searches and a queue per node; it queued 514,580 searches and polled 7,967,612
times, and it reserved a relay quota per hop. The Model has neither (searches finish at
once, see "Not modeled yet"), and racer-sim shed 909,298 hot-page requests. A per-node
128-entry route cache (racer-sim's layout) did not change the 10,000 result (78 failures).
Model pings: 0 at 10,000 and 27 at 50,000 against racer-sim's 3,986 and 15,254; Model pings
use one direct link and racer-sim routes them.

Flight failure semantics (checked, no change): when a leader's walk fails, production
fails every waiter with the leader's error unless it is an origin rejection or a
handed-over deadline/cancellation, which re-elect (`core/src/read/flight.rs` `fail`, used by
`src/read/flight.rs` `Flights::fail` and `src/read/fill.rs` at the `flights.fail` call;
waiters return the error in `fill.rs` `AcquisitionEvent::Failed`). racer-sim
(`finish_fill`) and the core `FlightTable::finish` the Model uses call the same function.

Not ported yet (they need the gaps above): `slow_disk_sheds_persistence...` (persistence
shedding), the relay quota (`relays_hold_their_reservation_only_while_the_body_streams`,
`relay_shed_rank0_probes...`), the full-mesh and watch-off variants of the hung-peer test, and
quota tests in `contention_scenarios.rs`, `sim/tests/mesh_scenarios.rs` (relays and
routes) and `sim/tests/phantom_memory.rs` (racer-sim memory accounting; see the memory
section).

## Model-only scenarios

| Scenario | What it covers |
|---|---|
| `model-range-readers` | range reads through `ReadWindow` with credits, spill holds, selections, worker fetches; a second pass reads every page from disk |
| `model-metadata-readers` | page reads behind `MetadataCohort` refreshes with 10% origin 5xx; refreshes coalesce per cohort |
| `model-faults` | origin 403/5xx mix, four cache outages, page and range readers; failed windows discard buffered pages |
| `model-node-faults` | 64 nodes read twelve objects, one a second, under a drawn schedule of up to 16 crash/restarts, node-pair partitions and slow peers; every read ends and failures are only `Unavailable`/`Overloaded` (seeds 1..6: none fail) |

## Memory

`model_memory_per_node` in `tests/model_parity.rs` (ignored; run alone in release with
`MODEL_NODES=<n>`) reports the peak RSS of a Model cold start divided by the node count.
The racer-sim baseline is 103,189 B/node for `full-cold-start` at 50,000 nodes (peak RSS).

| Nodes | Peak RSS | Per node | Wall (release) | Notes |
|---|---|---|---|---|
| 2,000 | 154,730,000 B | 77,365 B | under 1 s | measured before signature costs |
| 10,000 | 889,640,000 B | 88,964 B | 13 s | with relays, 0.05% fail (see Scale) |
| 50,000 | 4,280,800,000 B | 85,616 B | 100 s | with relays, 0.15% fail (see Scale) |

At 50,000 nodes the Model uses about 83% of racer-sim's per-node peak (85,616 B against
103,189 B) and runs in minutes where racer-sim's `full-cold-start` takes 39 min. The
workloads differ: racer-sim's `full-cold-start` runs the full control plane, and the Model
runs the contention cold start (every node reads one 16-page object).
