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
  racer-sim's `phantom.rs`) and real `topology::Membership` versions.
- Membership versions (`src/model/membership.rs`, racer-sim `Versions` in standalone mode):
  `ModelSpec::churn` and `random_churn` make nodes leave and rejoin; changes publish in
  250 ms batches, each node adopts a version up to `membership_lag` (100 ms) late and
  resolves the 2 versions before it (`RETAINED`). Rankings carry their version: the core's
  `plan`, `mesh` and `helper_of` closures read the version their node adopted, `Rank` answers
  `Verify`, `Latest`, `Previous` (non-members as `NOT_MEMBER`, `recent` within
  `PREVIOUS_MEMBERSHIP_WITHIN`) and `Fallback` under the named version, and `Refresh` reports
  the adopted version and the recent previous one. A holder that does not resolve a
  request's version answers `StaleMembership` (`decide_hop`), and the requester gets the
  unused remote credits and links back (production `request_traced`,
  `stale_unused_links`). Routes, neighbors, columns and helpers map node ids to the
  version's member positions. A node outside the membership it adopted takes no client
  reads (`reads_skipped_not_member`; production `/readyz`).
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
- Relay transfers: a relay reserves one `ResourceClass::Relay` transfer on its worker before
  it forwards (production `src/peer/server.rs:540`, limit `relay_transfers` 16 per worker,
  `src/worker.rs:265`, `core/src/admission.rs:145`); none free fails the hop `Overloaded`
  (`relay_sheds`). The reservation is held until the answer leaves, or for a page until the
  body has passed (racer-sim `send_reply`, `held`). The run fails if any is still held.
- Route search slots: production builds its `Paths` with `with_limits(.., active_path_searches)`
  (`src/app.rs:1320-1333`, `RACER_ACTIVE_PATH_SEARCHES_PER_WORKER` default 8,
  `src/worker.rs:261`) and no wait queue, so a search past 8 running on the worker fails
  `Overloaded` (`topology/src/paths.rs` `route`, `queue_limit` 0). The Model holds a slot until
  the search's CPU work is done (`search_sheds`). Divergence: racer-sim adds a 64-entry queue
  (`sim/src/node/state.rs:218-221`, `with_queue(search_queue)`), which production does not.
- Pings: a silence ping is a peer request routed over the mesh (production
  `CandidatePolicy::ping` through `request_traced`, `src/read/candidates.rs:2488-2531`;
  racer-sim `launch_control`) on `NOTICE_LINKS` links with no remote attempts; the
  requester's own ping may cross the silent link it tests and detours at least once. The
  destination answers `Miss` at once; `ping_verdict` classifies the end.
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
- Membership changes come only from scenario churn; the control plane (racer-sim combined
  mode, `Versions::external`, enrollment) is not modeled, and a node that left keeps serving
  peers and relaying.
- Link circuits (`Circuits`). Route searches run synchronously against one shared `Paths`
  cache (production keeps one per worker), so the Model has more cache hits and fewer
  searches needing a slot.
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
Model failures within 2x of racer-sim's and the same order of magnitude.

| Nodes | racer-sim | Model before relay quotas | Model with relay quotas, search slots, routed pings |
|---|---|---|---|
| 10,000 | 180 failed, 47 origin, 3,986 pings, p50 335 ms | 79 failed, 25 origin, 0 pings, p50 356 ms | 134 failed (22,926 relay sheds, 0 search sheds), 25 origin, 0 pings, p50 353 ms / p99 2.19 s, 89,276 B/node, 28 s |
| 50,000 | 24,742 failed (3.1%), 47 origin, 15,254 pings, p50 1.48 s | 757 failed, 16 origin, 27 pings, p50 1.73 s | 143,726 failed (18%; 1,541,629 relay sheds, 0 search sheds), 17 origin, 39 pings, p50 1.79 s / p99 2.91 s, 89,544 B/node, 221 s (measured before routed pings) |

10,000 is within tolerance. 50,000 is not: without relay quotas the Model failed 33x fewer
reads than racer-sim, with them 5.8x more. racer-sim's own report for the same run
(`rejections.relay`) shows 76,062 relay rejections against the Model's 1,541,629, so the gap
is relay load, not the quota itself. Leads checked:

- Shared route cache (refuted). A cache entry's routes depend only on its `PathKey`
  (`topology/src/paths.rs:165-180`: membership, ends, links, visited, blocked; the seed only
  picks among cached alternatives, `select_route`), so the node that searched does not change
  which relays carry a request. Per-node caches of production's 128 entries
  (`src/worker.rs:262` `cached_paths`) over a shared search memo gave 130 failures at 10,000
  (against 134), 0 search sheds, and 18 KB more per node, so the Model keeps one cache.
- Search latency (refuted for now). No Model search found its worker's 8 slots busy at 10,000
  or 50,000, shared cache or not. racer-sim's 514,580 queued searches come from its 64-entry
  queue, which production does not have (see Route search slots).
- Relay hold (matches production). Production keeps the `Relay` reservation on the connection
  from before `forward_inner` until the downstream answer has been written
  (`src/peer/server.rs:540-560` reserves and awaits `forward_inner`; `:778` clears
  `relay.reservation` after the relayed body is written), so a relay holds its transfer while
  an `Acquire` destination fetches, as the Model does. racer-sim releases at the same point
  (`sim/src/node/world.rs` `send_reply`, `held`).
