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
  waiters per worker, as racer-sim's `NodeConfig`). A join past the entry cap is answered
  `Overloaded`; a join past a flight's waiter cap is answered `FlightFull`
  (`core/src/read/flight.rs` `JoinReject::WaiterCap`), which proves the flight is fetching.
- Ranking CPU: a worker's first ranking of a page under a membership costs one
  `ranking_chunk_ns` per 256 members (production `topology/src/placement.rs` `rank_async`,
  `WORK_QUANTUM`; racer-sim `spawn`), later ones one chunk (the worker's placement cache).
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
  the search's CPU work is done (`search_sheds`). racer-sim's `search_queue` now defaults to 0
  as well (`sim/src/config.rs` `NodeConfig::default`). A requester's own shed search fails the
  probe `Overloaded` (production `request_network`, `src/peer.rs:919-931`), not `Unavailable`:
  it is no evidence against the destination (Model `fail_hop`, racer-sim `Reply::Overloaded`).
- Request verification: every holder verifies the request on the crypto helper of the
  connection's worker and dispatches or relays only after (production `src/peer/server.rs:515-518`,
  `:792`; `src/worker.rs` `WorkerPair`). racer-sim does the same since this commit (`hop`,
  `Msg::verified`, per-worker `crypto_free`); before, it charged one helper per node and went on.
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
| | flight joins | 998 | 1008 |
| | latency ms | 58 / 117 / 131 | 67 / 109 / 120 |
| cold start, 400 nodes | completed / failed | 6400 / 0 | 6400 / 0 |
| | origin requests, origin/page | 16, 1 | 16, 1 |
| | route unreachable | 7 | 8 |
| | relays | 5012 | see Scale |
| | flight joins | 6434 | 10336 |
| | sheds / helper copies | 604 / 3821 | 681 / 3896 |
| | latency ms | 92 / 184 / 2097 | 111 / 2049 / 2097 |
| cold start, 2000 nodes | completed / failed | 31997 / 3 (`FlightFull`) | 31998 / 2 (`FlightFull`) |
| | origin requests, origin/page | 16, 1 | 17, 2 |
| | flight joins | 32322 | 60927 |
| | sheds / helper copies | 6053 / 26056 | 4549 / 25587 |
| | latency ms | 150 / 2147 / 2218 | 172 / 2154 / 2193 |
| hot-page storm, 512 nodes | completed / failed | 513 / 0 | 513 / 0 |
| | origin requests | 1 | 1 |
| | sheds / helper probes / helper copies | 446 / 498 / 315 | 444 / 454 / 308 |
| | flight joins | 616 | 606 |
| | latency ms | 1879 / 3221 / 3307 | 1399 / 3163 / 3225 |
| origin 403, 64 nodes | completed / failed | 1004 / 20 | 1007 / 17 |
| | origin requests, origin/page | 36, 6 | 33, 12 |
| | latency ms | 83 / 218 / 232 | 75 / 402 / 410 |
| | flights reelected | 21 (`flight_reelected`) | 19 |
| partition heals, 32 nodes | completed / failed | at least 512 (racer-sim's check) | 1008 / 16 |
| | link failures | not recorded | 191 |
| hung peers, 200 nodes (`GRID`) | failed | at most 3 (racer-sim's bounds) | 0 of 12800 |
| | requests lost to hangs / silence timeouts / pings | not recorded | 691 / 505 / 1452 (seed 1) |

Tolerances. The Model passes every racer-sim check above, with these documented
differences:

- Origin requests: at most racer-sim's (exact on cold readers, the hot page and cold start
  400), except cold start 2000: 17 against 16, within `REPLICAS` per page as racer-sim's
  check requires. Both reach origin once per page at 400 nodes (see Scale). Every row was
  re-measured after racer-sim adopted production's search slots and request verification.
- Completed and failed reads: equal, except cold start 2000 (2 against 3 `FlightFull`) and origin 403 (fewer `OriginForbidden`
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
Model failures within 2x of racer-sim's and the same order of magnitude, origin requests at
most racer-sim's.

Since the overflow helper spends its own grant (`core/src/read/walk.rs` `helper_grant`), both
fidelities changed, because racer-sim drives the same racer-core walk. Before and after, both
at seed 8 (racer-sim from `NodeWorld` with the `cold_start` test's config, release):

| Nodes | racer-sim before | racer-sim after | Model before | Model after |
|---|---|---|---|---|
| 400 | 0 failed, 30 origin | 0 failed, 30 origin | 0 failed, 36 origin | 0 failed, 36 origin, p50 104 ms / p99 167 ms |
| 2,000 | 0 failed, 43 origin | 0 failed, 43 origin | 2 failed, 43 origin | 2 failed (`Overloaded`), 43 origin, p50 128 ms / p99 2.12 s |
| 10,000 | 180 failed, 47 origin | 44 failed, 47 origin, 11,432 pings | 682 failed, 25 origin | 95 failed (16,029 relay sheds), 25 origin, 8,773 pings, p50 295 ms / p99 2.23 s, 91,004 B/node |
| 50,000 | 24,742 failed (3.1%), 47 origin | 1,094 failed (0.14%), 35 origin, 97,468 pings | 56,145 failed (7.0%), 21 origin | 30,697 failed (3.8%; 1,030,672 relay sheds), 21 origin, 35,011 pings, p50 0.96 s / p99 2.69 s, 92,488 B/node |

At 10,000 the Model now fails 2.2x racer-sim's reads (was 3.8x). At 50,000 the gap grew to
28x (was 2.3x): racer-sim gained more from the helper fix than the Model did. The cause was
racer-sim's divergence from production, found below (50,000-node gap).

Current parity (seed 8, failed reads / origin requests, racer-sim release binary on
`tmp/parity/cold<N>.json`, Model `model_memory_per_node` with `MODEL_NODES`). racer-sim now has
production's search slots (no queue) and request verification (see Modeled):

| Nodes | racer-sim | Model | Model / racer-sim failures | relay sheds (racer-sim / Model) | latency p50 / p99 (racer-sim / Model) |
|---|---|---|---|---|---|
| 400 | 0 / 16 | 0 / 16 | equal | 12 / not recorded | 92 ms, 184 ms / 111 ms, 2.05 s |
| 2,000 | 3 / 16 (`FlightFull`) | 2 / 17 (`FlightFull`) | 0.7x | 787 / 863 | 150 ms, 2.15 s / 172 ms, 2.15 s |
| 10,000 | 204 / 16 | 118 / 16 | 0.6x | 25,023 / 15,768 | 268 ms, 2.15 s / 303 ms, 2.23 s |
| 50,000 | 56,073 / 16 (7.0%) | 36,933 / 16 (4.6%) | 0.7x | 994,407 / 1,094,694 | 1.07 s, 2.68 s / 1.12 s, 2.90 s |

Within tolerance at every size: failures within 2x and the same order of magnitude, origin
requests equal (cold start 2000: 17 against 16, within `REPLICAS` per page). The cold start's
own 1% failure check fails at 50,000 for both fidelities, so it is a production result (see
the open question below), not a Model artifact.

Since rank 0 answers `FlightFull` and the Model charges full first rankings (see below), seed
8, failed reads / origin requests. "racer-sim, no queue" sets `search_queue` to 0, as in
production (see Route search slots):

| Nodes | racer-sim | racer-sim, no queue | Model, `FlightFull` | Model, `FlightFull` and ranking CPU |
|---|---|---|---|---|
| 400 | 0 / 16 | not run | 0 / 16 | 0 / 16 (p50 111 ms, p99 2.05 s) |
| 2,000 | 2 / 16 (`FlightFull`) | not run | 3 / 16 | 2 / 17 (`FlightFull`) |
| 10,000 | 68 / 16 | 90 / 16 | 97 / 16 | 118 / 16 |
| 50,000 | 1,123 / 16 | 4,343 / 19 | 31,128 / 16 | 36,933 / 16 |

Within tolerance at 400, 2,000 and 10,000 (1.7x racer-sim, 1.3x racer-sim without the
queue). At 50,000 the Model fails 33x racer-sim and 8.5x racer-sim without the queue. Superseded
by Current parity above.

`FlightFull`. A flight at its waiter cap now answers `FlightFull`
(`core/src/read/flight.rs` `JoinReject::WaiterCap`, `join_copy_existing`; production
`src/read/flight.rs` `join_prepared`, through `peer_error` in `src/read.rs`), wire outcome
`flight-full` (`src/peer/protocol.rs`), HTTP 503 for clients (`src/client.rs`). The entry
cap, quotas and admission still answer `Overloaded`: none of them proves a fetch. The walk
takes a signed `FlightFull` as `Outcome::Fetching` (`core/src/read/walk.rs`): like a shed
(helper, busy evidence), and when it answers an owner's page probe of rank 0 it sets
`rank0_unanswered`, so the owner's walk ends `Exhausted` and its callers retry and join rank 0's
flight, as after a timeout of rank 0. Unlike a timeout this holds in every rank-0 watch state,
including `Overdue`: the watch tracks silence, and the answer disproves it; the flight ends
on its own deadline, which bounds the wait. Unsigned (relayed) `FlightFull` stays a relay
`Overloaded`. Cold start 400 went from 36 (Model) and 30 (racer-sim) origin requests to 16 in
both, and 2,000 from 43 to 16 or 17. The cost is tail latency at 400 nodes (Model p99 167 ms
to 2.05 s): a few readers whose owner gets `FlightFull` from rank 0 wait out an attempt and
retry instead of reading the owner's own origin copy.

Ranking CPU (production-faithful, adopted). Production ranks with `rank_async`, 256 members
per poll (`topology/src/placement.rs:35`, `:793-810`), in a placement cache per worker
(`src/app.rs:1343` `Placement::with_memory_budget`). racer-sim charges one `ranking_chunk_ns`
per poll (`sim/src/node/world.rs` `spawn`), 157 M polls at 50,000 nodes. The Model charged one
chunk per ranking; it now charges a worker's first ranking of a page in full
(`ranking_polls`). This was a lead for the 50,000-node gap and did not close it.

50,000-node gap (closed). The earlier comparison was against a racer-sim that diverged
from production in two ways, and the Model was the faithful side in both:

- Search queue. Production builds `Paths` with `with_limits` (`src/app.rs:1327`,
  `src/topology.rs:984-993`) and never calls `with_queue`, so `queue_limit` is 0
  (`topology/src/paths.rs:479`) and a search past the 8 slots fails `Overloaded`
  (`paths.rs:612-614`). racer-sim queued 64. It now defaults to 0. A requester's own shed
  search reached both fidelities' walks as a transport `Unavailable`, which counts against
  rank 0 and let owners above rank 0 overlap rank 0's origin fetch (racer-sim's
  `owners_above_rank_zero_do_not_overlap_a_slow_origin_fetch` failed once the queue was off).
  Production returns `Error::Overloaded` (`src/peer.rs:919-931` through `observer.result`),
  which `transport_outcome` (`core/src/read/errors.rs`) reads as `Overloaded`; both fidelities
  now do the same.
- Request verification (the root cause). Production verifies each request before it serves or
  relays it (`src/peer/server.rs:515-518`, `:792`), on the crypto helper paired with the
  connection's worker (`src/worker.rs` `WorkerPair`). A relay reserves its `Relay` transfer
  after its own verification (`server.rs:539-546`) and keeps it until the answer is written back
  (`:778`), so its hold includes the next holders' verification queues. The Model waits
  (`Model::hop`, `verified`). racer-sim charged `verify_ns` on one helper per node and dispatched
  at once (`hop`), so the destination answered, and the relays released their transfers, before
  the destination's verification queue drained. Relay holds on non-page answers
  (`relay_hold_answer_ns`, new) at 50,000: racer-sim p50 11.5 ms, p90 42 ms before; Model mostly
  64 to 256 ms (`relay_hold:answered:*`); racer-sim p50 75 ms, p90 252 ms after. With verification
  waited on one helper per node racer-sim failed 123,903 reads (1,438,866 relay sheds); with one
  helper per worker, as production and the Model, 56,073.

Relay sheds stay at the first relay in both: hot owners' verification queues hold every relay
toward them, and 16 `Relay` transfers per worker (`src/worker.rs:265`) run out. Both
fidelities release a relay when the destination decides its answer, before the answer is signed
and carried back (Model `reply`, racer-sim `send_reply`); production holds it until the answer
is written (`server.rs:778`), so both under-state holds slightly.

Open question (production). With these costs (`verify_ns` 40 us, `sign_ns` 20 us, 2 workers)
a 50,000-node cold start fails 4.6% (Model) to 7.0% (racer-sim) of reads, all from relay sheds
behind hot owners' verification queues. Remedies (more `Relay` transfers, shedding before
verification, or bounding a relay's hold) live in `src/peer/server.rs` and `src/worker.rs`,
outside racer-core; not attempted here.

Earlier leads, kept for the record: read attempts failed at similar rates but the Model's
failures clustered (four attempts within a second); per-node route caches, the destination not
waiting for verification (in the Model only, with one queue still coupling verify and sign),
and the ranking cost did not close the gap.

Crypto helpers (production-faithful, adopted). Production pairs each worker with its own
crypto helper (`src/worker.rs:124-130` `WorkerPair { io, crypto }`); racer-sim had one per
node and now has one per worker too. The Model now has one per worker (`resources.rs` `Fabric::crypto`), and a connection's
worker is `probe % workers` (racer-sim `hop`; an inference about production's listener). The
Model and production also wait for request verification before dispatch
(`src/peer/forwarding.rs:368` `verify_request_async`); racer-sim charged `verify_ns` and went
on until it adopted the wait (see 50,000-node gap).

Fixed: helper detours starved the remaining owners. Before the fix, the extra 10,000-node
failures were walks `OOver HUnav OFail OFail` (`MODEL_DIAG=1`, `walk:*`). Owner 0 shed the
reader (`Outcome::Shed`). The helper was funded from the walk's own 8-attempt, 16-link page
budget (`min(remaining, HELPER_ATTEMPTS)` attempts and `min(remaining, HELPER_LINKS)` links).
It then failed at once with `Unavailable` and came back empty. Owners 1 and 2 had no credits
left, so they failed to plan with `Unavailable` (0 attempts) or `HopBudgetExhausted` (0 links).
At 2,000 nodes there were 1,647 such walks and 4,879 unsent owner probes. After the fix there
are none and 504 unsent probes. The helper now spends its own grant, which is dropped after
the probe, so a walk asks at most one helper and the walk's budget never grows.

Pings. Production records a probe's silence evidence through `CandidatePolicy::apply`
(`src/read/candidates.rs:1546-1580`) with the helper as the destination
(`ProbeDest::Helper`, `:1486`, `:1563`), so a helper probe's timeout counts against the helper
(`needed` = `NO_PING`, `core/src/read/walk.rs:2184-2195`) and a later last-link timeout of the
same peer reaches `LINK_TIMEOUTS` (2, `src/peer.rs:1146`) and pings. `WorkerCore` (used only by
the Model) dropped that evidence because the acquisition asks for a helper without a
destination; it now names the resolved helper (`core/src/read/worker.rs` `feed`). At 10,000:
2,082 helper probe timeouts all yield silence evidence, 1,651 pings (racer-sim 3,986).

Cold start 400 origin requests (36 against 30, before `FlightFull`). Ranks 1 and 2 went to
origin after rank 0 answered their delegated `Acquire` with its flight's waiter cap, a signed
`Overloaded` the walk took as a shed. Fixed by `FlightFull` (above).

Slow origin (port in progress, `scenarios::slow_origin`, not in the catalog). racer-sim's
`relay_shed_rank0_probes_do_not_overlap_a_slow_origin_fetch` (seed 3) asserts one origin
request per page. The Model (seed 3) asks origin 144 times for 64 pages (max 3 per page) and
fails 141 of 3,072 reads. `MODEL_DIAG=1`: rank 0 fetches all 64 pages; ranks 1 and 2 fetch 80
times after rank 0 answered their probe `Miss` (`origin:rankSome(1):OMiss`). Those probes are
copy-only (`owner_probe:copy_onlytrue:attempts0`, 101 of 262 owner-to-owner probes): the owner
serving a reader's request did not have the attempts to delegate (`walk.rs:657-661` needs
`left + 2`), so it probed rank 0 copy-only (`walk.rs:674-677`), got a miss while rank 0 was still
fetching, and ended `End::Origin`. Open question for the next session: whether racer-sim funds
owner serving reads differently (reader `owner_funding`, retries' budgets) or this is a Model
divergence; compare the inherited `route.attempts` at owners with racer-sim's `serve`.

Earlier leads:

- Shared route cache (refuted). A cache entry's routes depend only on its `PathKey`
  (`topology/src/paths.rs:165-180`), so the node that searched does not change which relays
  carry a request. Per-node caches of production's 128 entries (`src/worker.rs:262`) gave 130
  failures at 10,000 (against 134) and 18 KB more per node, so the Model keeps one cache.
- Search latency (refuted for now). No Model search found its worker's 8 slots busy at 10,000
  or 50,000. racer-sim's queued searches come from its 64-entry queue, which production does
  not have (see Route search slots); without it racer-sim fails 3.9x more reads at 50,000.
- Relay hold (matches production): `src/peer/server.rs:540-560` reserves the `Relay` transfer
  and awaits `forward_inner`; `:778` clears it after the relayed body is written.
- Relay hold breakdown (`MODEL_DIAG=1`, `relay_hold:*`, `relay_shed:*`): every shed is at the
  first relay (`hop1`) of a probe, never a ping.

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
| 10,000 | 930,942,976 B | 93,094 B | 22 s | per-worker crypto helpers, relay quotas, routed pings |
| 50,000 | 4,673,978,368 B | 93,479 B | 201 s | per-worker crypto helpers, relay quotas, routed pings |

At 50,000 nodes the Model uses about 83% of racer-sim's per-node peak (85,616 B against
103,189 B) and runs in minutes where racer-sim's `full-cold-start` takes 39 min. The
workloads differ: racer-sim's `full-cold-start` runs the full control plane, and the Model
runs the contention cold start (every node reads one 16-page object).