- Relay hold breakdown (`MODEL_DIAG=1`, `relay_hold:<end>:<ms>` and `relay_shed:<kind>:hop<n>`
  in `attempt_errors`). At 50,000 every shed is at the first relay (`hop1`) of a probe, never a
  ping. Holds of answered requests: 128-511 ms for 562,000 of 1,108,000, mostly an owner's
  reply waiting on its crypto helper (owner sheds are decided at once, `served:owner*:Overloaded:0ms`,
  but reach the reader 250-750 ms later). At 10,000 most holds are 8-127 ms.
- Crypto helpers (open, the strongest lead). Production pairs each worker with its own crypto
  helper (`src/worker.rs:124-130` `WorkerPair { io, crypto }`); the Model and racer-sim have one
  per node (`src/resources.rs` `Fabric::crypto`). racer-sim also does not wait for request
  verification (`sim/src/node/world.rs` `hop` charges `verify_ns` and goes on), where the Model
  and production do (`src/peer/forwarding.rs:368` `verify_request_async`). One helper per
  worker, the connection's worker as `probe % workers` (an inference about production's
  listener), measured: 50,000 failed 58,358 (7.3%, 1,031,764 relay sheds, p50 0.90 s), 2.4x
  racer-sim; but 10,000 failed 806 (4.5x racer-sim; 15,979 relay sheds, p50 268 ms) and 400 and
  2,000 were unchanged. Not committed: it moves 10,000 out of tolerance. At 10,000 the extra
  failures are walks `OOver HUnav OFail OFail`: owner 0 sheds, the helper's flight fails at
  once because the helper is itself a reader whose own walk failed, and the reader's link
  budget is spent before owners 1 and 2 (`HopBudgetExhausted`, not sent).

Pings (diagnosed in part, not fixed). racer-sim at 10,000 (seed 8, this commit): 4,058 last-link
timeouts and 3,986 pings, about one ping per timeout; the Model: 1,289 last-link timeouts and 0
pings. Production asks a ping after `LINK_TIMEOUTS` (2) last-link timeouts of the same peer
(`src/peer.rs:1146`) or one rank 0 probe timeout (`core/src/read/walk.rs:2184-2195`), and the
probe's `TimedOut` note reaches `LinkHealth` only through `CandidatePolicy::apply`
(`src/read/candidates.rs:1546-1580`). So a single last-link timeout pings only when the same
peer already counts a timeout, which in racer-sim is the requester's own probe timeout
(`ProbeTimeout` runs before `LastHop` at the same instant). `MODEL_DIAG=1` shows the Model's
1,476 probe timeouts (all of helpers, `timeout:reader->helper:*`) are fed to the core
(`probe_timeout_fed:Ok`) but yield no `Effect::Silence` (`probe_timeout_fed:*:silence0`, no
`silence_timeout:*` keys), so each last link is the peer's first timeout
(`lasthop:knownfalse:wantfalse` 1,288 of 1,289). Next step: find why the acquisition reports no
evidence for a helper probe's `TimedOut` (`core/src/read/acquisition.rs:633-642` needs a held
ranking and a destination) and whether production behaves the same.

Cold start 400 origin requests (36 against 30) are not yet investigated further than the
note under Tolerances.

What closed the 10,000-node gap earlier: mesh relays (494 to 79 failures), signature costs
(production `verify_request_async`, `sign_response`), visit order, production flight limits
and silence demotion.

Flight failure semantics (checked, no change): when a leader's walk fails, production
fails every waiter with the leader's error unless it is an origin rejection or a
handed-over deadline/cancellation, which re-elect (`core/src/read/flight.rs` `fail`).

Not ported yet: `slow_disk_sheds_persistence...` (persistence shedding),
`relays_hold_their_reservation_only_while_the_body_streams` and `relay_shed_rank0_probes...`
(the quota now exists; the scenarios are not ported), the full-mesh and watch-off variants
of the hung-peer test, quota tests in `contention_scenarios.rs`, `sim/tests/mesh_scenarios.rs`
(topology-only mesh metrics) and `sim/tests/phantom_memory.rs` (racer-sim memory accounting).

## Model-only scenarios

| Scenario | What it covers |
|---|---|
| `model-range-readers` | range reads through `ReadWindow` with credits, spill holds, selections, worker fetches; a second pass reads every page from disk |
| `model-metadata-readers` | page reads behind `MetadataCohort` refreshes with 10% origin 5xx; refreshes coalesce per cohort |
| `model-faults` | origin 403/5xx mix, four cache outages, page and range readers; failed windows discard buffered pages |
| `model-membership-churn` | port of racer-sim's `churn` scenario (`sim/src/lib.rs` `scenario_schedules`: a tenth of the nodes leave and rejoin a quarter of the run later) on 200 nodes reading a new object every 250 ms; see below |
| `model-node-faults` | 64 nodes read twelve objects, one a second, under a drawn schedule of up to 16 crash/restarts, node-pair partitions and slow peers; every read ends and failures are only `Unavailable`/`Overloaded` (seeds 1..6: none fail) |

### Membership churn

racer-sim `--scenario churn --nodes 200 --seed 1` (2 s, its Poisson workload) against the
Model's `model-membership-churn` seed 1 (200 nodes, 8 storms of one 16-page object, 2 s of
churn). The workloads differ, so only the membership counters compare in kind:

| Metric | racer-sim | Model |
|---|---|---|
| reads completed / failed | 11,801 / 12 (`Unavailable`) | 24,960 / 0 |
| reads skipped on non-members | 166 | 640 |
| membership changes / publications | 37 / 8 | 40 / 9 |
| `StaleMembership` replies | 516 | 1,472 |
| stale waits (`Sleep`) | 7 | 11 |
| links refunded | 2,833 | 8,066 |

The Model's CI churn seeds exercise `Acquisition::Refresh`, `Acquisition::Sleep` and
`Acquisition::Batch` (handoff targets under the previous membership).

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
